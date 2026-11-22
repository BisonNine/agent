package models

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// errUnchanged, returned by an update function, leaves the catalogue as it
// was without an error.
var errUnchanged = errors.New("unchanged")

// ErrAdopted reports a call on a catalogue that has been handed to Adopt.
var ErrAdopted = errors.New("models: this catalogue was adopted by another; use that one")

// Catalog is the install-wide model object: every provider and model, their
// clients and their health, and one Set per agent. Its methods are safe from
// any goroutine. It has no exported fields; what it returns is a copy.
//
// Readers take an immutable snapshot (a generation) from an atomic pointer;
// writers build the next snapshot under one mutex. No lock is held across a
// model call, an Observer call or a SelectionStore call.
type Catalog struct {
	mu      sync.Mutex
	gen     atomic.Pointer[generation]
	handles map[string]*Set // under mu
	spent   atomic.Bool
	book    *parkedBook
	state   *runState
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

// New returns an empty catalogue with a default set (Set("")).
func New(opts ...Option) *Catalog {
	cfg := defaultSettings()
	for _, o := range opts {
		o(cfg)
	}
	c := &Catalog{handles: make(map[string]*Set), book: newParkedBook(), state: newRunState()}
	c.gen.Store(&generation{
		cfg:       cfg,
		providers: make(map[string]modelspec.Provider),
		models:    make(map[string]*modelEntry),
		sets:      make(map[string]*setData),
		health:    newHealth(cfg.clock, newPolicy(cfg.cooldown), c.book, cfg.observer),
		warned:    &sync.Map{},
	})
	return c
}

// update runs fn on a copy of the current generation under the writer lock
// and publishes the copy when fn succeeds. Slots fn returns are retired after
// the lock is released.
func (c *Catalog) update(fn func(g *generation) ([]*clientSlot, error)) error {
	_, err := c.change(fn)
	return err
}

// change is update, returning the selections the change reverted. The
// selections it affected are written to the store after the lock is
// released.
func (c *Catalog) change(fn func(g *generation) ([]*clientSlot, error)) ([]Reverted, error) {
	c.mu.Lock()
	if c.spent.Load() {
		c.mu.Unlock()
		return nil, ErrAdopted
	}
	prev := c.gen.Load()
	next := prev.clone()
	retire, err := fn(next)
	var saves []selSave
	var reverted []Reverted
	if err == nil {
		c.gen.Store(next)
		saves, reverted = c.state.reconcile(prev, next)
	}
	c.mu.Unlock()
	if errors.Is(err, errUnchanged) {
		err = nil
	}
	for _, s := range retire {
		s.retire()
	}
	writeSelections(next.cfg, saves)
	return reverted, err
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

// Use assigns a model to a purpose of the default set when it is added (Add).
type Use struct {
	purpose  Purpose
	priority int
	opts     []AssignOption
}

// For is a Use: the model serves purpose p in the default set at the given
// priority (lower first).
func For(p Purpose, priority int, opts ...AssignOption) Use {
	return Use{purpose: p, priority: priority, opts: opts}
}

// Add adds a model. It must be valid (modelspec.Model.Validate), its name new
// and its provider added. The model starts enabled, whatever m.Enabled says;
// Disable turns it off. uses assign it in the default set.
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
			if err := g.assign("", u.purpose, m.ModelName, u.priority, u.opts); err != nil {
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

// Remove removes a model, from the catalogue and from every set. A selection
// of it reverts to the default. Its client is closed once the calls in flight
// on it have ended.
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
// host owns the client: the catalogue never closes it. A client the
// catalogue had built in its place is closed once its calls in flight have
// ended. Registering the client the model already has changes nothing.
func (c *Catalog) RegisterClient(name string, cl spawnllm.LLMProvider) error {
	if cl == nil {
		return errors.New("models: nil client")
	}
	return c.update(func(g *generation) ([]*clientSlot, error) {
		e, ok := g.models[name]
		if !ok {
			return nil, fmt.Errorf("models: unknown model %q", name)
		}
		if e.registered && sameClient(e.slot.peek(), cl) {
			return nil, errUnchanged
		}
		n := *e
		n.registered = true
		n.slot = registeredSlot(cl)
		g.models[name] = &n
		return []*clientSlot{e.slot}, nil
	})
}

// Enable turns the named models on for every agent. An unknown name is an
// error and nothing is changed.
func (c *Catalog) Enable(names ...string) error { return c.setDisabled(names, false) }

// Disable turns the named models off for every agent: every chain skips them
// as it skips a parked model. A selection of one is kept. An unknown name is
// an error and nothing is changed.
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

// Enabled reports whether the model is enabled in the catalogue.
func (c *Catalog) Enabled(name string) bool {
	e, ok := c.gen.Load().models[name]
	return ok && !e.disabled
}

// Set returns agent id's set, created empty on first use. The handle stays
// valid across Adopt. Set("") is the default set.
func (c *Catalog) Set(id string) *Set {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.handles[id]
	if !ok {
		s = &Set{cat: c, id: id}
		c.handles[id] = s
	}
	return s
}

// Default returns the default set, Set("").
func (c *Catalog) Default() *Set { return c.Set("") }

// Diff is what Adopt changed, for the host to log.
type Diff struct {
	Added    []string   // models new in the adopted catalogue
	Removed  []string   // models no longer in it
	Changed  []string   // models whose definition or provider changed
	Reverted []Reverted // selections whose model is no longer assigned
}

// Reverted is a selection that reverted to the default.
type Reverted struct {
	Set     string
	Purpose Purpose
	Scope   Scope
	Model   string
}

// Adopt replaces the contents of c with those of next, in one step, and
// returns what changed. next must be a catalogue built for this (New, Build)
// and not used after: it is spent, and its methods return ErrAdopted.
//
//   - Every error was returned while next was built, so Adopt cannot fail
//     half-way.
//   - c and its Set handles stay the ones the agents hold.
//   - A chain begun before Adopt finishes on what it pinned, its clients
//     included; every chain begun after uses the new contents.
//   - Selections and what is serving now are carried over by model name; a
//     selection whose model is no longer assigned reverts to the default and
//     is listed in the Diff.
//   - Health is not carried over: every model starts available.
//   - A model whose definition, provider and client options are unchanged
//     keeps the client it has already built (a model whose client options
//     changed is listed as Changed). Any other client the catalogue built is
//     closed once its last call in flight ends; a client the host registered
//     is never closed.
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
	ng.health = newHealth(ng.cfg.clock, newPolicy(ng.cfg.cooldown), c.book, ng.cfg.observer)
	ng.warned = &sync.Map{}
	for _, name := range ng.order {
		ne := ng.models[name]
		oe, existed := old.models[name]
		switch {
		case !existed:
			diff.Added = append(diff.Added, name)
		case !reflect.DeepEqual(oe.def, ne.def) || oe.prov != ne.prov:
			diff.Changed = append(diff.Changed, name)
		case oe.registered || ne.registered:
			// The host's own client: the same one registered again keeps
			// its slot; anything else takes the new one.
			if oe.registered && ne.registered && sameClient(oe.slot.peek(), ne.slot.peek()) {
				kept := *ne
				kept.slot = oe.slot
				ng.models[name] = &kept
			}
		case !sameClientOptions(old.cfg, ng.cfg, &ne.def, &ne.prov):
			diff.Changed = append(diff.Changed, name)
		case oe.slot.peek() != nil:
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
	saves, reverted := c.state.reconcile(old, ng)
	diff.Reverted = reverted
	c.mu.Unlock()
	for _, s := range retire {
		s.retire()
	}
	writeSelections(ng.cfg, saves)
	return diff
}

// cloneModel returns a deep copy of m.
func cloneModel(m modelspec.Model) modelspec.Model {
	m.ExtraArgs = slices.Clone(m.ExtraArgs)
	m.DropParams = slices.Clone(m.DropParams)
	m.Env = maps.Clone(m.Env)
	if m.ExtraBody != nil {
		if body, ok := reflect.TypeAssert[map[string]any](deepCopy(reflect.ValueOf(m.ExtraBody))); ok {
			m.ExtraBody = body
		}
	}
	return m
}

// deepCopy returns a copy of v that shares no map, slice, array element or
// pointer target with it, keeping every value's type. A struct's unexported
// fields are copied as they are.
func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopy(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), deepCopy(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(deepCopy(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(deepCopy(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(deepCopy(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range out.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(deepCopy(v.Field(i)))
			}
		}
		return out
	default:
		return v // scalars
	}
}

// sameClient reports whether a and b are the same client. A client of a type
// that cannot be compared is never the same.
func sameClient(a, b spawnllm.LLMProvider) bool {
	if a == nil || b == nil || !reflect.TypeOf(a).Comparable() || !reflect.TypeOf(b).Comparable() {
		return false
	}
	return a == b
}

// sameClientOptions reports whether a client of model m on prov built with
// options a is what options b would build. The logger must be the same one,
// or one over the same handler; option functions are compared by identity,
// and the base environment and response-log modes by what they return now.
func sameClientOptions(a, b *settings, m *modelspec.Model, prov *modelspec.Provider) bool {
	if a.logContent != b.logContent || !sameLogger(a.logger, b.logger) || !sameFunc(a.declined, b.declined) {
		return false
	}
	if m.ResponseLogFile != "" && a.modesFor(m.ResponseLogFile) != b.modesFor(m.ResponseLogFile) {
		return false
	}
	if modelspec.CLIAgentByProtocol(prov.Protocol) != nil && !slices.Equal(a.baseEnv(), b.baseEnv()) {
		return false
	}
	return true
}

func sameLogger(a, b *slog.Logger) bool {
	if a == b {
		return true
	}
	ha, hb := a.Handler(), b.Handler()
	if !reflect.TypeOf(ha).Comparable() || !reflect.TypeOf(hb).Comparable() {
		return false
	}
	return ha == hb
}

func sameFunc[F any](a, b F) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
