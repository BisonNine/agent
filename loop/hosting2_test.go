package loop

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
)

// lines records every log record: its message and attribute keys.
type lines struct {
	mu  sync.Mutex
	got []slog.Record
}

func (l *lines) Enabled(context.Context, slog.Level) bool { return true }
func (l *lines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, r.Clone())
	return nil
}
func (l *lines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *lines) WithGroup(string) slog.Handler      { return l }

func (l *lines) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.got))
	for _, r := range l.got {
		out = append(out, r.Message)
	}
	return out
}

// hasAttr reports whether any record carries key.
func (l *lines) hasAttr(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	found := false
	for _, r := range l.got {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key {
				found = true
			}
			return !found
		})
	}
	return found
}

// What the agent reports in TurnFinished it does not also log: a failed
// turn and a panic delivered to the run add no line of the agent's own.
func TestHosting_NoLineForWhatIsReported(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Fail(errors.New("boom")))
	log := &lines{}
	a := f.agent(t, func(s *Spec) { s.Logger = slog.New(log) })
	a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
	b := f.agent(t, func(s *Spec) {
		s.Logger = slog.New(log)
		s.Session = func(context.Context, hook.Scope) (Session, error) { panic("host set-up broke") }
	})
	var ep *EnginePanicError
	if res := b.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); !errors.As(res.Err, &ep) {
		t.Fatalf("result = %+v", res)
	}
	for _, m := range log.messages() {
		if m == "panic in turn" || m == "turn ended with an error" {
			t.Fatalf("the agent logged %q", m)
		}
	}
}

// A run without a turn id has none in its scope and its engine lines.
func TestHosting_WithoutTurnID(t *testing.T) {
	f := newAgentFixture(t)
	f.tool(t, "lookup", func(context.Context, map[string]any) (string, error) { return "x", nil })
	f.p.Script("w1", calls("lookup"), modelstest.Reply("done"))
	log := &lines{}
	var scopes []string
	var mu sync.Mutex
	a := f.agent(t, func(s *Spec) {
		s.Logger = slog.New(log)
		s.Plugins = []hook.Plugin{{OnEvent: func(_ context.Context, ev hook.Event) {
			mu.Lock()
			scopes = append(scopes, ev.Header().Scope.TurnID)
			mu.Unlock()
		}}}
	})
	a.Run(context.Background(), "s1", hook.UserText("go"), hook.WithoutTurnID(), hook.WithTurnID("ignored")).Wait()
	mu.Lock()
	for _, id := range scopes {
		if id != "" {
			t.Errorf("an event has turn id %q", id)
		}
	}
	mu.Unlock()
	if log.hasAttr("turn_id") {
		t.Fatal("an engine line carries turn_id")
	}
	// With an id, the tool lines carry it.
	f.p.Script("w1", calls("lookup"), modelstest.Reply("done"))
	a.Run(context.Background(), "s1", hook.UserText("go"), hook.WithTurnID("t-1")).Wait()
	if !log.hasAttr("turn_id") {
		t.Fatal("no engine line carries turn_id")
	}
}

// Memory recalls for the message as given to Run unless a BeforeTurn sets
// the recall text.
func TestHosting_RecallTextDefault(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Reply("hi"))
	mem := &recallMemory{}
	a := f.agent(t, func(s *Spec) {
		s.Memory = memoryFunc(func() hook.MemorySession { return mem })
		s.Plugins = []hook.Plugin{{BeforeTurn: func(_ context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
			turn.Message.Text += "\n[attached: photo.jpg]"
			return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
		}}}
	})
	a.Run(context.Background(), "s1", hook.UserText("hello")).Wait()
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.recall) == 0 || mem.recall[0] != "hello" {
		t.Fatalf("recalled for %q", mem.recall)
	}
}

// countingCM is a context manager over a real one that counts forced
// compactions.
type countingCM struct {
	ctxengine.ContextManager

	forced atomic.Int32
}

func (c *countingCM) ForceCompress(ctx context.Context) error {
	c.forced.Add(1)
	return c.ContextManager.ForceCompress(ctx)
}

// An overflow retry compacts with the manager Session.Compaction gives, and
// releases it when the turn ends; without one, with the session's.
func TestHosting_CompactionManager(t *testing.T) {
	f := newAgentFixture(t)
	overflow := modelstest.Fail(errors.New("context window exceeded"))
	f.p.Script("w1", overflow, modelstest.Reply("fits now"))
	store := session.NewMemory()
	turnCM := &countingCM{ContextManager: ctxengine.New("s1", store)}
	fresh := &countingCM{ContextManager: ctxengine.New("s1", store)}
	var asked, released atomic.Int32
	a := f.agent(t, func(s *Spec) {
		s.Sessions = store
		s.Session = func(context.Context, hook.Scope) (Session, error) {
			return Session{Context: turnCM, Compaction: func() (ctxengine.ContextManager, func()) {
				asked.Add(1)
				return fresh, func() { released.Add(1) }
			}}, nil
		}
	})
	if res := a.Run(context.Background(), "s1", hook.UserText("hello")).Wait(); res.Reply != "fits now" {
		t.Fatalf("result = %+v", res)
	}
	if asked.Load() != 1 || released.Load() != 1 || fresh.forced.Load() != 1 || turnCM.forced.Load() != 0 {
		t.Fatalf("asked %d, released %d, fresh forced %d, turn's forced %d",
			asked.Load(), released.Load(), fresh.forced.Load(), turnCM.forced.Load())
	}
}

// A turn BeforeTurn answers needs no session context; one that is not
// answered fails with ErrNoSessionContext.
func TestHosting_NilSessionContext(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Reply("unreached"))
	answer := true
	a := f.agent(t, func(s *Spec) {
		s.Session = func(context.Context, hook.Scope) (Session, error) { return Session{}, nil }
		s.Plugins = []hook.Plugin{{BeforeTurn: func(context.Context, *hook.Turn) (*hook.TurnResult, error) {
			if answer {
				return &hook.TurnResult{Outcome: hook.OutcomeOK, Reply: "Bob answers himself."}, nil
			}
			return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
		}}}
	})
	if res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); !res.Skipped || res.Reply != "Bob answers himself." {
		t.Fatalf("answered turn = %+v", res)
	}
	answer = false
	if res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); !errors.Is(res.Err, ErrNoSessionContext) {
		t.Fatalf("unanswered turn = %+v", res)
	}
	if n := len(f.p.Calls()); n != 0 {
		t.Fatalf("%d model calls", n)
	}
}

// NoIterations makes no model request: the turn ends empty.
func TestHosting_NoIterations(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, func(s *Spec) { s.Limits = hook.Limits{MaxIterations: hook.NoIterations} })
	res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
	if res.Outcome != hook.OutcomeEmpty || res.FinishReason != "max_iterations" || res.Iterations != 0 || len(f.p.Calls()) != 0 {
		t.Fatalf("result = %+v, %d calls", res, len(f.p.Calls()))
	}
}

// Unqueued: two runs of one session overlap, and Interrupt stops both.
func TestHosting_Unqueued(t *testing.T) {
	f := newAgentFixture(t)
	hold := make(chan struct{})
	defer close(hold)
	started := make(chan struct{}, 2)
	f.p.Script("w1", modelstest.Step{Hold: hold, Response: &spawnllm.LLMResponse{Content: "late"}})
	a := f.agent(t, func(s *Spec) {
		s.Unqueued = true
		s.Plugins = []hook.Plugin{{BeforeLoop: func(context.Context, *hook.Loop) error { started <- struct{}{}; return nil }}}
	})
	r1 := a.Run(context.Background(), "s1", hook.UserText("one"))
	r2 := a.Run(context.Background(), "s1", hook.UserText("two"))
	<-started
	<-started
	if running, dropped := a.Interrupt("s1"); !running || dropped != 0 {
		t.Fatalf("Interrupt = %v, %d", running, dropped)
	}
	for _, r := range []hook.Run{r1, r2} {
		if res := r.Wait(); res.Outcome != hook.OutcomeCancelled {
			t.Fatalf("result = %+v", res)
		}
	}
}

// Unqueued runs end without forgetting a newer run of the session, and
// ForgetSession never closes the manager under a run's deferred compaction.
func TestHosting_UnqueuedForget(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Reply("ok"))
	a := f.agent(t, func(s *Spec) { s.Unqueued = true })
	for range 20 {
		var wg sync.WaitGroup
		for range 3 {
			wg.Go(func() { a.Run(context.Background(), "s1", hook.UserText("hi")).Wait() })
		}
		wg.Go(func() { a.ForgetSession("s1") })
		wg.Wait()
	}
	hold := make(chan struct{})
	started := make(chan struct{})
	f.p.Script("w1", modelstest.Step{Hold: hold, Started: started, Response: &spawnllm.LLMResponse{Content: "late"}})
	r := a.Run(context.Background(), "s1", hook.UserText("held"))
	<-started
	if running, _ := a.Interrupt("s1"); !running {
		t.Fatal("Interrupt did not find the running turn")
	}
	close(hold)
	if res := r.Wait(); res.Outcome != hook.OutcomeCancelled {
		t.Fatalf("result = %+v", res)
	}
}
