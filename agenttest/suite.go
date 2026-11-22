package agenttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

// waitLimit bounds every wait of the suite, so a broken implementation fails
// instead of hanging.
const waitLimit = 10 * time.Second

// collect reads every event of r and checks the contract every run keeps.
func collect(t *testing.T, r agent.Run) []hook.Event {
	t.Helper()
	done := make(chan []hook.Event, 1)
	go func() {
		var evs []hook.Event
		var errs []error
		for ev, err := range r.Events() {
			evs = append(evs, ev)
			errs = append(errs, err)
		}
		// Events yields TurnFinished's error with it, and nil with every
		// other event.
		for i, err := range errs {
			tf, last := evs[i].(hook.TurnFinished)
			if (last && !errors.Is(err, tf.Err)) || (!last && err != nil) {
				evs = append(evs, nil)
				break
			}
		}
		done <- evs
	}()
	select {
	case evs := <-done:
		checkContract(t, evs)
		return evs
	case <-time.After(waitLimit):
		t.Fatal("the run did not finish")
		return nil
	}
}

// checkContract checks the event rules of package hook that hold for every
// run.
func checkContract(t *testing.T, evs []hook.Event) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("a run emitted no event")
	}
	if evs[len(evs)-1] == nil {
		t.Fatal("Events yielded an error with an event other than TurnFinished, or not TurnFinished's")
	}
	finished := 0
	for _, ev := range evs {
		if _, ok := ev.(hook.TurnFinished); ok {
			finished++
		}
	}
	if finished != 1 {
		t.Errorf("%d TurnFinished, want exactly 1", finished)
	}
	if _, ok := evs[len(evs)-1].(hook.TurnFinished); !ok {
		t.Errorf("last event is %T, want TurnFinished", evs[len(evs)-1])
	}
	if len(evs) > 1 {
		if _, ok := evs[0].(hook.TurnStarted); !ok {
			t.Errorf("first event of a started run is %T, want TurnStarted", evs[0])
		}
	}
	started := map[string]int{}
	finishedCalls := map[string]int{}
	modelCalls := 0
	batchOpen := 0
	for i, ev := range evs {
		switch e := ev.(type) {
		case hook.ModelCallStarted:
			modelCalls++
		case hook.ModelCall:
			modelCalls--
			if modelCalls < 0 {
				t.Errorf("event %d: a ModelCall without its ModelCallStarted", i)
			}
		case hook.ToolBatchStarted:
			batchOpen = len(e.Calls)
		case hook.ToolCallStarted:
			started[fmt.Sprintf("%d/%s", e.Call.Iteration, e.Call.ID)]++
		case hook.ToolCallFinished:
			finishedCalls[fmt.Sprintf("%d/%s", e.Call.Iteration, e.Call.ID)]++
			batchOpen--
		case hook.ToolBatchFinished:
			if batchOpen != 0 {
				t.Errorf("event %d: ToolBatchFinished with %d calls not finished", i, batchOpen)
			}
		}
	}
	if modelCalls != 0 {
		t.Errorf("%d ModelCallStarted without a ModelCall", modelCalls)
	}
	for id, n := range started {
		if n != 1 || finishedCalls[id] != 1 {
			t.Errorf("tool call %s: %d started, %d finished; want one of each", id, n, finishedCalls[id])
		}
	}
}

// result is the run's TurnFinished.
func result(evs []hook.Event) hook.TurnFinished {
	tf, ok := evs[len(evs)-1].(hook.TurnFinished)
	if !ok {
		return hook.TurnFinished{}
	}
	return tf
}

// count is how many events of type E evs has.
func count[E hook.Event](evs []hook.Event) int {
	n := 0
	for _, ev := range evs {
		if _, ok := ev.(E); ok {
			n++
		}
	}
	return n
}

// first is the first event of type E in evs.
func first[E hook.Event](evs []hook.Event) (E, bool) {
	for _, ev := range evs {
		if e, ok := ev.(E); ok {
			return e, true
		}
	}
	var zero E
	return zero, false
}

// callsStep is a model answer calling the named tools, ids c1, c2, ….
func callsStep(names ...string) modelstest.Step {
	tc := make([]spawnllm.ToolCall, 0, len(names))
	for i, n := range names {
		tc = append(tc, spawnllm.ToolCall{ID: fmt.Sprintf("c%d", i+1), Name: n, Arguments: map[string]any{}})
	}
	return modelstest.Step{Response: &spawnllm.LLMResponse{ToolCalls: tc, FinishReason: "tool_calls"}}
}

// held is a model step that waits until release is closed, with started
// closed as it begins.
func held(started, release chan struct{}) modelstest.Step {
	return modelstest.Step{Started: started, Hold: release, Response: &spawnllm.LLMResponse{Content: "late"}}
}

// wait waits for ch, failing the test after waitLimit.
func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func testOutcomeOK(t *testing.T, factory Factory) {
	f := newFixture(t)
	f.Provider.Script(PrimaryWire, modelstest.Reply("Hello, Bob."))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello")))
	res := result(evs)
	if res.Outcome != hook.OutcomeOK || res.Reply != "Hello, Bob." || res.Err != nil {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	if count[hook.ModelCall](evs) != 1 || count[hook.LoopStarted](evs) != 1 {
		t.Fatalf("events = %v", kinds(evs))
	}
}

func testOutcomeEmpty(t *testing.T, factory Factory) {
	f := newFixture(t)
	f.Provider.Script(PrimaryWire, modelstest.Reply(""))
	a := build(t, factory, f)
	if res := result(collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello")))); res.Outcome != hook.OutcomeEmpty {
		t.Fatalf("result = %+v", res.TurnResult)
	}
}

func testOutcomeError(t *testing.T, factory Factory) {
	f := newFixture(t)
	boom := errors.New("boom")
	f.Provider.Script(PrimaryWire, modelstest.Fail(boom))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello")))
	res := result(evs)
	if res.Outcome != hook.OutcomeError || !errors.Is(res.Err, boom) {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	call, ok := first[hook.ModelCall](evs)
	if !ok || !errors.Is(call.Err, boom) {
		t.Fatalf("ModelCall = %+v", call)
	}
}

func testFallback(t *testing.T, factory Factory) {
	var observed []hook.Fallback
	f := newFixture(t, hook.Plugin{Name: "watch", OnFallback: func(_ context.Context, ev hook.Fallback) { observed = append(observed, ev) }})
	f.Provider.Script(PrimaryWire, modelstest.Status(503, 0))
	f.Provider.Script(BackupWire, modelstest.Reply("From the backup."))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello")))
	if res := result(evs); res.Outcome != hook.OutcomeOK || res.Reply != "From the backup." {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	fb, ok := first[hook.Fallback](evs)
	if !ok || fb.Next.Name != Backup || len(fb.Passed) != 1 || fb.Passed[0].Model.Name != Primary {
		t.Fatalf("Fallback = %+v", fb)
	}
	if count[hook.ModelCall](evs) != 2 || len(observed) != 1 {
		t.Fatalf("model calls %d, OnFallback %d", count[hook.ModelCall](evs), len(observed))
	}
	answer, _ := first[hook.ModelAnswer](evs)
	if answer.Model.Name != Backup {
		t.Fatalf("ModelAnswer.Model = %+v", answer.Model)
	}
}

func testToolCalls(t *testing.T, factory Factory) {
	f := newFixture(t)
	addTool(t, f, "lookup_order", func(context.Context, map[string]any) (*toolspec.Result, error) {
		return &toolspec.Result{ForLLM: "Order 42 shipped."}, nil
	})
	addTool(t, f, "refund", func(context.Context, map[string]any) (*toolspec.Result, error) {
		return &toolspec.Result{ForLLM: "No refund due."}, nil
	})
	f.Provider.Script(PrimaryWire, callsStep("lookup_order", "refund"), modelstest.Reply("It shipped."))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Where is my order?")))
	if res := result(evs); res.Outcome != hook.OutcomeOK || res.Reply != "It shipped." || res.Iterations != 2 {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	var order []string
	for _, ev := range evs {
		switch e := ev.(type) {
		case hook.ToolBatchStarted:
			order = append(order, "batch")
		case hook.ToolCallStarted:
			order = append(order, "start:"+e.Call.Name)
		case hook.ToolBatchFinished:
			order = append(order, "batchdone")
		case hook.ToolResultStored:
			order = append(order, "stored:"+e.Call.Name)
		}
	}
	want := []string{"batch", "start:lookup_order", "start:refund", "batchdone", "stored:lookup_order", "stored:refund"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("batch order = %v, want %v", order, want)
	}
	calls := f.Provider.Calls()
	if len(calls) != 2 || len(calls[0].Tools) != 2 {
		t.Fatalf("model calls = %d, tools offered = %d", len(calls), len(calls[0].Tools))
	}
}

func testToolError(t *testing.T, factory Factory) {
	f := newFixture(t)
	addTool(t, f, "lookup_order", func(context.Context, map[string]any) (*toolspec.Result, error) {
		return nil, errors.New("database down")
	})
	f.Provider.Script(PrimaryWire, callsStep("lookup_order"), modelstest.Reply("Sorry."))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Where is my order?")))
	if res := result(evs); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	fin, _ := first[hook.ToolCallFinished](evs)
	if fin.Result == nil || (!fin.Result.IsError && fin.Result.Err == nil) {
		t.Fatalf("ToolCallFinished.Result = %+v", fin.Result)
	}
}

func testToolRefused(t *testing.T, factory Factory) {
	ran := false
	f := newFixture(t, hook.Plugin{Name: "approve", BeforeTool: func(_ context.Context, call *hook.ToolCall) (*toolkit.Result, error) {
		if call.Name == "refund" {
			return nil, toolkit.Refusal(errors.New("refunds need approval"))
		}
		return nil, nil //nolint:nilnil // (nil, nil): run the tool
	}})
	addTool(t, f, "refund", func(context.Context, map[string]any) (*toolspec.Result, error) {
		ran = true
		return &toolspec.Result{ForLLM: "refunded"}, nil
	})
	f.Provider.Script(PrimaryWire, callsStep("refund"), modelstest.Reply("I cannot refund that."))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Refund me")))
	fin, _ := first[hook.ToolCallFinished](evs)
	if ran || !fin.Answered || fin.Result == nil || !fin.Result.IsError || !toolkit.IsExpectedRefusal(fin.Result.Err) {
		t.Fatalf("ran %v, ToolCallFinished = %+v", ran, fin)
	}
}

func testToolRemovedMidTurn(t *testing.T, factory Factory) {
	f := newFixture(t)
	addTool(t, f, "refund", func(context.Context, map[string]any) (*toolspec.Result, error) {
		return &toolspec.Result{ForLLM: "refunded"}, nil
	})
	addTool(t, f, "close_refunds", func(context.Context, map[string]any) (*toolspec.Result, error) {
		f.Tools.Remove("refund")
		return &toolspec.Result{ForLLM: "Refunds are closed."}, nil
	})
	f.Provider.Script(PrimaryWire, callsStep("close_refunds"), callsStep("refund"), modelstest.Reply("Done."))
	a := build(t, factory, f)
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Close refunds, then refund me")))
	calls := f.Provider.Calls()
	if len(calls) != 3 || len(calls[0].Tools) != 2 || len(calls[1].Tools) != 1 {
		t.Fatalf("tools offered per call: %d calls", len(calls))
	}
	var refund hook.ToolCallFinished
	for _, ev := range evs {
		if e, ok := ev.(hook.ToolCallFinished); ok && e.Call.Name == "refund" {
			refund = e
		}
	}
	if refund.Result == nil || !refund.Result.IsError {
		t.Fatalf("a removed tool's call = %+v", refund.Result)
	}
}

func testInterruptRunningTurn(t *testing.T, factory Factory) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.Provider.Script(PrimaryWire, held(started, release))
	a := build(t, factory, f)
	r := a.Run(context.Background(), "s1", agent.UserText("Hello"))
	wait(t, started, "the model call")
	running, dropped := a.Interrupt("s1")
	if !running || dropped != 0 {
		t.Fatalf("Interrupt = %v, %d", running, dropped)
	}
	evs := collect(t, r)
	res := result(evs)
	if res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	call, ok := first[hook.ModelCall](evs)
	if !ok || call.Err == nil {
		t.Fatalf("the call in flight ended with %+v", call)
	}
	if running, dropped := a.Interrupt("s1"); running || dropped != 0 {
		t.Fatalf("Interrupt of an idle session = %v, %d", running, dropped)
	}
}

func testInterruptDropsQueue(t *testing.T, factory Factory) {
	var interrupted []hook.Interrupted
	var mu sync.Mutex
	f := newFixture(t, hook.Plugin{Name: "watch", OnInterrupt: func(_ context.Context, ev hook.Interrupted) {
		mu.Lock()
		interrupted = append(interrupted, ev)
		mu.Unlock()
	}})
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.Provider.Script(PrimaryWire, held(started, release), modelstest.Reply("next"))
	a := build(t, factory, f)
	r1 := a.Run(context.Background(), "s1", agent.UserText("one"))
	wait(t, started, "the model call")
	r2 := a.Run(context.Background(), "s1", agent.UserText("two"))
	r3 := a.Run(context.Background(), "s1", agent.UserText("three"))
	if running, dropped := a.Interrupt("s1"); !running || dropped != 2 {
		t.Fatalf("Interrupt = %v, %d", running, dropped)
	}
	collect(t, r1)
	for _, r := range []agent.Run{r2, r3} {
		evs := collect(t, r)
		if len(evs) != 1 || result(evs).Outcome != hook.OutcomeCancelled {
			t.Fatalf("a dropped run: %v, %+v", kinds(evs), result(evs).TurnResult)
		}
	}
	if len(f.Provider.Calls()) != 1 {
		t.Fatalf("a dropped run reached the model: %d calls", len(f.Provider.Calls()))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(interrupted) != 1 || interrupted[0].Dropped != 2 || !interrupted[0].Running {
		t.Fatalf("OnInterrupt = %+v", interrupted)
	}
}

// spawnTool registers "spawn", which starts a task through the call's
// registrar and keeps its context.
func spawnTool(t *testing.T, f Fixture, task *context.Context, mu *sync.Mutex) {
	addTool(t, f, "spawn", func(ctx context.Context, _ map[string]any) (*toolspec.Result, error) {
		reg, ok := hook.TaskRegistrarFrom(ctx)
		if !ok {
			return nil, errors.New("no task registrar on the tool call's context")
		}
		tctx, _, err := reg.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{Kind: "subagent", Label: "Bob's research"}})
		if err != nil {
			return nil, err
		}
		mu.Lock()
		*task = tctx //nolint:fatcontext // kept for the test to inspect
		mu.Unlock()
		return &toolspec.Result{ForLLM: "started"}, nil
	})
}

func testInterruptLeavesTasks(t *testing.T, factory Factory) {
	f := newFixture(t)
	var task context.Context
	var mu sync.Mutex
	spawnTool(t, f, &task, &mu)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.Provider.Script(PrimaryWire, callsStep("spawn"), held(started, release))
	a := build(t, factory, f)
	r := a.Run(context.Background(), "s1", agent.UserText("Research this"))
	wait(t, started, "the second model call")
	a.Interrupt("s1")
	collect(t, r)
	mu.Lock()
	defer mu.Unlock()
	if task == nil || task.Err() != nil {
		t.Fatal("an interrupt reached the task")
	}
	tc, ok := a.(agent.TaskController)
	if !ok {
		t.Fatal("the agent has no TaskController")
	}
	if tasks := tc.Tasks(); len(tasks) != 1 || tasks[0].Session != "s1" {
		t.Fatalf("Tasks = %+v", tasks)
	}
}

func testRunCancelStopsTasks(t *testing.T, factory Factory) {
	f := newFixture(t)
	var task context.Context
	var mu sync.Mutex
	spawnTool(t, f, &task, &mu)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.Provider.Script(PrimaryWire, callsStep("spawn"), held(started, release))
	a := build(t, factory, f)
	r := a.Run(context.Background(), "s1", agent.UserText("Research this"))
	wait(t, started, "the second model call")
	cause := fmt.Errorf("host stop: %w", hook.ErrInterrupted)
	r.Cancel(cause)
	evs := collect(t, r)
	if res := result(evs); res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, cause) {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	mu.Lock()
	defer mu.Unlock()
	if task == nil || task.Err() == nil {
		t.Fatal("Run.Cancel left the turn's task running")
	}
	if count[hook.TaskStarted](evs) != 1 {
		t.Fatalf("TaskStarted events = %d", count[hook.TaskStarted](evs))
	}
}

func testAgentCancel(t *testing.T, factory Factory) {
	f := newFixture(t)
	var task context.Context
	var mu sync.Mutex
	spawnTool(t, f, &task, &mu)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.Provider.Script(PrimaryWire, callsStep("spawn"), held(started, release))
	a := build(t, factory, f)
	r := a.Run(context.Background(), "s1", agent.UserText("Research this"))
	wait(t, started, "the second model call")
	a.Cancel(errors.New("operator stop"))
	if res := result(collect(t, r)); res.Outcome != hook.OutcomeCancelled {
		t.Fatalf("result = %+v", res.TurnResult)
	}
	mu.Lock()
	defer mu.Unlock()
	if task == nil || task.Err() == nil {
		t.Fatal("Agent.Cancel left a task running")
	}
	if tc, ok := a.(agent.TaskController); ok && len(tc.Tasks()) != 0 {
		t.Fatalf("tasks left: %+v", tc.Tasks())
	}
}

func testAbandonedTool(t *testing.T, factory Factory) {
	f := newFixture(t)
	inTool := make(chan struct{})
	stuck := make(chan struct{})
	defer close(stuck)
	addTool(t, f, "slow", func(context.Context, map[string]any) (*toolspec.Result, error) {
		close(inTool)
		<-stuck // ignores its context
		return &toolspec.Result{ForLLM: "too late"}, nil
	})
	f.Provider.Script(PrimaryWire, callsStep("slow"), modelstest.Reply("unreached"))
	a := build(t, factory, f)
	r := a.Run(context.Background(), "s1", agent.UserText("Go slow"))
	wait(t, inTool, "the tool call")
	a.Interrupt("s1")
	evs := collect(t, r)
	fin, ok := first[hook.ToolCallFinished](evs)
	if !ok || !fin.Interrupted || fin.Result == nil || fin.Result.ForLLM != hook.InterruptedText {
		t.Fatalf("ToolCallFinished = %+v", fin)
	}
	if res := result(evs); res.Outcome != hook.OutcomeCancelled {
		t.Fatalf("result = %+v", res.TurnResult)
	}
}

func testCallbackPanic(t *testing.T, factory Factory) {
	f := newFixture(t, hook.Plugin{Name: "broken", AfterModel: func(context.Context, *hook.ModelInfo, *hook.ModelResponse) {
		panic("broken callback")
	}})
	f.Provider.Script(PrimaryWire, modelstest.Reply("Hello"))
	a := build(t, factory, f)
	res := result(collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello"))))
	var perr *hook.PanicError
	if res.Outcome != hook.OutcomeError || !errors.As(res.Err, &perr) || perr.Callback != "AfterModel" || perr.Plugin != "broken" {
		t.Fatalf("result = %+v", res.TurnResult)
	}
}

func testObserverPanic(t *testing.T, factory Factory) {
	f := newFixture(t, hook.Plugin{Name: "broken", OnEvent: func(context.Context, hook.Event) { panic("broken observer") }})
	f.Provider.Script(PrimaryWire, modelstest.Reply("Hello"))
	a := build(t, factory, f)
	if res := result(collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello")))); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res.TurnResult)
	}
}

func testSessionsConcurrent(t *testing.T, factory Factory) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.Provider.Script(PrimaryWire, held(started, release), modelstest.Reply("other"))
	a := build(t, factory, f)
	r1 := a.Run(context.Background(), "s1", agent.UserText("one"))
	wait(t, started, "the first session's call")
	r2 := a.Run(context.Background(), "s2", agent.UserText("two"))
	if res := result(collect(t, r2)); res.Outcome != hook.OutcomeOK {
		t.Fatalf("second session = %+v", res.TurnResult)
	}
	close(release)
	if res := result(collect(t, r1)); res.Outcome != hook.OutcomeOK {
		t.Fatalf("first session = %+v", res.TurnResult)
	}
}

func testClose(t *testing.T, factory Factory) {
	f := newFixture(t)
	f.Provider.Script(PrimaryWire, modelstest.Reply("Hello"))
	a := factory(f)
	collect(t, a.Run(context.Background(), "s1", agent.UserText("Hello")))
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	if err := a.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}
	evs := collect(t, a.Run(context.Background(), "s1", agent.UserText("Again")))
	if res := result(evs); res.Outcome != hook.OutcomeError || res.Err == nil {
		t.Fatalf("a run after Close = %+v", res.TurnResult)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatalf("a second Close = %v", err)
	}
}

// kinds names the events, for failure messages.
func kinds(evs []hook.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("%T", ev))
	}
	return out
}
