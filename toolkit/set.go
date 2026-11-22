package toolkit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Registry is what an agent reads its tools from: a snapshot per model
// call, execution, and change notices. A *Catalog (through its default set)
// and a *Set both satisfy it.
type Registry interface {
	// Snapshot returns the tools offered now. Take one per model call.
	Snapshot() *Snapshot
	// Execute runs a tool by internal or external name against the current
	// contents (see Set.Execute).
	Execute(ctx context.Context, name string, args map[string]any, cb AsyncCallback) *Result
	// Version changes whenever what is offered may have changed.
	Version() uint64
	// Subscribe registers fn for change notices (see Set.Subscribe).
	Subscribe(fn func(Change)) (unsubscribe func())
}

// AllowChecker decides which of a catalogue's tools a set is granted, by
// internal name. It is the host's grant policy; the toolkit only asks it.
type AllowChecker interface {
	IsToolAllowed(name string) bool
}

// Errors carried in a refused or failed call's Result.Err, and returned by
// ExecuteUnattended.
var (
	// ErrToolNotFound: no tool of that name is registered.
	ErrToolNotFound = errors.New("tool not found")
	// ErrToolRemoved: the tool was in the snapshot the call came from and
	// has been removed since.
	ErrToolRemoved = errors.New("tool no longer available")
	// ErrToolNotRegistered is ExecuteUnattended's refusal of a tool the set
	// does not hold.
	ErrToolNotRegistered = errors.New("tool not registered")
	// ErrToolSessionScoped is ExecuteUnattended's refusal of a
	// SessionScoped tool.
	ErrToolSessionScoped = errors.New("tool is session-scoped")
)

// setConfig is one immutable version of a set's own settings.
type setConfig struct {
	version uint64
	allow   AllowChecker // nil grants every tool
}

// Set is the tools one agent is granted from a catalogue, each listed or
// hidden. It holds no tools of its own: it is a view onto its catalogue, so
// every change to the catalogue reaches it at the next snapshot. It is safe
// for concurrent use. Obtain one from a Catalog; the zero value is not
// usable.
type Set struct {
	cat *Catalog
	cfg atomic.Pointer[setConfig]

	subMu  sync.Mutex
	subs   map[uint64]func(Change)
	nextID uint64

	statMu sync.Mutex
	stats  map[string]*Stat // internal name → its count
}

func newSet(c *Catalog, allow AllowChecker) *Set {
	s := &Set{cat: c, subs: map[uint64]func(Change){}, stats: map[string]*Stat{}}
	s.cfg.Store(&setConfig{allow: allow})
	return s
}

// granted reports whether cfg grants name.
func (cfg *setConfig) granted(name string) bool {
	return cfg.allow == nil || cfg.allow.IsToolAllowed(name)
}

// offered returns the tools the set offers under st and cfg: granted ones.
func (s *Set) offered(st *catState, cfg *setConfig) map[string]*entry {
	out := make(map[string]*entry, len(st.entries))
	for name, e := range st.entries {
		if cfg.granted(name) {
			out[name] = e
		}
	}
	return out
}

// Version changes whenever what the set offers may have changed: a change
// to the catalogue or to the set's own settings. It never goes back,
// including across Adopt.
func (s *Set) Version() uint64 {
	return s.cat.state.Load().version + s.cfg.Load().version
}

// Snapshot returns the tools the set offers now: granted, listed and
// hidden. Take one per model call; a turn sees later changes at its next
// snapshot.
func (s *Set) Snapshot() *Snapshot {
	cfg := s.cfg.Load()
	st := s.cat.state.Load()
	return newSnapshot(s, st, cfg)
}

// Subscribe registers fn to be called once for every change to the tools
// the set offers, with what changed. A change that alters nothing the set
// offers sends nothing. Notices arrive in order, one at a time, with no
// registry lock held, so fn may call the registry, including to change
// it; such a change is delivered after fn returns. A notice may be
// delivered on the goroutine of a later writer. fn must not block for
// long: it holds up every later notice.
func (s *Set) Subscribe(fn func(Change)) (unsubscribe func()) {
	c := s.cat
	c.mu.Lock()
	s.subMu.Lock()
	s.nextID++
	id := s.nextID
	s.subs[id] = fn
	s.subMu.Unlock()
	c.watched[s]++
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			s.subMu.Lock()
			delete(s.subs, id)
			s.subMu.Unlock()
			if c.watched[s]--; c.watched[s] <= 0 {
				delete(c.watched, s)
			}
			c.mu.Unlock()
		})
	}
}

// subscribed reports whether subscriber id is still registered.
func (s *Set) subscribed(id uint64) bool {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	_, ok := s.subs[id]
	return ok
}

// subscribers returns the current subscribers in registration order.
// Caller holds the catalogue's write lock.
func (s *Set) subscribers() []subscriber {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	out := make([]subscriber, 0, len(s.subs))
	for _, id := range slices.Sorted(maps.Keys(s.subs)) {
		out = append(out, subscriber{id: id, fn: s.subs[id]})
	}
	return out
}

// info describes e as seen by the set under st.
func (s *Set) info(st *catState, e *entry) Info {
	return Info{
		Name:           e.name,
		Published:      st.offered[e.name],
		Tool:           e.tool,
		Hidden:         e.hidden,
		Group:          e.group,
		RevealTogether: e.reveal,
		Source:         e.source,
		Enabled:        true,
	}
}

// List returns every tool granted to the set, sorted by internal name.
func (s *Set) List() []Info {
	st := s.cat.state.Load()
	cfg := s.cfg.Load()
	out := make([]Info, 0, len(st.names))
	for _, name := range st.names {
		if cfg.granted(name) {
			out = append(out, s.info(st, st.entries[name]))
		}
	}
	return out
}

// Listed returns the listed tools the set offers now (Snapshot().Listed()).
func (s *Set) Listed() []Info { return s.Snapshot().Listed() }

// Hidden returns the hidden tools the set offers now (Snapshot().Hidden()).
func (s *Set) Hidden() []Info { return s.Snapshot().Hidden() }

// Get returns the granted tool registered under exactly name.
func (s *Set) Get(name string) (Info, bool) {
	st := s.cat.state.Load()
	e, ok := st.entries[name]
	if !ok || !s.cfg.Load().granted(name) {
		return Info{}, false
	}
	return s.info(st, e), true
}

// Lookup is Get that also finds a tool by its ExternalName; Info.Name is
// the internal name.
func (s *Set) Lookup(name string) (Info, bool) {
	st := s.cat.state.Load()
	e, canonical := st.lookup(name)
	if e == nil || !s.cfg.Load().granted(canonical) {
		return Info{}, false
	}
	return s.info(st, e), true
}

// Stat counts the calls of one tool that ran through a set.
type Stat struct {
	Name   string
	Calls  uint64
	Errors uint64 // calls whose result was an error
}

// Stats returns a count per tool of the calls that ran through the set
// since it was created, sorted by name. Calls the set refused (an unknown,
// ungranted, disabled or removed tool) are not counted.
func (s *Set) Stats() []Stat {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	out := make([]Stat, 0, len(s.stats))
	for _, name := range slices.Sorted(maps.Keys(s.stats)) {
		out = append(out, *s.stats[name])
	}
	return out
}

func (s *Set) count(name string, r *Result) {
	s.statMu.Lock()
	defer s.statMu.Unlock()
	st, ok := s.stats[name]
	if !ok {
		st = &Stat{Name: name}
		s.stats[name] = st
	}
	st.Calls++
	if r.IsError {
		st.Errors++
	}
}

// Execute runs a tool, by internal name or ExternalName, against the
// current contents: a tool removed or disabled a moment ago is not run,
// and one added a moment ago is. When cb is non-nil and the tool is an
// AsyncExecutor, ExecuteAsync is called. Refusals and failures are
// ordinary error results, never a panic:
//
//   - no such tool: `tool "x" not found` (ErrToolNotFound)
//   - not granted to the set: NotEnabledMessage, an expected refusal
//
// The caller sets who the call is for on ctx (WithChat, WithSessionKey,
// WithAgentID). Arguments are never logged.
func (s *Set) Execute(ctx context.Context, name string, args map[string]any, cb AsyncCallback) *Result {
	return s.execute(ctx, name, args, cb, nil)
}

func (s *Set) execute(ctx context.Context, name string, args map[string]any, cb AsyncCallback, snap *Snapshot) *Result {
	log := s.cat.logger
	log.InfoContext(ctx, "Tool execution started", "tool", name)

	st := s.cat.state.Load()
	cfg := s.cfg.Load()
	e, canonical := st.lookup(name)
	if e == nil {
		if snap != nil && snap.has(name) {
			log.WarnContext(ctx, "Tool removed since the model saw it", "tool", name)
			return ErrorResult(fmt.Sprintf("tool %q is no longer available", name)).WithError(ErrToolRemoved)
		}
		log.ErrorContext(ctx, "Tool not found", "tool", name)
		return ErrorResult(fmt.Sprintf("tool %q not found", name)).WithError(ErrToolNotFound)
	}
	if !cfg.granted(canonical) {
		log.WarnContext(ctx, "Tool execution denied by agent allowlist", "tool", canonical)
		return ErrorResult(NotEnabledMessage(name)).WithError(Refusal(fmt.Errorf("tool not permitted: %s", name)))
	}

	var result *Result
	start := time.Now()
	if async, ok := e.tool.(AsyncExecutor); ok && cb != nil {
		log.DebugContext(ctx, "Executing async tool via ExecuteAsync", "tool", name)
		result = async.ExecuteAsync(ctx, args, cb)
	} else {
		result = e.tool.Execute(ctx, args)
	}
	duration := time.Since(start)
	if result == nil {
		result = ErrorResult("tool returned no result")
	}
	s.count(canonical, result)

	switch {
	case result.IsError && IsExpectedRefusal(result.Err):
		// A configured permission or limit said no: expected, not a fault.
		log.WarnContext(ctx, "Tool call refused",
			"tool", name, "duration", duration.Milliseconds(), "error", truncateRunes(result.ForLLM, 500))
	case result.IsError:
		log.ErrorContext(ctx, "Tool execution failed",
			"tool", name, "duration", duration.Milliseconds(), "error", truncateRunes(result.ForLLM, 500))
	case result.Async:
		log.InfoContext(ctx, "Tool started (async)", "tool", name, "duration", duration.Milliseconds())
	default:
		log.InfoContext(ctx, "Tool execution completed",
			"tool", name, "duration_ms", duration.Milliseconds(), "result_length", len(result.ForLLM))
	}
	return result
}

// ExecuteUnattended runs a tool with no session behind it (a scheduled
// probe, a classifier run). It refuses a tool the set does not hold under
// exactly name (ErrToolNotRegistered) and a SessionScoped one
// (ErrToolSessionScoped), passes no async callback, and turns an error
// result into an error carrying the tool's ForLLM text.
func (s *Set) ExecuteUnattended(ctx context.Context, name string, args map[string]any) (string, error) {
	info, ok := s.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %q: %w", name, ErrToolNotRegistered)
	}
	if scoped, isScoped := info.Tool.(SessionScoped); isScoped && scoped.IsSessionScoped() {
		return "", fmt.Errorf("tool %q: %w", name, ErrToolSessionScoped)
	}
	res := s.Execute(ctx, name, args, nil)
	if res.IsError {
		return "", fmt.Errorf("tool %q failed: %s", name, res.ForLLM)
	}
	return res.ForLLM, nil
}
