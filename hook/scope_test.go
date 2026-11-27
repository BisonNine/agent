/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package hook_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

var alice = hook.Scope{
	AgentID:   "alice",
	AgentName: "Alice",
	Session:   "s-42",
	TurnID:    "0a1b2c3d",
	Depth:     1,
	Project:   "support",
}

func TestScopeRoundTrip(t *testing.T) {
	if _, ok := hook.ScopeOf(context.Background()); ok {
		t.Fatal("ScopeOf found a scope on an empty context")
	}
	ctx := hook.WithScope(context.Background(), alice)
	got, ok := hook.ScopeOf(ctx)
	if !ok || got != alice {
		t.Fatalf("ScopeOf = %+v, %v; want %+v", got, ok, alice)
	}
	// A child context keeps it; a later WithScope replaces it.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if got, _ := hook.ScopeOf(child); got != alice {
		t.Fatalf("child ScopeOf = %+v", got)
	}
	bob := hook.Scope{AgentID: "bob", Session: "s-7"}
	if got, _ := hook.ScopeOf(hook.WithScope(child, bob)); got != bob {
		t.Fatalf("replaced ScopeOf = %+v", got)
	}
}

// WithScope bridges to toolkit's per-call values, which tools read.
func TestScopeSetsToolkitValues(t *testing.T) {
	ctx := hook.WithScope(context.Background(), alice)
	if got := toolkit.AgentID(ctx); got != "alice" {
		t.Errorf("toolkit.AgentID = %q", got)
	}
	if got := toolkit.SessionKey(ctx); got != "s-42" {
		t.Errorf("toolkit.SessionKey = %q", got)
	}
}

func TestScopeIdentity(t *testing.T) {
	want := accounting.Identity{Agent: "alice", Name: "Alice", Project: "support"}
	if got := alice.Identity(); got != want {
		t.Fatalf("Identity = %+v, want %+v", got, want)
	}
	n, err := hook.Scope{AgentID: "bob"}.Identity().Normalize()
	if err != nil || n.Project != accounting.DefaultProject || n.Origin != "bob" {
		t.Fatalf("Normalize = %+v, %v", n, err)
	}
	// A sub-agent's scope names the agent whose work caused it.
	sub := hook.Scope{Origin: "alice", AgentID: "bob"}
	if got := sub.Identity(); got.Origin != "alice" || got.Agent != "bob" {
		t.Fatalf("Identity = %+v", got)
	}
}

func TestScopeLogValue(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	log.Info("turn", "scope", alice)
	log.Info("turn", "scope", hook.Scope{AgentID: "bob"})
	log.Info("empty", "scope", hook.Scope{})
	log.Info("own", "scope", hook.Scope{Origin: "bob", AgentID: "bob"})
	log.Info("sub", "scope", hook.Scope{Origin: "alice", AgentID: "bob"})
	got := buf.String()
	for _, want := range []string{
		"scope.agent=alice scope.agent_name=Alice scope.session=s-42 scope.turn=0a1b2c3d scope.depth=1 scope.project=support",
		"msg=turn scope.agent=bob\n",
		"msg=empty\n",               // a zero scope logs nothing
		"msg=own scope.agent=bob\n", // an origin that is the agent itself is not repeated
		"msg=sub scope.agent=bob scope.origin=alice\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
}
