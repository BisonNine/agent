package agent_test

import (
	"testing"

	"go.bison9.com/agent"
	"go.bison9.com/agent/agenttest"
)

// The SDK's own agent passes the conformance suite.
func TestConformance(t *testing.T) {
	agenttest.Run(t, func(f agenttest.Fixture) agent.Agent {
		a, err := agent.New("alice", append(f.Options(), agent.WithName("Alice"))...)
		if err != nil {
			t.Fatal(err)
		}
		return a
	})
}
