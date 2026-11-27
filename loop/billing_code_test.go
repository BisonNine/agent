/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// billingCodeBox is a host's per-session billing code: what a billing tool
// sets and Spec.TurnBillingCode returns.
type billingCodeBox struct {
	mu    sync.Mutex
	code  string
	asked []hook.Scope
}

func (b *billingCodeBox) set(code string) {
	b.mu.Lock()
	b.code = code
	b.mu.Unlock()
}

func (b *billingCodeBox) turnBillingCode(_ context.Context, s hook.Scope) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, s)
	return b.code
}

// scopeEvents keeps the scope of every event of the kinds a test reads.
type scopeEvents struct {
	mu      sync.Mutex
	started []hook.Scope
	calls   []hook.Scope
	tools   []hook.Scope
	done    []hook.Scope
}

func (e *scopeEvents) onEvent(_ context.Context, ev hook.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch ev := ev.(type) {
	case hook.TurnStarted:
		e.started = append(e.started, ev.Scope)
	case hook.ModelCall:
		e.calls = append(e.calls, ev.Scope)
	case hook.ToolCallStarted:
		e.tools = append(e.tools, ev.Scope)
	case hook.TurnFinished:
		e.done = append(e.done, ev.Scope)
	}
}

// A billing code the host changes during a turn applies from the next model
// call: each call is accounted to (and its ModelCall reports) the billing
// code it was made under; the tool, a task it starts and what follows the
// loop see the billing code of their time.
func TestTurnBillingCode_ChangesBetweenModelCalls(t *testing.T) {
	f := newAgentFixture(t)
	box := &billingCodeBox{code: "alpha"}
	var toolScope, taskScope, afterTurn hook.Scope
	f.tool(t, "billing", func(ctx context.Context, _ map[string]any) (string, error) {
		toolScope, _ = hook.ScopeOf(ctx)
		box.set("beta")
		return "Billing code set to beta.", nil
	})
	f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
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
	f.p.Script("w1", calls("billing"), calls("spawn"), modelstest.Reply("done"))
	events := &scopeEvents{}
	a := f.agent(t, func(s *Spec) {
		s.BillingCode = "garden"
		s.TurnBillingCode = box.turnBillingCode
		s.Plugins = []hook.Plugin{{
			OnEvent: events.onEvent,
			AfterTurn: func(ctx context.Context, _ *hook.Turn, _ *hook.TurnResult) {
				afterTurn, _ = hook.ScopeOf(ctx)
			},
		}}
	})
	res := a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	if res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	pc := f.p.Calls()
	got := make([]string, 0, len(pc))
	for _, c := range pc {
		got = append(got, c.Identity.BillingCode)
	}
	if strings.Join(got, ",") != "alpha,beta,beta" {
		t.Fatalf("model calls accounted to %v, want alpha, beta, beta", got)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	evs := make([]string, 0, len(events.calls))
	for _, s := range events.calls {
		evs = append(evs, s.BillingCode)
	}
	if strings.Join(evs, ",") != "alpha,beta,beta" {
		t.Fatalf("ModelCall billing codes = %v", evs)
	}
	if events.started[0].BillingCode != "alpha" || events.done[0].BillingCode != "beta" {
		t.Fatalf("TurnStarted %q, TurnFinished %q", events.started[0].BillingCode, events.done[0].BillingCode)
	}
	if toolScope.BillingCode != "alpha" || events.tools[0].BillingCode != "alpha" || events.tools[1].BillingCode != "beta" {
		t.Fatalf("tool scopes %q, %q, %q", toolScope.BillingCode, events.tools[0].BillingCode, events.tools[1].BillingCode)
	}
	if taskScope.BillingCode != "beta" || afterTurn.BillingCode != "beta" {
		t.Fatalf("task %q, AfterTurn %q", taskScope.BillingCode, afterTurn.BillingCode)
	}
	// Asked at set-up and before each of the three dispatches, with the
	// scope as it stood.
	box.mu.Lock()
	defer box.mu.Unlock()
	var turnAsks []hook.Scope
	for _, s := range box.asked {
		if s.TurnID != "" {
			turnAsks = append(turnAsks, s)
		}
	}
	box.asked = turnAsks
	if len(box.asked) != 4 || box.asked[0].Session != "s1" || box.asked[0].TurnID == "" || box.asked[0].BillingCode != "garden" || box.asked[3].BillingCode != "beta" {
		t.Fatalf("asked %+v", box.asked)
	}
}

// Without TurnBillingCode, or when it answers "", the turn is accounted to the
// agent's own billing code.
func TestTurnBillingCode_EmptyKeepsIdentityBillingCode(t *testing.T) {
	for _, tt := range []struct {
		name string
		fn   func(context.Context, hook.Scope) string
	}{
		{name: "nil", fn: nil},
		{name: "empty", fn: func(context.Context, hook.Scope) string { return "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentFixture(t)
			f.p.Script("w1", modelstest.Reply("hi"))
			a := f.agent(t, func(s *Spec) {
				s.BillingCode = "garden"
				s.TurnBillingCode = tt.fn
			})
			if res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); res.Outcome != hook.OutcomeOK {
				t.Fatalf("result = %+v", res)
			}
			if got := f.p.Calls()[0].Identity.BillingCode; got != "garden" {
				t.Fatalf("billing code = %q, want garden", got)
			}
		})
	}
}

// An empty answer after a billing code was chosen goes back to the agent's
// own.
func TestTurnBillingCode_EmptyRevertsMidTurn(t *testing.T) {
	f := newAgentFixture(t)
	box := &billingCodeBox{code: "alpha"}
	f.tool(t, "clear", func(context.Context, map[string]any) (string, error) {
		box.set("")
		return "cleared", nil
	})
	f.p.Script("w1", calls("clear"), modelstest.Reply("done"))
	a := f.agent(t, func(s *Spec) { s.TurnBillingCode = box.turnBillingCode; s.BillingCode = "garden" })
	if res := a.Run(context.Background(), "s1", hook.UserText("go")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	c := f.p.Calls()
	if c[0].Identity.BillingCode != "alpha" || c[1].Identity.BillingCode != "garden" {
		t.Fatalf("billing codes %q, %q", c[0].Identity.BillingCode, c[1].Identity.BillingCode)
	}
}

// A billing code CheckBillingCode refuses ends the turn with an error wrapping
// accounting.ErrInvalidBillingCode, before any model call under it: at set-up,
// with no TurnStarted; mid-turn, before the next dispatch.
func TestTurnBillingCode_InvalidFailsTheTurn(t *testing.T) {
	t.Run("set-up", func(t *testing.T) {
		f := newAgentFixture(t)
		events := &scopeEvents{}
		a := f.agent(t, func(s *Spec) {
			s.TurnBillingCode = func(context.Context, hook.Scope) string { return "not valid" }
			s.Plugins = []hook.Plugin{{OnEvent: events.onEvent}}
		})
		res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
		if res.Outcome != hook.OutcomeError || !errors.Is(res.Err, accounting.ErrInvalidBillingCode) {
			t.Fatalf("result = %+v", res)
		}
		if n := len(f.p.Calls()); n != 0 {
			t.Fatalf("%d model calls", n)
		}
		events.mu.Lock()
		defer events.mu.Unlock()
		if len(events.started) != 0 || len(events.done) != 1 {
			t.Fatalf("TurnStarted %d, TurnFinished %d", len(events.started), len(events.done))
		}
		if got := a.sessions.GetHistory("s1"); len(got) != 0 {
			t.Fatalf("history = %+v", got)
		}
	})
	t.Run("mid-turn", func(t *testing.T) {
		f := newAgentFixture(t)
		box := &billingCodeBox{code: "alpha"}
		f.tool(t, "billing", func(context.Context, map[string]any) (string, error) {
			box.set(strings.Repeat("x", accounting.MaxNameLength+1))
			return "set", nil
		})
		f.p.Script("w1", calls("billing"), modelstest.Reply("done"))
		a := f.agent(t, func(s *Spec) { s.TurnBillingCode = box.turnBillingCode })
		res := a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
		if res.Outcome != hook.OutcomeError || !errors.Is(res.Err, accounting.ErrInvalidBillingCode) || res.Iterations != 1 {
			t.Fatalf("result = %+v", res)
		}
		if n := len(f.p.Calls()); n != 1 {
			t.Fatalf("%d model calls, want 1", n)
		}
	})
}

// A panic in TurnBillingCode ends the turn with an *EnginePanicError: a set-up
// one at set-up, an engine one mid-turn.
func TestTurnBillingCode_Panic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		after int // panic on this call (1 is set-up)
		setUp bool
	}{
		{name: "set-up", after: 1, setUp: true},
		{name: "mid-turn", after: 2, setUp: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentFixture(t)
			n := 0
			a := f.agent(t, func(s *Spec) {
				s.TurnBillingCode = func(context.Context, hook.Scope) string {
					n++
					if n == tt.after {
						panic("billing broke")
					}
					return "alpha"
				}
			})
			res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
			var ep *EnginePanicError
			if res.Outcome != hook.OutcomeError || !errors.As(res.Err, &ep) || ep.Value != "billing broke" || ep.SetUp != tt.setUp {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

// summaryFixture adds a summary model ("sum", wire id "ws") to f's
// catalogue.
func summaryFixture(t *testing.T, f *agentFixture) {
	t.Helper()
	if err := f.cat.Add(modelspec.Model{ModelName: "sum", Model: "ws", Provider: "api", MaxTokens: 4096}, models.For(models.Summarize, 1)); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.RegisterClient("sum", f.p); err != nil {
		t.Fatal(err)
	}
}

// The agent's summary model is accounted to the billing code of the scope on
// the call's context, else the agent's own.
func TestSummaryCaller_TakesBillingCodeFromScope(t *testing.T) {
	f := newAgentFixture(t)
	summaryFixture(t, f)
	a := f.agent(t, func(s *Spec) { s.BillingCode = "garden" })
	c := summaryCaller{a: a}
	for _, tt := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no scope", ctx: context.Background(), want: "garden"},
		{name: "scope without billing code", ctx: hook.WithScope(context.Background(), hook.Scope{AgentID: "x"}), want: "garden"},
		{name: "scope billing code", ctx: hook.WithScope(context.Background(), hook.Scope{BillingCode: "beta"}), want: "beta"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.p.Reset()
			if _, err := c.Complete(tt.ctx, hook.CompleteRequest{System: "s", User: "u"}); err != nil {
				t.Fatal(err)
			}
			calls := f.p.Calls()
			if len(calls) != 1 || calls[0].Model != "ws" || calls[0].Identity.BillingCode != tt.want || calls[0].Identity.Agent != "Alice" {
				t.Fatalf("calls = %+v", calls)
			}
		})
	}
}

// A compaction inside a turn summarises under the turn's billing code as it is
// then: inline at a message add, and the forced one after a context-window
// error, which here comes after the billing code changed.
func TestTurnBillingCode_CompactionInTurn(t *testing.T) {
	f := newAgentFixture(t)
	summaryFixture(t, f)
	box := &billingCodeBox{code: "alpha"}
	changedAt := 0
	f.tool(t, "billing", func(context.Context, map[string]any) (string, error) {
		changedAt = len(f.p.Calls())
		box.set("beta")
		return "set", nil
	})
	f.p.Script("w1", modelstest.Reply("one"), modelstest.Reply("two"), calls("billing"),
		modelstest.Fail(errors.New("context_length_exceeded: request too long")), modelstest.Reply("done"))
	f.p.Script("ws", modelstest.Reply("Alice and Bob talked."))
	a := f.agent(t, func(s *Spec) {
		s.BillingCode = "garden"
		s.Limits = hook.Limits{Retries: 1}
		s.TurnBillingCode = box.turnBillingCode
		// A small window, so the long messages fill it.
		s.TurnModels = func(context.Context, hook.Scope) TurnModels {
			return TurnModels{Chain: f.cat.Default().Begin(models.Chat), Limits: &modelspec.Limits{ContextWindow: 400, MaxTokens: 100}}
		}
	})
	long := strings.Repeat("Bob asks Alice about the garden. ", 40)
	for _, msg := range []string{long, long, long} {
		if res := a.Run(context.Background(), "s1", hook.UserText(msg)).Wait(); res.Outcome != hook.OutcomeOK {
			t.Fatalf("result = %+v", res)
		}
	}
	// Every summary made before the tool changed the billing code is accounted to
	// alpha, every one after it to beta.
	var sums []string
	for i, c := range f.p.Calls() {
		if c.Model != "ws" {
			continue
		}
		want := "alpha"
		if i >= changedAt {
			want = "beta"
		}
		if c.Identity.BillingCode != want {
			t.Fatalf("summary %d accounted to %q, want %q", i, c.Identity.BillingCode, want)
		}
		sums = append(sums, c.Identity.BillingCode)
	}
	if !slices.Contains(sums, "beta") {
		t.Fatalf("summaries accounted to %v, want one under beta", sums)
	}
}

// dueContext is a context manager whose deferred compaction records the
// scope and a value of its context, and summarises with the agent's caller.
type dueContext struct {
	*fakeContext

	caller hook.Caller
	scope  hook.Scope
	value  any
	ran    bool
}

type valueKey struct{}

func (d *dueContext) CompactIfDue(ctx context.Context) (bool, error) {
	d.ran = true
	d.scope, _ = hook.ScopeOf(ctx)
	d.value = ctx.Value(valueKey{})
	_, err := d.caller.Complete(ctx, hook.CompleteRequest{System: "s", User: "u"})
	return true, err
}

// The compaction the agent runs after a turn asks TurnBillingCode, with the
// run's context values and the session's scope (no turn), and its summary is
// accounted to that billing code. A billing code refused there, or a panic,
// skips the compaction.
func TestTurnBillingCode_DeferredCompaction(t *testing.T) {
	for _, tt := range []struct {
		name   string
		code   string
		panics bool
		want   string // "" when the compaction is skipped
	}{
		{name: "valid", code: "beta", want: "beta"},
		{name: "empty", code: "", want: "garden"},
		{name: "invalid", code: "be ta"},
		{name: "panic", panics: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentFixture(t)
			summaryFixture(t, f)
			var asked hook.Scope
			var askedValue any
			a := f.agent(t, func(s *Spec) {
				s.BillingCode = "garden"
				s.TurnBillingCode = func(ctx context.Context, sc hook.Scope) string {
					asked, askedValue = sc, ctx.Value(valueKey{})
					if tt.panics {
						panic("billing broke")
					}
					return tt.code
				}
			})
			due := &dueContext{fakeContext: &fakeContext{}, caller: summaryCaller{a: a}}
			st := &sessionState{key: "s1", cm: due}
			a.compactIfDue(context.WithValue(context.Background(), valueKey{}, "bob"), st)
			if asked.Session != "s1" || asked.TurnID != "" || askedValue != "bob" {
				t.Fatalf("asked with scope %+v, value %v", asked, askedValue)
			}
			if tt.want == "" {
				if due.ran || len(f.p.Calls()) != 0 {
					t.Fatal("compaction ran")
				}
				return
			}
			if !due.ran || due.scope.BillingCode != tt.want || due.value != "bob" {
				t.Fatalf("ran %v, scope %+v, value %v", due.ran, due.scope, due.value)
			}
			if c := f.p.Calls(); len(c) != 1 || c[0].Identity.BillingCode != tt.want {
				t.Fatalf("calls = %+v", c)
			}
		})
	}
}

// A deferred compaction of a session with no context manager does nothing,
// and asks nothing.
func TestTurnBillingCode_DeferredCompactionWithoutManager(t *testing.T) {
	f := newAgentFixture(t)
	asked := false
	a := f.agent(t, func(s *Spec) {
		s.TurnBillingCode = func(context.Context, hook.Scope) string { asked = true; return "" }
	})
	a.compactIfDue(context.Background(), &sessionState{key: "s1"})
	if asked {
		t.Fatal("TurnBillingCode asked")
	}
}
