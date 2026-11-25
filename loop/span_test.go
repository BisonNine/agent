/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

// Tests of the turn engine's span, callbacks and events against the hook
// contract. Like the engine files, they use only the standard library, the
// SDK and engine identifiers.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// spanFixture is a turn of one scripted model whose run stores its message
// (AlreadyStored off), with a log of every step the test watches.
type spanFixture struct {
	*engineFixture

	p *modelstest.Provider
}

func newSpanFixture(t *testing.T, steps ...modelstest.Step) *spanFixture {
	t.Helper()
	p := modelstest.New().Script("alice-wire", steps...)
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.options = hook.RunOptions{}
	return &spanFixture{engineFixture: f, p: p}
}

// finished is the turn's TurnFinished.
func (f *spanFixture) finished(t *testing.T) hook.TurnFinished {
	t.Helper()
	evs := f.rec.all()
	fin, ok := evs[len(evs)-1].(hook.TurnFinished)
	if !ok || countOf(f.rec.kinds(), "TurnFinished") != 1 {
		t.Fatalf("events = %v, want one TurnFinished, last", f.rec.kinds())
	}
	return fin
}

// stepLog is an ordered log of the steps of a turn.
type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, s)
}

func (l *stepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

// The span: TurnStarted (with the run's options) before BeforeTurn, which
// is before the message is stored; AfterTurn before the reply is stored;
// TurnFinished after the reply is stored, the session saved and the pending
// marker cleared.
func TestEngine_TurnSpan(t *testing.T) {
	f := newSpanFixture(t, modelstest.Reply("done"))
	f.spec.options = hook.RunOptions{Depth: 1}
	var log stepLog
	f.spec.hooks.add(hook.Plugin{
		Name: "watch",
		BeforeTurn: func(context.Context, *hook.Turn) (*hook.TurnResult, error) {
			log.add(fmt.Sprintf("BeforeTurn after %v", f.rec.kinds()))
			log.add(fmt.Sprintf("stored %d", len(f.cm.stored)))
			return nil, nil //nolint:nilnil // (nil, nil): go on with the step
		},
		AfterTurn: func(context.Context, *hook.Turn, *hook.TurnResult) {
			log.add(fmt.Sprintf("AfterTurn stored %d %v", len(f.cm.stored), f.store.events()))
		},
		OnEvent: func(_ context.Context, ev hook.Event) {
			if _, ok := ev.(hook.TurnFinished); ok {
				log.add(fmt.Sprintf("TurnFinished stored %d %v", len(f.cm.stored), f.store.events()))
			}
		},
	})
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"BeforeTurn after [TurnStarted]",
		"stored 0",
		"AfterTurn stored 1 [pending]",
		"TurnFinished stored 2 [pending save cleared]",
	}
	if got := log.all(); !slices.Equal(got, want) {
		t.Fatalf("steps = %q, want %q", got, want)
	}
	started, ok := f.rec.all()[0].(hook.TurnStarted)
	if !ok || started.Options != f.spec.options || started.Message.Text != "hello" {
		t.Fatalf("TurnStarted = %+v", started)
	}
	if f.cm.stored[0].Role != "user" || f.cm.stored[1].Content != "done" {
		t.Fatalf("stored = %+v", f.cm.stored)
	}
	if fin := f.finished(t); fin.Outcome != hook.OutcomeOK || fin.Reply != "done" {
		t.Fatalf("TurnFinished = %+v", fin.TurnResult)
	}
}

// BeforeTurn may change the message, which is stored as changed; several run
// in order, each seeing the edits of the one before.
func TestEngine_BeforeTurnChangesTheMessage(t *testing.T) {
	f := newSpanFixture(t, modelstest.Reply("done"))
	for _, name := range []string{"alice", "bob"} {
		f.spec.hooks.add(hook.Plugin{Name: name, BeforeTurn: func(_ context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
			turn.Message.Text = name + ": " + turn.Message.Text
			turn.Message.Source, turn.Message.Kind = "Alice", "callback"
			return nil, nil //nolint:nilnil // (nil, nil): go on with the step
		}})
	}
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := f.cm.stored[0]; got.Content != "bob: alice: hello" || got.Source != "Alice" || got.Type != "callback" {
		t.Fatalf("stored message = %+v", got)
	}
}

// A BeforeTurn result answers the turn: no model is called, nothing stored,
// the later BeforeTurn callbacks do not run, AfterTurn does; an error
// refuses the turn with OutcomeError; a panic ends it with a *PanicError.
func TestEngine_BeforeTurnAnswersRefusesOrPanics(t *testing.T) {
	refused := errors.New("Bob may not ask")
	tests := []struct {
		name  string
		fn    hook.BeforeTurnFunc
		check func(t *testing.T, fin hook.TurnFinished)
	}{
		{"answer", func(context.Context, *hook.Turn) (*hook.TurnResult, error) {
			return &hook.TurnResult{Outcome: hook.OutcomeOK, Reply: "Alice answered", Stored: "kept?"}, nil
		}, func(t *testing.T, fin hook.TurnFinished) {
			t.Helper()
			if !fin.Skipped || fin.Reply != "Alice answered" || fin.Outcome != hook.OutcomeOK {
				t.Fatalf("result = %+v", fin.TurnResult)
			}
		}},
		{
			"refusal", func(context.Context, *hook.Turn) (*hook.TurnResult, error) { return nil, refused },
			func(t *testing.T, fin hook.TurnFinished) {
				t.Helper()
				if !errors.Is(fin.Err, refused) || fin.Outcome != hook.OutcomeError {
					t.Fatalf("result = %+v", fin.TurnResult)
				}
			},
		},
		{
			"panic", func(context.Context, *hook.Turn) (*hook.TurnResult, error) { panic("Bob's bug") },
			func(t *testing.T, fin hook.TurnFinished) {
				t.Helper()
				if perr, ok := errors.AsType[*hook.PanicError](fin.Err); !ok || perr.Callback != "BeforeTurn" || fin.Outcome != hook.OutcomeError {
					t.Fatalf("result = %+v", fin.TurnResult)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSpanFixture(t, modelstest.Reply("done"))
			var later, after bool
			f.spec.hooks.add(hook.Plugin{Name: "first", BeforeTurn: tt.fn})
			f.spec.hooks.add(hook.Plugin{
				Name:       "later",
				BeforeTurn: func(context.Context, *hook.Turn) (*hook.TurnResult, error) { later = true; return nil, nil }, //nolint:nilnil // (nil, nil): go on with the step
				AfterTurn:  func(context.Context, *hook.Turn, *hook.TurnResult) { after = true },
			})
			_, _, err := f.run(t)
			if (err == nil) != (tt.name == "answer") {
				t.Fatalf("err = %v", err)
			}
			if later || !after {
				t.Fatalf("later BeforeTurn ran: %v, AfterTurn ran: %v", later, after)
			}
			if len(f.cm.stored) != 0 || len(f.p.Calls()) != 0 || len(f.store.events()) != 0 {
				t.Fatalf("stored %v, model calls %d, store %v", f.cm.stored, len(f.p.Calls()), f.store.events())
			}
			tt.check(t, f.finished(t))
		})
	}
}

// AfterTurn decides what is stored and replied; a panic in one makes the
// result an error that stores nothing, and the later ones still run.
func TestEngine_AfterTurnDecidesAndPanics(t *testing.T) {
	f := newSpanFixture(t, modelstest.Reply("done"))
	f.spec.hooks.add(hook.Plugin{Name: "render", AfterTurn: func(_ context.Context, _ *hook.Turn, r *hook.TurnResult) {
		r.Reply, r.Stored = "Done, Alice.", "done (noted)"
	}})
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if f.cm.stored[1].Content != "done (noted)" || f.finished(t).Reply != "Done, Alice." {
		t.Fatalf("stored %+v, finished %+v", f.cm.stored, f.finished(t).TurnResult)
	}

	f = newSpanFixture(t, modelstest.Reply("done"))
	var laterSaw hook.Outcome
	f.spec.hooks.add(hook.Plugin{Name: "bobs", AfterTurn: func(context.Context, *hook.Turn, *hook.TurnResult) { panic("Bob's bug") }})
	f.spec.hooks.add(hook.Plugin{Name: "later", AfterTurn: func(_ context.Context, _ *hook.Turn, r *hook.TurnResult) { laterSaw = r.Outcome }})
	_, _, err := f.run(t)
	if perr, ok := errors.AsType[*hook.PanicError](err); !ok || perr.Callback != "AfterTurn" || perr.Plugin != "bobs" {
		t.Fatalf("err = %v", err)
	}
	if laterSaw != hook.OutcomeError || len(f.cm.stored) != 1 || f.finished(t).Outcome != hook.OutcomeError {
		t.Fatalf("later saw %q, stored %+v", laterSaw, f.cm.stored)
	}
}

// A store refusal is a *hook.SessionStoreError: of the message, it ends the
// turn before the model; of the reply, it sets Err and OutcomeError and
// leaves the reply; a failed save is SaveErr only.
func TestEngine_SessionStoreErrors(t *testing.T) {
	refusal := errors.New("disk full")

	f := newSpanFixture(t, modelstest.Reply("done"))
	f.cm.failAdd = refusal
	_, _, err := f.run(t)
	if se, ok := errors.AsType[*hook.SessionStoreError](err); !ok || se.What != "user message" || len(f.p.Calls()) != 0 {
		t.Fatalf("err = %v, model calls %d", err, len(f.p.Calls()))
	}

	f = newSpanFixture(t, modelstest.Reply("done"))
	f.spec.hooks.add(hook.Plugin{Name: "refuse", AfterTurn: func(context.Context, *hook.Turn, *hook.TurnResult) { f.cm.failAdd = refusal }})
	_, _, err = f.run(t)
	fin := f.finished(t)
	if se, ok := errors.AsType[*hook.SessionStoreError](err); !ok || se.What != "assistant reply" ||
		fin.Outcome != hook.OutcomeError || fin.Reply != "done" || !errors.Is(fin.SaveErr, hook.ErrSessionStore) {
		t.Fatalf("err = %v, finished = %+v", err, fin.TurnResult)
	}

	f = newSpanFixture(t, modelstest.Reply("done"))
	f.store.saveErr = refusal
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	fin = f.finished(t)
	if se, ok := errors.AsType[*hook.SessionStoreError](fin.SaveErr); !ok || se.What != "session save" || fin.Outcome != hook.OutcomeOK {
		t.Fatalf("finished = %+v", fin.TurnResult)
	}
}

// The cause of a stopped turn decides its outcome (hook.CauseOutcome), with
// a host cause wrapping a hook cause; only a shutdown keeps the pending
// marker. OnInterrupt observes a cancel through the handle.
func TestEngine_CauseOutcome(t *testing.T) {
	hostCause := func(kind error) error { return fmt.Errorf("Alice's host: %w", kind) }
	tests := []struct {
		cause   error
		outcome hook.Outcome
		cleared bool
	}{
		{hostCause(hook.ErrInterrupted), hook.OutcomeCancelled, true},
		{hostCause(hook.ErrSuperseded), hook.OutcomeCancelled, true},
		{hostCause(hook.ErrShutdown), hook.OutcomeCancelled, false},
		{hostCause(hook.ErrDeadline), hook.OutcomeError, true},
	}
	for _, tt := range tests {
		t.Run(tt.cause.Error(), func(t *testing.T) {
			hold, started := make(chan struct{}), make(chan struct{})
			f := newSpanFixture(t, modelstest.Step{Hold: hold, Started: started})
			var interrupted []hook.Interrupted
			f.spec.hooks.add(hook.Plugin{Name: "tap", OnInterrupt: func(_ context.Context, ev hook.Interrupted) { interrupted = append(interrupted, ev) }})
			turn := newEngineTurn(f.spec, nil)
			done := make(chan struct{})
			var runErr error
			go func() { defer close(done); _, runErr = turn.run(context.Background()) }()
			<-started
			turn.handle.Cancel(tt.cause)
			<-done
			if runErr == nil {
				t.Fatal("the stopped turn reported no error")
			}
			fin := f.finished(t)
			if fin.Outcome != tt.outcome || !errors.Is(fin.Cause, tt.cause) {
				t.Fatalf("finished = %+v", fin.TurnResult)
			}
			if got := slices.Contains(f.store.events(), "cleared"); got != tt.cleared {
				t.Fatalf("store = %v, want cleared %v", f.store.events(), tt.cleared)
			}
			if len(interrupted) != 1 || !errors.Is(interrupted[0].Cause, tt.cause) || !interrupted[0].Running || interrupted[0].Scope != f.spec.scope {
				t.Fatalf("OnInterrupt = %+v", interrupted)
			}
		})
	}
}

// The model step's events: ModelCallStarted and ModelCall per provider call
// (the provider's own response, before AfterModel), a Fallback as the chain
// moves on, then ModelAnswer (as AfterModel left it) and an Image per
// generated image.
func TestEngine_ModelStepEvents(t *testing.T) {
	img := spawnllm.GeneratedImage{URL: "https://example.com/cat.png"}
	first := modelstest.New().Script("alice-wire", modelstest.Fail(errors.New("503 overloaded")))
	second := modelstest.New().Script("bob-wire", modelstest.Step{Response: &spawnllm.LLMResponse{Content: "here", Images: []spawnllm.GeneratedImage{img}, FinishReason: "stop"}})
	clk := newFake()
	_, chain := testChain(t, clk,
		engineModel{name: "Alice Model", wire: "alice-wire", client: first},
		engineModel{name: "Bob Model", wire: "bob-wire", client: second})
	f := newEngineFixture(t, chain, clk)
	f.spec.hooks.add(hook.Plugin{Name: "render", AfterModel: func(_ context.Context, _ *hook.ModelInfo, r *hook.ModelResponse) {
		r.Response.Content = "changed"
		r.ReplyLines = append(r.ReplyLines, "Image: cat.png")
	}})
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range f.rec.all() {
		switch e := ev.(type) {
		case hook.ModelCallStarted:
			got = append(got, "started "+e.Model.Name)
		case hook.ModelCall:
			got = append(got, fmt.Sprintf("call %s err=%v", e.Model.Name, e.Err != nil))
			if e.Response != nil && e.Response.Content != "here" {
				t.Errorf("ModelCall response = %q, want the provider's", e.Response.Content)
			}
		case hook.Fallback:
			got = append(got, "fallback to "+e.Next.Name)
		case hook.ModelAnswer:
			got = append(got, fmt.Sprintf("answer %s %q %v", e.Model.Name, e.Response.Content, e.ReplyLines))
		case hook.Image:
			got = append(got, "image")
		}
	}
	want := []string{
		"started Alice Model", "call Alice Model err=true", "fallback to Bob Model",
		"started Bob Model", "call Bob Model err=false",
		`answer Bob Model "changed" [Image: cat.png]`, "image",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A turn with no model to call fails without a provider call: no
// ModelCallStarted or ModelCall.
func TestEngine_NoModelNoModelCall(t *testing.T) {
	f := newEngineFixture(t, nil, newFake())
	if _, _, err := f.run(t); !errors.Is(err, errNoModelConfigured) {
		t.Fatalf("err = %v", err)
	}
	for _, ev := range f.rec.all() {
		switch ev.(type) {
		case hook.ModelCallStarted, hook.ModelCall:
			t.Fatalf("event %T for a turn with no model", ev)
		}
	}
}

// BeforeModel edits the working request (replacing a message), which the
// model is sent and which stays for later iterations; several run in order;
// an answer stands in for the models (no ModelCall, a supplied ModelAnswer);
// an error fails the dispatch through OnModelError.
func TestEngine_BeforeModel(t *testing.T) {
	f := newSpanFixture(t, callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
	f.spec.options = hook.RunOptions{AlreadyStored: true}
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	var order []string
	f.spec.hooks.add(hook.Plugin{Name: "alice", BeforeModel: func(_ context.Context, call *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
		order = append(order, "alice")
		if call.Iteration == 1 {
			m := call.Request.Messages[0]
			m.Content = "hello, Alice"
			call.Request.Messages[0] = m
		}
		return nil, nil //nolint:nilnil // (nil, nil): go on with the step
	}})
	f.spec.hooks.add(hook.Plugin{Name: "bob", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
		order = append(order, "bob")
		return nil, nil //nolint:nilnil // (nil, nil): go on with the step
	}})
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	calls := f.p.Calls()
	if calls[0].Messages[0].Content != "hello, Alice" || calls[1].Messages[0].Content != "hello, Alice" {
		t.Fatalf("sent %q then %q", calls[0].Messages[0].Content, calls[1].Messages[0].Content)
	}
	if !slices.Equal(order, []string{"alice", "bob", "alice", "bob"}) {
		t.Fatalf("order = %v", order)
	}

	f = newSpanFixture(t)
	var bobRan bool
	f.spec.hooks.add(hook.Plugin{Name: "alice", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
		return &spawnllm.LLMResponse{Content: "cached", FinishReason: "stop"}, nil
	}})
	f.spec.hooks.add(hook.Plugin{Name: "bob", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
		bobRan = true
		return nil, nil //nolint:nilnil // (nil, nil): go on with the step
	}})
	_, res, err := f.run(t)
	if err != nil || res.content != "cached" || bobRan || len(f.p.Calls()) != 0 {
		t.Fatalf("res = %+v, err = %v, bob ran %v", res, err, bobRan)
	}
	var supplied bool
	for _, ev := range f.rec.all() {
		switch e := ev.(type) {
		case hook.ModelCall:
			t.Fatal("a ModelCall for a supplied answer")
		case hook.ModelAnswer:
			supplied = e.Supplied
		}
	}
	if !supplied {
		t.Fatal("the ModelAnswer is not marked supplied")
	}
}

// OnModelError sees a dispatch that failed for good: an answer lets the
// turn go on, an error replaces the turn's; several run in order.
func TestEngine_OnModelError(t *testing.T) {
	refusal := errors.New("Bob's model refused")
	f := newSpanFixture(t, modelstest.Fail(refusal))
	f.spec.retries = 0
	var seen []error
	f.spec.hooks.add(hook.Plugin{Name: "alice", OnModelError: func(_ context.Context, _ *hook.ModelInfo, err error) (*spawnllm.LLMResponse, error) {
		seen = append(seen, err)
		return nil, nil //nolint:nilnil // (nil, nil): go on with the step
	}})
	f.spec.hooks.add(hook.Plugin{Name: "bob", OnModelError: func(context.Context, *hook.ModelInfo, error) (*spawnllm.LLMResponse, error) {
		return &spawnllm.LLMResponse{Content: "Sorry, Alice.", FinishReason: "stop"}, nil
	}})
	_, res, err := f.run(t)
	if err != nil || res.content != "Sorry, Alice." || len(seen) != 1 || !errors.Is(seen[0], refusal) {
		t.Fatalf("res = %+v, err = %v, seen = %v", res, err, seen)
	}

	f = newSpanFixture(t, modelstest.Fail(refusal))
	f.spec.retries = 0
	replaced := errors.New("Alice's words")
	f.spec.hooks.add(hook.Plugin{Name: "alice", OnModelError: func(context.Context, *hook.ModelInfo, error) (*spawnllm.LLMResponse, error) {
		return nil, replaced
	}})
	if _, _, err := f.run(t); !errors.Is(err, replaced) {
		t.Fatalf("err = %v, want the replacement", err)
	}
}

// A batch's events: ToolBatchStarted with the narration; a ToolCallStarted
// per call in call order; ToolCallFinished in completion order;
// ToolBatchFinished; ToolResultStored in call order, each after its result
// is stored.
func TestEngine_ToolBatchEvents(t *testing.T) {
	f := newSpanFixture(t,
		modelstest.Step{Response: &spawnllm.LLMResponse{
			Content: "Checking, Alice.", FinishReason: "tool_calls",
			ToolCalls: []spawnllm.ToolCall{toolCall("slow", "slow", nil), toolCall("fast", "fast", nil)},
		}},
		modelstest.Reply("done"))
	f.spec.options = hook.RunOptions{AlreadyStored: true}
	fastDone := make(chan struct{})
	f.tool("slow", func(context.Context, map[string]any) *toolkit.Result {
		<-fastDone
		return toolkit.SilentResult("slow")
	})
	f.tool("fast", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("fast") })
	var storedAt []int
	f.spec.hooks.add(hook.Plugin{Name: "watch", OnEvent: func(_ context.Context, ev hook.Event) {
		switch e := ev.(type) {
		case hook.ToolCallFinished:
			if e.Call.ID == "fast" {
				close(fastDone)
			}
		case hook.ToolResultStored:
			storedAt = append(storedAt, len(f.cm.stored))
		}
	}})
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range f.rec.all() {
		switch e := ev.(type) {
		case hook.ToolBatchStarted:
			got = append(got, fmt.Sprintf("batch %q %d", e.Content, len(e.Calls)))
		case hook.ToolCallStarted:
			got = append(got, "started "+e.Call.ID)
		case hook.ToolCallFinished:
			got = append(got, "finished "+e.Call.ID)
		case hook.ToolBatchFinished:
			got = append(got, fmt.Sprintf("batch finished %d", e.Calls))
		case hook.ToolResultStored:
			got = append(got, "stored "+e.Call.ID)
		}
	}
	want := []string{
		`batch "Checking, Alice." 2`, "started slow", "started fast", "finished fast", "finished slow",
		"batch finished 2", "stored slow", "stored fast",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	// stored[0] is the tool-call message; each result is stored before its
	// event.
	if !slices.Equal(storedAt, []int{2, 3}) {
		t.Fatalf("stored when reported = %v", storedAt)
	}
}

// BeforeTool may change the arguments (the tool gets them, ToolCallFinished
// carries them, the stored call keeps the model's), answer the call or
// refuse it (an error result, OnToolError on it); AfterTool changes the
// result that is stored; several of a kind run in order.
func TestEngine_ToolCallbacks(t *testing.T) {
	f := newSpanFixture(t,
		callStep(toolCall("c1", "echo", map[string]any{"to": "Alice"}), toolCall("c2", "echo", nil), toolCall("c3", "echo", nil)),
		modelstest.Reply("done"))
	f.spec.options = hook.RunOptions{AlreadyStored: true}
	var toolGot any
	f.tool("echo", func(_ context.Context, args map[string]any) *toolkit.Result {
		toolGot = args["to"]
		return toolkit.SilentResult("ok")
	})
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	f.spec.hooks.add(hook.Plugin{
		Name: "alice",
		BeforeTool: func(_ context.Context, call *hook.ToolCall) (*toolkit.Result, error) {
			switch call.ID {
			case "c1":
				call.Arguments["to"] = "Bob"
			case "c2":
				return toolkit.SilentResult("answered"), nil
			case "c3":
				return nil, errors.New("Bob may not")
			}
			return nil, nil //nolint:nilnil // (nil, nil): go on with the step
		},
		OnToolError: func(_ context.Context, call *hook.ToolCall, r *toolkit.Result) *toolkit.Result {
			note("error " + call.ID + " " + r.ForLLM)
			return nil
		},
		AfterTool: func(_ context.Context, call *hook.ToolCall, r *toolkit.Result) {
			if call.ID == "c1" {
				r.ForLLM += " (checked)"
			}
		},
	})
	f.spec.hooks.add(hook.Plugin{Name: "bob", AfterTool: func(_ context.Context, call *hook.ToolCall, r *toolkit.Result) {
		if call.ID == "c1" {
			r.ForLLM += " (twice)"
		}
	}})
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if toolGot != "Bob" {
		t.Fatalf("the tool got %v", toolGot)
	}
	if args := f.cm.stored[0].ToolCalls[0].Function.Arguments; args != `{"to":"Alice"}` {
		t.Fatalf("stored call arguments = %s, want the model's", args)
	}
	results := map[string]string{}
	for _, m := range f.cm.stored[1:4] {
		results[m.ToolCallID] = m.Content
	}
	if results["c1"] != "ok (checked) (twice)" || results["c2"] != "answered" || results["c3"] != "Bob may not" {
		t.Fatalf("stored results = %v", results)
	}
	if !slices.Equal(order, []string{"error c3 Bob may not"}) {
		t.Fatalf("OnToolError = %v", order)
	}
	for _, ev := range f.rec.all() {
		if fin, ok := ev.(hook.ToolCallFinished); ok && fin.Call.ID == "c1" && fin.Call.Arguments["to"] != "Bob" {
			t.Fatalf("ToolCallFinished arguments = %v, want BeforeTool's", fin.Call.Arguments)
		}
	}
}

// DescribeImages runs once per batch, for a model with vision off, after
// every ToolResultStored of the batch; the callbacks run in order until one
// answers, and its text is stored as one user message. No answer stores
// nothing; a model that sees images is never asked.
func TestEngine_DescribeImages(t *testing.T) {
	for _, tt := range []struct {
		name   string
		vision string
		answer bool
		asked  int
	}{
		{"described", modelspec.VisionOff, true, 1},
		{"no answer", modelspec.VisionOff, false, 1},
		{"vision model", modelspec.VisionUserMessage, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := modelstest.New().Script("alice-wire",
				callStep(toolCall("c1", "shot", nil), toolCall("c2", "shot", nil)), modelstest.Reply("done"))
			clk := newFake()
			cat := models.New(models.WithClock(clk))
			if err := cat.AddProvider(modelspec.Provider{Name: "Alice's API", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
				t.Fatal(err)
			}
			if err := cat.Add(modelspec.Model{ModelName: "Alice Model", Model: "alice-wire", Provider: "Alice's API", Vision: tt.vision}, models.For(models.Chat, 1)); err != nil {
				t.Fatal(err)
			}
			if err := cat.RegisterClient("Alice Model", p); err != nil {
				t.Fatal(err)
			}
			f := newEngineFixture(t, cat.Begin(models.Chat), clk)
			f.tool("shot", func(context.Context, map[string]any) *toolkit.Result {
				return &toolkit.Result{ForLLM: "a screenshot", Images: []string{"data:image/png;base64,AAA"}}
			})
			var asked []hook.UnseenImages
			var storedSeen int
			f.spec.hooks.add(hook.Plugin{Name: "watch", OnEvent: func(_ context.Context, ev hook.Event) {
				if _, ok := ev.(hook.ToolResultStored); ok {
					storedSeen++
				}
			}})
			f.spec.hooks.add(hook.Plugin{Name: "alice", DescribeImages: func(_ context.Context, images hook.UnseenImages) (string, bool) {
				if storedSeen != 2 {
					t.Errorf("described after %d results were stored, want both", storedSeen)
				}
				asked = append(asked, images)
				return "", false
			}})
			f.spec.hooks.add(hook.Plugin{Name: "bob", DescribeImages: func(context.Context, hook.UnseenImages) (string, bool) {
				return "two screenshots of Bob's desk", tt.answer
			}})
			if _, _, err := f.run(t); err != nil {
				t.Fatal(err)
			}
			if len(asked) != tt.asked {
				t.Fatalf("asked %d times", len(asked))
			}
			if tt.asked == 1 && (len(asked[0].Images) != 2 || !slices.Equal(asked[0].Focus, []string{"a screenshot", "a screenshot"}) || asked[0].Iteration != 1) {
				t.Fatalf("asked with %+v", asked[0])
			}
			described := slices.ContainsFunc(f.cm.stored, func(m spawnllm.Message) bool { return m.Content == "two screenshots of Bob's desk" })
			if described != (tt.answer && tt.asked == 1) {
				t.Fatalf("stored = %+v", f.cm.stored)
			}
		})
	}
}

// A panic in DescribeImages ends the turn like any model step.
func TestEngine_DescribeImagesPanicEndsTheTurn(t *testing.T) {
	f := newSpanFixture(t, callStep(toolCall("c1", "shot", nil)), modelstest.Reply("done"))
	f.spec.options = hook.RunOptions{AlreadyStored: true}
	f.tool("shot", func(context.Context, map[string]any) *toolkit.Result {
		return &toolkit.Result{ForLLM: "a screenshot", Images: []string{"data:image/png;base64,AAA"}}
	})
	f.spec.hooks.add(hook.Plugin{Name: "bobs", DescribeImages: func(context.Context, hook.UnseenImages) (string, bool) { panic("Bob's bug") }})
	_, _, err := f.run(t)
	if perr, ok := errors.AsType[*hook.PanicError](err); !ok || perr.Callback != "DescribeImages" {
		t.Fatalf("err = %v", err)
	}
	if fin := f.finished(t); fin.Outcome != hook.OutcomeError {
		t.Fatalf("finished = %+v", fin.TurnResult)
	}
}

// The turn's limits come from its candidates: max_tokens and the thinking
// level per model, as the catalogue resolves them.
func TestEngine_LimitsFromCandidates(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
	clk := newFake()
	cat := models.New(models.WithClock(clk), models.WithLimitDefaults(modelspec.LimitDefaults{MaxTokens: 1234}))
	if err := cat.AddProvider(modelspec.Provider{Name: "Alice's API", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Add(modelspec.Model{ModelName: "Alice Model", Model: "alice-wire", Provider: "Alice's API"}, models.For(models.Chat, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.RegisterClient("Alice Model", p); err != nil {
		t.Fatal(err)
	}
	f := newEngineFixture(t, cat.Begin(models.Chat), clk)
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := p.Calls()[0].Options["max_tokens"]; got != 1234 {
		t.Fatalf("max_tokens = %v, want the catalogue's default", got)
	}
}

// A run that asks for a reset has the session reset before its message is
// stored.
func TestEngine_ResetSessionBeforeStoring(t *testing.T) {
	f := newSpanFixture(t, modelstest.Reply("done"))
	f.spec.options = hook.RunOptions{ResetSession: true}
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if ev := f.cm.events(); len(ev) < 2 || ev[0] != "reset" || ev[1] != "add:user" {
		t.Fatalf("context = %v, want the reset before the message", ev)
	}
}

// A compaction reported on the turn's context after its TurnFinished still
// reaches OnCompaction but emits no event: TurnFinished stays the last.
func TestEngine_NoEventAfterTurnFinished(t *testing.T) {
	f := newSpanFixture(t, modelstest.Reply("done"))
	var notices int
	f.spec.hooks.add(hook.Plugin{Name: "notice", OnCompaction: func(context.Context, hook.Compaction) { notices++ }})
	ctx := beginTurnContext(context.Background(), f.spec.scope, f.spec.hooks, f.clk)
	if _, err := newEngineTurn(f.spec, nil).run(ctx); err != nil {
		t.Fatal(err)
	}
	CompactionStart(ctx, ctxengine.CompactionStart{SessionKey: "s", Blocking: true})
	kinds := f.rec.kinds()
	if len(kinds) == 0 || kinds[len(kinds)-1] != "TurnFinished" || notices != 1 {
		t.Fatalf("events = %v, notices = %d", kinds, notices)
	}
}
