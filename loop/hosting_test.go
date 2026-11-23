package loop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

// recallMemory is a session memory that records what it recalls for.
type recallMemory struct {
	mu     sync.Mutex
	recall []string
	closed int
}

func (m *recallMemory) Recall(_ context.Context, text string) []hook.Recalled {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recall = append(m.recall, text)
	return nil
}
func (m *recallMemory) Observe(context.Context, int64, string, string) {}
func (m *recallMemory) ToolsUsed(context.Context, ...string)           {}
func (m *recallMemory) Close() error                                   { m.closed++; return nil }

// The host supplies the session context: the agent uses its manager and
// memory, amends every request through it, releases it once per turn, never
// closes it, keeps no session state, and recalls for the message as
// BeforeTurn left it.
func TestHosting_SessionContext(t *testing.T) {
	f := newAgentFixture(t)
	f.p.Script("w1", modelstest.Reply("hi"))
	store := session.NewMemory()
	cm := ctxengine.New("s1", store)
	mem := &recallMemory{}
	var released, amended atomic.Int32
	a := f.agent(t, func(s *Spec) {
		s.Sessions = store
		s.Session = func(_ context.Context, sc hook.Scope) (Session, error) {
			if sc.Session != "s1" {
				t.Errorf("session scope = %+v", sc)
			}
			return Session{
				Context: cm, Memory: mem, Release: func() { released.Add(1) },
				Assemble: func(_ context.Context, req *ctxengine.AssembleRequest) {
					amended.Add(1)
					req.Channel, req.ChatID = "web", "bob"
				},
			}, nil
		}
		s.Plugins = []hook.Plugin{{BeforeTurn: func(_ context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
			turn.Message.Text = "[note from Bob] " + turn.Message.Text
			return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
		}}}
	})
	if res := a.Run(context.Background(), "s1", hook.UserText("hello")).Wait(); res.Outcome != hook.OutcomeOK {
		t.Fatalf("result = %+v", res)
	}
	// Release follows TurnFinished, before the session's next turn.
	for i := 0; released.Load() == 0 && i < 5000; i++ {
		time.Sleep(time.Millisecond)
	}
	if released.Load() != 1 || amended.Load() < 2 || mem.closed != 0 {
		t.Fatalf("released %d, amended %d, memory closed %d", released.Load(), amended.Load(), mem.closed)
	}
	if len(mem.recall) == 0 || mem.recall[0] != "[note from Bob] hello" {
		t.Fatalf("recalled for %q", mem.recall)
	}
	if h := store.GetHistory("s1"); len(h) != 2 || h[0].Content != "[note from Bob] hello" {
		t.Fatalf("history = %+v", h)
	}
	kept := 1
	for i := 0; kept != 0 && i < 5000; i++ {
		a.mu.Lock()
		kept = len(a.states)
		a.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if kept != 0 {
		t.Fatalf("the agent kept %d sessions of its own", kept)
	}
	if !a.ForgetSession("s1") {
		t.Fatal("ForgetSession of an idle session")
	}
}

// Without a host session the agent keeps its own until ForgetSession, which
// refuses a busy session.
func TestHosting_ForgetSession(t *testing.T) {
	f := newAgentFixture(t)
	hold, started := make(chan struct{}), make(chan struct{})
	f.p.Script("w1", modelstest.Reply("one"), modelstest.Step{Hold: hold, Started: started, Response: &spawnllm.LLMResponse{Content: "two"}})
	a := f.agent(t, nil)
	a.Run(context.Background(), "s1", hook.UserText("one")).Wait()
	r := a.Run(context.Background(), "s1", hook.UserText("two"))
	<-started
	if a.ForgetSession("s1") {
		t.Fatal("ForgetSession dropped a busy session")
	}
	close(hold)
	r.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for !a.ForgetSession("s1") {
		if time.Now().After(deadline) {
			t.Fatal("the session stayed busy")
		}
		time.Sleep(time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.states) != 0 {
		t.Fatalf("states = %d", len(a.states))
	}
}

// The host's turn id, identity, chain and limits are the turn's; callbacks
// see the chosen model.
func TestHosting_TurnIdentityAndModels(t *testing.T) {
	f := newAgentFixture(t)
	f.tool(t, "lookup", func(context.Context, map[string]any) (string, error) { return "x", nil })
	f.p.Script("w1", modelstest.Reply("hi"))
	var turnModel hook.ModelRef
	var started hook.TurnStarted
	var chains atomic.Int32
	a := f.agent(t, func(s *Spec) {
		s.ID = "Alice-clone-1"
		s.Identity = accounting.Identity{Agent: "alice", Name: "Alice's clone"}
		s.Models = nil
		s.TurnModels = func(context.Context, hook.Scope) TurnModels {
			chains.Add(1)
			return TurnModels{Chain: f.cat.Default().Begin(models.Chat), Limits: &modelspec.Limits{NoTools: true, MaxTokens: 77}}
		}
		s.Plugins = []hook.Plugin{{
			BeforeTurn: func(_ context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
				turnModel = turn.Model
				return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
			},
			OnEvent: func(_ context.Context, ev hook.Event) {
				if e, ok := ev.(hook.TurnStarted); ok {
					started = e
				}
			},
		}}
	})
	res := a.Run(context.Background(), "s1", hook.UserText("hello"), hook.WithTurnID("turn-42")).Wait()
	if res.Outcome != hook.OutcomeOK || chains.Load() != 1 {
		t.Fatalf("result %+v, chains %d", res, chains.Load())
	}
	if a.ID() != "Alice-clone-1" || started.Scope.TurnID != "turn-42" || started.Scope.AgentID != "alice" || started.Scope.AgentName != "Alice's clone" {
		t.Fatalf("ID %q, scope %+v", a.ID(), started.Scope)
	}
	if turnModel.Name != "m1" || turnModel.Model != "w1" {
		t.Fatalf("Turn.Model = %+v", turnModel)
	}
	call := f.p.Calls()[0]
	if call.Identity.Agent != "alice" || call.Identity.Name != "Alice's clone" || len(call.Tools) != 0 {
		t.Fatalf("model call: identity %+v, %d tools", call.Identity, len(call.Tools))
	}
}

// An agent with no chat model is built when the host chooses the models; a
// turn BeforeTurn answers needs none, and one that reaches the model fails
// with an error matching hook.ErrNoModelAvailable.
func TestHosting_NoChatModel(t *testing.T) {
	empty := models.New()
	answer := true
	a, err := New(Spec{
		ID: "bob", TurnModels: func(context.Context, hook.Scope) TurnModels { return TurnModels{} },
		Plugins: []hook.Plugin{{BeforeTurn: func(context.Context, *hook.Turn) (*hook.TurnResult, error) {
			if answer {
				return &hook.TurnResult{Outcome: hook.OutcomeOK, Reply: "Bob will answer himself."}, nil
			}
			return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) }) //nolint:errcheck // nothing to report
	if res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); res.Reply != "Bob will answer himself." {
		t.Fatalf("answered turn = %+v", res)
	}
	answer = false
	res := a.Run(context.Background(), "s1", hook.UserText("hi")).Wait()
	if res.Outcome != hook.OutcomeError || !errors.Is(res.Err, hook.ErrNoModelAvailable) {
		t.Fatalf("turn without a model = %+v", res)
	}
	if _, err := New(Spec{ID: "bob", Models: empty.Default()}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("without TurnModels New = %v", err)
	}
}

type ctxKey struct{}

// The host decorates each tool call's context; the gate sees the name the
// model called; a background tool's late result reaches the async sink.
func TestHosting_ToolContextGateAndAsync(t *testing.T) {
	f := newAgentFixture(t)
	var seen atomic.Value
	f.tool(t, "lookup", func(ctx context.Context, _ map[string]any) (string, error) {
		seen.Store(ctx.Value(ctxKey{}))
		return "x", nil
	})
	if err := f.reg.Add(toolspec.ToolDefinition{
		Name: "research", Description: "slow", Async: true,
		Handler: func(c *toolspec.ToolCall) (*toolspec.Result, error) {
			notify := c.Notify
			go notify(&toolspec.Result{ForLLM: "late result"})
			return &toolspec.Result{ForLLM: "started", Async: true}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.p.Script("w1", calls("lookup", "research"), modelstest.Reply("done"))
	var gated []string
	var gmu sync.Mutex
	late := make(chan string, 1)
	a := f.agent(t, func(s *Spec) {
		s.ToolContext = func(ctx context.Context, _ hook.ToolCall) context.Context {
			return context.WithValue(ctx, ctxKey{}, "bob's chat")
		}
		s.Gate = func(ctx context.Context, _ toolkit.Info) *toolkit.Result {
			c, _ := hook.ToolCallFrom(ctx)
			gmu.Lock()
			gated = append(gated, c.Name)
			gmu.Unlock()
			return nil
		}
		s.AsyncResult = func(_ context.Context, call hook.ToolCall, _ toolkit.Info, r *toolkit.Result) {
			late <- call.Name + ": " + r.ForLLM
		}
	})
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	if seen.Load() != "bob's chat" {
		t.Fatalf("tool context value = %v", seen.Load())
	}
	gmu.Lock()
	if len(gated) != 2 {
		t.Fatalf("gate saw %v", gated)
	}
	gmu.Unlock()
	select {
	case got := <-late:
		if got != "research: late result" {
			t.Fatalf("late result = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no late result")
	}
}

// The host's limits are read per turn, the tool timeout per batch; no turn
// budget means the turn's context has no deadline of the agent's.
func TestHosting_TurnLimits(t *testing.T) {
	f := newAgentFixture(t)
	f.tool(t, "lookup", func(context.Context, map[string]any) (string, error) { return "x", nil })
	f.p.Script("w1", calls("lookup"), calls("lookup"), modelstest.Reply("done"))
	var asked atomic.Int32
	var deadline atomic.Bool
	a := f.agent(t, func(s *Spec) {
		s.TurnLimits = func(context.Context, hook.Scope) hook.Limits {
			asked.Add(1)
			return hook.Limits{TurnBudget: hook.NoTurnBudget, ToolTimeout: time.Minute}
		}
		s.Plugins = []hook.Plugin{{BeforeTurn: func(ctx context.Context, _ *hook.Turn) (*hook.TurnResult, error) {
			_, has := ctx.Deadline()
			deadline.Store(has)
			return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
		}}}
	})
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	if deadline.Load() || asked.Load() < 3 { // the turn, then each of two batches
		t.Fatalf("deadline %v, limits asked %d", deadline.Load(), asked.Load())
	}
}

// A panic in the engine or in the host's set-up ends the run with one
// TurnFinished whose error is an *EnginePanicError, for the host to re-raise on
// its own goroutine.
func TestHosting_EnginePanicError(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, func(s *Spec) {
		s.Session = func(context.Context, hook.Scope) (Session, error) { panic("host set-up broke") }
	})
	r := a.Run(context.Background(), "s1", hook.UserText("hi"))
	n := 0
	for _, err := range r.Events() {
		if err == nil && n > 0 {
			t.Error("TurnFinished without its error")
		}
		n++
	}
	var ep *EnginePanicError
	if res := r.Wait(); n != 1 || !errors.As(res.Err, &ep) || ep.Value != "host set-up broke" || !ep.SetUp {
		t.Fatalf("%d events, result %+v", n, res)
	}

	started := make(chan struct{})
	close(started) // deliberate: the scripted call closes it again, so the client panics inside the engine
	f.p.Script("w1", modelstest.Step{Started: started, Response: &spawnllm.LLMResponse{Content: "x"}})
	b := f.agent(t, nil)
	if res := b.Run(context.Background(), "s1", hook.UserText("hi")).Wait(); !errors.As(res.Err, &ep) || ep.SetUp {
		t.Fatalf("engine panic result = %+v", res)
	}
}

// No empty instruction layer is added for an agent without an instruction.
func TestHosting_NoEmptyInstructionLayer(t *testing.T) {
	f := newAgentFixture(t)
	a := f.agent(t, func(s *Spec) {
		s.InstructionLayers = func(context.Context) []ctxengine.Layer { return []ctxengine.Layer{{Name: "host", Text: "x"}} }
	})
	env := &agentEnv{a: a}
	if got := env.layers(context.Background()); len(got) != 1 || got[0].Name != "host" {
		t.Fatalf("layers = %+v", got)
	}
}

// A session forgotten while a run arrives is closed once, and the run starts
// on a new one; after Close, ForgetSession closes nothing again.
func TestHosting_ForgetSessionAndClose(t *testing.T) {
	f := newAgentFixture(t)
	var mu sync.Mutex
	var mems []*recallMemory
	f.p.Script("w1", modelstest.Reply("hi"))
	spec := Spec{ID: "alice", Models: f.cat.Default(), Memory: memoryFunc(func() hook.MemorySession {
		mu.Lock()
		defer mu.Unlock()
		m := &recallMemory{}
		mems = append(mems, m)
		return m
	})}
	a, err := New(spec)
	if err != nil {
		t.Fatal(err)
	}
	a.Run(context.Background(), "s1", hook.UserText("one")).Wait()
	done := make(chan hook.TurnResult, 1)
	go func() {
		for !a.ForgetSession("s1") {
			time.Sleep(time.Millisecond)
		}
	}()
	go func() { done <- a.Run(context.Background(), "s1", hook.UserText("two")).Wait() }()
	if res := <-done; res.Outcome != hook.OutcomeOK {
		t.Fatalf("second run = %+v", res)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.ForgetSession("s1")
	mu.Lock()
	defer mu.Unlock()
	for i, m := range mems {
		if m.closed != 1 {
			t.Fatalf("memory %d closed %d times", i, m.closed)
		}
	}
}

// memoryFunc is a hook.Memory that opens what fn makes.
type memoryFunc func() hook.MemorySession

func (f memoryFunc) Open(context.Context, hook.Scope) (hook.MemorySession, error) { return f(), nil }
