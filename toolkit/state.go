package toolkit

import (
	"maps"
	"slices"
	"strings"
)

// entry is one registered tool. It is never modified once published:
// a change replaces the entry, so a pointer comparison tells whether a
// tool was redefined.
type entry struct {
	name   string
	tool   Tool
	hidden bool
	group  string
	reveal bool
	source string // toolset or provider namespace; "" for a tool added alone
}

// catState is one immutable version of a catalogue's contents. Writers
// build a new one and publish it; readers load the pointer and never lock.
type catState struct {
	version uint64
	entries map[string]*entry
	names   []string          // sorted internal names
	offered map[string]string // internal name → the name it is offered under
	extern  map[string]string // ExternalName → internal name
	sources map[string]uint64 // toolset or provider name → registration id
	// disabled holds the names switched off for everyone. A flag belongs
	// to the name: it survives the tool being replaced, removed and added
	// again, until the name is enabled.
	disabled map[string]bool
}

func emptyState() *catState {
	return &catState{
		entries:  map[string]*entry{},
		offered:  map[string]string{},
		extern:   map[string]string{},
		sources:  map[string]uint64{},
		disabled: map[string]bool{},
	}
}

// clone returns a writable copy of s for a writer to modify and publish
// with seal. Entries are shared: they are immutable.
func (s *catState) clone() *catState {
	return &catState{
		version:  s.version,
		entries:  maps.Clone(s.entries),
		sources:  maps.Clone(s.sources),
		disabled: maps.Clone(s.disabled),
	}
}

// seal derives the sorted names and the name maps from entries. Call it
// once a writer has finished changing a cloned state.
func (s *catState) seal() {
	s.names = slices.Sorted(maps.Keys(s.entries))
	s.extern = make(map[string]string)
	for _, name := range s.names {
		if ext := externalName(s.entries[name].tool); ext != "" && ext != name {
			if _, taken := s.extern[ext]; !taken {
				s.extern[ext] = name
			}
		}
	}
	s.offered = offeredNames(s.entries, s.names)
}

// externalName is t's ExternalName, or "" when it has none.
func externalName(t Tool) string {
	if en, ok := t.(ExternalNamer); ok {
		return en.ExternalName()
	}
	return ""
}

// offeredNames maps every internal name to the name the tool is offered
// under: its ExternalName, unless that would shadow another tool (it equals
// some tool's internal name, or two tools want it), else the internal name.
func offeredNames(entries map[string]*entry, names []string) map[string]string {
	want := make(map[string]int)
	for _, name := range names {
		if ext := externalName(entries[name].tool); ext != "" && ext != name {
			want[ext]++
		}
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = name
		ext := externalName(entries[name].tool)
		if ext != "" && ext != name && entries[ext] == nil && want[ext] == 1 {
			out[name] = ext
		}
	}
	return out
}

// lookup finds a tool by internal name, then by ExternalName. canonical is
// its internal name; e is nil when there is no such tool.
func (s *catState) lookup(name string) (e *entry, canonical string) {
	if e, ok := s.entries[name]; ok {
		return e, name
	}
	if internal, ok := s.extern[name]; ok {
		return s.entries[internal], internal
	}
	return nil, ""
}

// fromSource returns the internal names mounted by source. It reads
// entries, so it is valid on a state being changed.
func (s *catState) fromSource(source string) []string {
	var out []string
	for name, e := range s.entries {
		if e.source == source {
			out = append(out, name)
		}
	}
	return out
}

// group returns the registered names in group: the tools mounted by the
// toolset or provider of that name when there is one, else the tools whose
// internal name starts with group.
func (s *catState) group(group string) []string {
	if _, ok := s.sources[group]; ok {
		return s.fromSource(group)
	}
	var out []string
	for name := range s.entries {
		if strings.HasPrefix(name, group) {
			out = append(out, name)
		}
	}
	return out
}
