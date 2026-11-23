package loop

// Fakes for the turn engine's own tests (engine_*_test.go). Like the engine
// files, they use only the standard library, the SDK and engine identifiers.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// fakeContext is an in-memory context manager that records what the engine
// does with it, in order.
type fakeContext struct {
	mu       sync.Mutex
	stored   []spawnllm.Message
	log      []string // "add:<kind>", "assemble", "force", "observe"
	observed [][2]int
	// estimate is every Assembly's prompt-token estimate.
	estimate int
	// evictOnce makes the next Assemble of the model loop (not the turn's
	// first request) report one eviction.
	evictOnce bool
	assembles int
	// failAdd makes every add fail with this error.
	failAdd error
	// failAfterForce makes every Assemble after a forced compression fail.
	failAfterForce bool
	// assembled is the history every Assembly returns.
	assembled []spawnllm.Message
}

func (f *fakeContext) add(kind string, msg spawnllm.Message) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "add:"+kind)
	if f.failAdd != nil {
		return 0, f.failAdd
	}
	f.stored = append(f.stored, msg)
	return int64(len(f.stored)), nil
}

func (f *fakeContext) AddUserMessage(_ context.Context, m spawnllm.Message) (int64, error) {
	return f.add("user", m)
}

func (f *fakeContext) AddAssistantMessage(_ context.Context, m spawnllm.Message) (int64, error) {
	return f.add("assistant", m)
}

func (f *fakeContext) AddToolCallMessage(_ context.Context, m spawnllm.Message) (int64, error) {
	return f.add("toolcall", m)
}

func (f *fakeContext) AddToolResult(_ context.Context, m spawnllm.Message) (int64, error) {
	return f.add("toolresult", m)
}

func (f *fakeContext) Assemble(context.Context, ctxengine.AssembleRequest) (ctxengine.Assembly, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "assemble")
	f.assembles++
	if f.failAfterForce && slices.Contains(f.log, "force") {
		return ctxengine.Assembly{}, errors.New("assembly failed")
	}
	a := ctxengine.Assembly{PromptTokenEstimate: f.estimate, Messages: slices.Clone(f.assembled)}
	if f.evictOnce && f.assembles > 1 {
		f.evictOnce = false
		a.Evictions = []ctxengine.EvictionEvent{{Tool: "file_read_lines", Resource: "notes.md", Bytes: 2048, Reason: "superseded"}}
		a.Messages = slices.Clone(f.stored)
	}
	return a, nil
}

func (f *fakeContext) ObserveUsage(est, actual int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "observe")
	f.observed = append(f.observed, [2]int{est, actual})
}

func (f *fakeContext) SetContextWindow(int) error                        { return nil }
func (f *fakeContext) Compact(context.Context) error                     { return nil }
func (f *fakeContext) CompactIfDue(context.Context) (bool, error)        { return false, nil }
func (f *fakeContext) LastCompactionReport() *ctxengine.CompactionReport { return nil }
func (f *fakeContext) RenderedSummary() string                           { return "" }
func (f *fakeContext) Stats() ctxengine.ContextStats                     { return ctxengine.ContextStats{} }

func (f *fakeContext) Reset(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "reset")
	return nil
}
func (f *fakeContext) Close(context.Context) error { return nil }

func (f *fakeContext) ForceCompress(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "force")
	return nil
}

func (f *fakeContext) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// fakeEnv is the host's services as the engine tests give them.
type fakeEnv struct {
	cm    *fakeContext
	defs  []spawnllm.ToolDefinition
	tools map[string]func(ctx context.Context, args map[string]any) *toolkit.Result
}

func (e *fakeEnv) toolDefs() []spawnllm.ToolDefinition { return e.defs }

func (e *fakeEnv) executeTool(ctx context.Context, call hook.ToolCall) *toolkit.Result {
	fn, ok := e.tools[call.Name]
	if !ok {
		return toolkit.ErrorResult("no tool " + call.Name)
	}
	return fn(ctx, call.Arguments)
}

func (e *fakeEnv) assembleRequest(context.Context, []spawnllm.ToolDefinition, []spawnllm.Message) ctxengine.AssembleRequest {
	return ctxengine.AssembleRequest{}
}

func (e *fakeEnv) compressionManager() (ctxengine.ContextManager, func()) { return e.cm, func() {} }

func (e *fakeEnv) sessionSummary() string { return "" }
func (e *fakeEnv) firstRequest(context.Context) ctxengine.AssembleRequest {
	return ctxengine.AssembleRequest{}
}

// fakeStore is a session store's pending-turn marker and save, recording
// what the engine does with them.
type fakeStore struct {
	mu      sync.Mutex
	log     []string // "pending", "cleared", "save"
	saveErr error
}

func (s *fakeStore) record(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, what)
}

func (s *fakeStore) SetPendingTurn(string) error   { s.record("pending"); return nil }
func (s *fakeStore) ClearPendingTurn(string) error { s.record("cleared"); return nil }
func (s *fakeStore) Save(string) error             { s.record("save"); return s.saveErr }

func (s *fakeStore) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

// noMemory is a session without long-term memory.
type noMemory struct{}

func (noMemory) Observe(context.Context, int64, string, string) {}
func (noMemory) RecordToolUse(...string)                        {}

// recorder is an OnEvent consumer that keeps every event, in order.
type recorder struct {
	mu     sync.Mutex
	events []hook.Event
}

func (r *recorder) on(_ context.Context, ev hook.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) all() []hook.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// kinds names the events recorded, in order.
func (r *recorder) kinds() []string {
	var out []string
	for _, ev := range r.all() {
		switch ev.(type) {
		case hook.TurnStarted:
			out = append(out, "TurnStarted")
		case hook.ModelCall:
			out = append(out, "ModelCall")
		case hook.TextDelta:
			out = append(out, "TextDelta")
		case hook.ToolCallStarted:
			out = append(out, "ToolCallStarted")
		case hook.ToolCallFinished:
			out = append(out, "ToolCallFinished")
		case hook.Fallback:
			out = append(out, "Fallback")
		case hook.Compaction:
			out = append(out, "Compaction")
		case hook.Image:
			out = append(out, "Image")
		case hook.TurnFinished:
			out = append(out, "TurnFinished")
		default:
			out = append(out, "other")
		}
	}
	return out
}

// engineModel is one model of a test catalogue: its name, its wire id and
// the scripted client that answers it.
type engineModel struct {
	name, wire string
	client     spawnllm.LLMProvider
	// maxTokens is the model's max_tokens; 0 means 4096.
	maxTokens int
}

// testChain is a catalogue holding models, in priority order, under one
// provider, on clk, and a chain begun on it for a turn.
func testChain(t *testing.T, clk clock.Clock, ms ...engineModel) (*models.Catalog, *models.Chain) {
	t.Helper()
	cat := models.New(models.WithClock(clk))
	if err := cat.AddProvider(modelspec.Provider{Name: "Alice's API", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		maxTokens := m.maxTokens
		if maxTokens == 0 {
			maxTokens = 4096
		}
		if err := cat.Add(modelspec.Model{ModelName: m.name, Model: m.wire, Provider: "Alice's API", MaxTokens: maxTokens}, models.For(models.Chat, i+1)); err != nil {
			t.Fatal(err)
		}
		if err := cat.RegisterClient(m.name, m.client); err != nil {
			t.Fatal(err)
		}
	}
	return cat, cat.Begin(models.Chat)
}

// engineFixture is a turn of the engine for Alice on session s, with a
// recorder of every event.
type engineFixture struct {
	spec  engineSpec
	env   *fakeEnv
	cm    *fakeContext
	store *fakeStore
	rec   *recorder
	clk   *clock.Fake
}

func newEngineFixture(t *testing.T, chain *models.Chain, clk *clock.Fake) *engineFixture {
	t.Helper()
	cm := &fakeContext{assembled: []spawnllm.Message{{Role: "user", Content: "hello"}}}
	store := &fakeStore{}
	env := &fakeEnv{cm: cm, tools: map[string]func(context.Context, map[string]any) *toolkit.Result{}}
	rec := &recorder{}
	hooks := &turnHooks{}
	hooks.add(hook.Plugin{Name: "recorder", OnEvent: rec.on})
	spec := engineSpec{
		agentID: "alice",
		scope:   hook.Scope{AgentID: "alice", AgentName: "Alice", Session: "s", TurnID: "t1"},
		message: hook.UserText("hello"),
		// The message is in the session's history (cm.assembled): the
		// engine tests drive the turn from its first request.
		options:       hook.RunOptions{AlreadyStored: true},
		store:         store,
		maxIterations: 6,
		retries:       llmMaxRetries,
		retryBackoff:  time.Second,
		clock:         clk,
		cm:            cm,
		mem:           noMemory{},
		env:           env,
		hooks:         hooks,
		chain:         chain,
		contextWindow: 128000,
	}
	if chain != nil {
		spec.candidates = chain.Candidates()
		if len(spec.candidates) > 0 {
			spec.model = spec.candidates[0].Name
		}
	}
	spec.modelMaxTokens = 4096
	return &engineFixture{spec: spec, env: env, cm: cm, store: store, rec: rec, clk: clk}
}

// tool declares a tool and gives it fn.
func (f *engineFixture) tool(name string, fn func(ctx context.Context, args map[string]any) *toolkit.Result) {
	f.env.defs = append(f.env.defs, spawnllm.ToolDefinition{Type: "function", Function: spawnllm.ToolFunctionDefinition{Name: name}})
	f.env.tools[name] = fn
}

// run runs the turn over the user's message.
func (f *engineFixture) run(t *testing.T) (*engineTurn, engineResult, error) {
	t.Helper()
	turn := newEngineTurn(f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
	res, err := turn.run(context.Background())
	return turn, res, err
}

// callStep is a scripted answer with tool calls.
func callStep(calls ...spawnllm.ToolCall) modelstest.Step {
	return modelstest.Step{Response: &spawnllm.LLMResponse{ToolCalls: calls, FinishReason: "tool_calls"}}
}

// toolCall is a call of name with args.
func toolCall(id, name string, args map[string]any) spawnllm.ToolCall {
	return spawnllm.ToolCall{ID: id, Name: name, Arguments: args}
}

var engineIdentity = accounting.Identity{Agent: "alice", Name: "Alice"}
