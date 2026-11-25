/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
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
		return fmt.Errorf("models: %w %q", ErrUnknownModel, name)
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
	cat     *Catalog
	id      string
	removed atomic.Bool // set by Catalog.RemoveSet, under cat.mu and the run state's lock
}

// ID returns the set's agent id ("" for the default set).
func (s *Set) ID() string { return s.id }

func (s *Set) update(fn func(g *generation) error) error {
	return s.cat.update(func(g *generation) ([]*clientSlot, error) {
		if s.removed.Load() {
			return nil, s.errRemoved()
		}
		return nil, fn(g)
	})
}

func (s *Set) errRemoved() error {
	return fmt.Errorf("models: set %q was removed; take it again with Catalog.Set", s.id)
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
		return nil
	})
	return err == nil && removed
}

// Assignment is one model of a purpose's list for Replace: its name and
// its per-assignment options. A list is in priority order.
type Assignment struct {
	Name    string
	Options []AssignOption
}

// Replace replaces purpose p's whole list (an empty list removes it); it is
// ReplaceAll for one purpose.
func (s *Set) Replace(p Purpose, list ...Assignment) ([]Reverted, error) {
	return s.ReplaceAll(map[Purpose][]Assignment{p: list})
}

// ReplaceAll replaces the lists of the purposes given, all in one step: a
// host reloading an agent replaces every one of its purposes together, so no
// turn sees some purposes old and some new. Purposes not given are left as
// they are; an empty list removes the purpose. Each list is in priority order
// (its position is the priority).
//
// Everything is validated first (an unknown model or a name twice in a list
// is an error, and nothing changes). Readers see the old lists or the new
// ones, never a mix. A selection, and what is serving, whose model is still in
// its new list is kept; a selection whose model is gone reverts to the
// default and is returned. The store is written as for every list change
// (see SelectionStore). Lists identical to the current ones change nothing.
func (s *Set) ReplaceAll(lists map[Purpose][]Assignment) ([]Reverted, error) {
	return s.cat.change(func(g *generation) ([]*clientSlot, error) {
		sd := g.sets[s.id].clone()
		changed := false
		for p, list := range lists {
			if err := checkPurpose(p); err != nil {
				return nil, err
			}
			out := make([]assignment, 0, len(list))
			for i, a := range list {
				if _, ok := g.models[a.Name]; !ok {
					return nil, fmt.Errorf("models: %w %q", ErrUnknownModel, a.Name)
				}
				if slices.ContainsFunc(out, func(x assignment) bool { return x.name == a.Name }) {
					return nil, fmt.Errorf("models: model %q is listed twice for %s", a.Name, p)
				}
				n := assignment{name: a.Name, priority: i, seq: i}
				for _, o := range a.Options {
					o(&n)
				}
				out = append(out, n)
			}
			if sameAssignments(sd.lists[p], out) {
				continue
			}
			changed = true
			if len(out) == 0 {
				delete(sd.lists, p)
				continue
			}
			for i := range out {
				out[i].seq = sd.seq
				sd.seq++
			}
			sd.lists[p] = out
		}
		if !changed {
			return nil, errUnchanged
		}
		g.sets[s.id] = sd
		return nil, nil
	})
}

// sameAssignments reports whether two lists are the same models in the same
// order with the same priorities, switches and settings.
func sameAssignments(cur, next []assignment) bool {
	return slices.EqualFunc(cur, next, func(a, b assignment) bool {
		return a.name == b.name && a.priority == b.priority && a.off == b.off && sameSettings(a.settings, b.settings)
	})
}

// sameSettings compares opaque settings: equal when both are nil or the same
// comparable value.
func sameSettings(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
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
				return fmt.Errorf("models: %w %q", ErrUnknownModel, n)
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
// restart. An empty name means no selection.
//
// LoadSelection is called the first time a selection is needed. An error
// means "cannot load now": the default is used for that use and the
// selection is read again next time.
//
// SaveSelection is called, after the change is published and with no lock
// held, whenever a selection held in memory may mean something different in
// the store:
//   - Select and Reset;
//   - a selection reverted because its model no longer serves the purpose
//     (Remove, Unassign, Replace, ReplaceAll, Adopt): saved as "";
//   - a selection kept by name whose purpose list changed in members or
//     order (Assign, Unassign, Reorder, Replace, ReplaceAll, Remove, Adopt):
//     saved again with the same name, so a store that keeps a position in
//     the list rather than the name can re-save it (the new list is already
//     what Set.List returns).
//
// Writes are made one at a time, in order, each with the selection memory
// holds at that moment, so the store ends with what memory holds; a write may
// be made by another call's goroutine. A selection never loaded in this
// process is not written. A write error is logged and the selection is kept
// in memory.
type SelectionStore interface {
	LoadSelection(set string, p Purpose, scope Scope) (string, error)
	SaveSelection(set string, p Purpose, scope Scope, name string) error
}

// Select makes model name the first one tried for purpose p, in the given
// scope or, without one, as the set's default. name must be in the list that
// serves p. It takes effect at the next Begin.
func (s *Set) Select(p Purpose, name string, scope ...Scope) error {
	k := selKey{set: s.id, purpose: p, scope: scopeOf(scope)}
	// Checked and recorded under the writer lock, so an Unassign or Remove
	// cannot slip in between; the store is written after it is released.
	s.cat.mu.Lock()
	if s.cat.spent.Load() {
		s.cat.mu.Unlock()
		return ErrAdopted
	}
	if s.cat.life.isClosed() {
		s.cat.mu.Unlock()
		return ErrClosed
	}
	if s.removed.Load() {
		s.cat.mu.Unlock()
		return s.errRemoved()
	}
	g := s.cat.gen.Load()
	list, _ := g.effective(s.id, p)
	if !slices.ContainsFunc(list, func(a assignment) bool { return a.name == name }) {
		s.cat.mu.Unlock()
		return fmt.Errorf("models: %q is not assigned to %s", name, p)
	}
	s.cat.state.set(k, name)
	s.cat.mu.Unlock()
	s.save(g, k, name)
	return nil
}

// Forget drops what the set holds in memory for a scope: its selections and
// what is serving it, for every purpose. The store is not written; a later
// use of the scope reads it again. For a scope that has ended, such as a
// deleted session.
func (s *Set) Forget(scope Scope) {
	s.cat.state.forget(s.id, scope)
}

// Reset removes the selection for purpose p in the given scope (or the set's
// default), so the top-priority model is tried first again.
func (s *Set) Reset(p Purpose, scope ...Scope) {
	if s.removed.Load() {
		return
	}
	k := selKey{set: s.id, purpose: p, scope: scopeOf(scope)}
	s.cat.state.set(k, "")
	s.save(s.cat.gen.Load(), k, "")
}

func (s *Set) save(g *generation, k selKey, name string) {
	s.cat.state.persist(g.cfg, []selSave{{key: k, name: name}})
}

// persist writes selections to the store, if there is one. Writes are made
// one at a time, in the order they were asked for, by whichever goroutine is
// writing, with no lock held during a write; each writes the selection as
// memory holds it at that moment, so the last write of a key matches memory
// even when changes race.
func (r *runState) persist(cfg *settings, saves []selSave) {
	if cfg.store == nil || len(saves) == 0 {
		return
	}
	r.mu.Lock()
	for _, sv := range saves {
		r.writes = append(r.writes, pendingWrite{key: sv.key, cfg: cfg})
	}
	if r.writing {
		r.mu.Unlock()
		return
	}
	r.writing = true
	for len(r.writes) > 0 {
		w := r.writes[0]
		r.writes = r.writes[1:]
		name := r.selected[w.key]
		r.mu.Unlock()
		r.write(w, name)
		r.mu.Lock()
	}
	r.writing = false
	r.mu.Unlock()
}

// write makes one store write; a panic in the store releases the writer.
func (r *runState) write(w pendingWrite, name string) {
	defer func() {
		if p := recover(); p != nil {
			r.mu.Lock()
			r.writing = false
			r.mu.Unlock()
			panic(p)
		}
	}()
	k := w.key
	if err := w.cfg.store.SaveSelection(k.set, k.purpose, k.scope, name); err != nil {
		w.cfg.logger.Warn("model selection not stored; it is kept until restart",
			"component", component, "set", k.set, "purpose", string(k.purpose), "scope", string(k.scope), "error", err.Error())
	}
}

type pendingWrite struct {
	key selKey
	cfg *settings
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
		name := s.cat.state.get(k, g.cfg, s)
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
	ch := &Chain{cat: s.cat, from: s, gen: g, set: s.id, purpose: p, used: used, scope: sc}
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

// Caller returns a Caller for purpose p in scope ("" for the set's default):
// each Complete begins a new chain, so it follows selections and Adopt. Its
// calls are made for id, with the call options given.
func (s *Set) Caller(p Purpose, id accounting.Identity, scope Scope, opts ...CallOption) Caller {
	return &caller{chain: func() *Chain { return s.Begin(p, scope) }, id: id, opts: opts}
}

// Client returns the chat client of model name from the catalogue's current
// contents. It reads and records no health and ignores the enable switches.
// The client stays usable until release is called, which must be done
// exactly once when the caller is done with it; a client Adopt or
// RegisterClient replaced is closed only after that. For a CLI model's
// isolated client, use Begin(...).Isolated(workspace).Client(name).
func (s *Set) Client(name string) (client spawnllm.LLMProvider, release func(), err error) {
	return s.cat.Client(name)
}

// Client is Set.Client.
func (c *Catalog) Client(name string) (client spawnllm.LLMProvider, release func(), err error) {
	if err = c.life.enter(); err != nil {
		return nil, nil, err
	}
	e, ok := c.gen.Load().models[name]
	if !ok {
		c.life.leave()
		return nil, nil, fmt.Errorf("models: %w %q", ErrUnknownModel, name)
	}
	client, release, err = e.slot.acquire()
	return client, c.counted(release, err), err
}

// Direct returns a chain of one model for a caller that must call model
// name's client with a wire model id the catalogue does not define (model):
// for instance an agent whose configured models do not resolve, served by a
// default model's client. The chain's candidate is named model, with name's
// provider and protocol, and the catalogue's limit defaults as its limits;
// opts replace what it reports (WithDirectProvider, WithDirectLimits,
// WithDirectRequireReasoningContent), never the client it calls. Its Name
// is the wire id model, not a catalogue name, and Client takes that name. A
// call on it (Chat, Do, Caller) calls the client once: health is neither
// read nor recorded, the enable switches are ignored, there is no fallback,
// no observer event and no record of what is serving, and an error is
// returned as the client gave it, unless ctx ended (then ctx.Err()). model
// is sent as given, even empty: the client decides what an empty id means
// (a CLI runs its own default model). It fails with ErrUnknownModel when
// the catalogue has no model name (an empty name included).
func (c *Catalog) Direct(name, model string, opts ...DirectOption) (*Chain, error) {
	g := c.gen.Load()
	e, ok := g.models[name]
	if !ok {
		return nil, fmt.Errorf("models: %w %q", ErrUnknownModel, name)
	}
	cfg := directConfig{
		provider:  e.prov.Name,
		limits:    modelspec.ResolveLimits(nil, g.cfg.limitDefaults),
		reasoning: e.prov.RequireReasoningContent,
	}
	for _, o := range opts {
		o(&cfg)
	}
	cand := Candidate{
		Name: model, Provider: cfg.provider, Protocol: e.prov.Protocol, Model: model,
		Limits:       cfg.limits,
		ProviderSpec: e.prov,
	}
	cand.ProviderSpec.RequireReasoningContent = cfg.reasoning
	return &Chain{
		cat: c, gen: g, direct: name,
		cands: []Candidate{cand},
		off:   []bool{false},
	}, nil
}

// DirectOption changes what the candidate of a chain made by Direct
// reports: to its caller (Candidates, Limits), to WithPrepare, WithFinish
// and a Do function, and in a call's Result and Attempts. It never changes
// the client called, its protocol or the wire model id.
type DirectOption func(*directConfig)

// directConfig is what Direct's options set.
type directConfig struct {
	provider  string
	limits    modelspec.Limits
	reasoning bool
}

// WithDirectProvider sets the provider name the candidate reports
// (Candidate.Provider, and so Attempt.Provider); "" reports none. Its
// ProviderSpec stays the client's provider's.
func WithDirectProvider(name string) DirectOption {
	return func(c *directConfig) { c.provider = name }
}

// WithDirectLimits replaces the candidate's limits (and so Chain.Limits)
// with l, for a caller whose own limits apply to the wire model rather than
// the catalogue's defaults. l is taken as given: zero fields are not filled.
func WithDirectLimits(l modelspec.Limits) DirectOption {
	return func(c *directConfig) { c.limits = l }
}

// WithDirectRequireReasoningContent sets the candidate's
// ProviderSpec.RequireReasoningContent, for a caller whose own setting for
// the wire model's provider decides whether a request backfills reasoning
// content, rather than that of the provider of the client it calls.
func WithDirectRequireReasoningContent(require bool) DirectOption {
	return func(c *directConfig) { c.reasoning = require }
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
	writes   []pendingWrite // store writes not yet made (persist)
	writing  bool           // a goroutine is making them
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
func (r *runState) get(k selKey, cfg *settings, from *Set) string {
	r.mu.Lock()
	name, loaded := r.selected[k], r.loaded[k]
	r.mu.Unlock()
	if loaded || cfg.store == nil {
		return name
	}
	stored, err := cfg.store.LoadSelection(k.set, k.purpose, k.scope)
	if err != nil {
		// Left unloaded: it is read again on the next use.
		cfg.logger.Warn("model selection could not be read; using the default for now",
			"component", component, "set", k.set, "purpose", string(k.purpose), "scope", string(k.scope), "error", err.Error())
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if from.removed.Load() {
		return ""
	}
	if !r.loaded[k] {
		r.loaded[k] = true
		if stored != "" {
			r.selected[k] = stored
		}
	}
	return r.selected[k]
}

func (r *runState) forget(set string, scope Scope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.selected {
		if k.set == set && k.scope == scope {
			delete(r.selected, k)
		}
	}
	for k := range r.loaded {
		if k.set == set && k.scope == scope {
			delete(r.loaded, k)
		}
	}
	for k := range r.serve {
		if k.set == set && k.scope == scope {
			delete(r.serve, k)
		}
	}
}

// drop removes everything held for set id and marks its handle removed, so
// a chain still running from it records nothing. Store writes not yet made
// for it are dropped.
func (r *runState) drop(id string, handle *Set) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if handle != nil {
		handle.removed.Store(true)
	}
	for k := range r.selected {
		if k.set == id {
			delete(r.selected, k)
		}
	}
	for k := range r.loaded {
		if k.set == id {
			delete(r.loaded, k)
		}
	}
	for k := range r.serve {
		if k.set == id {
			delete(r.serve, k)
		}
	}
	r.writes = slices.DeleteFunc(r.writes, func(w pendingWrite) bool { return w.key.set == id })
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
func (r *runState) served(k selKey, from *Set, answered string, reason string, attempts []Attempt, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if from != nil && from.removed.Load() {
		return // the set was removed while the chain ran
	}
	s := r.serve[k]
	if answered != "" && answered != s.Serving {
		s.Serving, s.Since = answered, now
	}
	s.Reason = reason
	s.Attempts = slices.Clone(attempts)
	r.serve[k] = s
}

// selSave is one selection to write to the store ("" clears it).
type selSave struct {
	key  selKey
	name string
}

// reconcile brings the selections and serving records in memory in line with
// next, after a change from prev. A selection whose model no longer serves its
// purpose reverts to the default and is returned as reverted. A selection
// still served, of a purpose whose list changed (members or order), is kept
// and written again, so a store that keeps a position rather than a name can
// re-save it. It returns what to write to the store, after the caller has
// released its locks.
func (r *runState) reconcile(prev, next *generation) (saves []selSave, reverted []Reverted) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, name := range r.selected {
		list, _ := next.effective(k.set, k.purpose)
		if !slices.ContainsFunc(list, func(a assignment) bool { return a.name == name }) {
			delete(r.selected, k)
			saves = append(saves, selSave{key: k})
			reverted = append(reverted, Reverted{Set: k.set, Purpose: k.purpose, Scope: k.scope, Model: name})
			continue
		}
		before, _ := prev.effective(k.set, k.purpose)
		if !sameOrder(before, list) {
			saves = append(saves, selSave{key: k, name: name})
		}
	}
	for k, s := range r.serve {
		if _, ok := next.models[s.Serving]; s.Serving != "" && !ok {
			delete(r.serve, k)
		}
	}
	less := func(a, b selKey) int {
		return strings.Compare(a.set+"\x00"+string(a.purpose)+"\x00"+string(a.scope), b.set+"\x00"+string(b.purpose)+"\x00"+string(b.scope))
	}
	slices.SortFunc(saves, func(a, b selSave) int { return less(a.key, b.key) })
	slices.SortFunc(reverted, func(a, b Reverted) int {
		return less(selKey{a.Set, a.Purpose, a.Scope}, selKey{b.Set, b.Purpose, b.Scope})
	})
	return saves, reverted
}

// sameOrder reports whether two lists hold the same models in the same order.
func sameOrder(a, b []assignment) bool {
	return slices.EqualFunc(a, b, func(x, y assignment) bool { return x.name == y.name })
}
