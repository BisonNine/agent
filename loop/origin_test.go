/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
)

// subAgent makes the fixture's agent Bob, a sub-agent started for Alice's
// work.
func subAgent(s *Spec) { s.ID, s.Name = "Bob", "Bob" }

// The origin TurnOrigin names, asked once at set-up with the run's context
// and the turn's scope, is the origin of every model call of the turn and is
// seen by the tool, a task it starts, the events and AfterTurn.
func TestTurnOrigin_CarriedThroughTheTurn(t *testing.T) {
	f := newAgentFixture(t)
	var toolScope, taskScope, afterTurn hook.Scope
	f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
		toolScope, _ = hook.ScopeOf(ctx)
		reg, _ := hook.TaskRegistrarFrom(ctx)
		tctx, h, err := reg.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{Kind: "subagent"}})
		if err != nil {
			return "", err
		}
		taskScope, _ = hook.ScopeOf(tctx)
		h.Finish(nil)
		h.Exited()
		return "started", nil
	})
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"))
	events := &scopeEvents{}
	// The compaction after the turn asks too (no TurnID), from the run's
	// goroutine, possibly after Wait.
	var mu sync.Mutex
	var asked []hook.Scope
	var askedValue any
	a := f.agent(t, func(s *Spec) {
		subAgent(s)
		s.Project = "garden"
		s.TurnProject = func(context.Context, hook.Scope) string { return "alpha" }
		s.TurnOrigin = func(ctx context.Context, sc hook.Scope) string {
			mu.Lock()
			defer mu.Unlock()
			if sc.TurnID != "" {
				asked, askedValue = append(asked, sc), ctx.Value(valueKey{})
			}
			return "Alice"
		}
		s.Plugins = []hook.Plugin{{
			OnEvent: events.onEvent,
			AfterTurn: func(ctx context.Context, _ *hook.Turn, _ *hook.TurnResult) {
				afterTurn, _ = hook.ScopeOf(ctx)
			},
		}}
	})
	ctx := context.WithValue(context.Background(), valueKey{}, "run")
	if res := a.Run(ctx, "s1", hook.UserText("go")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0].Session != "s1" || asked[0].TurnID == "" || asked[0].Project != "alpha" || asked[0].Origin != "" || askedValue != "run" {
		t.Fatalf("asked %+v with value %v, want once at set-up", asked, askedValue)
	}
	pc := f.p.Calls()
	if len(pc) != 2 {
		t.Fatalf("%d model calls, want 2", len(pc))
	}
	for i, c := range pc {
		if c.Identity.Origin != "Alice" || c.Identity.Agent != "Bob" {
			t.Fatalf("model call %d identity %+v, want origin Alice, agent Bob", i, c.Identity)
		}
	}
	if toolScope.Origin != "Alice" || taskScope.Origin != "Alice" || afterTurn.Origin != "Alice" {
		t.Fatalf("tool %q, task %q, AfterTurn %q", toolScope.Origin, taskScope.Origin, afterTurn.Origin)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if events.started[0].Origin != "Alice" || events.calls[0].Origin != "Alice" || events.tools[0].Origin != "Alice" || events.done[0].Origin != "Alice" {
		t.Fatalf("event origins %q, %q, %q, %q", events.started[0].Origin, events.calls[0].Origin, events.tools[0].Origin, events.done[0].Origin)
	}
}

// Without TurnOrigin, or when it answers "", the turn's origin is the
// identity's: empty (the agent itself, as a record names it) unless the
// identity names one.
func TestTurnOrigin_EmptyKeepsIdentityOrigin(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fn       func(context.Context, hook.Scope) string
		identity string
		want     string
	}{
		{name: "nil", fn: nil, want: ""},
		{name: "empty", fn: func(context.Context, hook.Scope) string { return "" }, want: ""},
		{name: "identity origin", fn: func(context.Context, hook.Scope) string { return "" }, identity: "Alice", want: "Alice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentFixture(t)
			f.p.Script("w1", modelstest.Reply("hi"))
			a := f.agent(t, func(s *Spec) {
				subAgent(s)
				if tt.identity != "" {
					s.Identity = accounting.Identity{Origin: tt.identity, Agent: "Bob", Name: "Bob"}
				}
				s.TurnOrigin = tt.fn
			})
			if res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); res.Outcome != hook.OutcomeOK {
				t.Fatalf("result = %+v", res)
			}
			id := f.p.Calls()[0].Identity
			if id.Origin != tt.want {
				t.Fatalf("origin = %q, want %q", id.Origin, tt.want)
			}
			// What a launch records: the agent itself when none is named.
			n, err := id.Normalize()
			wantRecorded := tt.want
			if wantRecorded == "" {
				wantRecorded = "Bob"
			}
			if err != nil || n.Origin != wantRecorded {
				t.Fatalf("recorded origin = %q (%v), want %q", n.Origin, err, wantRecorded)
			}
		})
	}
}

// A panic in TurnOrigin at set-up ends the turn with a set-up
// *EnginePanicError, before any model call.
func TestTurnOrigin_Panic(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, func(s *Spec) {
		s.TurnOrigin = func(context.Context, hook.Scope) string { panic("origin broke") }
	})
	res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
	var ep *EnginePanicError
	if res.Outcome != hook.OutcomeError || !errors.As(res.Err, &ep) || ep.Value != "origin broke" || !ep.SetUp {
		t.Fatalf("result = %+v", res)
	}
	if n := len(f.p.Calls()); n != 0 {
		t.Fatalf("%d model calls", n)
	}
}

// The agent's summary model takes its origin from the scope on the call's
// context, else the identity's.
func TestSummaryCaller_TakesOriginFromScope(t *testing.T) {
	f := newAgentFixture(t)
	summaryFixture(t, f)
	a := f.agent(t, subAgent)
	c := summaryCaller{a: a}
	for _, tt := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no scope", ctx: context.Background(), want: ""},
		{name: "scope without origin", ctx: hook.WithScope(context.Background(), hook.Scope{AgentID: "Bob"}), want: ""},
		{name: "scope origin", ctx: hook.WithScope(context.Background(), hook.Scope{Origin: "Alice"}), want: "Alice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.p.Reset()
			if _, err := c.Complete(tt.ctx, hook.CompleteRequest{System: "s", User: "u"}); err != nil {
				t.Fatal(err)
			}
			calls := f.p.Calls()
			if len(calls) != 1 || calls[0].Identity.Origin != tt.want || calls[0].Identity.Agent != "Bob" {
				t.Fatalf("calls = %+v", calls)
			}
		})
	}
}

// The compaction the agent runs after a turn asks TurnOrigin, after
// TurnProject, with the run's context values and the session's scope (no
// turn), and its summary names that origin. A panic skips the compaction.
func TestTurnOrigin_DeferredCompaction(t *testing.T) {
	for _, tt := range []struct {
		name   string
		origin string
		panics bool
		want   string
		skip   bool
	}{
		{name: "named", origin: "Alice", want: "Alice"},
		{name: "empty", origin: "", want: ""},
		{name: "panic", panics: true, skip: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentFixture(t)
			summaryFixture(t, f)
			var asked hook.Scope
			var askedValue any
			a := f.agent(t, func(s *Spec) {
				subAgent(s)
				s.TurnProject = func(context.Context, hook.Scope) string { return "beta" }
				s.TurnOrigin = func(ctx context.Context, sc hook.Scope) string {
					asked, askedValue = sc, ctx.Value(valueKey{})
					if tt.panics {
						panic("origin broke")
					}
					return tt.origin
				}
			})
			due := &dueContext{fakeContext: &fakeContext{}, caller: summaryCaller{a: a}}
			st := &sessionState{key: "s1", cm: due}
			a.compactIfDue(context.WithValue(context.Background(), valueKey{}, "run"), st)
			if asked.Session != "s1" || asked.TurnID != "" || asked.Project != "beta" || askedValue != "run" {
				t.Fatalf("asked with scope %+v, value %v", asked, askedValue)
			}
			if tt.skip {
				if due.ran || len(f.p.Calls()) != 0 {
					t.Fatal("compaction ran")
				}
				return
			}
			if !due.ran || due.scope.Origin != tt.want || due.scope.Project != "beta" {
				t.Fatalf("ran %v, scope %+v", due.ran, due.scope)
			}
			if c := f.p.Calls(); len(c) != 1 || c[0].Identity.Origin != tt.want || c[0].Identity.Project != "beta" {
				t.Fatalf("calls = %+v", c)
			}
		})
	}
}
