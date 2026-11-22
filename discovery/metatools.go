package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.bison9.com/agent/toolkit"
)

// The meta-tools' names.
const (
	SearchToolName  = "search_tools"
	DetailsToolName = "get_tool_details"
)

// ToolsetName is the name of the toolset MetaTools returns.
const ToolsetName = "discovery"

const searchDescription = "Search for available tools by capability. Returns matching tool names with one-line descriptions and makes those tools available: call one directly, or call get_tool_details on a name to see its full schema. Natural-language query by default; set regex:true to match a regular expression if the natural-language search can't find the tool."

// searchDescriptionFor is search_tools' description for a set whose hidden
// tools fall in namespaces. It changes only when the hidden tools do, never
// with what a conversation has searched, so the tool list stays stable.
func searchDescriptionFor(namespaces []string) string {
	if len(namespaces) == 0 {
		return searchDescription
	}
	return searchDescription + " Discoverable tool groups: " + strings.Join(namespaces, ", ") + "."
}

// unavailable is the result of a meta-tool whose resolver found no index.
const unavailable = "Tool discovery is not available."

// MetaTools returns the toolset holding search_tools and get_tool_details
// (both listed), answering for the calling agent's tools: resolve returns
// the Index of the registry the call came through (for instance by
// toolkit.AgentID(ctx)), or nil when there is none. Results reference what
// they find (toolkit.Result.ToolReferences), from the registry's current
// snapshot, so a tool the agent is not granted, or a disabled one, is never
// found.
//
// Without a context, search_tools' Description cannot know the agent and
// leaves out the hidden namespaces; ToolDefs, which knows the snapshot,
// adds them. For one registry, Index.MetaTools does both.
func MetaTools(resolve func(ctx context.Context) *Index) toolkit.Toolset {
	return metaToolset{resolve: resolve}
}

// MetaTools is MetaTools answering always from ix: search_tools'
// Description then lists the namespaces of ix's registry's hidden tools.
func (ix *Index) MetaTools() toolkit.Toolset {
	return metaToolset{resolve: func(context.Context) *Index { return ix }, fixed: ix}
}

type metaToolset struct {
	resolve func(ctx context.Context) *Index
	fixed   *Index
}

func (metaToolset) Name() string { return ToolsetName }

func (m metaToolset) Tools(context.Context) ([]toolkit.Entry, error) {
	return []toolkit.Entry{
		{Tool: &searchTool{resolve: m.resolve, fixed: m.fixed}},
		{Tool: &detailsTool{resolve: m.resolve}},
	}, nil
}

// index returns the index for a call, or nil.
func resolveIndex(ctx context.Context, resolve func(context.Context) *Index) *Index {
	if resolve == nil {
		return nil
	}
	return resolve(ctx)
}

// searchTool is search_tools, the entry point of progressive discovery: it
// returns the hidden tools matching a query, one line each (name and first
// line of description), and references them, which loads their deferred
// definitions so the model can call them right away. BM25 is the default;
// regex:true matches a regular expression over name and description.
type searchTool struct {
	resolve func(context.Context) *Index
	fixed   *Index
}

func (t *searchTool) Name() string { return SearchToolName }

func (t *searchTool) Description() string {
	if t.fixed == nil || t.fixed.reg == nil {
		return searchDescription
	}
	return searchDescriptionFor(t.fixed.Namespaces(t.fixed.reg.Snapshot()))
}

func (t *searchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "What you want to do (natural language), or a regex pattern when regex is true.",
			},
			"regex": map[string]any{
				"type":        "boolean",
				"description": "Treat query as a regex over tool name/description instead of a natural-language search.",
			},
		},
		"required": []string{"query"},
	}
}

func (t *searchTool) Execute(ctx context.Context, args map[string]any) *toolkit.Result {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		// An empty query would match every hidden tool.
		return toolkit.ErrorResult("Missing or invalid 'query' argument. Must be a non-empty string.")
	}
	useRegex, _ := args["regex"].(bool) //nolint:errcheck // absent or not a bool: a natural-language search
	ix := resolveIndex(ctx, t.resolve)
	if ix == nil || ix.reg == nil {
		return toolkit.ErrorResult(unavailable)
	}
	snap := ix.reg.Snapshot()

	var hits []Hit
	if useRegex {
		if len(query) > MaxRegexPatternLength {
			return toolkit.ErrorResult(fmt.Sprintf("Pattern too long: max %d characters allowed", MaxRegexPatternLength))
		}
		var err error
		hits, err = ix.SearchRegex(snap, query)
		if err != nil {
			return toolkit.ErrorResult(fmt.Sprintf("Invalid regex pattern syntax: %v. Please fix your regex and try again.", err))
		}
	} else {
		hits = ix.Search(snap, query)
	}
	if len(hits) == 0 {
		return toolkit.SilentResult("No tools found matching the query.")
	}
	ix.logger.InfoContext(ctx, "Tool search completed", "regex", useRegex, "results", len(hits))

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d tool(s):\n", len(hits))
	refs := make([]string, 0, len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "- %s: %s\n", h.Name, firstLine(h.Description))
		refs = append(refs, h.Name)
	}
	b.WriteString("\nThese tools can be called now. Call get_tool_details(name) to see a tool's full schema.")
	res := toolkit.SilentResult(b.String())
	res.ToolReferences = refs
	return res
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// detailsTool is get_tool_details: one tool's full schema and, for a hidden
// tool, a reference to it (with the rest of its group when the group is
// reveal-together), which loads its deferred definition.
type detailsTool struct {
	resolve func(context.Context) *Index
}

func (t *detailsTool) Name() string { return DetailsToolName }

func (t *detailsTool) Description() string {
	return "Show the full input schema of a tool found via search_tools. Pass the exact tool name from the search results."
}

func (t *detailsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Exact tool name from search_tools results.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *detailsTool) Execute(ctx context.Context, args map[string]any) *toolkit.Result {
	name, ok := args["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return toolkit.ErrorResult("Missing or invalid 'name' argument. Pass an exact tool name from search_tools.")
	}
	name = strings.TrimSpace(name)
	ix := resolveIndex(ctx, t.resolve)
	if ix == nil || ix.reg == nil {
		return toolkit.ErrorResult(unavailable)
	}

	d, ok := ix.Details(ix.reg.Snapshot(), name)
	if !ok {
		return toolkit.ErrorResult(fmt.Sprintf("No tool named %q. Use search_tools to find the correct name.", name))
	}
	b, err := json.Marshal(d.Schema)
	if err != nil {
		return toolkit.ErrorResult("Failed to format tool schema: " + err.Error())
	}
	ix.logger.InfoContext(ctx, "Tool details returned", "tool", d.Published, "references", len(d.References))
	res := toolkit.SilentResult(fmt.Sprintf("%s\n\nCall it as `%s`.", string(b), d.Published))
	res.ToolReferences = d.References
	return res
}
