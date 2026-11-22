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

// publishedTool is what is registered with mcp-go under one name, so a
// sync can tell an unchanged tool from one whose description or schema
// moved.
type publishedTool struct {
	desc   string
	schema []byte
}

func (p publishedTool) equal(o publishedTool) bool {
	return p.desc == o.desc && bytes.Equal(p.schema, o.schema)
}

// sync brings the tools registered with mcp-go in step with the catalogue:
// a tool that appeared is added, one whose description or schema changed is
// replaced, one that went is deleted. mcp-go sends tools/list_changed for
// the adds and for the deletes. A sync whose catalogue version was already
// synced does nothing.
func (e *Endpoint) sync() {
	if e.synced.Load() && e.cfg.Catalogue.Version() == e.seen.Load() {
		return
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	snap := e.cfg.Catalogue.Snapshot()
	if e.synced.Load() && snap.Version() <= e.version {
		return // a later sync got here first
	}

	want := make(map[string]publishedTool, len(e.published))
	var add []server.ServerTool
	for _, info := range snap.List() {
		tool, pub, ok := e.describe(info)
		if !ok {
			continue
		}
		if _, dup := want[tool.Name]; dup {
			e.logger.Warn("Skipping tool: published name already taken", "tool", info.Name, "published", tool.Name)
			continue
		}
		want[tool.Name] = pub
		if prior, ok := e.published[tool.Name]; ok && prior.equal(pub) {
			continue
		}
		add = append(add, server.ServerTool{Tool: tool, Handler: e.call})
	}
	var del []string
	for name := range e.published {
		if _, ok := want[name]; !ok {
			del = append(del, name)
		}
	}
	sort.Strings(del)
	if len(del) > 0 {
		e.srv.DeleteTools(del...)
	}
	if len(add) > 0 {
		e.addTools(add)
	}
	e.published = want
	e.version = snap.Version()
	e.seen.Store(snap.Version())
	e.synced.Store(true)
	if len(add) > 0 || len(del) > 0 {
		e.logger.Info("MCP catalogue updated", "added", len(add), "deleted", len(del), "tools", len(want))
	}
}

// addTools registers tools in one batch (one tools/list_changed). mcp-go
// panics on a schema it cannot register; then the tools are added one at a
// time and the ones it refuses are left out and logged.
func (e *Endpoint) addTools(tools []server.ServerTool) {
	if e.tryAdd(tools...) {
		return
	}
	for _, t := range tools {
		if !e.tryAdd(t) {
			e.logger.Error("Skipping tool: MCP refused its schema", "published", t.Tool.Name)
		}
	}
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

// describe returns the MCP form of one tool: its published name, its
// description, and its schema with free-form objects opened (and the
// TokenArgument added on an Argument endpoint). ok is false, logged, for a
// schema that cannot be encoded.
func (e *Endpoint) describe(info toolkit.Info) (mcp.Tool, publishedTool, bool) {
	params := toolkit.OpenObjectProperties(info.Tool.Parameters())
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if e.cfg.Transport == Argument {
		params = injectTokenArgument(params, e.cfg.TokenDescription)
	}
	schema, err := json.Marshal(params)
	if err != nil {
		e.logger.Warn("Skipping tool: schema cannot be encoded", "tool", info.Name, "error", err.Error())
		return mcp.Tool{}, publishedTool{}, false
	}
	pub := publishedTool{desc: info.Tool.Description(), schema: schema}
	return mcp.NewToolWithRawSchema(info.Published, pub.desc, schema), pub, true
}

// afterList replaces the list mcp-go built with one from snapshots taken
// now: the catalogue's on an Argument endpoint; on a Bearer endpoint the
// caller's set, limited to the catalogue. A list is never older than the
// registries at the time of the request.
func (e *Endpoint) afterList(ctx context.Context, _ any, _ *mcp.ListToolsRequest, result *mcp.ListToolsResult) {
	if result == nil {
		return
	}
	infos := e.cfg.Catalogue.Snapshot().List()
	if e.cfg.Transport == Bearer {
		infos = e.callerTools(ctx, infos)
	}
	tools := make([]mcp.Tool, 0, len(infos))
	seen := make(map[string]bool, len(infos))
	for _, info := range infos {
		tool, _, ok := e.describe(info)
		if !ok || seen[tool.Name] {
			continue
		}
		seen[tool.Name] = true
		tools = append(tools, tool)
	}
	slices.SortFunc(tools, func(a, b mcp.Tool) int { return cmp.Compare(a.Name, b.Name) })
	result.Tools = tools
	result.NextCursor = ""
}

// callerTools returns the tools of the caller on ctx whose published names
// the catalogue offers; none without a caller.
func (e *Endpoint) callerTools(ctx context.Context, catalogue []toolkit.Info) []toolkit.Info {
	c, ok := CallerOf(ctx)
	if !ok || c.Set == nil || c.revoked() {
		return nil
	}
	offered := make(map[string]bool, len(catalogue))
	for _, info := range catalogue {
		offered[info.Published] = true
	}
	var out []toolkit.Info
	for _, info := range c.Set.Snapshot().List() {
		if offered[info.Published] {
			out = append(out, info)
		}
	}
	return out
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
