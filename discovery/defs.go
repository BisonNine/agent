package discovery

import (
	"maps"
	"slices"
	"sync"

	"go.bison9.com/agent/toolkit"
)

// Def is one tool definition for a model call, in the shape a provider
// client takes: the caller copies it into its own type (for spawnllm, a
// ToolDefinition of type "function" with Deferred and ToolSearch).
type Def struct {
	// Name is the name the tool is offered and called under.
	Name        string
	Description string
	// Parameters is the tool's schema, free-form objects declared open
	// (toolkit.OpenObjectProperties).
	Parameters map[string]any
	// Deferred marks a hidden tool sent for the provider to load only once
	// a tool result references it. Only set in a list for a provider that
	// loads deferred tools natively.
	Deferred bool
	// ToolSearch marks search_tools, whose results carry the references.
	ToolSearch bool
}

// Defs is the tool list for one model call.
type Defs struct {
	// Send is the list to give the provider. For a provider that loads
	// deferred tools natively it is every tool the snapshot offers, sorted
	// by internal name, the hidden ones Deferred: the same on every call
	// while the tools are unchanged, so the prompt-cache prefix holds.
	// Otherwise it is the listed tools, sorted by internal name, then the
	// hidden tools the session has revealed, in the order they were first
	// revealed; none is Deferred.
	Send []Def
	// InContext is what reaches the model's context, and so what the tool
	// schemas cost: the listed tools, sorted by internal name, then the
	// revealed hidden ones in order of first reveal, none Deferred. It is
	// the same for either kind of provider.
	InContext []Def
}

// ToolDefs returns the tool definitions for one model call: snap is the
// call's snapshot, sess what this conversation has revealed (nil for
// nothing) and native whether the provider loads deferred tools natively
// for the model in use (see Native). With no hidden tool in snap (discovery
// off) both lists are every offered tool, with no flag but ToolSearch.
//
// search_tools' description is built from snap's hidden tools, so it
// matches the list it is sent with.
func ToolDefs(snap *toolkit.Snapshot, sess *Session, native bool) Defs {
	all := snap.List()
	var hidden []doc
	for _, in := range all {
		if _, search := in.Tool.(*searchTool); in.Hidden && !search {
			adv := externalName(in.Tool)
			if adv == "" {
				adv = in.Name
			}
			hidden = append(hidden, doc{advertised: adv})
		}
	}
	searchDesc := searchDescriptionFor(namespaces(hidden))

	var revealed map[string]int // published name → order of first reveal
	if sess != nil {
		revealed = sess.order()
	}

	var out Defs
	type pending struct {
		order int
		def   Def
	}
	var late []pending
	for _, in := range all {
		def := Def{Name: in.Published, Parameters: toolkit.OpenObjectProperties(in.Tool.Parameters())}
		if _, ok := in.Tool.(*searchTool); ok {
			// Never deferred, as spawnllm treats its tool-search tool.
			def.ToolSearch = true
			def.Description = searchDesc
			in.Hidden = false
		} else {
			def.Description = in.Tool.Description()
		}
		if native {
			sent := def
			sent.Deferred = in.Hidden
			out.Send = append(out.Send, sent)
		}
		if !in.Hidden {
			out.InContext = append(out.InContext, def)
		} else if i, ok := revealed[in.Published]; ok {
			late = append(late, pending{order: i, def: def})
		}
	}
	slices.SortFunc(late, func(a, b pending) int { return a.order - b.order })
	for _, p := range late {
		out.InContext = append(out.InContext, p.def)
	}
	if !native {
		out.Send = slices.Clone(out.InContext)
	}
	return out
}

// DeferredLoader is a provider that may load deferred tools natively for a
// model. spawnllm's DeferredToolsCapable is one.
type DeferredLoader interface {
	SupportsDeferredTools(model string) bool
}

// Native reports whether provider loads deferred tools natively for model:
// it is a DeferredLoader that says so.
func Native(provider any, model string) bool {
	dl, ok := provider.(DeferredLoader)
	return ok && dl.SupportsDeferredTools(model)
}

// Session is what one conversation's tool results have revealed: the tool
// references they carried, in order of first reference. It must mirror the
// history the model is sent: record each tool result's references as it is
// added, and when the history is rebuilt (a restart, a compaction that drops
// messages) build a new Session from the references of the messages kept.
// It is safe for concurrent use.
type Session struct {
	mu    sync.Mutex
	first map[string]int // published name → order of first reference
}

// NewSession returns a session that has seen refs, each a tool result's
// references, in history order.
func NewSession(refs ...[]string) *Session {
	s := &Session{first: map[string]int{}}
	for _, r := range refs {
		s.Record(r)
	}
	return s
}

// Record notes the references a tool result carried
// (toolkit.Result.ToolReferences). A name already revealed keeps its place.
func (s *Session) Record(refs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		s.first = map[string]int{}
	}
	for _, name := range refs {
		if _, ok := s.first[name]; !ok {
			s.first[name] = len(s.first)
		}
	}
}

// Revealed returns every name recorded, in order of first reference.
func (s *Session) Revealed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.first))
	for name, i := range s.first {
		out[i] = name
	}
	return out
}

// order returns a copy of the first-reference order.
func (s *Session) order() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.first)
}
