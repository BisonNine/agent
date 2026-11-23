package loop

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
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
)

// interrupts collects OnInterrupt events.
type interrupts struct {
	mu  sync.Mutex
	got []hook.Interrupted
}

func (i *interrupts) on(_ context.Context, ev hook.Interrupted) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.got = append(i.got, ev)
}

func (i *interrupts) all() []hook.Interrupted {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.got)
}

// heldStep is a model call that waits for release, closing started first.
func heldStep(started, release chan struct{}) modelstest.Step {
	return modelstest.Step{Started: started, Hold: release, Response: &spawnllm.LLMResponse{Content: "late"}}
}

// wait waits for ch or fails the test.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// The agent reports what it was built with.
func TestAgent_Accessors(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, func(s *Spec) { s.Description = "Answers Bob's questions." })
	if a.Description() != "Answers Bob's questions." || a.Tools() != f.reg || a.Models() != f.cat.Default() {
		t.Fatalf("Description, Tools, Models = %q, %v, %v", a.Description(), a.Tools(), a.Models())
	}
}

// Run.Interrupt drops a queued run alone: the running turn goes on, the
// dropped run ends cancelled with TurnFinished alone, and OnInterrupt counts
// it. A finished run's Interrupt does nothing.
func TestRun_InterruptQueued(t *testing.T) {
	f := newAgentFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.p.Script("w1", heldStep(started, release), modelstest.Reply("second"))
	obs := &interrupts{}
	a := f.agent(t, func(s *Spec) { s.Plugins = []hook.Plugin{{OnInterrupt: obs.on}} })
	first := a.Run(context.Background(), "s1", hook.UserText("one"))
	waitFor(t, started, "the first call")
	second := a.Run(context.Background(), "s1", hook.UserText("two"))
	second.Interrupt()
	res := second.Wait()
	if res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
		t.Fatalf("dropped run = %+v", res)
	}
	waitFor(t, second.Done(), "the dropped run's Done")
	n := 0
	for _, err := range second.Events() {
		if !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("the dropped run yielded error %v", err)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("the dropped run yielded %d events, want TurnFinished alone", n)
	}
	close(release)
	if res := first.Wait(); res.Outcome != hook.OutcomeOK || res.Reply != "late" {
		t.Fatalf("running run = %+v", res)
	}
	got := obs.all()
	if len(got) != 1 || got[0].Dropped != 1 || got[0].Running || got[0].Scope.Session != "s1" {
		t.Fatalf("OnInterrupt = %+v", got)
	}
	first.Interrupt()
	first.Cancel(nil)
	if len(obs.all()) != 1 {
		t.Fatalf("stopping a finished run was reported: %+v", obs.all())
	}
	if len(f.p.Calls()) != 1 {
		t.Fatalf("the dropped run reached the model: %d calls", len(f.p.Calls()))
	}
}

// Run.Interrupt stops the running turn and leaves its tasks; Run.Cancel(nil)
// stops it with ErrInterrupted and its tasks too, and OnInterrupt names them.
func TestRun_InterruptAndCancelRunning(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		f := newAgentFixture(t)
		var task context.Context
		var mu sync.Mutex
		f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
			reg, _ := hook.TaskRegistrarFrom(ctx)
			tctx, _, err := reg.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{ID: "bob-research"}})
			mu.Lock()
			task = tctx //nolint:fatcontext // kept for the test to inspect
			mu.Unlock()
			return "started", err
		})
		started, release := make(chan struct{}), make(chan struct{})
		f.p.Script("w1", calls("spawn"), heldStep(started, release))
		obs := &interrupts{}
		a := f.agent(t, func(s *Spec) { s.Plugins = []hook.Plugin{{OnInterrupt: obs.on}} })
		r := a.Run(context.Background(), "s1", hook.UserText("research"))
		waitFor(t, started, "the second call")
		if cancel {
			r.Cancel(nil)
		} else {
			r.Interrupt()
		}
		res := r.Wait()
		close(release)
		if res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
			t.Fatalf("cancel %v: result = %+v", cancel, res)
		}
		mu.Lock()
		taskErr := task.Err()
		mu.Unlock()
		if cancel != (taskErr != nil) {
			t.Fatalf("cancel %v: task context error %v", cancel, taskErr)
		}
		got := obs.all()
		if len(got) != 1 || !got[0].Running || got[0].Scope.TurnID == "" {
			t.Fatalf("cancel %v: OnInterrupt = %+v", cancel, got)
		}
		if cancel && (len(got[0].Cancelled) != 1 || got[0].Cancelled[0].ID != "bob-research") {
			t.Fatalf("OnInterrupt.Cancelled = %+v", got[0].Cancelled)
		}
	}
}

// A turn with no turn id has no tasks of its own to cancel: Run.Cancel stops
// it and leaves the tasks.
func TestRun_CancelWithoutTurnID(t *testing.T) {
	f := newAgentFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.p.Script("w1", heldStep(started, release))
	a := f.agent(t, nil)
	r := a.Run(context.Background(), "s1", hook.UserText("hi"), hook.WithoutTurnID())
	waitFor(t, started, "the call")
	cause := errors.New("operator stop")
	r.Cancel(cause)
	if res := r.Wait(); res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, cause) {
		t.Fatalf("result = %+v", res)
	}
}

// Interrupt names the session's tasks still running; Cancel(nil) stops the
// rest with ErrInterrupted, dropping queued runs.
func TestAgent_InterruptStillRunningAndCancelNil(t *testing.T) {
	f := newAgentFixture(t)
	f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
		reg, _ := hook.TaskRegistrarFrom(ctx)
		_, _, err := reg.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{ID: "bob-research"}})
		return "started", err
	})
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.p.Script("w1", calls("spawn"), heldStep(started, release))
	obs := &interrupts{}
	a := f.agent(t, func(s *Spec) { s.Plugins = []hook.Plugin{{OnInterrupt: obs.on}} })
	r := a.Run(context.Background(), "s1", hook.UserText("research"))
	waitFor(t, started, "the second call")
	a.Interrupt("s1")
	r.Wait()
	got := obs.all()
	if len(got) != 1 || len(got[0].StillRunning) != 1 || got[0].StillRunning[0].ID != "bob-research" {
		t.Fatalf("OnInterrupt = %+v", got)
	}

	started2, release2 := make(chan struct{}), make(chan struct{})
	defer close(release2)
	f.p.Script("w1", heldStep(started2, release2))
	running := a.Run(context.Background(), "s1", hook.UserText("two"))
	waitFor(t, started2, "the next call")
	queued := a.Run(context.Background(), "s1", hook.UserText("three"))
	a.Cancel(nil)
	for _, r := range []hook.Run{running, queued} {
		if res := r.Wait(); res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
			t.Fatalf("result = %+v", res)
		}
	}
	got = obs.all()
	last := got[len(got)-1]
	if !last.Running || last.Dropped != 1 || len(last.Cancelled) != 1 {
		t.Fatalf("Cancel's OnInterrupt = %+v", last)
	}
	if len(a.Tasks()) != 0 {
		t.Fatalf("tasks left: %+v", a.Tasks())
	}
}

// failingMemory is a session memory that recalls what it holds, records the
// tools used, and fails to close.
type failingMemory struct {
	mu     sync.Mutex
	recall []hook.Recalled
	used   []string
}

func (m *failingMemory) Recall(context.Context, string) []hook.Recalled { return m.recall }
func (m *failingMemory) Observe(context.Context, int64, string, string) {}
func (m *failingMemory) ToolsUsed(_ context.Context, names ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.used = append(m.used, names...)
}
func (m *failingMemory) Close() error { return errors.New("memory disk full") }

// What memory recalls is placed where it asks; the tools a turn used are
// recorded; a memory that fails to close is logged by ForgetSession,
// redacted, and reported by Close.
func TestAgent_MemoryRecallToolsAndClose(t *testing.T) {
	f := newAgentFixture(t)
	f.tool(t, "lookup_order", func(context.Context, map[string]any) (string, error) { return "shipped", nil })
	f.p.Script("w1", calls("lookup_order"), modelstest.Reply("It shipped."), modelstest.Reply("again"))
	mem := &failingMemory{recall: []hook.Recalled{
		{Placement: hook.PlaceSystem, Text: "Bob prefers email."},
		{Placement: hook.PlaceCurrentUser, Text: "Bob ordered on Monday."},
	}}
	log := &lines{}
	a, err := New(Spec{
		ID: "alice", Models: f.cat.Default(), Tools: f.reg,
		Memory:   memoryFunc(func() hook.MemorySession { return mem }),
		Logger:   slog.New(log),
		Redactor: func(s string) string { return strings.ReplaceAll(s, "disk", "[redacted]") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := a.Run(context.Background(), "s1", hook.UserText("Where is my order?")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	msgs := f.p.Calls()[0].Messages
	if !strings.Contains(msgs[0].Content, "Bob prefers email.") || strings.Contains(msgs[0].Content, "Bob ordered on Monday.") {
		t.Fatalf("system message = %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[len(msgs)-1].Content, "Bob ordered on Monday.") {
		t.Fatalf("user message = %q", msgs[len(msgs)-1].Content)
	}
	mem.mu.Lock()
	used := slices.Clone(mem.used)
	mem.mu.Unlock()
	if !slices.Contains(used, "lookup_order") {
		t.Fatalf("tools used = %v", used)
	}
	// The session may still be finishing its turn for a moment.
	for deadline := time.Now().Add(10 * time.Second); !a.ForgetSession("s1"); runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the session stayed busy")
		}
	}
	if !slices.Contains(log.messages(), "memory close failed") || logged(log, "memory disk full") || !logged(log, "memory [redacted] full") {
		t.Fatalf("log = %v", log.messages())
	}
	a.Run(context.Background(), "s1", hook.UserText("again")).Wait()
	if err := a.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "close memory of session") {
		t.Fatalf("Close = %v", err)
	}
}

// logged reports whether any record has an attribute whose value contains s.
func logged(l *lines, s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	found := false
	for _, r := range l.got {
		r.Attrs(func(a slog.Attr) bool {
			found = found || strings.Contains(a.Value.String(), s)
			return !found
		})
	}
	return found
}

// A memory that cannot be opened is logged and the session runs without it.
func TestAgent_MemoryNotOpened(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Reply("hi"))
	log := &lines{}
	a := f.agent(t, func(s *Spec) {
		s.Memory = memoryOpenFails{}
		s.Logger = slog.New(log)
	})
	if res := a.Run(context.Background(), "s1", hook.UserText("hello")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Contains(log.messages(), "memory not opened; the session runs without it") {
		t.Fatalf("log = %v", log.messages())
	}
}

type memoryOpenFails struct{}

func (memoryOpenFails) Open(context.Context, hook.Scope) (hook.MemorySession, error) {
	return nil, errors.New("memory offline")
}

// A host session that fails ends the turn with its error, after releasing
// what it gave.
func TestHosting_SessionFails(t *testing.T) {
	f := newAgentFixture(t)
	released := 0
	boom := errors.New("store offline")
	a := f.agent(t, func(s *Spec) {
		s.Session = func(context.Context, hook.Scope) (Session, error) {
			return Session{Release: func() { released++ }}, boom
		}
	})
	res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
	if res.Outcome != hook.OutcomeError || !errors.Is(res.Err, boom) || released != 1 {
		t.Fatalf("result %+v, released %d", res, released)
	}
	if len(f.p.Calls()) != 0 {
		t.Fatal("the model was called")
	}
}

// failingServed cannot issue a credential.
type failingServed struct{}

func (failingServed) ServeTurn(context.Context, hook.Scope) (string, func(), error) {
	return "", nil, errors.New("token store down")
}

// A CLI model's turn runs without served tools when they cannot be served.
func TestAgent_ServedToolsFail(t *testing.T) {
	p := modelstest.New().Script("w1", modelstest.Reply("ok"))
	cat := models.New()
	if err := cat.AddProvider(modelspec.Provider{Name: "api", Protocol: "claude-cli"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Add(modelspec.Model{ModelName: "m1", Model: "w1", Provider: "api"}, models.For(models.Chat, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.RegisterClient("m1", p); err != nil {
		t.Fatal(err)
	}
	log := &lines{}
	a, err := New(Spec{ID: "alice", Models: cat.Default(), Served: failingServed{}, Logger: slog.New(log)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if res := a.Run(context.Background(), "s1", hook.UserText("hello")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Contains(log.messages(), "tools not served to the CLI model; the turn runs without them") {
		t.Fatalf("log = %v", log.messages())
	}
}

// A second Close waits for the first, or for its own context.
func TestAgent_SecondCloseWaits(t *testing.T) {
	f := newAgentFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.p.Script("w1", heldStep(started, release))
	a, err := New(Spec{ID: "alice", Models: f.cat.Default()})
	if err != nil {
		t.Fatal(err)
	}
	r := a.Run(context.Background(), "s1", hook.UserText("hi"))
	waitFor(t, started, "the call")
	first := make(chan error, 1)
	go func() { first <- a.Close(context.Background()) }()
	for closed := false; !closed; {
		runtime.Gosched()
		a.mu.Lock()
		closed = a.closed
		a.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a second Close with an ended context = %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if res := r.Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("the running turn = %+v", res)
	}
}

// The engine's panic error says where it happened.
func TestEnginePanicError_Text(t *testing.T) {
	if got := (&EnginePanicError{Value: "x", SetUp: true}).Error(); got != "turn set-up: panic: x" {
		t.Fatalf("set-up panic text = %q", got)
	}
	if got := (&EnginePanicError{Value: "x"}).Error(); got != "turn engine: panic: x" {
		t.Fatalf("engine panic text = %q", got)
	}
	if !errors.Is(ErrNoSessionContext, errEngineIncomplete) || ErrNoSessionContext.Error() != "turn engine: the turn has no session context" {
		t.Fatalf("ErrNoSessionContext = %v", ErrNoSessionContext)
	}
}

// Ended tasks are kept up to keptEnded; beyond that the oldest is forgotten.
func TestTaskRegistry_ForgetsOldEnded(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, nil)
	ctx := hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s1", TurnID: "t1"})
	for range keptEnded + 1 {
		_, h, err := a.tasks.StartTask(ctx, hook.TaskStart{})
		if err != nil {
			t.Fatal(err)
		}
		h.Finish(nil)
	}
	if _, err := a.CancelTask("task-1", nil); !errors.Is(err, hook.ErrUnknownTask) {
		t.Fatalf("CancelTask of the oldest ended task = %v, want ErrUnknownTask", err)
	}
	got, err := a.CancelTask("task-2", nil)
	if err != nil || len(got.AlreadyEnded) != 1 {
		t.Fatalf("CancelTask of a kept ended task = %+v, %v", got, err)
	}
	if _, _, err := a.tasks.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{ID: "task-2"}}); err == nil {
		t.Fatal("a duplicate task id was accepted")
	}
}

// A call's signature is the same whether its name and arguments come as
// fields or in Function; an empty batch has none.
func TestToolCallSignature(t *testing.T) {
	if got := toolCallSignature(nil); got != "" {
		t.Fatalf("empty batch = %q", got)
	}
	fields := toolCallSignature([]spawnllm.ToolCall{{Name: "lookup_order", Arguments: map[string]any{"order": "42"}}})
	function := toolCallSignature([]spawnllm.ToolCall{{Function: &spawnllm.FunctionCall{Name: "lookup_order", Arguments: `{"order":"42"}`}}})
	if fields != function || fields != `lookup_order:{"order":"42"}` {
		t.Fatalf("signatures %q and %q", fields, function)
	}
}

// Image notes follow the content after a blank line, or stand alone.
func TestWithImageNotes(t *testing.T) {
	tests := []struct {
		content string
		lines   []string
		want    string
	}{
		{"text", nil, "text"},
		{"", []string{"a", "b"}, "a\nb"},
		{"text", []string{"a"}, "text\n\na"},
	}
	for _, tt := range tests {
		if got := withImageNotes(tt.content, tt.lines); got != tt.want {
			t.Errorf("withImageNotes(%q, %v) = %q, want %q", tt.content, tt.lines, got, tt.want)
		}
	}
}

// The trailing skips are the cooldown skips after the last model called.
func TestTrailingSkips(t *testing.T) {
	attempts := []models.Attempt{{Name: "a", Skipped: true}, {Name: "b"}, {Name: "c", Skipped: true}, {Name: "d", Skipped: true}}
	got := trailingSkips(attempts)
	if len(got) != 2 || got[0].Name != "c" || got[1].Name != "d" {
		t.Fatalf("trailingSkips = %+v", got)
	}
	if got := trailingSkips(attempts[:2]); len(got) != 0 {
		t.Fatalf("trailingSkips after a call = %+v", got)
	}
}
