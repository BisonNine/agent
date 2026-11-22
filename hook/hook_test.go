package hook_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm/protocoltypes"
	"go.bison9.com/agent/toolkit"
)

// The documented shapes: every event type is an Event, the callback types
// take plain functions of their signature, and a Plugin holds one of each.
var (
	_ hook.Event = hook.TurnStarted{}
	_ hook.Event = hook.ModelCallStarted{}
	_ hook.Event = hook.ModelCall{}
	_ hook.Event = hook.ModelAnswer{}
	_ hook.Event = hook.ToolBatchStarted{}
	_ hook.Event = hook.ToolBatchFinished{}
	_ hook.Event = hook.ToolResultStored{}
	_ hook.Event = hook.TextDelta{}
	_ hook.Event = hook.ToolCallStarted{}
	_ hook.Event = hook.ToolCallFinished{}
	_ hook.Event = hook.Fallback{}
	_ hook.Event = hook.Compaction{}
	_ hook.Event = hook.Image{}
	_ hook.Event = hook.TaskStarted{}
	_ hook.Event = hook.TaskFinished{}
	_ hook.Event = hook.TurnFinished{}

	_ = hook.Plugin{
		BeforeTurn: func(context.Context, *hook.Turn) (*hook.TurnResult, error) { return nil, nil }, //nolint:nilnil // (nil, nil): run the step
		AfterTurn:  func(context.Context, *hook.Turn, *hook.TurnResult) {},
		BeforeModel: func(context.Context, *hook.ModelInfo) (*protocoltypes.LLMResponse, error) {
			return nil, nil //nolint:nilnil // (nil, nil): run the step
		},
		AfterModel: func(context.Context, *hook.ModelInfo, *hook.ModelResponse) {},
		OnModelError: func(_ context.Context, _ *hook.ModelInfo, err error) (*protocoltypes.LLMResponse, error) {
			return nil, err
		},
		OnFallback:   func(context.Context, hook.Fallback) {},
		BeforeTool:   func(context.Context, *hook.ToolCall) (*toolkit.Result, error) { return nil, nil }, //nolint:nilnil // (nil, nil): run the step
		AfterTool:    func(context.Context, *hook.ToolCall, *toolkit.Result) {},
		OnToolError:  func(context.Context, *hook.ToolCall, *toolkit.Result) *toolkit.Result { return nil },
		OnCompaction: func(context.Context, hook.Compaction) {},
		OnTextDelta:  func(context.Context, hook.TextDelta) {},
		OnInterrupt:  func(context.Context, hook.Interrupted) {},
		OnEvent:      func(context.Context, hook.Event) {},

		DescribeImages: func(context.Context, hook.UnseenImages) (string, bool) { return "", false },
	}

	_ hook.Run            = (*fakeRun)(nil)
	_ hook.TaskController = fakeTasks{}
	_ hook.Memory         = fakeMemory{}
	_ hook.MemorySession  = fakeMemory{}
)

// The header promoted by every event is the one it was built with.
func TestEventHeader(t *testing.T) {
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	h := hook.EventHeader{Scope: alice, At: at}
	events := []hook.Event{
		hook.TurnStarted{EventHeader: h},
		hook.ToolCallFinished{EventHeader: h},
		hook.TurnFinished{EventHeader: h, Outcome: hook.OutcomeOK},
	}
	for _, ev := range events {
		if got := ev.Header(); got != h {
			t.Errorf("%T header = %+v", ev, got)
		}
	}
}

// The outcomes keep the strings hosts already carry on a turn's reply.
func TestOutcomeValues(t *testing.T) {
	for o, want := range map[hook.Outcome]string{
		hook.OutcomeOK:        "ok",
		hook.OutcomeError:     "error",
		hook.OutcomeCancelled: "cancelled",
		hook.OutcomeEmpty:     "empty",
	} {
		if string(o) != want {
			t.Errorf("outcome %q, want %q", o, want)
		}
	}
}

func TestCancelCauses(t *testing.T) {
	causes := []error{hook.ErrInterrupted, hook.ErrShutdown, hook.ErrDeadline, hook.ErrSuperseded}
	for i, a := range causes {
		for j, b := range causes {
			if (i == j) != errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = %v", a, b, i != j)
			}
		}
	}
	// A host's own cause is recognised when it wraps one of them.
	host := fmt.Errorf("service stopping: %w", hook.ErrShutdown)
	if !errors.Is(host, hook.ErrShutdown) {
		t.Error("a wrapped cause is not recognised")
	}
}

// hostStopError is a host's own stop cause, wrapping a hook cause.
type hostStopError struct{ cause error }

func (c hostStopError) Error() string { return "stopped by the host: " + c.cause.Error() }
func (c hostStopError) Unwrap() error { return c.cause }

// A host's cause reaches the engine unchanged through context.Cause, is
// recognised with errors.Is, and decides the outcome.
func TestCauseThroughHostWrapper(t *testing.T) {
	cases := []struct {
		cause error
		want  hook.Outcome
	}{
		{nil, ""},
		{hostStopError{hook.ErrInterrupted}, hook.OutcomeCancelled},
		{fmt.Errorf("service stopping: %w", hook.ErrShutdown), hook.OutcomeCancelled},
		{hostStopError{hook.ErrSuperseded}, hook.OutcomeCancelled},
		{hostStopError{hook.ErrDeadline}, hook.OutcomeError},
		{fmt.Errorf("%w: %w", hook.ErrDeadline, context.DeadlineExceeded), hook.OutcomeError},
		{context.DeadlineExceeded, hook.OutcomeError},
		{errors.New("a host cause wrapping nothing"), hook.OutcomeCancelled},
	}
	for _, tc := range cases {
		if got := hook.CauseOutcome(tc.cause); got != tc.want {
			t.Errorf("CauseOutcome(%v) = %q, want %q", tc.cause, got, tc.want)
		}
		if tc.cause == nil {
			continue
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(tc.cause)
		got := context.Cause(ctx)
		if got != tc.cause { //nolint:errorlint // the very value is returned
			t.Errorf("context.Cause = %v, want the host's own error", got)
		}
		if w, ok := errors.AsType[hostStopError](tc.cause); ok && !errors.Is(got, w.cause) {
			t.Errorf("errors.Is(%v, %v) = false", got, w.cause)
		}
	}
}

func TestSessionStoreError(t *testing.T) {
	disk := errors.New("disk full")
	err := fmt.Errorf("turn: %w", &hook.SessionStoreError{What: "tool result", Err: disk})
	if !errors.Is(err, hook.ErrSessionStore) || !errors.Is(err, disk) {
		t.Fatalf("errors.Is fails on %v", err)
	}
	var se *hook.SessionStoreError
	if !errors.As(err, &se) || se.What != "tool result" {
		t.Fatalf("errors.As = %+v", se)
	}
	if want := "turn: session store rejected the tool result: disk full"; err.Error() != want {
		t.Fatalf("text = %q, want %q", err.Error(), want)
	}
}

func TestPanicError(t *testing.T) {
	a := &hook.PanicError{Callback: "AfterModel", Plugin: "images", Value: "boom"}
	if got, want := a.Error(), "AfterModel callback of images panicked: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	b := &hook.PanicError{Callback: "BeforeTool", Value: 42}
	if got, want := b.Error(), "BeforeTool callback panicked: 42"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestInterruptedResult(t *testing.T) {
	a, b := hook.InterruptedResult(), hook.InterruptedResult()
	if a == b {
		t.Fatal("InterruptedResult returned a shared value")
	}
	if !a.IsError || a.ForLLM != hook.InterruptedText {
		t.Fatalf("InterruptedResult = %+v", a)
	}
}

func TestTaskStateTerminal(t *testing.T) {
	for s, want := range map[hook.TaskState]bool{
		hook.TaskRunning:   false,
		hook.TaskDone:      true,
		hook.TaskError:     true,
		hook.TaskCancelled: true,
	} {
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
}

func TestLimitsWithDefaults(t *testing.T) {
	got := hook.Limits{}.WithDefaults()
	want := hook.Limits{
		MaxIterations: 20,
		ToolTimeout:   5 * time.Minute,
		TurnBudget:    15 * time.Minute,
		CancelGrace:   5 * time.Second,
		Retries:       2,
		RetryBackoff:  5 * time.Second,
	}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	set := hook.Limits{
		MaxIterations: 3, ToolTimeout: time.Second, TurnBudget: time.Minute,
		CancelGrace: time.Millisecond, Retries: 1, RetryBackoff: time.Second,
	}
	if got := set.WithDefaults(); got != set {
		t.Fatalf("set limits changed: %+v", got)
	}
	if got := (hook.Limits{Retries: -1}).WithDefaults().Retries; got != -1 {
		t.Fatalf("negative Retries = %d, want -1 (none)", got)
	}
	if got := (hook.Limits{ToolTimeout: -time.Second}).WithDefaults().ToolTimeout; got != hook.DefaultToolTimeout {
		t.Fatalf("negative ToolTimeout = %v, want the default", got)
	}
	for _, l := range []hook.Limits{{}, {Retries: -1}, set, {CancelGrace: -1}} {
		once := l.WithDefaults()
		if twice := once.WithDefaults(); twice != once {
			t.Errorf("WithDefaults not idempotent for %+v: %+v then %+v", l, once, twice)
		}
	}
}

func TestRunOptions(t *testing.T) {
	got := hook.ApplyRunOptions(hook.AlreadyStored(), nil, hook.ResetSession(),
		hook.TurnBudget(time.Hour), hook.AtDepth(2))
	want := hook.RunOptions{AlreadyStored: true, ResetSession: true, TurnBudget: time.Hour, Depth: 2}
	if got != want {
		t.Fatalf("ApplyRunOptions = %+v, want %+v", got, want)
	}
	if got := hook.ApplyRunOptions(); got != (hook.RunOptions{}) {
		t.Fatalf("no options = %+v", got)
	}
}

func TestUserText(t *testing.T) {
	if got := hook.UserText("hello"); got.Text != "hello" || got.Media != nil || got.Source != "" || got.Kind != "" {
		t.Fatalf("UserText = %+v", got)
	}
}

// fakeRun is a finished run that replays its events, enough to show a
// consumer loop.
type fakeRun struct {
	events []hook.Event
	done   chan struct{}
}

func newFakeRun(events ...hook.Event) *fakeRun {
	r := &fakeRun{events: events, done: make(chan struct{})}
	close(r.done)
	return r
}

func (r *fakeRun) Events() iter.Seq2[hook.Event, error] {
	return func(yield func(hook.Event, error) bool) {
		for _, ev := range r.events {
			var err error
			if fin, ok := ev.(hook.TurnFinished); ok {
				err = fin.Err
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (r *fakeRun) Interrupt()            {}
func (r *fakeRun) Cancel(error)          {}
func (r *fakeRun) Done() <-chan struct{} { return r.done }

func (r *fakeRun) Wait() hook.TurnResult {
	for _, ev := range r.events {
		if fin, ok := ev.(hook.TurnFinished); ok {
			return fin.TurnResult
		}
	}
	return hook.TurnResult{}
}

type fakeTasks struct{}

func (fakeTasks) Tasks() []hook.TaskInfo         { return nil }
func (fakeTasks) CancelTask(string, error) error { return nil }

type fakeMemory struct{}

func (fakeMemory) Open(context.Context, hook.Scope) (hook.MemorySession, error) {
	return fakeMemory{}, nil
}
func (fakeMemory) Recall(context.Context, string) []hook.Recalled { return nil }
func (fakeMemory) Observe(context.Context, int64, string, string) {}
func (fakeMemory) ToolsUsed(context.Context, ...string)           {}
func (fakeMemory) Close() error                                   { return nil }
