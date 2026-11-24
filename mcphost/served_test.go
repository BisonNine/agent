package mcphost

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"
)

// A served turn's token resolves to its scope until the turn ends, and the
// prompt carries it for an Argument endpoint.
func TestServed_IssuesAndRevokes(t *testing.T) {
	st := NewStore()
	var served hook.ServedTools = Served{Store: st, CLI: CLIConfig{Transport: Argument, ToolPrefix: "mcp__alice__"}}
	prompt, end, err := served.ServeTurn(context.Background(), hook.Scope{AgentID: "alice", Session: "s1", TurnID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	_, token, ok := strings.Cut(prompt, TokenArgument+": ")
	if !ok || token == "" {
		t.Fatalf("prompt carries no token: %q", prompt)
	}
	g, _, ok := st.Resolve(token)
	if !ok || g.Scope.TurnID != "t1" {
		t.Fatalf("Resolve = %+v, %v", g, ok)
	}
	end()
	end()
	if _, _, ok := st.Resolve(token); ok {
		t.Fatal("token still resolves after end")
	}
}
