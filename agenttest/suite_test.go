package agenttest

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

// The SDK's agent passes the suite, queued and unqueued.
func TestSuitePassesTheSDKAgent(t *testing.T) {
	Run(t, honest(t))
	RunUnqueued(t, honest(t))
}

// honest builds the SDK's agent over the fixture.
func honest(tb testing.TB) Factory {
	tb.Helper()
	return func(f Fixture) agent.Agent {
		a, err := agent.New("alice", f.Options()...)
		if err != nil {
			tb.Fatal(err)
		}
		return a
	}
}

// recorder is a testing.TB that keeps the failures of one suite case
// instead of failing the test that runs it, so a test can prove a case fails
// an agent that breaks its rule. Fatal ends the case's goroutine, as it does
// a test's.
type recorder struct {
	testing.TB

	mu       sync.Mutex
	failures []string
	cleanups []func()
}

func (r *recorder) Helper() {}

func (r *recorder) record(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, msg)
}

func (r *recorder) Error(args ...any)                 { r.record(fmt.Sprint(args...)) }
func (r *recorder) Errorf(format string, args ...any) { r.record(fmt.Sprintf(format, args...)) }
func (r *recorder) Fail()                             { r.record("Fail") }

func (r *recorder) Fatal(args ...any) {
	r.Error(args...)
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

func (r *recorder) FailNow() {
	r.Fail()
	runtime.Goexit()
}

func (r *recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failures) > 0
}

func (r *recorder) Cleanup(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, fn)
}

func (r *recorder) runCleanups() {
	r.mu.Lock()
	fns := slices.Clone(r.cleanups)
	r.mu.Unlock()
	for _, fn := range slices.Backward(fns) {
		fn()
	}
}

// runCase runs one suite case against the agents factory builds and returns
// its failures.
func runCase(t *testing.T, fn func(testing.TB, Factory), factory func(testing.TB) Factory) []string {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer r.runCleanups()
		fn(r, factory(r))
	}()
	select {
	case <-done:
	case <-time.After(2 * waitLimit):
		t.Fatal("the case did not end")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures
}

// yielded is one value pair of Run.Events.
type yielded struct {
	ev  hook.Event
	err error
}

// broken wraps the SDK's agent and breaks the rules its fields say: it
// rewrites each run's events, or overrides a method.
type broken struct {
	agent.Agent

	events    func([]yielded) []yielded
	interrupt func(a agent.Agent, session string) (bool, int)
	cancel    func(a agent.Agent, cause error)
	runCancel func(r agent.Run, cause error)
	close     func(n int) error
	tasks     func() []hook.TaskInfo
	closes    int
}

func (b *broken) Run(ctx context.Context, session string, msg agent.Message, opts ...agent.RunOption) agent.Run {
	return &brokenRun{Run: b.Agent.Run(ctx, session, msg, opts...), b: b}
}

func (b *broken) Interrupt(session string) (bool, int) {
	if b.interrupt != nil {
		return b.interrupt(b.Agent, session)
	}
	return b.Agent.Interrupt(session)
}

func (b *broken) Cancel(cause error) {
	if b.cancel != nil {
		b.cancel(b.Agent, cause)
		return
	}
	b.Agent.Cancel(cause)
}

func (b *broken) Close(ctx context.Context) error {
	b.closes++
	err := b.Agent.Close(ctx)
	if b.close != nil {
		return b.close(b.closes)
	}
	return err
}

func (b *broken) Tasks() []hook.TaskInfo {
	if b.tasks != nil {
		return b.tasks()
	}
	return b.controller().Tasks()
}

// controller is the wrapped agent's TaskController.
func (b *broken) controller() agent.TaskController {
	tc, ok := b.Agent.(agent.TaskController)
	if !ok {
		panic("the SDK's agent has no TaskController")
	}
	return tc
}

func (b *broken) CancelTask(id string, cause error) (hook.TaskCancel, error) {
	return b.controller().CancelTask(id, cause)
}

func (b *broken) CancelAll(cause error) hook.TaskCancel {
	return b.controller().CancelAll(cause)
}

type brokenRun struct {
	agent.Run

	b *broken
}

func (r *brokenRun) Events() iter.Seq2[hook.Event, error] {
	return func(yield func(hook.Event, error) bool) {
		var all []yielded
		for ev, err := range r.Run.Events() {
			all = append(all, yielded{ev, err})
		}
		if r.b.events != nil {
			all = r.b.events(all)
		}
		for _, y := range all {
			if !yield(y.ev, y.err) {
				return
			}
		}
	}
}

func (r *brokenRun) Cancel(cause error) {
	if r.b.runCancel != nil {
		r.b.runCancel(r.Run, cause)
		return
	}
	r.Run.Cancel(cause)
}

// bare hides every capability of the agent.
type bare struct{ agent.Agent }

// building returns a factory of b over the SDK's agent, with fix applied to
// the fixture first.
func building(b broken, fix func(*Fixture)) func(testing.TB) Factory {
	return func(tb testing.TB) Factory {
		tb.Helper()
		return func(f Fixture) agent.Agent {
			if fix != nil {
				fix(&f)
			}
			bb := b
			bb.Agent = honest(tb)(f)
			return &bb
		}
	}
}

// rewriting is a broken agent whose runs' events fn rewrites.
func rewriting(fn func([]yielded) []yielded) func(testing.TB) Factory {
	return building(broken{events: fn}, nil)
}

// fixing is the SDK's agent over a fixture fix changes.
func fixing(fix func(*Fixture)) func(testing.TB) Factory {
	return building(broken{}, fix)
}

// each rewrites every event of type E with fn, keeping TurnFinished's error
// in step with its result.
func each[E hook.Event](fn func(*E)) func([]yielded) []yielded {
	return func(in []yielded) []yielded {
		out := slices.Clone(in)
		for i, y := range out {
			if e, ok := y.ev.(E); ok {
				fn(&e)
				out[i].ev = e
				if tf, ok := any(e).(hook.TurnFinished); ok {
					out[i].err = tf.Err
				}
			}
		}
		return out
	}
}

// finished rewrites the run's TurnFinished.
func finished(fn func(*hook.TurnFinished)) func([]yielded) []yielded { return each(fn) }

// dropping drops every event of type E.
func dropping[E hook.Event]() func([]yielded) []yielded {
	return func(in []yielded) []yielded {
		return slices.DeleteFunc(slices.Clone(in), func(y yielded) bool {
			_, ok := y.ev.(E)
			return ok
		})
	}
}

// withPlugins is a fixture without the test's callbacks.
func withoutPlugins(f *Fixture) { f.Plugins = nil }

// outcome changes the outcome of a turn that ended with from to to.
func outcome(from, to hook.Outcome) func([]yielded) []yielded {
	return finished(func(tf *hook.TurnFinished) {
		if tf.Outcome == from {
			tf.Outcome, tf.Err = to, nil
		}
	})
}

// Each case of the suite fails an agent that breaks the rule it checks,
// with the failure that names it.
func TestSuiteFailsBrokenAgents(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		check   func(testing.TB, Factory)
		factory func(testing.TB) Factory
		want    string
	}{
		// The event rules every run keeps.
		{
			"no event", testOutcomeOK, rewriting(func([]yielded) []yielded { return nil }),
			"a run emitted no event",
		},
		{"an error with another event", testOutcomeOK, rewriting(func(in []yielded) []yielded {
			out := slices.Clone(in)
			out[0].err = boom
			return out
		}), "Events yielded an error with an event other than TurnFinished"},
		{"a wrong error with TurnFinished", testOutcomeError, rewriting(func(in []yielded) []yielded {
			out := slices.Clone(in)
			out[len(out)-1].err = errors.New("another")
			return out
		}), "or not TurnFinished's"},
		{"two TurnFinished", testOutcomeOK, rewriting(func(in []yielded) []yielded {
			return append(slices.Clone(in), in[len(in)-1])
		}), "2 TurnFinished, want exactly 1"},
		{"TurnFinished not last", testOutcomeOK, rewriting(func(in []yielded) []yielded {
			return append(slices.Clone(in), yielded{ev: hook.LoopStarted{}})
		}), "last event is hook.LoopStarted, want TurnFinished"},
		{
			"TurnStarted not first", testOutcomeOK, rewriting(dropping[hook.TurnStarted]()),
			"first event of a started run is",
		},
		{
			"a ModelCall not started", testOutcomeOK, rewriting(dropping[hook.ModelCallStarted]()),
			"a ModelCall without its ModelCallStarted",
		},
		{
			"a model call not ended", testOutcomeOK, rewriting(dropping[hook.ModelCall]()),
			"1 ModelCallStarted without a ModelCall",
		},
		{
			"a batch finished early", testToolCalls, rewriting(dropping[hook.ToolCallFinished]()),
			"ToolBatchFinished with 2 calls not finished",
		},
		{
			"a tool call not finished", testToolCalls, rewriting(dropping[hook.ToolCallFinished]()),
			"1 started, 0 finished; want one of each",
		},

		// Outcomes.
		{
			"OutcomeOK: wrong reply", testOutcomeOK, rewriting(finished(func(tf *hook.TurnFinished) { tf.Reply = "Hi" })),
			"result = ",
		},
		{"OutcomeOK: two loops", testOutcomeOK, rewriting(func(in []yielded) []yielded {
			return slices.Insert(slices.Clone(in), 1, yielded{ev: hook.LoopStarted{}})
		}), "events = "},
		{"OutcomeEmpty", testOutcomeEmpty, rewriting(outcome(hook.OutcomeEmpty, hook.OutcomeOK)), "result = "},
		{"OutcomeError", testOutcomeError, rewriting(outcome(hook.OutcomeError, hook.OutcomeOK)), "result = "},
		{
			"OutcomeError: the call's error", testOutcomeError, rewriting(each(func(e *hook.ModelCall) { e.Err = nil })),
			"ModelCall = ",
		},

		// Fallback.
		{
			"Fallback: result", testFallback, rewriting(finished(func(tf *hook.TurnFinished) { tf.Reply = "" })),
			"result = ",
		},
		{"Fallback: no event", testFallback, rewriting(dropping[hook.Fallback]()), "Fallback = "},
		{"Fallback: no observer", testFallback, fixing(withoutPlugins), "OnFallback 0"},
		{
			"Fallback: answering model", testFallback,
			rewriting(each(func(e *hook.ModelAnswer) { e.Model.Name = Primary })), "ModelAnswer.Model = ",
		},

		// Tools.
		{
			"ToolCalls: result", testToolCalls, rewriting(finished(func(tf *hook.TurnFinished) { tf.Iterations = 1 })),
			"result = ",
		},
		{"ToolCalls: order", testToolCalls, rewriting(dropping[hook.ToolResultStored]()), "batch order = "},
		{
			"ToolCalls: tools not offered", testToolCalls, fixing(func(f *Fixture) { f.Tools = toolkit.NewRegistry() }),
			"tools offered = 0",
		},
		{"ToolError: result", testToolError, rewriting(outcome(hook.OutcomeOK, hook.OutcomeError)), "result = "},
		{"ToolError: call result", testToolError, rewriting(each(func(e *hook.ToolCallFinished) {
			e.Result = &toolkit.Result{ForLLM: "fine"}
		})), "ToolCallFinished.Result = "},
		{"ToolPanic: result", testToolPanic, rewriting(outcome(hook.OutcomeOK, hook.OutcomeError)), "result = "},
		{"ToolPanic: not a panic error", testToolPanic, rewriting(each(func(e *hook.ToolCallFinished) {
			e.Result = &toolkit.Result{ForLLM: "failed", IsError: true, Err: boom}
		})), "want a *hook.ToolPanicError"},
		{"ToolPanic: error text", testToolPanic, rewriting(each(func(e *hook.ToolCallFinished) {
			r := *e.Result
			r.Err = fmt.Errorf("wrapped: %w", r.Err)
			e.Result = &r
		})), "error text = "},
		{"ToolRefused", testToolRefused, fixing(withoutPlugins), "ran true"},
		{"ToolRemovedMidTurn: still offered", testToolRemovedMidTurn, fixing(func(f *Fixture) {
			f.Tools = toolkit.NewRegistry()
			for _, name := range []string{"refund", "close_refunds"} {
				if err := f.Tools.Add(toolspec.ToolDefinition{Name: name, Handler: func(*toolspec.ToolCall) (*toolspec.Result, error) {
					return &toolspec.Result{ForLLM: "done"}, nil
				}}); err != nil {
					panic(err)
				}
			}
		}), "tools offered per call"},
		{"ToolRemovedMidTurn: call answered", testToolRemovedMidTurn, rewriting(each(func(e *hook.ToolCallFinished) {
			e.Result = &toolkit.Result{ForLLM: "refunded"}
		})), "a removed tool's call = "},

		// Interrupts and cancels.
		{"InterruptRunningTurn: not running", testInterruptRunningTurn, building(broken{
			interrupt: func(a agent.Agent, s string) (bool, int) { a.Interrupt(s); return false, 0 },
		}, nil), "Interrupt = false, 0"},
		{
			"InterruptRunningTurn: result", testInterruptRunningTurn,
			rewriting(outcome(hook.OutcomeCancelled, hook.OutcomeOK)), "result = ",
		},
		{
			"InterruptRunningTurn: call in flight", testInterruptRunningTurn,
			rewriting(each(func(e *hook.ModelCall) { e.Err = nil })), "the call in flight ended with",
		},
		{"InterruptRunningTurn: idle session", testInterruptRunningTurn, building(broken{
			interrupt: func(a agent.Agent, s string) (bool, int) { a.Interrupt(s); return true, 0 },
		}, nil), "Interrupt of an idle session = true, 0"},
		{"InterruptDropsQueue: count", testInterruptDropsQueue, building(broken{
			interrupt: func(a agent.Agent, s string) (bool, int) { a.Interrupt(s); return true, 0 },
		}, nil), "Interrupt = true, 0"},
		{"InterruptDropsQueue: a dropped run started", testInterruptDropsQueue, rewriting(func(in []yielded) []yielded {
			if len(in) == 1 {
				return append([]yielded{{ev: hook.TurnStarted{}}}, in...)
			}
			return in
		}), "a dropped run: "},
		{"InterruptDropsQueue: a dropped run reached the model", testInterruptDropsQueue, building(broken{
			interrupt: func(a agent.Agent, s string) (bool, int) {
				running, dropped := a.Interrupt(s)
				a.Run(context.Background(), "other", agent.UserText("two")).Wait()
				return running, dropped
			},
		}, nil), "a dropped run reached the model: 2 calls"},
		{"InterruptDropsQueue: no observer", testInterruptDropsQueue, fixing(withoutPlugins), "OnInterrupt = []"},
		{"InterruptLeavesTasks: task stopped", testInterruptLeavesTasks, building(broken{
			interrupt: func(a agent.Agent, _ string) (bool, int) { a.Cancel(boom); return true, 0 },
		}, nil), "an interrupt reached the task"},
		{"InterruptLeavesTasks: no TaskController", testInterruptLeavesTasks, func(tb testing.TB) Factory {
			tb.Helper()
			return func(f Fixture) agent.Agent { return bare{honest(tb)(f)} }
		}, "the agent has no TaskController"},
		{"InterruptLeavesTasks: tasks not listed", testInterruptLeavesTasks, building(broken{
			tasks: func() []hook.TaskInfo { return nil },
		}, nil), "Tasks = []"},
		{"RunCancelStopsTasks: cause", testRunCancelStopsTasks, rewriting(finished(func(tf *hook.TurnFinished) {
			tf.Cause = boom
		})), "result = "},
		{
			"RunCancelStopsTasks: task left", testRunCancelStopsTasks, interruptingCancel(),
			"Run.Cancel left the turn's task running",
		},
		{
			"RunCancelStopsTasks: no TaskStarted", testRunCancelStopsTasks, rewriting(dropping[hook.TaskStarted]()),
			"TaskStarted events = 0",
		},
		{"AgentCancel: result", testAgentCancel, rewriting(outcome(hook.OutcomeCancelled, hook.OutcomeOK)), "result = "},
		{"AgentCancel: task left", testAgentCancel, building(broken{
			cancel: func(a agent.Agent, _ error) { a.Interrupt("s1") },
		}, nil), "Agent.Cancel left a task running"},
		{"AgentCancel: tasks listed", testAgentCancel, building(broken{
			tasks: func() []hook.TaskInfo { return []hook.TaskInfo{{ID: "t1"}} },
		}, nil), "tasks left: "},
		{
			"AbandonedTool: call", testAbandonedTool, rewriting(each(func(e *hook.ToolCallFinished) { e.Interrupted = false })),
			"ToolCallFinished = ",
		},
		{
			"AbandonedTool: result", testAbandonedTool, rewriting(outcome(hook.OutcomeCancelled, hook.OutcomeOK)),
			"result = ",
		},

		// Panics.
		{"CallbackPanic", testCallbackPanic, rewriting(outcome(hook.OutcomeError, hook.OutcomeOK)), "result = "},
		{"ObserverPanic", testObserverPanic, rewriting(outcome(hook.OutcomeOK, hook.OutcomeError)), "result = "},

		// Sessions and lifetime.
		{"SessionsConcurrent: second", testSessionsConcurrent, rewriting(finished(func(tf *hook.TurnFinished) {
			if tf.Reply == "other" {
				tf.Outcome = hook.OutcomeEmpty
			}
		})), "second session = "},
		{"SessionsConcurrent: first", testSessionsConcurrent, rewriting(finished(func(tf *hook.TurnFinished) {
			if tf.Reply == "late" {
				tf.Outcome = hook.OutcomeEmpty
			}
		})), "first session = "},
		{"Close: fails", testClose, building(broken{close: func(int) error { return boom }}, nil), "Close = boom"},
		{"Close: a run after it", testClose, rewriting(outcome(hook.OutcomeError, hook.OutcomeOK)), "a run after Close = "},
		{"Close: a second one fails", testClose, building(broken{close: func(n int) error {
			if n > 1 {
				return boom
			}
			return nil
		}}, nil), "a second Close = boom"},
		{
			"Close: at the end of a test", testOutcomeOK, building(broken{close: func(int) error { return boom }}, nil),
			"Close: boom",
		},
		{"no agent", testOutcomeOK, func(testing.TB) Factory {
			return func(Fixture) agent.Agent { return nil }
		}, "the factory returned no agent"},

		// Turn ids and the model.
		{"TurnIDAndModel: id", testTurnIDAndModel, rewriting(finished(func(tf *hook.TurnFinished) {
			tf.Scope.TurnID = "turn-7"
		})), `has turn id "turn-7", want the run's`},
		{"TurnIDAndModel: model", testTurnIDAndModel, fixing(withoutPlugins), "BeforeTurn's Turn.Model = "},
		{"WithoutTurnID", testWithoutTurnID, rewriting(finished(func(tf *hook.TurnFinished) {
			tf.Scope.TurnID = "turn-7"
		})), `has turn id "turn-7", want none`},

		// Unqueued.
		{"Unqueued: second run", testUnqueued, building(broken{events: finished(func(tf *hook.TurnFinished) {
			if tf.Reply == "second" {
				tf.Outcome = hook.OutcomeEmpty
			}
		})}, unqueued), "an unqueued second run of the session = "},
		{"Unqueued: interrupt", testUnqueued, building(broken{
			interrupt: func(a agent.Agent, s string) (bool, int) { a.Interrupt(s); return false, 0 },
		}, unqueued), "Interrupt = false, 0"},
		{"Unqueued: first run", testUnqueued, building(broken{
			events: outcome(hook.OutcomeCancelled, hook.OutcomeOK),
		}, unqueued), "first run = "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failures := runCase(t, tt.check, tt.factory)
			if !slices.ContainsFunc(failures, func(f string) bool { return strings.Contains(f, tt.want) }) {
				t.Fatalf("failures = %q, want one containing %q", failures, tt.want)
			}
		})
	}
}

// unqueued builds the agent not to queue runs, as RunUnqueued does.
func unqueued(f *Fixture) { f.Unqueued = true }

// interruptingCancel is an agent whose Run.Cancel only interrupts the turn,
// leaving its tasks, while reporting the cause it was given.
func interruptingCancel() func(testing.TB) Factory {
	var mu sync.Mutex
	var given error
	return building(broken{
		runCancel: func(r agent.Run, cause error) {
			mu.Lock()
			given = cause
			mu.Unlock()
			r.Interrupt()
		},
		events: finished(func(tf *hook.TurnFinished) {
			mu.Lock()
			defer mu.Unlock()
			if given != nil {
				tf.Cause = given
			}
		}),
	}, nil)
}
