package loop

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/toolkit"
)

// lateTool is a background tool registered under an internal name and
// offered under an external one. It keeps the callback it is handed, for
// the test to report a late result through, as the tool's own goroutine
// would.
type lateTool struct {
	mu sync.Mutex
	cb toolkit.AsyncCallback
}

func (*lateTool) Name() string               { return "mcp_bob_research" }
func (*lateTool) ExternalName() string       { return "bob_research" }
func (*lateTool) Description() string        { return "slow research" }
func (*lateTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (*lateTool) Execute(context.Context, map[string]any) *toolkit.Result {
	return toolkit.NewResult("sync report")
}

func (t *lateTool) ExecuteAsync(_ context.Context, _ map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	t.mu.Lock()
	t.cb = cb
	t.mu.Unlock()
	return toolkit.AsyncResult("started")
}

// report delivers r through the kept callback on a new goroutine, with a
// context of the tool's own (carrying none of the host's values), and
// returns a channel closed when the callback has returned.
func (t *lateTool) report(tb testing.TB, r *toolkit.Result) <-chan struct{} {
	tb.Helper()
	t.mu.Lock()
	cb := t.cb
	t.mu.Unlock()
	if cb == nil {
		tb.Fatal("the tool was not run in the background")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cb(context.Background(), r)
	}()
	return done
}

func addLateTool(t *testing.T, f *agentFixture) *lateTool {
	t.Helper()
	tool := &lateTool{}
	if err := f.reg.AddEntries(toolkit.Entry{Tool: tool}); err != nil {
		t.Fatal(err)
	}
	return tool
}

// lateDelivery is what the async sink saw.
type lateDelivery struct {
	chat    any
	scope   hook.Scope
	callID  string
	ctxErr  error
	call    hook.ToolCall
	info    toolkit.Info
	content string
}

// A late result that arrives after the turn ended and after Close carries
// the values ToolContext set when the call ran, the turn's scope and the
// call, on a context that is not cancelled, and the resolved tool.
func TestAsyncResult_LateAfterTurnAndClose(t *testing.T) {
	f := newAgentFixture(t)
	tool := addLateTool(t, f)
	f.p.Script("w1", calls("bob_research"), modelstest.Reply("done"))
	var chat atomic.Value
	chat.Store("alice's chat")
	got := make(chan lateDelivery, 1)
	a := f.agent(t, func(s *Spec) {
		s.ToolContext = func(ctx context.Context, _ hook.ToolCall) context.Context {
			return context.WithValue(ctx, ctxKey{}, chat.Load())
		}
		s.AsyncResult = func(ctx context.Context, call hook.ToolCall, info toolkit.Info, r *toolkit.Result) {
			d := lateDelivery{chat: ctx.Value(ctxKey{}), ctxErr: ctx.Err(), call: call, info: info, content: r.ForLLM}
			d.scope, _ = hook.ScopeOf(ctx)
			if c, ok := hook.ToolCallFrom(ctx); ok {
				d.callID = c.ID
			}
			got <- d
		}
	})
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	// The host's answer changes after the call; the result must not follow it.
	chat.Store("bob's chat")
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-tool.report(t, toolkit.NewResult("late report"))
	var d lateDelivery
	select {
	case d = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no late result")
	}
	if d.chat != "alice's chat" {
		t.Errorf("chat = %v, want the value at call time", d.chat)
	}
	if d.scope.Session != "s1" || d.scope.AgentID != "Alice" {
		t.Errorf("scope = %+v, want the turn's", d.scope)
	}
	if d.callID != "c1" || d.call.ID != "c1" || d.call.Name != "bob_research" {
		t.Errorf("call = %q on ctx, %+v passed, want c1 bob_research", d.callID, d.call)
	}
	if d.ctxErr != nil {
		t.Errorf("ctx.Err() = %v, want a context that is not cancelled", d.ctxErr)
	}
	if d.info.Name != "mcp_bob_research" || d.info.Published != "bob_research" {
		t.Errorf("info = %q/%q, want the internal and published names", d.info.Name, d.info.Published)
	}
	if d.content != "late report" {
		t.Errorf("result = %q", d.content)
	}
}

// A panic in the async sink is recovered: the tool's goroutine returns and
// the process goes on.
func TestAsyncResult_SinkPanicRecovered(t *testing.T) {
	f := newAgentFixture(t)
	tool := addLateTool(t, f)
	f.p.Script("w1", calls("bob_research"), modelstest.Reply("done"))
	var called atomic.Bool
	a := f.agent(t, func(s *Spec) {
		s.AsyncResult = func(context.Context, hook.ToolCall, toolkit.Info, *toolkit.Result) {
			called.Store(true)
			panic("Bob's sink failed")
		}
	})
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	select {
	case <-tool.report(t, toolkit.NewResult("late report")):
	case <-time.After(5 * time.Second):
		t.Fatal("the tool's goroutine did not return")
	}
	if !called.Load() {
		t.Fatal("the sink was not called")
	}
}

// Without a sink a background tool runs synchronously: its full result is
// what the model sees.
func TestAsyncResult_NilSinkRunsSynchronously(t *testing.T) {
	f := newAgentFixture(t)
	tool := addLateTool(t, f)
	f.p.Script("w1", calls("bob_research"), modelstest.Reply("done"))
	a := f.agent(t, nil)
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	tool.mu.Lock()
	async := tool.cb != nil
	tool.mu.Unlock()
	if async {
		t.Fatal("the tool ran in the background without a sink")
	}
	cs := f.p.Calls()
	if len(cs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(cs))
	}
	msgs := cs[1].Messages
	if last := msgs[len(msgs)-1]; last.Content != "sync report" {
		t.Fatalf("tool result = %q, want the synchronous result", last.Content)
	}
}
