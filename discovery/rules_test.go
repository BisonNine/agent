package discovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/toolkit"
)

// With discovery on, a provider that loads deferred tools natively is sent
// every tool on every call, with the same definitions: listed tools plain,
// search_tools marked as the tool-search tool, hidden tools deferred, sorted
// by internal name. Searching, asking for details and calling a hidden tool
// change nothing.
func TestToolDefs_DiscoveryOn_FullStableFlaggedList(t *testing.T) {
	c, _ := discoveryOnRegistry(t)
	first := ToolDefs(c.Snapshot(), nil, true).Send
	wantNames := []string{
		"file_read", "get_tool_details", "mcp_alice_read", "mcp_alice_write",
		"mcp_bob_fetch", "mcp_bob_post", "x_weather", "search_tools", "time_now",
	}
	if got := defNames(first); !slices.Equal(got, wantNames) {
		t.Fatalf("names = %v, want %v (every tool, sorted by internal name)", got, wantNames)
	}
	core := map[string]bool{"file_read": true, "time_now": true, "get_tool_details": true}
	for _, d := range first {
		switch {
		case d.Name == SearchToolName:
			if !d.ToolSearch || d.Deferred {
				t.Errorf("search_tools: ToolSearch=%v Deferred=%v, want true/false", d.ToolSearch, d.Deferred)
			}
		case core[d.Name]:
			if d.ToolSearch || d.Deferred {
				t.Errorf("listed %s: ToolSearch=%v Deferred=%v, want false/false", d.Name, d.ToolSearch, d.Deferred)
			}
		default:
			if !d.Deferred || d.ToolSearch {
				t.Errorf("hidden %s: Deferred=%v ToolSearch=%v, want true/false", d.Name, d.Deferred, d.ToolSearch)
			}
		}
	}

	sess := NewSession()
	for _, step := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"search", SearchToolName, map[string]any{"query": "notebook note"}},
		{"regex search", SearchToolName, map[string]any{"query": "bob", "regex": true}},
		{"details", DetailsToolName, map[string]any{"name": "mcp_alice_read"}},
		{"details by bare name", DetailsToolName, map[string]any{"name": "x_weather"}},
		{"call a hidden tool", "mcp_bob_post", nil},
	} {
		res := call(t, c, step.tool, step.args)
		if res.IsError {
			t.Fatalf("%s: %s", step.name, res.ForLLM)
		}
		sess.Record(res.ToolReferences)
		if got := ToolDefs(c.Snapshot(), sess, true).Send; !reflect.DeepEqual(got, first) {
			t.Fatalf("after %s the definitions changed:\n got %+v\nwant %+v", step.name, got, first)
		}
	}
}

// With discovery off nothing is hidden and there are no discovery tools:
// the list is exactly the registry's, with no flag, for either kind of
// provider.
func TestToolDefs_DiscoveryOff_NoFlags(t *testing.T) {
	c := toolkit.New()
	mustAdd(t, c,
		listed(newMockTool("time_now", "Current time")),
		listed(&extTool{mockTool: *newMockTool("mcp_x_weather", "Weather forecast for a city"), external: "x_weather"}),
		listed(newMockTool("file_read", "Read a file")),
	)
	params := newMockTool("", "").Parameters()
	want := []Def{
		{Name: "file_read", Description: "Read a file", Parameters: params},
		{Name: "x_weather", Description: "Weather forecast for a city", Parameters: params},
		{Name: "time_now", Description: "Current time", Parameters: params},
	}
	for _, native := range []bool{true, false} {
		for range 3 {
			got := ToolDefs(c.Snapshot(), NewSession([]string{"x_weather"}), native)
			if !reflect.DeepEqual(got.Send, want) || !reflect.DeepEqual(got.InContext, want) {
				t.Fatalf("native=%v: defs =\n %+v\nwant\n %+v", native, got, want)
			}
		}
	}
}

// For a provider that cannot load deferred tools, the list is the listed
// tools, then the hidden tools revealed so far in order of first reveal; an
// unrevealed hidden tool is left out. InContext is the same for both kinds
// of provider. This is what spawnllm's request builder makes of the full
// flagged list.
func TestToolDefs_ListedPlusRevealed(t *testing.T) {
	c := toolkit.New()
	mustAdd(t, c,
		listed(newMockTool("file_read", "Read a file")),
		listed(newMockTool("time_now", "Current time")),
		hidden(newMockTool("mcp_x_read", "Read a record from X"), "", false),
		hidden(newMockTool("mcp_x_write", "Write a record to X"), "", false),
		hidden(newMockTool("mcp_bob_fetch", "Fetch a page for Bob"), "", false),
	)
	ix := NewIndex(c, MaxResults(5))
	mountMeta(t, c, ix)
	core := []string{"file_read", "get_tool_details", "search_tools", "time_now"}

	found := call(t, c, SearchToolName, map[string]any{"query": "^mcp_x_read$", "regex": true}).ToolReferences
	if want := []string{"mcp_x_read"}; !slices.Equal(found, want) {
		t.Fatalf("search references = %v, want %v", found, want)
	}
	tests := []struct {
		name string
		sess *Session
		want []string
	}{
		{"referenced", NewSession(found), append(slices.Clone(core), "mcp_x_read")},
		{"no references", NewSession(nil), core},
		{"no session", nil, core},
		{"reveal order", NewSession([]string{"mcp_x_write"}, []string{"mcp_bob_fetch", "mcp_x_write"}), append(slices.Clone(core), "mcp_x_write", "mcp_bob_fetch")},
		{"listed and unknown names ignored", NewSession([]string{"time_now", "mcp_carol_gone", "mcp_x_read"}), append(slices.Clone(core), "mcp_x_read")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defs := ToolDefs(c.Snapshot(), tt.sess, false)
			if got := defNames(defs.Send); !slices.Equal(got, tt.want) {
				t.Errorf("send = %v, want %v", got, tt.want)
			}
			if !reflect.DeepEqual(defs.Send, defs.InContext) {
				t.Errorf("InContext differs from Send for a provider without deferred loading")
			}
			for _, d := range defs.Send {
				if d.Deferred {
					t.Errorf("%s is marked deferred", d.Name)
				}
			}
			native := ToolDefs(c.Snapshot(), tt.sess, true)
			if !reflect.DeepEqual(native.InContext, defs.InContext) {
				t.Errorf("native InContext = %v, want %v", defNames(native.InContext), defNames(defs.InContext))
			}
		})
	}
}

// A hidden tool runs whether or not anything revealed it; an unknown name
// is still not found.
func TestDiscovery_HiddenToolExecutesWithoutSearch(t *testing.T) {
	c, _ := discoveryOnRegistry(t)
	for _, name := range []string{"mcp_bob_fetch", "x_weather", "mcp_x_weather"} {
		if res := call(t, c, name, nil); res.IsError {
			t.Errorf("hidden %s: %s, want it run", name, res.ForLLM)
		}
	}
	res := call(t, c, "mcp_carol_missing", nil)
	if want := fmt.Sprintf("tool %q not found", "mcp_carol_missing"); !res.IsError || res.ForLLM != want {
		t.Errorf("unknown tool: %+v, want %q", res, want)
	}
}

// Hiding changes only what is listed: what an MCP host serves (the whole
// snapshot) and what may be called are the same with or without it.
func TestHidden_ChangesOnlyListing(t *testing.T) {
	tools := []toolkit.Entry{
		listed(newMockTool("crm_lookup", "Look up a customer")),
		listed(newMockTool("crm_refund", "Refund an order")),
	}
	plain, hid := toolkit.New(), toolkit.New()
	if _, err := plain.AddToolset(t.Context(), staticToolset{"crm", tools}); err != nil {
		t.Fatal(err)
	}
	if _, err := hid.AddToolset(t.Context(), Hidden(staticToolset{"crm", tools})); err != nil {
		t.Fatal(err)
	}
	if got, want := hid.Snapshot().Names(), plain.Snapshot().Names(); !slices.Equal(got, want) {
		t.Errorf("served = %v, want %v", got, want)
	}
	if len(hid.Snapshot().Listed()) != 0 || len(hid.Snapshot().Hidden()) != 2 {
		t.Errorf("listed %d, hidden %d; want 0 and 2", len(hid.Snapshot().Listed()), len(hid.Snapshot().Hidden()))
	}
	for _, name := range []string{"crm_lookup", "crm_refund"} {
		if res := call(t, hid, name, nil); res.IsError {
			t.Errorf("%s: %s", name, res.ForLLM)
		}
	}
	if Hidden(nil) != nil {
		t.Error("Hidden(nil) is not nil")
	}
}

type staticToolset struct {
	name    string
	entries []toolkit.Entry
}

func (s staticToolset) Name() string { return s.name }
func (s staticToolset) Tools(context.Context) ([]toolkit.Entry, error) {
	return slices.Clone(s.entries), nil
}

type failingToolset struct{}

func (failingToolset) Name() string { return "broken" }
func (failingToolset) Tools(context.Context) ([]toolkit.Entry, error) {
	return nil, errors.New("upstream down")
}

func TestHidden_PassesToolsetError(t *testing.T) {
	if _, err := toolkit.New().AddToolset(t.Context(), Hidden(failingToolset{})); err == nil || !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("err = %v, want the toolset's error", err)
	}
}

// Pins keep tools listed; the rest are hidden and carry their group.
func TestHideEntries_Pins(t *testing.T) {
	entries := []toolkit.Entry{
		listed(newMockTool("crm_start_here", "Start")),
		listed(newMockTool("crm_lookup", "Look up")),
		listed(newMockTool("CRM__Refund", "Refund")),
		listed(&extTool{mockTool: *newMockTool("mcp_bob_fetch", "Fetch"), external: "bob_fetch"}),
		listed(&groupedTool{mockTool: *newMockTool("svc_alice_read", "Read"), group: "alice", reveal: true}),
		{Tool: &groupedTool{mockTool: *newMockTool("svc_alice_write", "Write"), group: "alice", reveal: true}, Group: "own"},
	}
	tests := []struct {
		name   string
		opts   []HideOption
		listed []string
	}{
		{"no pins", nil, nil},
		{"exact name", []HideOption{AlwaysShown("crm_start_here")}, []string{"crm_start_here"}},
		{"prefix, case and underscores", []HideOption{AlwaysShown(" crm_* ")}, []string{"crm_start_here", "crm_lookup", "CRM__Refund"}},
		{"external name", []HideOption{AlwaysShown("bob")}, []string{"mcp_bob_fetch"}},
		{"internal name", []HideOption{AlwaysShown("mcp_")}, []string{"mcp_bob_fetch"}},
		{"everything", []HideOption{AlwaysShown("*")}, []string{"crm_start_here", "crm_lookup", "CRM__Refund", "mcp_bob_fetch", "svc_alice_read", "svc_alice_write"}},
		{"empty pattern", []HideOption{AlwaysShown("", "  ")}, nil},
		{"func and patterns combine", []HideOption{AlwaysShown("svc_"), AlwaysShownFunc(func(n string) bool { return n == "bob_fetch" })}, []string{"mcp_bob_fetch", "svc_alice_read", "svc_alice_write"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := HideEntries(entries, tt.opts...)
			var got []string
			for _, e := range out {
				if !e.Hidden {
					got = append(got, e.Tool.Name())
				}
			}
			if !slices.Equal(got, tt.listed) {
				t.Errorf("listed = %v, want %v", got, tt.listed)
			}
			if entries[0].Hidden {
				t.Fatal("HideEntries changed its input")
			}
		})
	}
	out := HideEntries(entries)
	if out[4].Group != "alice" || !out[4].RevealTogether {
		t.Errorf("grouped tool: group %q reveal %v, want alice/true", out[4].Group, out[4].RevealTogether)
	}
	if out[5].Group != "own" || out[5].RevealTogether {
		t.Errorf("an entry's own group is kept: got %q/%v", out[5].Group, out[5].RevealTogether)
	}
}

// AlwaysShownFunc is asked with both names of a tool.
func TestAlwaysShownFunc_BothNames(t *testing.T) {
	var asked []string
	HideEntries([]toolkit.Entry{listed(&extTool{mockTool: *newMockTool("mcp_bob_fetch", ""), external: "bob_fetch"})},
		AlwaysShownFunc(func(n string) bool { asked = append(asked, n); return false }))
	if want := []string{"mcp_bob_fetch", "bob_fetch"}; !slices.Equal(asked, want) {
		t.Errorf("asked = %v, want %v", asked, want)
	}
}

// A tool the agent is not granted, or a disabled one, is never searched,
// described, counted in the namespaces or sent.
func TestDiscovery_GrantsAndDisabled(t *testing.T) {
	c := toolkit.New()
	mustAdd(t, c,
		hidden(newMockTool("crm_lookup", "Look up a customer record"), "crm", true),
		hidden(newMockTool("crm_refund", "Refund a customer order"), "crm", true),
		hidden(newMockTool("crm_export", "Export customer data"), "crm", true),
		hidden(newMockTool("hr_payroll", "Customer payroll"), "", false),
	)
	alice := c.Set("alice")
	alice.Grant(allowOnly{"crm_lookup": true, "crm_refund": true, "crm_export": true, SearchToolName: true, DetailsToolName: true})
	if err := alice.Disable("crm_export"); err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(alice)
	if _, err := c.AddToolset(t.Context(), MetaTools(func(context.Context) *Index { return ix })); err != nil {
		t.Fatal(err)
	}
	visible := []string{"crm_lookup", "crm_refund"}
	snap := alice.Snapshot()

	if got := hitNames(ix.Search(snap, "customer")); !sameSet(got, visible) {
		t.Errorf("search = %v, want %v", got, visible)
	}
	regex, err := ix.SearchRegex(snap, ".")
	if err != nil {
		t.Fatal(err)
	}
	if got := hitNames(regex); !slices.Equal(got, visible) {
		t.Errorf("regex = %v, want %v", got, visible)
	}
	if got := ix.Namespaces(snap); !slices.Equal(got, []string{"crm"}) {
		t.Errorf("namespaces = %v, want [crm]", got)
	}
	for _, name := range []string{"crm_export", "hr_payroll"} {
		if _, ok := ix.Details(snap, name); ok {
			t.Errorf("details of %s returned", name)
		}
		res := alice.Execute(t.Context(), DetailsToolName, map[string]any{"name": name}, nil)
		if !res.IsError || len(res.ToolReferences) != 0 {
			t.Errorf("get_tool_details %s: %+v, want not found", name, res)
		}
	}
	if d, _ := ix.Details(snap, "crm_lookup"); !slices.Equal(d.References, visible) {
		t.Errorf("reveal-together references = %v, want %v (no disabled member)", d.References, visible)
	}
	res := alice.Execute(t.Context(), SearchToolName, map[string]any{"query": "customer"}, nil)
	if !sameSet(res.ToolReferences, visible) {
		t.Errorf("search_tools references = %v, want %v", res.ToolReferences, visible)
	}
	sess := NewSession([]string{"crm_export", "hr_payroll", "crm_lookup"})
	for _, native := range []bool{true, false} {
		defs := ToolDefs(snap, sess, native)
		for _, d := range slices.Concat(defs.Send, defs.InContext) {
			if d.Name == "crm_export" || d.Name == "hr_payroll" {
				t.Errorf("native=%v sends %s", native, d.Name)
			}
		}
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// A query is answered from the snapshot it names: an older snapshot never
// sees a tool added after it, a newer one always does, whatever order the
// queries come in and whether or not a change notice has been delivered.
func TestIndex_AnswersForItsSnapshot(t *testing.T) {
	c := toolkit.New()
	ix := NewIndex(c)
	mustAdd(t, c, hidden(newMockTool("tool_alpha", "alpha functionality"), "", false))
	old := c.Snapshot()
	mustAdd(t, c, hidden(newMockTool("tool_beta", "beta functionality"), "", false))
	cur := c.Snapshot()

	for range 3 {
		if got := hitNames(ix.Search(cur, "beta")); !slices.Equal(got, []string{"tool_beta"}) {
			t.Fatalf("current snapshot: %v", got)
		}
		if got := hitNames(ix.Search(old, "beta")); len(got) != 0 {
			t.Fatalf("old snapshot found %v", got)
		}
	}
	// The newer index stays cached after a query about the older snapshot.
	if first := ix.corpusFor(cur); ix.corpusFor(cur) != first {
		t.Error("the current snapshot's index is rebuilt on every query")
	}

	// A grant policy that changes its answers moves no version; the index
	// still follows the snapshot.
	policy := &switchable{allow: map[string]bool{"tool_alpha": true}}
	set := c.Set("bob")
	set.Grant(policy)
	six := NewIndex(set)
	before := set.Snapshot()
	if got := hitNames(six.Search(before, "functionality")); !slices.Equal(got, []string{"tool_alpha"}) {
		t.Fatalf("before: %v", got)
	}
	policy.set("tool_beta", true)
	after := set.Snapshot()
	if before.Version() != after.Version() {
		t.Fatalf("precondition: version moved %d -> %d", before.Version(), after.Version())
	}
	if got := hitNames(six.Search(after, "functionality")); !sameSet(got, []string{"tool_alpha", "tool_beta"}) {
		t.Errorf("after the policy changed: %v", got)
	}
}

type switchable struct {
	mu    sync.Mutex
	allow map[string]bool
}

func (s *switchable) IsToolAllowed(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allow[name]
}

func (s *switchable) set(name string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allow[name] = v
}

// Searches race registry changes: every answer matches the snapshot it was
// asked about.
func TestIndex_ConcurrentChanges(t *testing.T) {
	c := toolkit.New()
	ix := NewIndex(c, MaxResults(100))
	mustAdd(t, c, hidden(newMockTool("tool_000", "widget"), "", false))

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 1; i < 60; i++ {
			if err := c.AddEntries(hidden(newMockTool(fmt.Sprintf("tool_%03d", i), "widget"), "", false)); err != nil {
				t.Error(err)
				return
			}
			if i%3 == 0 {
				c.Remove(fmt.Sprintf("tool_%03d", i-1))
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 60 {
				snap := c.Snapshot()
				hidden := snap.Hidden()
				want := make([]string, 0, len(hidden))
				for _, in := range hidden {
					want = append(want, in.Published)
				}
				got := hitNames(ix.Search(snap, "widget"))
				regex, err := ix.SearchRegex(snap, "widget")
				if err != nil {
					t.Error(err)
					return
				}
				if !sameSet(got, want) || !slices.Equal(hitNames(regex), want) {
					t.Errorf("version %d: search %v, regex %v, want %v", snap.Version(), got, hitNames(regex), want)
					return
				}
			}
		})
	}
	wg.Wait()
}

type fakeProvider struct{ native bool }

func (f fakeProvider) SupportsDeferredTools(model string) bool { return f.native && model == "m1" }

func TestNative(t *testing.T) {
	tests := []struct {
		name     string
		provider any
		model    string
		want     bool
	}{
		{"capable model", fakeProvider{native: true}, "m1", true},
		{"other model", fakeProvider{native: true}, "m2", false},
		{"not capable", fakeProvider{}, "m1", false},
		{"no interface", struct{}{}, "m1", false},
		{"nil", nil, "m1", false},
	}
	for _, tt := range tests {
		if got := Native(tt.provider, tt.model); got != tt.want {
			t.Errorf("%s: Native = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSession(t *testing.T) {
	s := NewSession([]string{"b", "a"}, nil, []string{"a", "c"})
	s.Record([]string{"d", "b"})
	if got, want := s.Revealed(), []string{"b", "a", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("revealed = %v, want %v", got, want)
	}
	var zero Session
	zero.Record([]string{"x"})
	if got := zero.Revealed(); !slices.Equal(got, []string{"x"}) {
		t.Errorf("zero session revealed = %v", got)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { s.Record([]string{strconv.Itoa(i)}) })
	}
	wg.Wait()
	if n := len(s.Revealed()); n != 12 {
		t.Errorf("revealed %d names, want 12", n)
	}
}

// The meta-tools stay listed when their toolset is hidden, search_tools is
// never deferred, and a search still works (no index recursion).
func TestMetaTools_NeverHidden(t *testing.T) {
	c := toolkit.New()
	ix := NewIndex(c)
	mustAdd(t, c, hidden(newMockTool("crm_lookup", "Look up a customer"), "", false))
	if _, err := c.AddToolset(t.Context(), Hidden(ix.MetaTools())); err != nil {
		t.Fatal(err)
	}
	if got := len(c.Snapshot().Listed()); got != 2 {
		t.Fatalf("listed %d tools, want the 2 meta-tools", got)
	}

	// Mounted hidden by hand: search_tools is still sent and searchable.
	h := toolkit.New()
	hix := NewIndex(h)
	mustAdd(t, h,
		hidden(newMockTool("crm_lookup", "Look up a customer"), "", false),
		toolkit.Entry{Tool: &searchTool{resolve: func(context.Context) *Index { return hix }, fixed: hix}, Hidden: true},
	)
	for _, native := range []bool{true, false} {
		defs := ToolDefs(h.Snapshot(), nil, native)
		if !slices.Contains(defNames(defs.Send), SearchToolName) || !slices.Contains(defNames(defs.InContext), SearchToolName) {
			t.Errorf("native=%v: search_tools not sent: %v", native, defNames(defs.Send))
		}
		for _, d := range defs.Send {
			if d.Name == SearchToolName && (d.Deferred || !strings.HasSuffix(d.Description, "groups: crm.")) {
				t.Errorf("native=%v: search_tools = %+v", native, d)
			}
		}
	}
	if res := call(t, h, SearchToolName, map[string]any{"query": "customer"}); !slices.Equal(res.ToolReferences, []string{"crm_lookup"}) {
		t.Errorf("search: %+v", res)
	}
}

func TestMaxResults_BelowOneKeepsDefault(t *testing.T) {
	for _, n := range []int{0, -3} {
		if got := NewIndex(toolkit.New(), MaxResults(n)).maxResults; got != DefaultMaxResults {
			t.Errorf("MaxResults(%d) = %d, want %d", n, got, DefaultMaxResults)
		}
	}
}

// get_tool_details on search_tools shows the description the tool list
// carries, with the agent's hidden namespaces, also when resolved per agent.
func TestToolDetails_SearchToolDescription(t *testing.T) {
	c := toolkit.New()
	ix := NewIndex(c)
	mustAdd(t, c, hidden(newMockTool("crm_lookup", "Look up a customer"), "", false))
	if _, err := c.AddToolset(t.Context(), MetaTools(func(context.Context) *Index { return ix })); err != nil {
		t.Fatal(err)
	}
	res := call(t, c, DetailsToolName, map[string]any{"name": SearchToolName})
	if res.IsError || !strings.Contains(res.ForLLM, "Discoverable tool groups: crm.") || len(res.ToolReferences) != 0 {
		t.Errorf("details of search_tools: %+v", res)
	}
}
