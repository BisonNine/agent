/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

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

// It passes the suite unqueued too.
func TestConformanceUnqueued(t *testing.T) {
	agenttest.RunUnqueued(t, func(f agenttest.Fixture) agent.Agent {
		a, err := agent.New("alice", f.Options()...)
		if err != nil {
			t.Fatal(err)
		}
		return a
	})
}
