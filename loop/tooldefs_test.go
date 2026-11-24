package loop

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// defsTool is a tool with a fixed description and no parameters.
type defsTool struct{ name, desc string }

func (d defsTool) Name() string               { return d.name }
func (d defsTool) Description() string        { return d.desc }
func (d defsTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (d defsTool) Execute(context.Context, map[string]any) *toolkit.Result {
	return toolkit.SilentResult("ran " + d.name)
}

// discoveryCatalog is a registry with discovery on: two listed tools, three
// hidden ones and the discovery meta-tools.
func discoveryCatalog(t *testing.T) *toolkit.Catalog {
	t.Helper()
	c := toolkit.New()
	err := c.AddEntries(
		toolkit.Entry{Tool: defsTool{"time_now", "Current time"}},
		toolkit.Entry{Tool: defsTool{"file_read", "Read a file"}},
		toolkit.Entry{Tool: defsTool{"mcp_alice_read", "Read a note from Alice's notebook"}, Hidden: true},
		toolkit.Entry{Tool: defsTool{"mcp_bob_fetch", "Fetch a page for Bob"}, Hidden: true},
		toolkit.Entry{Tool: defsTool{"mcp_bob_post", "Post a page for Bob"}, Hidden: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddToolset(t.Context(), discovery.Hidden(discovery.NewIndex(c).MetaTools())); err != nil {
		t.Fatal(err)
	}
	return c
}

func defNames(defs []spawnllm.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Function.Name)
	}
	return out
}

// ToolDefs: Send is every tool sorted by internal name, the hidden ones
// Deferred, whatever was revealed; InContext is the listed tools, then the
// revealed hidden ones in order of first reveal, none Deferred.
func TestToolDefs_SendAndInContext(t *testing.T) {
	c := discoveryCatalog(t)
	allNames := []string{"file_read", "get_tool_details", "mcp_alice_read", "mcp_bob_fetch", "mcp_bob_post", "search_tools", "time_now"}
	listed := []string{"file_read", "get_tool_details", "search_tools", "time_now"}
	tests := []struct {
		name      string
		revealed  *discovery.Session
		inContext []string
	}{
		{"nothing revealed", nil, listed},
		{"empty session", discovery.NewSession(), listed},
		{"reveal order", discovery.NewSession([]string{"mcp_bob_post"}, []string{"mcp_alice_read", "mcp_bob_post"}), append(slices.Clone(listed), "mcp_bob_post", "mcp_alice_read")},
	}
	first := ToolDefs(c.Snapshot(), nil).Send
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := ToolDefs(c.Snapshot(), tt.revealed)
			if got := defNames(d.Send); !slices.Equal(got, allNames) {
				t.Fatalf("Send = %v, want %v", got, allNames)
			}
			if !reflect.DeepEqual(d.Send, first) {
				t.Fatalf("Send changed with what was revealed:\n got %+v\nwant %+v", d.Send, first)
			}
			for _, def := range d.Send {
				hidden := slices.Contains([]string{"mcp_alice_read", "mcp_bob_fetch", "mcp_bob_post"}, def.Function.Name)
				if def.Type != "function" || def.Deferred != hidden || def.ToolSearch != (def.Function.Name == "search_tools") {
					t.Errorf("Send %s = %+v, want type function, Deferred %v", def.Function.Name, def, hidden)
				}
			}
			if got := defNames(d.InContext); !slices.Equal(got, tt.inContext) {
				t.Fatalf("InContext = %v, want %v", got, tt.inContext)
			}
			for _, def := range d.InContext {
				if def.Deferred {
					t.Errorf("InContext %s is Deferred", def.Function.Name)
				}
			}
		})
	}
}

// No snapshot (an agent with no tools) gives nil lists; a snapshot with no
// tool gives empty ones.
func TestToolDefs_Empty(t *testing.T) {
	if d := ToolDefs(nil, nil); d.Send != nil || d.InContext != nil {
		t.Fatalf("ToolDefs(nil) = %+v, want nil lists", d)
	}
	d := ToolDefs(toolkit.New().Snapshot(), discovery.NewSession([]string{"mcp_bob_fetch"}))
	if d.Send == nil || len(d.Send) != 0 || d.InContext == nil || len(d.InContext) != 0 {
		t.Fatalf("ToolDefs(empty) = %#v, want empty non-nil lists", d)
	}
}

// The list the engine sends the model is ToolDefs' Send for the agent's
// registry, definition for definition and in order.
func TestToolDefs_IsWhatTheEngineSends(t *testing.T) {
	f := newAgentFixture(t)
	f.reg = discoveryCatalog(t)
	f.p.Script("w1", modelstest.Reply("hi Bob"))
	a := f.agent(t, nil)
	if res := a.Run(context.Background(), "s1", hook.UserText("hello")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	got := f.p.Calls()
	if len(got) != 1 {
		t.Fatalf("calls = %d, want 1", len(got))
	}
	if want := ToolDefs(f.reg.Snapshot(), nil).Send; !reflect.DeepEqual(got[0].Tools, want) {
		t.Fatalf("sent tools =\n %+v\nwant\n %+v", got[0].Tools, want)
	}
}
