package toolkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"

	"go.bison9.com/agent/toolspec"
)

// ErrDuplicate is returned when a tool name is already registered, or
// appears twice in one call.
var ErrDuplicate = errors.New("tool already registered")

// Catalog is the process-wide registry: every tool, toolset and provider
// the host mounted. It has a default set granting every tool, and its read
// side (Snapshot, Execute, Subscribe, Version, List, Get, ...) is that
// set's, so a host with one agent never deals with sets. It is safe for
// use from any goroutine at any time, including while turns run. Create
// one with New; the zero value is not usable.
type Catalog struct {
	logger *slog.Logger

	mu      sync.Mutex // serialises writers; readers never lock
	state   atomic.Pointer[catState]
	regSeq  uint64          // toolset and provider registration ids, under mu
	def     *Set            // the default set, also sets[""]
	sets    map[string]*Set // named sets, under mu
	watched map[*Set]int    // sets with subscribers → their count, under mu

	notes notifier
}

// Option configures a Catalog.
type Option func(*Catalog)

// WithLogger sets the logger. Records carry component=toolkit. The default
// discards everything.
func WithLogger(l *slog.Logger) Option {
	return func(c *Catalog) {
		if l != nil {
			c.logger = l.With("component", "toolkit")
		}
	}
}

// New returns an empty catalogue with a default set that grants every
// tool.
func New(opts ...Option) *Catalog {
	c := &Catalog{
		logger:  slog.New(slog.DiscardHandler),
		watched: map[*Set]int{},
	}
	for _, o := range opts {
		o(c)
	}
	c.state.Store(emptyState())
	c.def = newSet(c, nil)
	c.sets = map[string]*Set{"": c.def}
	return c
}

// NewRegistry is New, under the name a host with one agent reads best:
// reg := toolkit.NewRegistry(); reg.Add(...); agent.WithTools(reg).
func NewRegistry(opts ...Option) *Catalog { return New(opts...) }

// update applies fn to a writable copy of the state under the write lock.
// When fn reports a change, the copy is published with the next version
// and subscribers are notified after the lock is released.
func (c *Catalog) update(fn func(next *catState) (changed bool, err error)) error {
	c.mu.Lock()
	cur := c.state.Load()
	next := cur.clone()
	changed, err := fn(next)
	if err != nil || !changed {
		c.mu.Unlock()
		return err
	}
	c.commitLocked(cur, next, nil)
	c.mu.Unlock()
	c.notify()
	return nil
}

// change is update for a change that cannot fail.
func (c *Catalog) change(fn func(next *catState) (changed bool)) {
	c.mu.Lock()
	cur := c.state.Load()
	next := cur.clone()
	if !fn(next) {
		c.mu.Unlock()
		return
	}
	c.commitLocked(cur, next, nil)
	c.mu.Unlock()
	c.notify()
}

// commitLocked publishes next (a changed copy of cur, or nil when only set
// configurations change) and the new configurations in cfgs, and queues
// the change notice, so notices queue in commit order. The caller
// delivers with c.notify once it has released the lock. Caller holds c.mu.
func (c *Catalog) commitLocked(cur, next *catState, cfgs map[*Set]*setConfig) {
	if next == nil {
		next = cur
	} else {
		next.version = cur.version + 1
		next.seal()
	}
	ev := c.eventLocked(cur, next, cfgs)
	c.state.Store(next)
	for s, cfg := range cfgs {
		s.cfg.Store(cfg)
	}
	if len(ev.sets) > 0 {
		c.notes.push(ev)
	}
}

// eventLocked records a change for every subscribed set. cfgs holds the
// new configuration of the sets whose own settings change with it.
// Caller holds c.mu and has not yet stored the new state or configs.
func (c *Catalog) eventLocked(oldState, newState *catState, cfgs map[*Set]*setConfig) event {
	ev := event{oldState: oldState, newState: newState}
	for s := range c.watched {
		oldCfg := s.cfg.Load()
		newCfg := oldCfg
		if cfg, ok := cfgs[s]; ok {
			newCfg = cfg
		}
		ev.sets = append(ev.sets, setEvent{set: s, oldCfg: oldCfg, newCfg: newCfg, subs: s.subscribers()})
	}
	return ev
}

// notify delivers queued change notices. Call it with no lock held.
func (c *Catalog) notify() { c.notes.drain() }

// newEntry validates in and returns it as an entry from source.
func newEntry(in Entry, source string) (*entry, error) {
	if in.Tool == nil {
		return nil, errors.New("toolkit: entry has no tool")
	}
	name := in.Tool.Name()
	if name == "" {
		return nil, errors.New("toolkit: tool has no name")
	}
	return &entry{
		name:   name,
		tool:   in.Tool,
		hidden: in.Hidden,
		group:  in.Group,
		reveal: in.RevealTogether,
		source: source,
	}, nil
}

// newEntries validates a batch, refusing a name given twice.
func newEntries(in []Entry, source string) ([]*entry, error) {
	out := make([]*entry, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, e := range in {
		ne, err := newEntry(e, source)
		if err != nil {
			return nil, err
		}
		if seen[ne.name] {
			return nil, fmt.Errorf("toolkit: %q: %w", ne.name, ErrDuplicate)
		}
		seen[ne.name] = true
		out = append(out, ne)
	}
	return out, nil
}

// Add registers toolspec definitions under their own names, listed. It
// refuses the whole call, changing nothing, when a name is already
// registered or given twice (ErrDuplicate).
func (c *Catalog) Add(defs ...toolspec.ToolDefinition) error {
	return c.AddEntries(entriesFromSpec("", defs)...)
}

// AddEntries registers tools. Like Add, it refuses the whole call when a
// name is already registered or given twice.
func (c *Catalog) AddEntries(entries ...Entry) error {
	add, err := newEntries(entries, "")
	if err != nil {
		return err
	}
	return c.update(func(next *catState) (bool, error) {
		for _, e := range add {
			if _, exists := next.entries[e.name]; exists {
				return false, fmt.Errorf("toolkit: %q: %w", e.name, ErrDuplicate)
			}
		}
		for _, e := range add {
			next.entries[e.name] = e
		}
		return len(add) > 0, nil
	})
}

// Put registers tools, replacing any already registered under the same
// names, in one step. A replaced tool counts as changed for subscribers.
func (c *Catalog) Put(entries ...Entry) error {
	put, err := newEntries(entries, "")
	if err != nil {
		return err
	}
	var overwritten []string
	err = c.update(func(next *catState) (bool, error) {
		for _, e := range put {
			if _, exists := next.entries[e.name]; exists {
				overwritten = append(overwritten, e.name)
			}
			next.entries[e.name] = e
		}
		return len(put) > 0, nil
	})
	for _, name := range overwritten {
		c.logger.Warn("Tool registration overwrites existing tool", "name", name)
	}
	return err
}

// Remove unregisters the named tools and returns how many were removed.
// Unknown names are ignored. A call already running finishes.
func (c *Catalog) Remove(names ...string) int {
	removed := 0
	c.change(func(next *catState) bool {
		for _, name := range names {
			if _, ok := next.entries[name]; ok {
				delete(next.entries, name)
				removed++
			}
		}
		return removed > 0
	})
	return removed
}

// Replace swaps every tool whose name starts with prefix for defs
// (registered under their own names, listed), in one step: a concurrent
// reader sees the old tools or the new ones, never neither, and
// subscribers get one notice. A definition whose name lacks the prefix is
// registered all the same, replacing a tool of that name; the caller keeps
// them in step. A name given twice keeps the last. It returns how many
// tools were added and removed; a tool replaced by one of the same name
// counts in both.
func (c *Catalog) Replace(prefix string, defs []toolspec.ToolDefinition) (added, removed int) {
	return c.ReplaceEntries(prefix, entriesFromSpec("", defs))
}

// ReplaceEntries is Replace for entries. An entry with no tool or no name
// is skipped and logged.
func (c *Catalog) ReplaceEntries(prefix string, entries []Entry) (added, removed int) {
	put := make(map[string]*entry, len(entries))
	for _, in := range entries {
		e, err := newEntry(in, "")
		if err != nil {
			c.logger.Warn("Skipping invalid tool entry", "prefix", prefix, "error", err)
			continue
		}
		put[e.name] = e
	}
	c.change(func(next *catState) bool {
		for name := range next.entries {
			if strings.HasPrefix(name, prefix) {
				delete(next.entries, name)
				removed++
			}
		}
		maps.Copy(next.entries, put)
		added = len(put)
		return removed > 0 || added > 0
	})
	c.logger.Debug("Replaced tools by prefix", "prefix", prefix, "removed", removed, "added", added)
	return added, removed
}

// AddToolset mounts a toolset's current tools, with the toolset's name as
// their Source. Adding a toolset again under the same name replaces its
// tools in one step. It refuses the call, changing nothing, when ts.Tools
// fails, when the toolset has no name, or when one of its tools clashes
// with a tool from elsewhere (ErrDuplicate). remove unmounts the toolset's
// tools; it does nothing once the toolset has been mounted again or
// removed.
func (c *Catalog) AddToolset(ctx context.Context, ts Toolset) (remove func(), err error) {
	name := ts.Name()
	if name == "" {
		return nil, errors.New("toolkit: toolset has no name")
	}
	entries, err := ts.Tools(ctx)
	if err != nil {
		return nil, fmt.Errorf("toolkit: toolset %q: %w", name, err)
	}
	return c.mount(name, entries)
}

// AddProvider mounts a toolspec provider's tools under namespace ns: each
// definition is published as "<ns>_<name>" (see FromSpec), with ns as its
// Source. Mounting ns again replaces its tools in one step. Clashes are
// refused as for AddToolset.
func (c *Catalog) AddProvider(ns string, p toolspec.ToolProvider, deps toolspec.Deps) (remove func(), err error) {
	if ns == "" {
		return nil, errors.New("toolkit: provider has no namespace")
	}
	return c.mount(ns, entriesFromSpec(ns, p.RegisterTools(deps)))
}

// mount replaces source's tools with entries.
func (c *Catalog) mount(source string, entries []Entry) (func(), error) {
	add, err := newEntries(entries, source)
	if err != nil {
		return nil, fmt.Errorf("toolkit: %q: %w", source, err)
	}
	var id uint64
	err = c.update(func(next *catState) (bool, error) {
		for _, e := range add {
			if old, exists := next.entries[e.name]; exists && old.source != source {
				return false, fmt.Errorf("toolkit: %q from %q: %w", e.name, source, ErrDuplicate)
			}
		}
		for _, name := range next.fromSource(source) {
			delete(next.entries, name)
		}
		for _, e := range add {
			next.entries[e.name] = e
		}
		c.regSeq++
		id = c.regSeq
		next.sources[source] = id
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { c.unmount(source, id) }) }, nil
}

// unmount removes source's tools if it is still the registration id.
func (c *Catalog) unmount(source string, id uint64) {
	c.change(func(next *catState) bool {
		if next.sources[source] != id {
			return false
		}
		delete(next.sources, source)
		for _, name := range next.fromSource(source) {
			delete(next.entries, name)
		}
		return true
	})
}

func entriesFromSpec(ns string, defs []toolspec.ToolDefinition) []Entry {
	out := make([]Entry, 0, len(defs))
	for _, d := range defs {
		out = append(out, EntryFromSpec(ns, d))
	}
	return out
}

// Names returns the internal names of every registered tool, sorted.
func (c *Catalog) Names() []string {
	return append([]string(nil), c.state.Load().names...)
}

// The read side is the default set's.

// Version is the default set's Version.
func (c *Catalog) Version() uint64 { return c.def.Version() }

// Snapshot is the default set's Snapshot.
func (c *Catalog) Snapshot() *Snapshot { return c.def.Snapshot() }

// Subscribe is the default set's Subscribe.
func (c *Catalog) Subscribe(fn func(Change)) (unsubscribe func()) { return c.def.Subscribe(fn) }

// Execute is the default set's Execute.
func (c *Catalog) Execute(ctx context.Context, name string, args map[string]any, cb AsyncCallback) *Result {
	return c.def.Execute(ctx, name, args, cb)
}

// ExecuteUnattended is the default set's ExecuteUnattended.
func (c *Catalog) ExecuteUnattended(ctx context.Context, name string, args map[string]any) (string, error) {
	return c.def.ExecuteUnattended(ctx, name, args)
}

// List is the default set's List.
func (c *Catalog) List() []Info { return c.def.List() }

// Listed is the default set's Listed.
func (c *Catalog) Listed() []Info { return c.def.Listed() }

// Hidden is the default set's Hidden.
func (c *Catalog) Hidden() []Info { return c.def.Hidden() }

// Get is the default set's Get.
func (c *Catalog) Get(name string) (Info, bool) { return c.def.Get(name) }

// Lookup is the default set's Lookup.
func (c *Catalog) Lookup(name string) (Info, bool) { return c.def.Lookup(name) }

// Stats is the default set's Stats.
func (c *Catalog) Stats() []Stat { return c.def.Stats() }
