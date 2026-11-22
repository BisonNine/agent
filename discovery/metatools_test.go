package discovery

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/toolkit"
)

func TestSearchTool_RegexMode(t *testing.T) {
	c, _ := populatedRegistry(t)

	tests := []struct {
		name     string
		args     map[string]any
		wantErr  string
		wantText string
		wantRefs []string
	}{
		{name: "missing query", args: map[string]any{"regex": true}, wantErr: "Missing or invalid 'query'"},
		{name: "invalid syntax", args: map[string]any{"query": "[unclosed", "regex": true}, wantErr: "Invalid regex pattern syntax"},
		{name: "no match", args: map[string]any{"query": "alien", "regex": true}, wantText: "No tools found matching"},
		{
			name: "match", args: map[string]any{"query": "system", "regex": true},
			wantText: "get_tool_details", wantRefs: []string{"mcp_list_dir", "mcp_read_file"},
		},
		{
			name: "pattern too long", args: map[string]any{"query": strings.Repeat("a", MaxRegexPatternLength+1), "regex": true},
			wantErr: "Pattern too long",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, c, SearchToolName, tt.args)
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.ForLLM, tt.wantErr) {
					t.Errorf("result = %+v, want an error containing %q", res, tt.wantErr)
				}
				return
			}
			if res.IsError || !strings.Contains(res.ForLLM, tt.wantText) {
				t.Errorf("result = %+v, want text containing %q", res, tt.wantText)
			}
			if !slices.Equal(res.ToolReferences, tt.wantRefs) {
				t.Errorf("ToolReferences = %v, want %v", res.ToolReferences, tt.wantRefs)
			}
			for _, name := range tt.wantRefs {
				if !strings.Contains(res.ForLLM, "- "+name+": ") {
					t.Errorf("no line for %s in %q", name, res.ForLLM)
				}
			}
		})
	}
}

func TestSearchTool_BM25Mode(t *testing.T) {
	c, _ := populatedRegistry(t, MaxResults(3))

	if res := call(t, c, SearchToolName, map[string]any{"query": "   "}); !res.IsError || !strings.Contains(res.ForLLM, "Missing or invalid 'query'") {
		t.Errorf("blank query: %+v", res)
	}
	if res := call(t, c, SearchToolName, map[string]any{"query": "aliens spaceships"}); res.IsError || !strings.Contains(res.ForLLM, "No tools found matching") {
		t.Errorf("no match: %+v", res)
	}
	res := call(t, c, SearchToolName, map[string]any{"query": "read files"})
	if res.IsError {
		t.Fatalf("search: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "- mcp_read_file: Read the contents of a system file") {
		t.Errorf("no line for mcp_read_file in %q", res.ForLLM)
	}
	if !slices.Contains(res.ToolReferences, "mcp_read_file") {
		t.Errorf("ToolReferences = %v, want mcp_read_file among them", res.ToolReferences)
	}
	for _, ref := range res.ToolReferences {
		if !strings.Contains(res.ForLLM, "- "+ref+": ") {
			t.Errorf("reference %s has no line in %q", ref, res.ForLLM)
		}
	}
}

// The result text is the host's, byte for byte.
func TestMetaTools_ExactText(t *testing.T) {
	c, _ := populatedRegistry(t)

	res := call(t, c, SearchToolName, map[string]any{"query": "^mcp_(read_file|list_dir)$", "regex": true})
	want := "Found 2 tool(s):\n" +
		"- mcp_list_dir: List directories and files in the system\n" +
		"- mcp_read_file: Read the contents of a system file\n" +
		"\nThese tools can be called now. Call get_tool_details(name) to see a tool's full schema."
	if res.ForLLM != want || !res.Silent || res.IsError {
		t.Errorf("search = %+v\nwant text %q, silent", res, want)
	}

	res = call(t, c, DetailsToolName, map[string]any{"name": " mcp_read_file "})
	want = `{"function":{"description":"Read the contents of a system file","name":"mcp_read_file","parameters":{"type":"object"}},"type":"function"}` +
		"\n\nCall it as `mcp_read_file`."
	if res.ForLLM != want || !res.Silent || res.IsError {
		t.Errorf("details = %+v\nwant text %q, silent", res, want)
	}

	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{SearchToolName, map[string]any{"query": ""}, "Missing or invalid 'query' argument. Must be a non-empty string."},
		{SearchToolName, map[string]any{"query": "zzz"}, "No tools found matching the query."},
		{SearchToolName, map[string]any{"query": "(", "regex": true}, "Invalid regex pattern syntax: failed to compile regex pattern \"(\": error parsing regexp: missing closing ): `(?i)(`. Please fix your regex and try again."},
		{SearchToolName, map[string]any{"query": strings.Repeat("a", 201), "regex": true}, "Pattern too long: max 200 characters allowed"},
		{DetailsToolName, map[string]any{}, "Missing or invalid 'name' argument. Pass an exact tool name from search_tools."},
		{DetailsToolName, map[string]any{"name": "no_such_tool"}, `No tool named "no_such_tool". Use search_tools to find the correct name.`},
	}
	for _, tt := range tests {
		if res := call(t, c, tt.tool, tt.args); res.ForLLM != tt.want {
			t.Errorf("%s %v = %q, want %q", tt.tool, tt.args, res.ForLLM, tt.want)
		}
	}
}

// The meta-tools' definitions are the host's.
func TestMetaTools_Definitions(t *testing.T) {
	c, _ := populatedRegistry(t)
	snap := c.Snapshot()
	search, _ := snap.Get(SearchToolName)
	details, _ := snap.Get(DetailsToolName)

	wantSearch := "Search for available tools by capability. Returns matching tool names with one-line descriptions and makes those tools available: call one directly, or call get_tool_details on a name to see its full schema. Natural-language query by default; set regex:true to match a regular expression if the natural-language search can't find the tool. Discoverable tool groups: mcp."
	if got := search.Tool.Description(); got != wantSearch {
		t.Errorf("search_tools description = %q", got)
	}
	if got := details.Tool.Description(); got != "Show the full input schema of a tool found via search_tools. Pass the exact tool name from the search results." {
		t.Errorf("get_tool_details description = %q", got)
	}
	if got := fmt.Sprint(search.Tool.Parameters()); got != "map[properties:map[query:map[description:What you want to do (natural language), or a regex pattern when regex is true. type:string] regex:map[description:Treat query as a regex over tool name/description instead of a natural-language search. type:boolean]] required:[query] type:object]" {
		t.Errorf("search_tools parameters = %s", got)
	}
	if got := fmt.Sprint(details.Tool.Parameters()); got != "map[properties:map[name:map[description:Exact tool name from search_tools results. type:string]] required:[name] type:object]" {
		t.Errorf("get_tool_details parameters = %s", got)
	}
}

func TestToolDetails(t *testing.T) {
	c, _ := populatedRegistry(t)

	if res := call(t, c, DetailsToolName, map[string]any{"name": "no_such_tool"}); !res.IsError || !strings.Contains(res.ForLLM, "No tool named") {
		t.Errorf("unknown tool: %+v", res)
	}
	res := call(t, c, DetailsToolName, map[string]any{"name": "mcp_read_file"})
	if res.IsError || !strings.Contains(res.ForLLM, `"name":"mcp_read_file"`) || !strings.Contains(res.ForLLM, "Read the contents of a system file") {
		t.Errorf("hidden tool: %+v", res)
	}
	if want := []string{"mcp_read_file"}; !slices.Equal(res.ToolReferences, want) {
		t.Errorf("ToolReferences = %v, want %v", res.ToolReferences, want)
	}
	res = call(t, c, DetailsToolName, map[string]any{"name": "core_search"})
	if res.IsError || len(res.ToolReferences) != 0 {
		t.Errorf("listed tool: %+v, want its schema and no reference", res)
	}
}

func TestSearchRegex_DeterministicOrder(t *testing.T) {
	c := toolkit.New()
	ix := NewIndex(c, MaxResults(20))
	for i := range 20 {
		mustAdd(t, c, hidden(newMockTool(fmt.Sprintf("tool_%02d", i), "searchable tool"), "", false))
	}
	var first []string
	for attempt := range 10 {
		hits, err := ix.SearchRegex(c.Snapshot(), "searchable")
		if err != nil {
			t.Fatal(err)
		}
		names := hitNames(hits)
		if attempt == 0 {
			first = names
			if !slices.IsSorted(first) || len(first) != 20 {
				t.Fatalf("first run = %v, want all 20 in name order", first)
			}
		} else if !slices.Equal(names, first) {
			t.Fatalf("attempt %d = %v, want %v", attempt, names, first)
		}
	}
}

func TestSearch_LimitsAndListedFiltering(t *testing.T) {
	c := toolkit.New()
	mustAdd(t, c, listed(newMockTool("core_match", "I am core with match")))
	for i := range 10 {
		mustAdd(t, c, hidden(newMockTool(fmt.Sprintf("hidden_match_%d", i), "this has a match"), "", false))
	}
	snap := c.Snapshot()

	regex, err := NewIndex(c, MaxResults(4)).SearchRegex(snap, "match")
	if err != nil {
		t.Fatal(err)
	}
	bm25 := NewIndex(c, MaxResults(3)).Search(snap, "match")
	for name, hits := range map[string][]Hit{"regex": regex, "bm25": bm25} {
		want := map[string]int{"regex": 4, "bm25": 3}[name]
		if len(hits) != want {
			t.Errorf("%s: %d hits, want %d", name, len(hits), want)
		}
		if slices.Contains(hitNames(hits), "core_match") {
			t.Errorf("%s returned the listed tool", name)
		}
	}
}

// search_tools references exactly its hits, no more than MaxResults, each
// with one line; no hits means no references.
func TestSearchTool_ReferencesHits(t *testing.T) {
	c := toolkit.New()
	mountMeta(t, c, NewIndex(c, MaxResults(4)))
	mustAdd(t, c, listed(newMockTool("core_match", "core match tool")))
	for i := range 10 {
		mustAdd(t, c, hidden(newMockTool(fmt.Sprintf("hidden_match_%d", i), "this has a match\nsecond line"), "", false))
	}
	for _, args := range []map[string]any{{"query": "match"}, {"query": "match", "regex": true}} {
		res := call(t, c, SearchToolName, args)
		if res.IsError || len(res.ToolReferences) != 4 {
			t.Fatalf("%v: %+v, want 4 references", args, res)
		}
		for _, ref := range res.ToolReferences {
			if !strings.HasPrefix(ref, "hidden_match_") || !strings.Contains(res.ForLLM, "- "+ref+": this has a match\n") {
				t.Errorf("%v: reference %q lacks its one-line entry in %q", args, ref, res.ForLLM)
			}
		}
		if strings.Contains(res.ForLLM, "second line") {
			t.Errorf("%v: description beyond its first line in %q", args, res.ForLLM)
		}
	}
	for _, args := range []map[string]any{{"query": "spaceship"}, {"query": "spaceship", "regex": true}} {
		if res := call(t, c, SearchToolName, args); res.IsError || len(res.ToolReferences) != 0 {
			t.Errorf("%v: %+v, want no hits and no references", args, res)
		}
	}
}

// Search hits and details references use the name the tool is offered
// under, so they match the tool list.
func TestDiscovery_ReferencesUsePublishedNames(t *testing.T) {
	c, _ := discoveryOnRegistry(t)
	offered := defNames(ToolDefs(c.Snapshot(), nil, true).Send)

	res := call(t, c, SearchToolName, map[string]any{"query": "weather forecast city"})
	if !slices.Contains(res.ToolReferences, "x_weather") {
		t.Fatalf("search references = %v, want x_weather", res.ToolReferences)
	}
	res2 := call(t, c, DetailsToolName, map[string]any{"name": "mcp_x_weather"})
	if want := []string{"x_weather"}; !slices.Equal(res2.ToolReferences, want) {
		t.Fatalf("details references = %v, want %v", res2.ToolReferences, want)
	}
	for _, ref := range append(res.ToolReferences, res2.ToolReferences...) {
		if !slices.Contains(offered, ref) {
			t.Errorf("reference %q is not in the tool list %v", ref, offered)
		}
	}
}

// get_tool_details on a member of a reveal-together group references the
// whole group, the asked-for tool first; on other tools only that tool; on
// a listed tool nothing.
func TestToolDetails_RevealTogetherGroup(t *testing.T) {
	c, _ := discoveryOnRegistry(t)
	tests := []struct {
		name string
		want []string
	}{
		{"mcp_alice_write", []string{"mcp_alice_write", "mcp_alice_read"}},
		{"mcp_alice_read", []string{"mcp_alice_read", "mcp_alice_write"}},
		{"mcp_bob_fetch", []string{"mcp_bob_fetch"}},
		{"time_now", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := call(t, c, DetailsToolName, map[string]any{"name": tt.name})
			if res.IsError {
				t.Fatal(res.ForLLM)
			}
			if !slices.Equal(res.ToolReferences, tt.want) {
				t.Errorf("references = %v, want %v", res.ToolReferences, tt.want)
			}
			if !strings.Contains(res.ForLLM, `"name":"`+tt.name+`"`) {
				t.Errorf("text lacks the schema of %s: %q", tt.name, res.ForLLM)
			}
		})
	}
}

// search_tools' description depends only on the hidden tools.
func TestSearchTool_DescriptionStable(t *testing.T) {
	c, _ := discoveryOnRegistry(t)
	desc := func() string {
		in, _ := c.Snapshot().Get(SearchToolName)
		return in.Tool.Description()
	}
	before := desc()
	call(t, c, SearchToolName, map[string]any{"query": "notebook"})
	call(t, c, DetailsToolName, map[string]any{"name": "mcp_bob_fetch"})
	if got := desc(); got != before {
		t.Errorf("description changed after discovery calls:\n got %q\nwant %q", got, before)
	}
	if !strings.Contains(before, "Discoverable tool groups: mcp, x.") {
		t.Errorf("description = %q, want the hidden namespaces listed", before)
	}
}

// A tool added after a search is found by the next one.
func TestSearch_PicksUpNewTools(t *testing.T) {
	c := toolkit.New()
	mountMeta(t, c, NewIndex(c))
	mustAdd(t, c, hidden(newMockTool("tool_alpha", "alpha functionality"), "", false))
	if res := call(t, c, SearchToolName, map[string]any{"query": "alpha"}); !strings.Contains(res.ForLLM, "tool_alpha") {
		t.Fatalf("first search: %q", res.ForLLM)
	}
	mustAdd(t, c, hidden(newMockTool("tool_beta", "beta functionality"), "", false))
	if res := call(t, c, SearchToolName, map[string]any{"query": "beta"}); !strings.Contains(res.ForLLM, "tool_beta") {
		t.Errorf("after adding tool_beta: %q", res.ForLLM)
	}
}

// One catalogue, two agents: the meta-tools resolve each call's own set, so
// each agent finds only what it is granted.
func TestMetaTools_ResolvePerAgent(t *testing.T) {
	c := toolkit.New()
	mustAdd(t, c,
		hidden(newMockTool("crm_alice_lookup", "Look up a customer"), "", false),
		hidden(newMockTool("crm_bob_lookup", "Look up a customer"), "", false),
	)
	alice, bob := c.Set("alice"), c.Set("bob")
	alice.Grant(allowOnly{"crm_alice_lookup": true, SearchToolName: true, DetailsToolName: true})
	bob.Grant(allowOnly{"crm_bob_lookup": true, SearchToolName: true, DetailsToolName: true})
	indexes := map[string]*Index{"alice": NewIndex(alice), "bob": NewIndex(bob)}
	if _, err := c.AddToolset(t.Context(), MetaTools(func(ctx context.Context) *Index {
		return indexes[toolkit.AgentID(ctx)]
	})); err != nil {
		t.Fatal(err)
	}

	for agent, set := range map[string]*toolkit.Set{"alice": alice, "bob": bob} {
		ctx := toolkit.WithAgentID(t.Context(), agent)
		res := set.Execute(ctx, SearchToolName, map[string]any{"query": "customer"}, nil)
		if want := []string{"crm_" + agent + "_lookup"}; !slices.Equal(res.ToolReferences, want) {
			t.Errorf("%s: references = %v, want %v", agent, res.ToolReferences, want)
		}
	}
	res := c.Default().Execute(t.Context(), SearchToolName, map[string]any{"query": "customer"}, nil)
	if !res.IsError || res.ForLLM != unavailable {
		t.Errorf("no index: %+v, want %q", res, unavailable)
	}

	// Without a context the description cannot name the agent's groups;
	// the tool list built for a snapshot does.
	in, _ := alice.Snapshot().Get(SearchToolName)
	if got := in.Tool.Description(); got != searchDescription {
		t.Errorf("resolver description = %q, want the plain one", got)
	}
	for _, d := range ToolDefs(alice.Snapshot(), nil, true).Send {
		if d.Name == SearchToolName && !strings.HasSuffix(d.Description, " Discoverable tool groups: crm.") {
			t.Errorf("listed description = %q, want alice's groups", d.Description)
		}
	}
}
