package toolkit

import "context"

// Info describes one registered tool. It is a copy: changing it changes
// nothing in the registry.
type Info struct {
	// Name is the internal name the tool is registered under.
	Name string
	// Published is the name the tool is offered to models under: its
	// ExternalName when it has one that shadows no other tool, else Name.
	// Search hits and tool references use it.
	Published string
	// Tool is the tool itself, for its description, schema and optional
	// interfaces. Run it through Execute, not directly, so removal and
	// disabling are honoured.
	Tool Tool
	// Hidden marks a tool offered only through progressive discovery.
	Hidden bool
	// Group and RevealTogether are the discovery group the tool belongs to.
	Group          string
	RevealTogether bool
	// Source is the toolset or provider namespace that mounted the tool,
	// "" for one added on its own.
	Source string
	// Enabled reports whether the tool is usable: enabled in the catalogue
	// and in the set.
	Enabled bool
}

// Snapshot is the tools one set offered at one moment: every tool granted
// and enabled, listed or hidden. It never changes; take a new one per model
// call. It is safe for concurrent use.
type Snapshot struct {
	set     *Set
	state   *catState
	version uint64
	names   []string // offered internal names, sorted
	offered map[string]*entry
}

func newSnapshot(s *Set, st *catState, cfg *setConfig) *Snapshot {
	offered := s.offered(st, cfg)
	names := make([]string, 0, len(offered))
	for _, name := range st.names {
		if _, ok := offered[name]; ok {
			names = append(names, name)
		}
	}
	return &Snapshot{set: s, state: st, version: st.version + cfg.version, names: names, offered: offered}
}

// Version is the set's Version when the snapshot was taken.
func (sn *Snapshot) Version() uint64 { return sn.version }

// Names returns the internal names of the offered tools, sorted.
func (sn *Snapshot) Names() []string {
	return append([]string(nil), sn.names...)
}

// List returns every offered tool, listed and hidden, sorted by internal
// name.
func (sn *Snapshot) List() []Info { return sn.filter(func(*entry) bool { return true }) }

// Listed returns the offered tools that are listed, sorted by internal
// name: what goes into the model's tool list on every call.
func (sn *Snapshot) Listed() []Info { return sn.filter(func(e *entry) bool { return !e.hidden }) }

// Hidden returns the offered tools that are hidden, sorted by internal
// name: the corpus progressive discovery searches.
func (sn *Snapshot) Hidden() []Info { return sn.filter(func(e *entry) bool { return e.hidden }) }

func (sn *Snapshot) filter(keep func(*entry) bool) []Info {
	out := make([]Info, 0, len(sn.names))
	for _, name := range sn.names {
		if e := sn.offered[name]; keep(e) {
			out = append(out, sn.set.info(sn.state, e))
		}
	}
	return out
}

// Get returns the offered tool registered under exactly name.
func (sn *Snapshot) Get(name string) (Info, bool) {
	e, ok := sn.offered[name]
	if !ok {
		return Info{}, false
	}
	return sn.set.info(sn.state, e), true
}

// Lookup is Get that also finds a tool by its ExternalName; Info.Name is
// the internal name.
func (sn *Snapshot) Lookup(name string) (Info, bool) {
	e, canonical := sn.state.lookup(name)
	if e == nil {
		return Info{}, false
	}
	return sn.Get(canonical)
}

// has reports whether the snapshot offered name, by either name.
func (sn *Snapshot) has(name string) bool {
	_, ok := sn.Lookup(name)
	return ok
}

// Execute runs a tool the model chose from this snapshot. It is the set's
// Execute against the current contents, with one difference: a tool that
// was in the snapshot and has since been removed returns
// `tool "x" is no longer available` (ErrToolRemoved). A call already
// running when its tool is removed or replaced finishes on the old tool.
func (sn *Snapshot) Execute(ctx context.Context, name string, args map[string]any, cb AsyncCallback) *Result {
	return sn.set.execute(ctx, name, args, cb, sn)
}
