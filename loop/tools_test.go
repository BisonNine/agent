/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

import (
	"context"
	"slices"
	"testing"

	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// The tools a result references (a discovery search's hits) reach the tool
// message the engine stores and sends next, so spawnllm loads their
// deferred definitions for the rest of the conversation.
func TestEngine_ToolReferencesReachTheToolMessage(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "find_tools", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	refs := []string{"bob_search", "bob_fetch"}
	f.tool("find_tools", func(context.Context, map[string]any) *toolkit.Result {
		return &toolkit.Result{ForLLM: "found 2 tools", ToolReferences: refs}
	})

	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	var stored int
	for _, m := range f.cm.stored {
		if m.Role == "tool" {
			stored++
			if m.ToolCallID != "c1" || !slices.Equal(m.ToolReferences, refs) {
				t.Fatalf("stored tool message = %+v, want call c1 with references %v", m, refs)
			}
		}
	}
	if stored != 1 {
		t.Fatalf("stored %d tool messages, want 1", stored)
	}
	calls := p.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	i := slices.IndexFunc(calls[1].Messages, func(m spawnllm.Message) bool { return m.Role == "tool" })
	if i < 0 || !slices.Equal(calls[1].Messages[i].ToolReferences, refs) {
		t.Fatalf("sent messages = %+v, want the tool message with references %v", calls[1].Messages, refs)
	}
}
