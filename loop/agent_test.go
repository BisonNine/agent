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
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

// agentFixture is an agent for Alice over one scripted model ("m1", wire id
// "w1") and a registry the test fills.
type agentFixture struct {
	p   *modelstest.Provider
	cat *models.Catalog
	reg *toolkit.Catalog
}

func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	p := modelstest.New()
	cat := models.New()
	if err := cat.AddProvider(modelspec.Provider{Name: "api", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Add(modelspec.Model{ModelName: "m1", Model: "w1", Provider: "api", MaxTokens: 4096}, models.For(models.Chat, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.RegisterClient("m1", p); err != nil {
		t.Fatal(err)
	}
	return &agentFixture{p: p, cat: cat, reg: toolkit.NewRegistry()}
}

// agent builds the agent, closed when the test ends.
func (f *agentFixture) agent(t *testing.T, edit func(*Spec)) *Agent {
	t.Helper()
	spec := Spec{ID: "Alice", Name: "Alice", Models: f.cat.Default(), Tools: f.reg}
	if edit != nil {
		edit(&spec)
	}
	a, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return a
}

// tool adds a tool that runs fn.
func (f *agentFixture) tool(t *testing.T, name string, fn func(ctx context.Context, args map[string]any) (string, error)) {
	t.Helper()
	err := f.reg.Add(toolspec.ToolDefinition{
		Name: name, Description: "test tool " + name,
		Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
			out, err := fn(call.Ctx, call.Args)
			if err != nil {
				return nil, err
			}
			return &toolspec.Result{ForLLM: out}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func calls(names ...string) modelstest.Step {
	tc := make([]spawnllm.ToolCall, 0, len(names))
	for i, n := range names {
		tc = append(tc, spawnllm.ToolCall{ID: "c" + string(rune('1'+i)), Name: n, Arguments: map[string]any{}})
	}
	return modelstest.Step{Response: &spawnllm.LLMResponse{ToolCalls: tc, FinishReason: "tool_calls"}}
}

func TestNew_Refuses(t *testing.T) {
	f := newAgentFixture(t)
	empty := models.New()
	tests := []struct {
		name string
		spec Spec
	}{
		{"bad id", Spec{ID: "", Models: f.cat.Default()}},
		{"no models", Spec{ID: "alice"}},
		{"no chat model", Spec{ID: "alice", Models: empty.Default()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.spec); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("New = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestAgent_RunAnswersAndStores(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Reply("hi Bob"))
	a := f.agent(t, nil)
	if a.ID() != "Alice" || a.Name() != "Alice" {
		t.Fatalf("ID, Name = %q, %q", a.ID(), a.Name())
	}
	res := a.Run(context.Background(), "s1", hook.UserText("hello")).Wait()
	if res.Outcome != hook.OutcomeOK || res.Reply != "hi Bob" {
		t.Fatalf("result = %+v", res)
	}
	hist := a.sessions.GetHistory("s1")
	if len(hist) != 2 || hist[0].Content != "hello" || hist[1].Content != "hi Bob" {
		t.Fatalf("history = %+v", hist)
	}
}

// A tool runs from the registry; a removed tool is offered no more and
// refused when called.
func TestAgent_ToolsFromRegistry(t *testing.T) {
	f := newAgentFixture(t)
	f.tool(t, "lookup", func(context.Context, map[string]any) (string, error) { return "found", nil })
	f.p.Script("w1", calls("lookup"), modelstest.Reply("done"))
	a := f.agent(t, nil)
	res := a.Run(context.Background(), "s1", hook.UserText("look")).Wait()
	if res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	got := f.p.Calls()
	if len(got) != 2 || len(got[0].Tools) != 1 || got[0].Tools[0].Function.Name != "lookup" {
		t.Fatalf("calls = %+v", got)
	}

	f.reg.Remove("lookup")
	f.p.Reset()
	f.p.Script("w1", calls("lookup"), modelstest.Reply("done"))
	var refused *toolkit.Result
	a2 := f.agent(t, func(s *Spec) {
		s.Plugins = []hook.Plugin{{AfterTool: func(_ context.Context, _ *hook.ToolCall, r *toolkit.Result) { refused = r }}}
	})
	a2.Run(context.Background(), "s1", hook.UserText("look")).Wait()
	if refused == nil || !refused.IsError {
		t.Fatalf("removed tool result = %+v", refused)
	}
	if c := f.p.Calls(); len(c[0].Tools) != 0 {
		t.Fatalf("removed tool still offered: %+v", c[0].Tools)
	}
}

// The allow checker refuses a tool the agent is not granted.
func TestAgent_AllowRefuses(t *testing.T) {
	f := newAgentFixture(t)
	ran := false
	f.tool(t, "refund", func(context.Context, map[string]any) (string, error) { ran = true; return "ok", nil })
	f.p.Script("w1", calls("refund"), modelstest.Reply("done"))
	a := f.agent(t, func(s *Spec) { s.Allow = allowNone{} })
	a.Run(context.Background(), "s1", hook.UserText("refund")).Wait()
	if ran {
		t.Fatal("a refused tool ran")
	}
}

type allowNone struct{}

func (allowNone) IsToolAllowed(string) bool { return false }

// Runs of one session wait in turn; Interrupt stops the running one and
// drops the queue.
func TestAgent_QueueAndInterrupt(t *testing.T) {
	f := newAgentFixture(t)
	hold := make(chan struct{})
	started := make(chan struct{})
	f.p.Script("w1", modelstest.Step{Hold: hold, Started: started, Response: &spawnllm.LLMResponse{Content: "late"}}, modelstest.Reply("ok"))
	var interrupted []hook.Interrupted
	var mu sync.Mutex
	a := f.agent(t, func(s *Spec) {
		s.Plugins = []hook.Plugin{{OnInterrupt: func(_ context.Context, ev hook.Interrupted) {
			mu.Lock()
			interrupted = append(interrupted, ev)
			mu.Unlock()
		}}}
	})
	first := a.Run(context.Background(), "s1", hook.UserText("one"))
	<-started
	second := a.Run(context.Background(), "s1", hook.UserText("two"))
	third := a.Run(context.Background(), "s1", hook.UserText("three"))
	other := a.Run(context.Background(), "s2", hook.UserText("other"))

	running, dropped := a.Interrupt("s1")
	if !running || dropped != 2 {
		t.Fatalf("Interrupt = %v, %d", running, dropped)
	}
	for i, r := range []hook.Run{first, second, third} {
		if res := r.Wait(); res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
			t.Fatalf("run %d = %+v", i, res)
		}
	}
	var kinds []string
	for ev, err := range second.Events() {
		if !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("dropped run error = %v", err)
		}
		kinds = append(kinds, strings.TrimPrefix(typeName(ev), "hook."))
	}
	if !slices.Equal(kinds, []string{"TurnFinished"}) {
		t.Fatalf("dropped run events = %v", kinds)
	}
	close(hold)
	if res := other.Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("other session = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(interrupted) != 1 || !interrupted[0].Running || interrupted[0].Dropped != 2 {
		t.Fatalf("OnInterrupt = %+v", interrupted)
	}
}

func typeName(ev hook.Event) string {
	switch ev.(type) {
	case hook.TurnFinished:
		return "TurnFinished"
	case hook.TurnStarted:
		return "TurnStarted"
	default:
		return "other"
	}
}

// A task started by a tool outlives an interrupt of its turn; cancelling it
// cascades to its own task first and runs OnCancel before its context ends.
func TestAgent_Tasks(t *testing.T) {
	f := newAgentFixture(t)
	var parentCtx, childCtx context.Context
	var order []string
	var mu sync.Mutex
	note := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}
	f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
		reg, ok := hook.TaskRegistrarFrom(ctx)
		if !ok {
			return "", errors.New("no registrar")
		}
		pctx, _, err := reg.StartTask(ctx, hook.TaskStart{
			Info:     hook.TaskInfo{ID: "p", Kind: "subagent"},
			OnCancel: func(error) bool { note("p"); return true },
		})
		if err != nil {
			return "", err
		}
		cctx, _, err := reg.StartTask(pctx, hook.TaskStart{
			Info:     hook.TaskInfo{ID: "c"},
			OnCancel: func(error) bool { note("c"); return true },
		})
		if err != nil {
			return "", err
		}
		parentCtx, childCtx = pctx, cctx //nolint:fatcontext // kept for the test to inspect
		return "started", nil
	})
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"))
	a := f.agent(t, nil)
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()

	tasks := a.Tasks()
	if len(tasks) != 2 || tasks[1].Parent != "p" || tasks[0].Owner != "Alice" || tasks[0].Session != "s1" {
		t.Fatalf("Tasks = %+v", tasks)
	}
	a.Interrupt("s1")
	if parentCtx.Err() != nil {
		t.Fatal("an interrupt reached a task")
	}
	got, err := a.CancelTask("p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cancelled) != 2 || got.Cancelled[0].ID != "c" || got.Cancelled[1].ID != "p" {
		t.Fatalf("CancelTask = %+v", got)
	}
	if !slices.Equal(order, []string{"c", "p"}) || childCtx.Err() == nil || parentCtx.Err() == nil {
		t.Fatalf("order %v, ctx errs %v %v", order, childCtx.Err(), parentCtx.Err())
	}
	again, err := a.CancelTask("p", nil)
	if err != nil || len(again.AlreadyEnded) != 1 {
		t.Fatalf("second cancel = %+v, %v", again, err)
	}
	if _, err := a.CancelTask("nobody", nil); !errors.Is(err, hook.ErrUnknownTask) {
		t.Fatalf("unknown = %v", err)
	}
}

// Detach turns a wait task into a callback task, except at shutdown, where it
// cancels it; a shut-down agent starts no task.
func TestTaskRegistry_DetachAndShutdown(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, nil)
	ctx := hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s1", TurnID: "t1"})
	detached := false
	_, h, err := a.tasks.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{Mode: hook.TaskWait}, OnDetach: func() bool { detached = true; return true }})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Detach(hook.ErrInterrupted) || !detached || h.Info().Mode != hook.TaskCallback {
		t.Fatalf("detach: %+v", h.Info())
	}
	if !h.Finish(toolkit.NewResult("ok")) || h.Info().State != hook.TaskDone || h.Finish(nil) {
		t.Fatalf("finish: %+v", h.Info())
	}

	cancelled := false
	tctx, h2, err := a.tasks.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{Mode: hook.TaskWait}, OnCancel: func(error) bool { cancelled = true; return true }})
	if err != nil {
		t.Fatal(err)
	}
	a.registry.Shutdown(errClosing)
	if tctx.Err() == nil || cancelled || h2.Info().State != hook.TaskRunning {
		t.Fatalf("shutdown: ctx %v, OnCancel %v, %+v", tctx.Err(), cancelled, h2.Info())
	}
	if h2.Detach(hook.ErrInterrupted) || !cancelled {
		t.Fatal("Detach at shutdown did not cancel")
	}
	if _, _, err := a.tasks.StartTask(ctx, hook.TaskStart{}); !errors.Is(err, hook.ErrShutdown) {
		t.Fatalf("StartTask after shutdown = %v", err)
	}
}

// Close lets a running turn finish, then refuses new runs.
func TestAgent_Close(t *testing.T) {
	f := newAgentFixture(t)
	hold := make(chan struct{})
	started := make(chan struct{})
	f.p.Script("w1", modelstest.Step{Hold: hold, Started: started, Response: &spawnllm.LLMResponse{Content: "late"}})
	a := f.agent(t, nil)
	r := a.Run(context.Background(), "s1", hook.UserText("one"))
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v", err)
	}
	if res := r.Wait(); res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrShutdown) {
		t.Fatalf("stopped turn = %+v", res)
	}
	close(hold)
	if res := a.Run(context.Background(), "s1", hook.UserText("two")).Wait(); !errors.Is(res.Err, ErrClosed) {
		t.Fatalf("run after close = %+v", res)
	}
}

// servedFake records the credentials it issues and revokes.
type servedFake struct {
	mu      sync.Mutex
	scopes  []hook.Scope
	revoked int
}

func (s *servedFake) ServeTurn(_ context.Context, scope hook.Scope) (string, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append(s.scopes, scope)
	return "Use token T1.", func() {
		s.mu.Lock()
		s.revoked++
		s.mu.Unlock()
	}, nil
}

// A CLI model's turn gets its served tools' prompt and credential, revoked
// when the turn ends; a model that is not a CLI gets none.
func TestAgent_ServedToolsForCLIModels(t *testing.T) {
	for _, cli := range []bool{true, false} {
		p := modelstest.New().Script("w1", modelstest.Reply("ok"))
		cat := models.New()
		prov := modelspec.Provider{Name: "api", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}
		if cli {
			prov = modelspec.Provider{Name: "api", Protocol: "claude-cli"}
		}
		if err := cat.AddProvider(prov); err != nil {
			t.Fatal(err)
		}
		if err := cat.Add(modelspec.Model{ModelName: "m1", Model: "w1", Provider: "api"}, models.For(models.Chat, 1)); err != nil {
			t.Fatal(err)
		}
		if err := cat.RegisterClient("m1", p); err != nil {
			t.Fatal(err)
		}
		served := &servedFake{}
		a, err := New(Spec{ID: "alice", Models: cat.Default(), Served: served})
		if err != nil {
			t.Fatal(err)
		}
		a.Run(context.Background(), "s1", hook.UserText("hello")).Wait()
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		served.mu.Lock()
		issued, revoked := len(served.scopes), served.revoked
		served.mu.Unlock()
		system := p.Calls()[0].Messages[0].Content
		if cli != (issued == 1 && revoked == 1 && strings.Contains(system, "Use token T1.")) || (!cli && issued != 0) {
			t.Fatalf("cli %v: issued %d, revoked %d, system %q", cli, issued, revoked, system)
		}
	}
}

// A cancel whose OnCancel declines (the task had ended) asks once, leaves the
// task to its Finish, and CancelAll returns.
func TestTaskRegistry_DeclinedCancel(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, nil)
	ctx := hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s1"})
	asked := 0
	_, h, err := a.tasks.StartTask(ctx, hook.TaskStart{OnCancel: func(error) bool { asked++; return false }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan hook.TaskCancel, 1)
	go func() { done <- a.CancelAll(nil) }()
	select {
	case got := <-done:
		if len(got.Cancelled) != 0 || len(got.AlreadyEnded) != 1 {
			t.Fatalf("CancelAll = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelAll did not return")
	}
	again, err := a.CancelTask(h.ID(), nil)
	if err != nil || asked != 1 || len(again.AlreadyEnded) != 1 {
		t.Fatalf("OnCancel asked %d times; second cancel %+v, %v", asked, again, err)
	}
	if !h.Finish(toolkit.NewResult("result")) || h.Info().State != hook.TaskDone {
		t.Fatalf("Finish after a declined cancel: %+v", h.Info())
	}
}

// Finish racing a cancel's OnCancel waits for it: a declined cancel lets the
// result through, an accepted one wins.
func TestTaskRegistry_FinishWaitsForSettle(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, nil)
	ctx := hook.WithScope(context.Background(), hook.Scope{AgentID: "alice"})
	for _, accept := range []bool{false, true} {
		inCancel, release := make(chan struct{}), make(chan struct{})
		_, h, err := a.tasks.StartTask(ctx, hook.TaskStart{OnCancel: func(error) bool {
			close(inCancel)
			<-release
			return accept
		}})
		if err != nil {
			t.Fatal(err)
		}
		go h.Cancel(nil)
		<-inCancel
		finished := make(chan bool, 1)
		go func() { finished <- h.Finish(toolkit.NewResult("r")) }()
		close(release)
		if got := <-finished; got == accept {
			t.Fatalf("accept %v: Finish = %v", accept, got)
		}
	}
}

// A sub-agent's turn on a task's context starts its tasks as that task's
// children in the parent's registry: cancelling the task reaches them.
func TestAgent_SubAgentTasksAreChildren(t *testing.T) {
	f := newAgentFixture(t)
	parent := f.agent(t, nil)
	var grandchild context.Context
	f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
		reg, _ := hook.TaskRegistrarFrom(ctx)
		gctx, _, err := reg.StartTask(ctx, hook.TaskStart{})
		grandchild = gctx //nolint:fatcontext // kept for the test to inspect
		return "ok", err
	})
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"))
	worker := f.agent(t, func(s *Spec) { s.ID = "alice-worker" })

	ctx := hook.WithTaskRegistrar(hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s1"}), parent.tasks)
	tctx, h, err := parent.tasks.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{Kind: "subagent"}})
	if err != nil {
		t.Fatal(err)
	}
	worker.Run(tctx, "worker-s", hook.UserText("work")).Wait()
	if grandchild == nil || len(parent.Tasks()) != 2 || parent.Tasks()[1].Parent != h.ID() {
		t.Fatalf("parent's tasks = %+v", parent.Tasks())
	}
	if !h.Cancel(nil) || grandchild.Err() == nil {
		t.Fatal("cancelling the task left its sub-agent's task running")
	}
}
