package models

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// ErrAdopted reports a call on a catalogue that has been handed to Adopt.
var ErrAdopted = errors.New("models: this catalogue was adopted by another; use that one")

// Catalog is the install-wide model object: every provider and model, their
// clients and their health, and the models of each purpose. Its methods are safe from
// any goroutine. It has no exported fields; what it returns is a copy.
//
// Readers take an immutable snapshot (a generation) from an atomic pointer;
// writers build the next snapshot under one mutex. No lock is held across a
// model call, an Observer call or a SelectionStore call.
type Catalog struct {
	mu    sync.Mutex
	gen   atomic.Pointer[generation]
	spent atomic.Bool
	book  *parkedBook
}

// generation is one immutable snapshot of a catalogue's contents. Writers
// copy it; nothing in it is changed once it is published.
type generation struct {
	cfg       *settings
	providers map[string]modelspec.Provider
	models    map[string]*modelEntry
	order     []string // model names, in the order they were added
	sets      map[string]*setData
	health    *health
	warned    *sync.Map // model name → struct{}: ignored bypass flags of an isolated client, logged once
}

// modelEntry is one model. It is immutable: a change makes a new entry that
// shares the client slot.
type modelEntry struct {
	def        modelspec.Model
	prov       modelspec.Provider
	disabled   bool
	registered bool // the client was registered by the host
	slot       *clientSlot
}

func (g *generation) clone() *generation {
	n := *g
	n.providers = maps.Clone(g.providers)
	n.models = maps.Clone(g.models)
	n.order = slices.Clone(g.order)
	n.sets = maps.Clone(g.sets)
	return &n
}

// New returns an empty catalogue.
func New(opts ...Option) *Catalog {
	cfg := defaultSettings()
	for _, o := range opts {
		o(cfg)
	}
	c := &Catalog{book: newParkedBook()}
	c.gen.Store(&generation{
		cfg:       cfg,
		providers: make(map[string]modelspec.Provider),
		models:    make(map[string]*modelEntry),
		sets:      make(map[string]*setData),
		health:    newHealth(cfg.clock, newPolicy(cfg.cooldown), c.book),
		warned:    &sync.Map{},
	})
	return c
}

// update runs fn on a copy of the current generation under the writer lock
// and publishes the copy when fn succeeds. Slots fn returns are retired after
// the lock is released.
func (c *Catalog) update(fn func(g *generation) ([]*clientSlot, error)) error {
	c.mu.Lock()
	if c.spent.Load() {
		c.mu.Unlock()
		return ErrAdopted
	}
	next := c.gen.Load().clone()
	retire, err := fn(next)
	if err == nil {
		c.gen.Store(next)
	}
	c.mu.Unlock()
	for _, s := range retire {
		s.retire()
	}
	return err
}

// AddProvider adds a provider. Its name must be new and its protocol valid
// (modelspec.Provider.ValidateProtocol).
func (c *Catalog) AddProvider(p modelspec.Provider) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return errors.New("models: provider name is required")
	}
	if err := p.ValidateProtocol(); err != nil {
		return fmt.Errorf("models: provider %q: %w", p.Name, err)
	}
	return c.update(func(g *generation) ([]*clientSlot, error) {
		if _, dup := g.providers[p.Name]; dup {
			return nil, fmt.Errorf("models: provider %q already exists", p.Name)
		}
		g.providers[p.Name] = p
		return nil, nil
	})
}

// Use assigns a model to a purpose when it is added (Add).
type Use struct {
	purpose  Purpose
	priority int
}

// For is a Use: the model serves purpose p at the given priority (lower
// first; equal priorities keep the order they were added in).
func For(p Purpose, priority int) Use {
	return Use{purpose: p, priority: priority}
}

// Add adds a model. It must be valid (modelspec.Model.Validate), its name new
// and its provider added. The model starts enabled, whatever m.Enabled says;
// Disable turns it off. uses assign it to purposes.
func (c *Catalog) Add(m modelspec.Model, uses ...Use) error {
	m = cloneModel(m)
	m.ModelName = strings.TrimSpace(m.ModelName)
	if err := m.Validate(); err != nil {
		return fmt.Errorf("models: %w", err)
	}
	for _, u := range uses {
		if err := checkPurpose(u.purpose); err != nil {
			return err
		}
	}
	return c.update(func(g *generation) ([]*clientSlot, error) {
		if _, dup := g.models[m.ModelName]; dup {
			return nil, fmt.Errorf("models: model %q already exists", m.ModelName)
		}
		prov, ok := g.providers[m.Provider]
		if !ok {
			return nil, fmt.Errorf("models: model %q: unknown provider %q", m.ModelName, m.Provider)
		}
		g.models[m.ModelName] = newEntry(g.cfg, m, prov)
		g.order = append(g.order, m.ModelName)
		for _, u := range uses {
			if err := g.assign("", u.purpose, m.ModelName, u.priority); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
}

func newEntry(cfg *settings, m modelspec.Model, prov modelspec.Provider) *modelEntry {
	e := &modelEntry{def: m, prov: prov}
	e.slot = newSlot(func() (spawnllm.LLMProvider, error) { return buildClient(cfg, &e.def, &e.prov) })
	return e
}

// Remove removes a model, from the catalogue and from every purpose. Its
// client is closed once the calls in flight on it have ended.
func (c *Catalog) Remove(name string) error {
	return c.update(func(g *generation) ([]*clientSlot, error) {
		e, ok := g.models[name]
		if !ok {
			return nil, fmt.Errorf("models: unknown model %q", name)
		}
		delete(g.models, name)
		g.order = slices.DeleteFunc(g.order, func(n string) bool { return n == name })
		for id, sd := range g.sets {
			if sd.has(name) {
				g.sets[id] = sd.without(name)
			}
		}
		return []*clientSlot{e.slot}, nil
	})
}

// Info describes one model.
type Info struct {
	Model    modelspec.Model    // its definition (a copy)
	Provider modelspec.Provider // its provider's
	Enabled  bool               // enabled in the catalogue
	Health   Health
}

// Get returns the model named name.
func (c *Catalog) Get(name string) (Info, bool) {
	g := c.gen.Load()
	e, ok := g.models[name]
	if !ok {
		return Info{}, false
	}
	return Info{
		Model:    cloneModel(e.def),
		Provider: e.prov,
		Enabled:  !e.disabled,
		Health:   g.health.state(name, e.prov.Name, e.def.Model),
	}, true
}

// Names returns the model names in the order they were added.
func (c *Catalog) Names() []string {
	return slices.Clone(c.gen.Load().order)
}

// Health returns the health of every model, ordered by name.
func (c *Catalog) Health() []Health {
	g := c.gen.Load()
	out := make([]Health, 0, len(g.models))
	for name, e := range g.models {
		out = append(out, g.health.state(name, e.prov.Name, e.def.Model))
	}
	sortHealth(out)
	return out
}

// ResetHealth clears the cooldown and failure counts of the named models, or
// of every model when none is named, so they are tried again at once. It
// returns how many health records it cleared; models that share a provider
// and wire model share one record.
func (c *Catalog) ResetHealth(names ...string) int {
	g := c.gen.Load()
	if len(names) == 0 {
		return g.health.clear(nil)
	}
	keys := make([]string, 0, len(names))
	for _, n := range names {
		if e, ok := g.models[n]; ok {
			keys = append(keys, spawnllm.ModelKey(e.prov.Name, e.def.Model))
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if len(keys) == 0 {
		return 0
	}
	return g.health.clear(keys)
}

// RegisterClient sets a ready-made client for an added model, in place of the
// one the catalogue would build: for a protocol the SDK does not know. The
// client replaced is closed once its calls in flight have ended.
func (c *Catalog) RegisterClient(name string, cl spawnllm.LLMProvider) error {
	if cl == nil {
		return errors.New("models: nil client")
	}
	return c.update(func(g *generation) ([]*clientSlot, error) {
		e, ok := g.models[name]
		if !ok {
			return nil, fmt.Errorf("models: unknown model %q", name)
		}
		n := *e
		n.registered = true
		n.slot = registeredSlot(cl)
		g.models[name] = &n
		return []*clientSlot{e.slot}, nil
	})
}

// Enable turns the named models on. An unknown name is an error and nothing
// is changed.
func (c *Catalog) Enable(names ...string) error { return c.setDisabled(names, false) }

// Disable turns the named models off: every chain skips them as it skips a
// parked model. An unknown name is an error and nothing is changed.
func (c *Catalog) Disable(names ...string) error { return c.setDisabled(names, true) }

func (c *Catalog) setDisabled(names []string, off bool) error {
	return c.update(func(g *generation) ([]*clientSlot, error) {
		for _, n := range names {
			if _, ok := g.models[n]; !ok {
				return nil, fmt.Errorf("models: unknown model %q", n)
			}
		}
		for _, n := range names {
			e := *g.models[n]
			e.disabled = off
			g.models[n] = &e
		}
		return nil, nil
	})
}

// Enabled reports whether the model is enabled.
func (c *Catalog) Enabled(name string) bool {
	e, ok := c.gen.Load().models[name]
	return ok && !e.disabled
}

// Diff is what Adopt changed, for the host to log.
type Diff struct {
	Added   []string // models new in the adopted catalogue
	Removed []string // models no longer in it
	Changed []string // models whose definition or provider changed
}

// Adopt replaces the contents of c with those of next, in one step, and
// returns what changed. next must be a catalogue built for this (New, Build)
// and not used after: it is spent, and its methods return ErrAdopted.
//
//   - Every error was returned while next was built, so Adopt cannot fail
//     half-way.
//   - c stays the one the agents hold.
//   - A chain begun before Adopt finishes on what it pinned, its clients
//     included; every chain begun after uses the new contents.
//   - Health is not carried over: every model starts available.
//   - A model whose definition and provider are unchanged keeps its client.
//     Any other old client is closed once its last call in flight ends.
//   - next's options replace c's.
func (c *Catalog) Adopt(next *Catalog) Diff {
	if next == nil || next == c {
		return Diff{}
	}
	next.mu.Lock()
	if next.spent.Swap(true) {
		next.mu.Unlock()
		return Diff{}
	}
	ng := next.gen.Load()
	next.mu.Unlock()

	var diff Diff
	var retire []*clientSlot
	c.mu.Lock()
	if c.spent.Load() {
		c.mu.Unlock()
		return Diff{}
	}
	old := c.gen.Load()
	ng = ng.clone()
	ng.health = newHealth(ng.cfg.clock, newPolicy(ng.cfg.cooldown), c.book)
	ng.warned = &sync.Map{}
	for _, name := range ng.order {
		ne := ng.models[name]
		oe, existed := old.models[name]
		switch {
		case !existed:
			diff.Added = append(diff.Added, name)
		case !reflect.DeepEqual(oe.def, ne.def) || oe.prov != ne.prov:
			diff.Changed = append(diff.Changed, name)
		case !oe.registered && !ne.registered:
			// Unchanged: keep the client, and its connections, already built.
			if ne.slot.peek() != nil {
				retire = append(retire, ne.slot)
			}
			kept := *ne
			kept.slot = oe.slot
			ng.models[name] = &kept
		}
	}
	for _, name := range old.order {
		if _, ok := ng.models[name]; !ok {
			diff.Removed = append(diff.Removed, name)
		}
	}
	for name, oe := range old.models {
		if ne, ok := ng.models[name]; !ok || ne.slot != oe.slot {
			retire = append(retire, oe.slot)
		}
	}
	c.gen.Store(ng)
	c.mu.Unlock()
	for _, s := range retire {
		s.retire()
	}
	return diff
}

// cloneModel returns a deep copy of m.
func cloneModel(m modelspec.Model) modelspec.Model {
	m.ExtraArgs = slices.Clone(m.ExtraArgs)
	m.DropParams = slices.Clone(m.DropParams)
	m.Env = maps.Clone(m.Env)
	if m.ExtraBody != nil {
		if body, ok := cloneJSON(m.ExtraBody).(map[string]any); ok {
			m.ExtraBody = body
		}
	}
	return m
}

func cloneJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = cloneJSON(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = cloneJSON(x)
		}
		return out
	default:
		return v
	}
}
