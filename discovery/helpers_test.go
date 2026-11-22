package discovery

import (
	"context"
	"testing"

	"go.bison9.com/agent/toolkit"
)

// mockTool is a tool whose result names it.
type mockTool struct {
	name string
	desc string
}

func newMockTool(name, desc string) *mockTool { return &mockTool{name: name, desc: desc} }

func (m *mockTool) Name() string               { return m.name }
func (m *mockTool) Description() string        { return m.desc }
func (m *mockTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (m *mockTool) Execute(context.Context, map[string]any) *toolkit.Result {
	return toolkit.SilentResult("mock executed: " + m.name)
}

// extTool is offered under another name, as an upstream MCP tool is.
type extTool struct {
	mockTool

	external string
}

func (e *extTool) ExternalName() string { return e.external }

// groupedTool reports its discovery group itself.
type groupedTool struct {
	mockTool

	group  string
	reveal bool
}

func (g *groupedTool) DiscoveryGroup() (string, bool) { return g.group, g.reveal }

func listed(t toolkit.Tool) toolkit.Entry { return toolkit.Entry{Tool: t} }

func hidden(t toolkit.Tool, group string, reveal bool) toolkit.Entry {
	return toolkit.Entry{Tool: t, Hidden: true, Group: group, RevealTogether: reveal}
}

// mustAdd registers entries on c.
func mustAdd(t *testing.T, c *toolkit.Catalog, entries ...toolkit.Entry) {
	t.Helper()
	if err := c.AddEntries(entries...); err != nil {
		t.Fatalf("AddEntries: %v", err)
	}
}

// mountMeta adds ix's meta-tools to c.
func mountMeta(t *testing.T, c *toolkit.Catalog, ix *Index) {
	t.Helper()
	if _, err := c.AddToolset(t.Context(), ix.MetaTools()); err != nil {
		t.Fatalf("AddToolset: %v", err)
	}
}

// discoveryOnRegistry is a registry as an agent with discovery on has it:
// two listed tools, the two discovery tools, and hidden tools: Alice's
// server (reveal-together), Bob's server (not), and a tool offered under a
// bare ExternalName.
func discoveryOnRegistry(t *testing.T) (*toolkit.Catalog, *Index) {
	t.Helper()
	c := toolkit.New()
	ix := NewIndex(c)
	mustAdd(t, c,
		listed(newMockTool("file_read", "mock tool file_read")),
		listed(newMockTool("time_now", "mock tool time_now")),
		hidden(newMockTool("mcp_alice_read", "Read a note from Alice's notebook"), "alice", true),
		hidden(newMockTool("mcp_alice_write", "Write a note to Alice's notebook"), "alice", true),
		hidden(newMockTool("mcp_bob_fetch", "Fetch a page for Bob"), "bob", false),
		hidden(newMockTool("mcp_bob_post", "Post a page for Bob"), "bob", false),
		hidden(&extTool{mockTool: *newMockTool("mcp_x_weather", "Weather forecast for a city"), external: "x_weather"}, "", false),
	)
	mountMeta(t, c, ix)
	return c, ix
}

// populatedRegistry: one listed tool that searches must not find, three
// hidden ones they must.
func populatedRegistry(t *testing.T, opts ...Option) (*toolkit.Catalog, *Index) {
	t.Helper()
	c := toolkit.New()
	ix := NewIndex(c, opts...)
	mustAdd(t, c,
		listed(newMockTool("core_search", "I am a visible core tool for searching files")),
		hidden(newMockTool("mcp_read_file", "Read the contents of a system file"), "", false),
		hidden(newMockTool("mcp_list_dir", "List directories and files in the system"), "", false),
		hidden(newMockTool("mcp_fetch_net", "Fetch data from a network database"), "", false),
	)
	mountMeta(t, c, ix)
	return c, ix
}

func call(t *testing.T, reg toolkit.Registry, name string, args map[string]any) *toolkit.Result {
	t.Helper()
	return reg.Execute(t.Context(), name, args, nil)
}

func defNames(defs []Def) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func hitNames(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Name)
	}
	return out
}

// allowOnly grants exactly the named tools.
type allowOnly map[string]bool

func (a allowOnly) IsToolAllowed(name string) bool { return a[name] }
