package spawnllm

import (
	"context"
	"testing"
)

func TestAgentIDFromContext(t *testing.T) {
	if got := AgentIDFromContext(context.Background()); got != "" {
		t.Errorf("unset agent id = %q, want empty", got)
	}
	ctx := WithAgentID(context.Background(), "alice")
	if got := AgentIDFromContext(ctx); got != "alice" {
		t.Errorf("agent id = %q, want alice", got)
	}
	// A plain string key with the same name is a different key.
	//nolint:staticcheck // deliberately a colliding built-in key type
	if got := AgentIDFromContext(context.WithValue(context.Background(), "agent_id", "bob")); got != "" {
		t.Errorf("agent id from a foreign key = %q, want empty", got)
	}
}
