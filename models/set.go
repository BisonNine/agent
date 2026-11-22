package models

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/accounting"
)

// Purpose is what a model is used for. It is an open set: a host adds its
// own simply by using them.
type Purpose string

// The purposes the SDK itself uses.
const (
	Chat      Purpose = "chat"      // the agent's turns
	Summarize Purpose = "summarize" // compaction summaries
	Memory    Purpose = "memory"    // cognitive-memory consolidation
	Worker    Purpose = "worker"    // sub-agents; falls back to Chat
)

// Scope narrows a selection, e.g. to one session. The empty scope is the
// set's default.
type Scope string

func checkPurpose(p Purpose) error {
	if strings.TrimSpace(string(p)) == "" {
		return errors.New("models: purpose is required")
	}
	return nil
}

// AssignOption configures one assignment.
type AssignOption func(*assignment)

// WithSettings attaches opaque settings to an assignment; List and the
// chain's candidates return them.
func WithSettings(v any) AssignOption {
	return func(a *assignment) { a.settings = v }
}

// WithEnabled sets whether the assignment is on; it is by default. An
// assignment that is off is skipped as a disabled model is.
func WithEnabled(on bool) AssignOption {
	return func(a *assignment) { a.off = !on }
}

// assignment is one model in a purpose's list.
type assignment struct {
	name     string
	priority int
	seq      int // insertion order, for equal priorities
	settings any
	off      bool
}

// setData is one set's assignments. Immutable once published.
type setData struct {
	lists    map[Purpose][]assignment // each sorted by priority, then seq
	disabled map[string]bool
	seq      int
}

func (sd *setData) clone() *setData {
	if sd == nil {
		return &setData{lists: map[Purpose][]assignment{}, disabled: map[string]bool{}}
	}
	n := &setData{lists: make(map[Purpose][]assignment, len(sd.lists)), disabled: maps.Clone(sd.disabled), seq: sd.seq}
	for p, l := range sd.lists {
		n.lists[p] = slices.Clone(l)
	}
	if n.disabled == nil {
		n.disabled = map[string]bool{}
	}
	return n
}

func (sd *setData) has(name string) bool {
	if sd == nil {
		return false
	}
	for _, l := range sd.lists {
		if slices.ContainsFunc(l, func(a assignment) bool { return a.name == name }) {
			return true
		}
	}
	return false
}

func (sd *setData) without(name string) *setData {
	n := sd.clone()
	for p, l := range n.lists {
		l = slices.DeleteFunc(l, func(a assignment) bool { return a.name == name })
		if len(l) == 0 {
			delete(n.lists, p)
		} else {
			n.lists[p] = l
		}
	}
	delete(n.disabled, name)
	return n
}

func sortAssignments(l []assignment) {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].priority != l[j].priority {
			return l[i].priority < l[j].priority
		}
		return l[i].seq < l[j].seq
	})
}

// assign adds an assignment to set id in g.
func (g *generation) assign(id string, p Purpose, name string, priority int, opts []AssignOption) error {
	if err := checkPurpose(p); err != nil {
		return err
	}
	if _, ok := g.models[name]; !ok {
		return fmt.Errorf("models: unknown model %q", name)
	}
	sd := g.sets[id].clone()
	if slices.ContainsFunc(sd.lists[p], func(a assignment) bool { return a.name == name }) {
		return fmt.Errorf("models: model %q is already assigned to %s", name, p)
	}
	a := assignment{name: name, priority: priority, seq: sd.seq}
	for _, o := range opts {
		o(&a)
	}
	sd.seq++
	sd.lists[p] = append(sd.lists[p], a)
	sortAssignments(sd.lists[p])
	g.sets[id] = sd
	return nil
}

// effective returns the list that serves purpose p in set id, following the
// declared fallback rules when p has none, and the purpose it belongs to.
func (g *generation) effective(id string, p Purpose) ([]assignment, Purpose) {
	sd := g.sets[id]
	seen := map[Purpose]bool{}
	for cur := p; cur != "" && !seen[cur]; cur = g.cfg.fallbacks[cur] {
		seen[cur] = true
		if sd != nil && len(sd.lists[cur]) > 0 {
			return sd.lists[cur], cur
		}
	}
	return nil, p
}

// usable reports whether model name may be used by set id: on in the
// catalogue and in the set.
func (g *generation) usable(id, name string) (bool, bool) {
	e, ok := g.models[name]
	if !ok {
		return false, false
	}
	if e.disabled {
		return false, true
	}
	if sd := g.sets[id]; sd != nil && sd.disabled[name] {
		return false, true
	}
	return true, true
}

// Set is one agent's models, by purpose and priority: a view onto its
// catalogue. The handle stays valid across Catalog.Adopt. It has no exported
// fields; what it returns is a copy.
type Set struct {
	cat *Catalog
	id  string
}

// ID returns the set's agent id ("" for the default set).
func (s *Set) ID() string { return s.id }

func (s *Set) update(fn func(g *generation) error) error {
	return s.cat.update(func(g *generation) ([]*clientSlot, error) { return nil, fn(g) })
}

// Assign adds model name to purpose p at priority (lower first; equal
// priorities keep the order they were assigned in). The model must be in the
// catalogue and not already assigned to p.
func (s *Set) Assign(p Purpose, name string, priority int, opts ...AssignOption) error {
	return s.update(func(g *generation) error { return g.assign(s.id, p, name, priority, opts) })
}

// Unassign removes model name from purpose p, and reports whether it was
// there. A selection of it for p reverts to the default.
func (s *Set) Unassign(p Purpose, name string) bool {
	removed := false
	err := s.update(func(g *generation) error {
		sd := g.sets[s.id]
		if sd == nil {
			return nil
		}
		i := slices.IndexFunc(sd.lists[p], func(a assignment) bool { return a.name == name })
		if i < 0 {
			return nil
		}
		n := sd.clone()
		n.lists[p] = slices.Delete(n.lists[p], i, i+1)
		if len(n.lists[p]) == 0 {
			delete(n.lists, p)
		}
		g.sets[s.id] = n
		removed = true
		s.cat.state.prune(g)
		return nil
	})
	return err == nil && removed
}

// Reorder sets the order of purpose p's models: names must be exactly the
// models assigned to p, and each gets its position as its priority.
func (s *Set) Reorder(p Purpose, names []string) error {
	return s.update(func(g *generation) error {
		sd := g.sets[s.id]
		var cur []assignment
		if sd != nil {
			cur = sd.lists[p]
		}
		if len(names) != len(cur) {
			return fmt.Errorf("models: reorder %s: %d names given, %d assigned", p, len(names), len(cur))
		}
		n := sd.clone()
		out := make([]assignment, 0, len(cur))
		for i, name := range names {
			j := slices.IndexFunc(cur, func(a assignment) bool { return a.name == name })
			if j < 0 || slices.ContainsFunc(out, func(a assignment) bool { return a.name == name }) {
				return fmt.Errorf("models: reorder %s: %q is not assigned once", p, name)
			}
			a := cur[j]
			a.priority, a.seq = i, n.seq
			n.seq++
			out = append(out, a)
		}
		n.lists[p] = out
		g.sets[s.id] = n
		return nil
	})
}

// Entry is one model of a purpose's list.
type Entry struct {
	Name     string
	Purpose  Purpose
	Priority int
	Settings any
	Enabled  bool // usable: on in the catalogue, in the set and in the assignment
	Health   Health
}

func entryOf(g *generation, id string, p Purpose, a assignment) Entry {
	e := g.models[a.name]
	ok, _ := g.usable(id, a.name)
	return Entry{
		Name: a.name, Purpose: p, Priority: a.priority, Settings: a.settings,
		Enabled: ok && !a.off,
		Health:  g.health.state(a.name, e.prov.Name, e.def.Model),
	}
}

// List returns purpose p's own models in priority order (not those of the
// purpose it falls back to).
func (s *Set) List(p Purpose) []Entry {
	g := s.cat.gen.Load()
	sd := g.sets[s.id]
	if sd == nil {
		return nil
	}
	out := make([]Entry, 0, len(sd.lists[p]))
	for _, a := range sd.lists[p] {
		out = append(out, entryOf(g, s.id, p, a))
	}
	return out
}

// Purposes returns the purposes the set has models for, sorted.
func (s *Set) Purposes() []Purpose {
	sd := s.cat.gen.Load().sets[s.id]
	if sd == nil {
		return nil
	}
	out := slices.Collect(maps.Keys(sd.lists))
	slices.Sort(out)
	return out
}

// Enable turns the named models on for this set. A name not in the
// catalogue is an error and nothing is changed.
func (s *Set) Enable(names ...string) error { return s.setDisabled(names, false) }

// Disable turns the named models off for this set: its chains skip them as
// they skip a parked model. A selection of one is kept.
func (s *Set) Disable(names ...string) error { return s.setDisabled(names, true) }

func (s *Set) setDisabled(names []string, off bool) error {
	return s.update(func(g *generation) error {
		for _, n := range names {
			if _, ok := g.models[n]; !ok {
				return fmt.Errorf("models: unknown model %q", n)
			}
		}
		sd := g.sets[s.id].clone()
		for _, n := range names {
			if off {
				sd.disabled[n] = true
			} else {
				delete(sd.disabled, n)
			}
		}
		g.sets[s.id] = sd
		return nil
	})
}

// Enabled reports whether the model is on for this set: in the catalogue and
// in the set.
func (s *Set) Enabled(name string) bool {
	ok, _ := s.cat.gen.Load().usable(s.id, name)
	return ok
}

// SelectionStore persists selections (Set.Select), so they survive a
// restart. The catalogue reads a selection the first time it needs it and
// writes each change through; it holds no lock across either call. An empty
// name means no selection. A write error is logged and the selection is kept
// in memory.
type SelectionStore interface {
	LoadSelection(set string, p Purpose, scope Scope) (string, error)
	SaveSelection(set string, p Purpose, scope Scope, name string) error
}

// Select makes model name the first one tried for purpose p, in the given
// scope or, without one, as the set's default. name must be in the list that
// serves p. It takes effect at the next Begin.
func (s *Set) Select(p Purpose, name string, scope ...Scope) error {
	g := s.cat.gen.Load()
	if s.cat.spent.Load() {
		return ErrAdopted
	}
	list, _ := g.effective(s.id, p)
	if !slices.ContainsFunc(list, func(a assignment) bool { return a.name == name }) {
		return fmt.Errorf("models: %q is not assigned to %s", name, p)
	}
	k := selKey{set: s.id, purpose: p, scope: scopeOf(scope)}
	s.cat.state.set(k, name)
	s.save(g, k, name)
	return nil
}

// Reset removes the selection for purpose p in the given scope (or the set's
// default), so the top-priority model is tried first again.
func (s *Set) Reset(p Purpose, scope ...Scope) {
	k := selKey{set: s.id, purpose: p, scope: scopeOf(scope)}
	s.cat.state.set(k, "")
	s.save(s.cat.gen.Load(), k, "")
}

func (s *Set) save(g *generation, k selKey, name string) {
	if g.cfg.store == nil {
		return
	}
	if err := g.cfg.store.SaveSelection(k.set, k.purpose, k.scope, name); err != nil {
		g.cfg.logger.Warn("model selection not stored; it is kept until restart",
			"component", component, "set", k.set, "purpose", string(k.purpose), "scope", string(k.scope), "error", err.Error())
	}
}

func scopeOf(scope []Scope) Scope {
	if len(scope) == 0 {
		return ""
	}
	return scope[0]
}

// selectedName returns the model selected for p in scope: the scope's own
// selection, else the set's default selection, else "". A selection that is
// not in list is ignored.
func (s *Set) selectedName(g *generation, p Purpose, scope Scope, list []assignment) string {
	keys := []selKey{{set: s.id, purpose: p, scope: scope}}
	if scope != "" {
		keys = append(keys, selKey{set: s.id, purpose: p})
	}
	for _, k := range keys {
		name := s.cat.state.get(k, g.cfg)
		if name != "" && slices.ContainsFunc(list, func(a assignment) bool { return a.name == name }) {
			return name
		}
	}
	return ""
}

// Selected returns the entry tried first for purpose p in scope: the
// selection, or the top-priority model. It is the zero Entry when p has no
// models.
func (s *Set) Selected(p Purpose, scope ...Scope) Entry {
	g := s.cat.gen.Load()
	list, used := g.effective(s.id, p)
	if len(list) == 0 {
		return Entry{}
	}
	name := s.selectedName(g, p, scopeOf(scope), list)
	for _, a := range list {
		if name == "" || a.name == name {
			return entryOf(g, s.id, used, a)
		}
	}
	return Entry{}
}

// Serving says which model is serving purpose p in a scope, and why it is
// not the selected one when they differ.
type Serving struct {
	Selected string    // the model tried first
	Serving  string    // the model that answered last; "" before any answer
	Since    time.Time // when Serving last changed
	Reason   string    // why the selected model did not answer: a failover reason, "disabled", or ""
	Attempts []Attempt // the models passed over by the last call
}

// Current returns what is serving purpose p in scope.
func (s *Set) Current(p Purpose, scope ...Scope) Serving {
	sc := scopeOf(scope)
	out := s.cat.state.serving(selKey{set: s.id, purpose: p, scope: sc})
	out.Selected = s.Selected(p, sc).Name
	return out
}

// Begin pins the models for one turn of purpose p in scope: the selected
// model first, then the rest in priority order. The chain does not change
// when the set or the catalogue does, except that each call re-reads whether
// a model is enabled.
func (s *Set) Begin(p Purpose, scope ...Scope) *Chain {
	g := s.cat.gen.Load()
	sc := scopeOf(scope)
	list, used := g.effective(s.id, p)
	ch := &Chain{cat: s.cat, gen: g, set: s.id, purpose: p, used: used, scope: sc}
	if len(list) == 0 {
		return ch
	}
	first := s.selectedName(g, p, sc, list)
	if first == "" {
		first = list[0].name
	}
	ordered := make([]assignment, 0, len(list))
	for _, a := range list {
		if a.name == first {
			ordered = append(ordered, a)
		}
	}
	for _, a := range list {
		if a.name != first {
			ordered = append(ordered, a)
		}
	}
	for _, a := range ordered {
		ch.cands = append(ch.cands, candidateOf(g, a))
		ch.off = append(ch.off, a.off)
	}
	return ch
}

// Caller returns a Caller for purpose p: each Complete begins a new chain,
// so it follows selections and Adopt. Its calls are made for id.
func (s *Set) Caller(p Purpose, id accounting.Identity, scope ...Scope) Caller {
	sc := scopeOf(scope)
	return callerFunc(func() (*Chain, accounting.Identity) { return s.Begin(p, sc), id })
}

// The catalogue's own methods for its default set.

// Assign is Default().Assign.
func (c *Catalog) Assign(p Purpose, name string, priority int, opts ...AssignOption) error {
	return c.Default().Assign(p, name, priority, opts...)
}

// List is Default().List.
func (c *Catalog) List(p Purpose) []Entry { return c.Default().List(p) }

// Select is Default().Select.
func (c *Catalog) Select(p Purpose, name string, scope ...Scope) error {
	return c.Default().Select(p, name, scope...)
}

// Selected is Default().Selected.
func (c *Catalog) Selected(p Purpose, scope ...Scope) Entry { return c.Default().Selected(p, scope...) }

// Reset is Default().Reset.
func (c *Catalog) Reset(p Purpose, scope ...Scope) { c.Default().Reset(p, scope...) }

// Current is Default().Current.
func (c *Catalog) Current(p Purpose, scope ...Scope) Serving {
	return c.Default().Current(p, scope...)
}

// Begin is Default().Begin.
func (c *Catalog) Begin(p Purpose, scope ...Scope) *Chain { return c.Default().Begin(p, scope...) }

// selKey names one selection.
type selKey struct {
	set     string
	purpose Purpose
	scope   Scope
}

// runState is what a catalogue learns while it runs: selections and what is
// serving now. It is kept across Adopt, matched by model name.
type runState struct {
	mu       sync.Mutex
	selected map[selKey]string
	loaded   map[selKey]bool
	serve    map[selKey]Serving
}

func newRunState() *runState {
	return &runState{selected: map[selKey]string{}, loaded: map[selKey]bool{}, serve: map[selKey]Serving{}}
}

func (r *runState) set(k selKey, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaded[k] = true
	if name == "" {
		delete(r.selected, k)
		return
	}
	r.selected[k] = name
}

// get returns the selection for k, reading it from the store the first time.
func (r *runState) get(k selKey, cfg *settings) string {
	r.mu.Lock()
	name, loaded := r.selected[k], r.loaded[k]
	r.mu.Unlock()
	if loaded || cfg.store == nil {
		return name
	}
	stored, err := cfg.store.LoadSelection(k.set, k.purpose, k.scope)
	if err != nil {
		cfg.logger.Warn("model selection could not be read; using the default",
			"component", component, "set", k.set, "purpose", string(k.purpose), "scope", string(k.scope), "error", err.Error())
		stored = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.loaded[k] {
		r.loaded[k] = true
		if stored != "" {
			r.selected[k] = stored
		}
	}
	return r.selected[k]
}

func (r *runState) serving(k selKey) Serving {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.serve[k]
	s.Attempts = slices.Clone(s.Attempts)
	return s
}

// served records a call's outcome: answered is the model that answered, ""
// when none did.
func (r *runState) served(k selKey, answered string, reason string, attempts []Attempt, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.serve[k]
	if answered != "" && answered != s.Serving {
		s.Serving, s.Since = answered, now
	}
	s.Reason = reason
	s.Attempts = slices.Clone(attempts)
	r.serve[k] = s
}

type prunedSel struct {
	key   selKey
	model string
}

// prune drops the selections whose model no longer serves their purpose in
// g, and the serving records of models g no longer has, and returns the
// selections it dropped.
func (r *runState) prune(g *generation) []prunedSel {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []prunedSel
	for k, name := range r.selected {
		list, _ := g.effective(k.set, k.purpose)
		if !slices.ContainsFunc(list, func(a assignment) bool { return a.name == name }) {
			delete(r.selected, k)
			out = append(out, prunedSel{key: k, model: name})
		}
	}
	for k, s := range r.serve {
		if _, ok := g.models[s.Serving]; s.Serving != "" && !ok {
			delete(r.serve, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].key, out[j].key
		if a.set != b.set {
			return a.set < b.set
		}
		if a.purpose != b.purpose {
			return a.purpose < b.purpose
		}
		return a.scope < b.scope
	})
	return out
}
