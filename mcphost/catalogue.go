package mcphost

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"go.bison9.com/agent/toolkit"
)

// Catalogue is the endpoint-wide list of tools: the names a call may use (a
// name it does not publish is answered with MCP's "tool not found" error),
// what tools/list shows on an Argument endpoint, and the bound of what a
// Bearer caller is shown.
//
// A Catalogue that also has Version (it never goes back, and moves whenever
// Tools may have changed) is followed automatically: the endpoint reads it
// again before a tools/list or tools/call once its version moved. One that
// also has Subscribe sends tools/list_changed on its notices. A Catalogue
// with neither changes what the endpoint publishes only when the host calls
// Endpoint.Refresh.
type Catalogue interface {
	// Tools returns the tools offered now, sorted by internal name, each
	// internal name once (as a toolkit snapshot lists them).
	Tools() []toolkit.Info
}

type (
	versioned    interface{ Version() uint64 }
	subscribable interface {
		Subscribe(fn func(toolkit.Change)) (unsubscribe func())
	}
)

// FromRegistry returns a toolkit registry (a catalogue, a set or a view) as
// a Catalogue: followed automatically, with a notice on each change.
func FromRegistry(reg toolkit.Registry) Catalogue { return registryCatalogue{reg: reg} }

type registryCatalogue struct{ reg toolkit.Registry }

func (c registryCatalogue) Tools() []toolkit.Info { return c.reg.Snapshot().List() }
func (c registryCatalogue) Version() uint64       { return c.reg.Version() }
func (c registryCatalogue) Subscribe(fn func(toolkit.Change)) func() {
	return c.reg.Subscribe(fn)
}

// Union returns a Catalogue over several registries (a host whose agents
// each have their own toolkit catalogue): each internal name once, taken
// from the first registry regs returns that offers it. It has no Version:
// the host calls Endpoint.Refresh when a registry changes. regs is called on
// every read.
func Union(regs func() []toolkit.Registry) Catalogue { return unionCatalogue{regs: regs} }

type unionCatalogue struct{ regs func() []toolkit.Registry }

func (c unionCatalogue) Tools() []toolkit.Info {
	seen := map[string]bool{}
	var out []toolkit.Info
	for _, reg := range c.regs() {
		if reg == nil {
			continue
		}
		for _, info := range reg.Snapshot().List() {
			if !seen[info.Name] {
				seen[info.Name] = true
				out = append(out, info)
			}
		}
	}
	slices.SortFunc(out, func(a, b toolkit.Info) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// NameRule is how an endpoint names the tools it publishes. Tools are taken
// in internal-name order; a tool whose name is already taken is left out.
type NameRule int

const (
	// PublishedNames publishes each tool under toolkit's Info.Published: its
	// ExternalName unless that would shadow another tool, else its name.
	PublishedNames NameRule = iota
	// ExternalFirst publishes each tool under its ExternalName when it has
	// one, else its name, whatever it shadows; of two tools wanting one
	// name the first by internal name keeps it.
	ExternalFirst
)

// published is one tool as an endpoint publishes it.
type published struct {
	name string
	info toolkit.Info
}

// publish names infos (sorted by internal name) by the endpoint's rule.
// Tools whose name is taken are left out.
func (e *Endpoint) publish(infos []toolkit.Info) []published {
	if e.cfg.Names == ExternalFirst {
		out := make([]published, 0, len(infos))
		taken := make(map[string]bool, len(infos))
		for _, info := range infos {
			name := info.Name
			if en, ok := info.Tool.(toolkit.ExternalNamer); ok && en.ExternalName() != "" {
				name = en.ExternalName()
			}
			if !taken[name] {
				taken[name] = true
				out = append(out, published{name: name, info: info})
			}
		}
		return out
	}
	// PublishedNames: toolkit's no-shadow rule over all of infos (which a
	// Union draws from several registries, each with its own Published). A
	// tool is offered under its external name when that is no tool's
	// internal name and no earlier tool took it, else under its own name,
	// which is always free: internal names are unique in infos.
	names := make([]string, len(infos))
	taken := make(map[string]bool, len(infos))
	for _, info := range infos {
		taken[info.Name] = true
	}
	for i, info := range infos {
		names[i] = info.Name
		if ext := info.Published; ext != "" && ext != info.Name && !taken[ext] {
			taken[ext] = true
			names[i] = ext
		}
	}
	out := make([]published, 0, len(infos))
	for i, info := range infos {
		if names[i] != "" {
			out = append(out, published{name: names[i], info: info})
		}
	}
	return out
}

// registered is what is registered with mcp-go under one published name.
type registered struct {
	internal string
	desc     string
	schema   []byte
	tool     mcp.Tool
}

func (r registered) equal(o registered) bool {
	return r.internal == o.internal && r.desc == o.desc && bytes.Equal(r.schema, o.schema)
}

// catalogueState is one immutable result of a sync.
type catalogueState struct {
	byName map[string]registered
	tools  []mcp.Tool // sorted by published name
}

// autoSync syncs a catalogue that reports a version; one that does not
// changes only on Refresh.
func (e *Endpoint) autoSync() {
	if e.versioned != nil {
		e.sync()
	}
}

// sync brings the tools registered with mcp-go in step with the catalogue:
// a tool that appeared is added, one whose name mapping, description or
// schema changed is replaced, one that went is deleted. mcp-go sends
// tools/list_changed for the adds and for the deletes. It reports whether
// anything changed. A versioned catalogue whose version was already synced
// is not read again.
func (e *Endpoint) sync() bool {
	if e.versioned != nil && e.synced.Load() && e.versioned.Version() == e.seen.Load() {
		return false
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	var version uint64
	if e.versioned != nil {
		version = e.versioned.Version()
		if e.synced.Load() && version <= e.version {
			return false // a later sync got here first
		}
	}
	cur := e.state.Load()
	want := make(map[string]registered, len(cur.byName))
	var add []server.ServerTool
	for _, p := range e.publish(e.cfg.Catalogue.Tools()) {
		reg, ok := e.describe(p)
		if !ok {
			continue
		}
		want[p.name] = reg
		if prior, ok := cur.byName[p.name]; ok && prior.equal(reg) {
			continue
		}
		add = append(add, server.ServerTool{Tool: reg.tool, Handler: e.callFor(reg.internal)})
	}
	var del []string
	for name := range cur.byName {
		if _, ok := want[name]; !ok {
			del = append(del, name)
		}
	}
	sort.Strings(del)
	if len(del) > 0 {
		e.srv.DeleteTools(del...)
	}
	added := 0
	if len(add) > 0 {
		refused := e.addTools(add)
		added = len(add) - len(refused)
		var gone []string
		for _, name := range refused {
			// A refused replacement of the same tool leaves the registration
			// mcp-go holds, which stays published (its old schema, until the
			// catalogue changes again); a name now meant for another tool,
			// or none, is unpublished.
			if prior, ok := cur.byName[name]; ok && prior.internal == want[name].internal {
				want[name] = prior
				continue
			}
			delete(want, name)
			if _, ok := cur.byName[name]; ok {
				gone = append(gone, name)
			}
		}
		if len(gone) > 0 {
			e.srv.DeleteTools(gone...)
			del = append(del, gone...)
		}
	}
	tools := make([]mcp.Tool, 0, len(want))
	for _, r := range want {
		tools = append(tools, r.tool)
	}
	slices.SortFunc(tools, func(a, b mcp.Tool) int { return cmp.Compare(a.Name, b.Name) })
	e.state.Store(&catalogueState{byName: want, tools: tools})
	e.version = version
	e.seen.Store(version)
	e.synced.Store(true)
	changed := added > 0 || len(del) > 0
	if changed {
		e.logger.Info("MCP catalogue updated", "added", added, "deleted", len(del), "tools", len(want))
	}
	return changed
}

// addTools registers tools in one batch (one tools/list_changed). mcp-go
// panics on a schema it cannot register; then the tools are added one at a
// time and the names of the ones it refuses are returned, and logged.
func (e *Endpoint) addTools(tools []server.ServerTool) (refused []string) {
	if e.tryAdd(tools...) {
		return nil
	}
	for _, t := range tools {
		if !e.tryAdd(t) {
			e.logger.Error("Skipping tool: MCP refused its schema", "published", t.Tool.Name)
			refused = append(refused, t.Tool.Name)
		}
	}
	return refused
}

func (e *Endpoint) tryAdd(tools ...server.ServerTool) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	e.srv.AddTools(tools...)
	return true
}

// describe returns the MCP form of one published tool: its name, its
// description, and its schema with free-form objects opened (and the
// TokenArgument added on an Argument endpoint). ok is false, logged, for a
// schema that cannot be encoded.
func (e *Endpoint) describe(p published) (registered, bool) {
	params := toolkit.OpenObjectProperties(p.info.Tool.Parameters())
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if e.cfg.Transport == Argument {
		params = injectTokenArgument(params, e.cfg.TokenDescription)
	}
	schema, err := json.Marshal(params)
	if err != nil {
		e.logger.Warn("Skipping tool: schema cannot be encoded", "tool", p.info.Name, "error", err.Error())
		return registered{}, false
	}
	desc := p.info.Tool.Description()
	return registered{
		internal: p.info.Name,
		desc:     desc,
		schema:   schema,
		tool:     mcp.NewToolWithRawSchema(p.name, desc, schema),
	}, true
}

// afterList replaces the list mcp-go built: on an Argument endpoint the
// catalogue as published; on a Bearer endpoint the caller's own tools, named
// and described from the caller's own set alone (from a snapshot taken
// now), limited to the names the catalogue publishes.
func (e *Endpoint) afterList(ctx context.Context, _ any, _ *mcp.ListToolsRequest, result *mcp.ListToolsResult) {
	if result == nil {
		return
	}
	e.autoSync()
	st := e.state.Load()
	result.NextCursor = ""
	if e.cfg.Transport == Argument {
		result.Tools = slices.Clone(st.tools)
		return
	}
	result.Tools = []mcp.Tool{}
	c, ok := CallerOf(ctx)
	if !ok || c.Set == nil || c.revoked() {
		return
	}
	for _, p := range e.publish(c.Set.Snapshot().List()) {
		if _, offered := st.byName[p.name]; !offered {
			continue
		}
		if reg, ok := e.describe(p); ok {
			result.Tools = append(result.Tools, reg.tool)
		}
	}
	slices.SortFunc(result.Tools, func(a, b mcp.Tool) int { return cmp.Compare(a.Name, b.Name) })
}

// resolve finds the tool a call of a published name runs for caller;
// internal is the internal name the catalogue publishes under that name
// (bound to the name's handler, so it always matches mcp-go's routing).
//
// On a Bearer endpoint it is the tool the caller is listed under the name.
// Otherwise, and for a disabled tool a Bearer caller is not listed, it is
// the tool of that internal name in the caller's set, enabled or not, so
// that toolkit refuses a disabled one as it does. ok is false when the
// caller has no such tool to run: it is refused as one not granted.
func (e *Endpoint) resolve(caller Caller, name, internal string) (toolkit.Info, bool) {
	if e.cfg.Transport == Bearer {
		for _, p := range e.publish(caller.Set.Snapshot().List()) {
			if p.name == name {
				return p.info, true
			}
		}
		// Not listed to the caller: only a disabled tool is resolved, to be
		// refused as disabled; never one listed under another name.
		if info, ok := caller.Set.Get(internal); ok && !info.Enabled {
			return info, true
		}
		return toolkit.Info{}, false
	}
	return caller.Set.Get(internal)
}

// injectTokenArgument returns a copy of params with the TokenArgument added
// to its properties and to its required list. params is not changed.
func injectTokenArgument(params map[string]any, description string) map[string]any {
	out := maps.Clone(params)
	if out == nil {
		out = map[string]any{}
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	props, ok := out["properties"].(map[string]any)
	if ok && props != nil {
		props = maps.Clone(props)
	} else {
		props = map[string]any{}
	}
	props[TokenArgument] = map[string]any{"type": "string", "description": description}
	out["properties"] = props

	required := requiredNames(out["required"])
	if !slices.Contains(required, TokenArgument) {
		required = append(required, TokenArgument)
	}
	out["required"] = required
	return out
}

// requiredNames returns a schema's required list as a new []string.
func requiredNames(v any) []string {
	switch raw := v.(type) {
	case []string:
		return slices.Clone(raw)
	case []any:
		out := make([]string, 0, len(raw)+1)
		for _, item := range raw {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
