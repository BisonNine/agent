// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"errors"
	"testing"

	"go.bison9.com/agent/hook"
)

// A nil session adapts to a memory that does nothing.
func TestAsMemorySession_NilDoesNothing(t *testing.T) {
	ms := AsMemorySession(nil)
	ctx := context.Background()
	if got := ms.Recall(ctx, "hello"); got != nil {
		t.Fatalf("Recall = %v, want nil", got)
	}
	ms.Observe(ctx, 1, "user", "hello")
	ms.ToolsUsed(ctx, "file_read")
	if err := ms.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// Memory opens a session for the scope it is given, and passes an open error
// through.
func TestMemory_Open(t *testing.T) {
	var got hook.Scope
	var m hook.Memory = Memory(func(_ context.Context, s hook.Scope) (*Session, error) {
		got = s
		return nil, nil
	})
	if _, err := m.Open(context.Background(), hook.Scope{AgentID: "alice", Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "alice" || got.Session != "s1" {
		t.Fatalf("scope = %+v", got)
	}
	boom := errors.New("boom")
	m = Memory(func(context.Context, hook.Scope) (*Session, error) { return nil, boom })
	if _, err := m.Open(context.Background(), hook.Scope{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}
