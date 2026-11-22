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
	_ hook.Event = hook.ModelCall{}
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
	if got := (hook.Limits{Retries: -1}).WithDefaults().Retries; got != 0 {
		t.Fatalf("negative Retries = %d, want 0", got)
	}
}

func TestRunOptions(t *testing.T) {
	got := hook.ApplyRunOptions(hook.AlreadyStored(), nil, hook.ResetSession(),
		hook.TurnBudget(time.Hour), hook.AtDepth(2))
	want := hook.RunOptions{AlreadyStored: true, ResetSession: true, Budget: time.Hour, Depth: 2}
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
func (fakeMemory) ToolsUsed(...string)                            {}
func (fakeMemory) Close() error                                   { return nil }
