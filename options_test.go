package agent_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.bison9.com/agent"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

type allowOnly string

func (a allowOnly) IsToolAllowed(name string) bool { return name == string(a) }

type memory struct{}

func (*memory) Open(context.Context, hook.Scope) (hook.MemorySession, error) {
	return nil, errors.New("no memory")
}

type served struct{}

func (*served) ServeTurn(context.Context, hook.Scope) (string, func(), error) {
	return "", func() {}, nil
}

// spec is the loop.Spec opts build.
func spec(opts ...agent.Option) loop.Spec {
	var s loop.Spec
	for _, o := range opts {
		o(&s)
	}
	return s
}

// Each setting option puts its value in its field of the agent's spec.
func TestOptions_SetTheirField(t *testing.T) {
	cat := catalog(t, modelstest.New())
	reg := toolkit.NewRegistry()
	sessions := session.NewMemory()
	mem := &memory{}
	st := &served{}
	clk := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	logger := slog.New(slog.DiscardHandler)
	limits := agent.Limits{Retries: 2, CancelGrace: time.Second}

	s := spec(
		agent.WithName("Alice"),
		agent.WithDescription("Answers Bob's questions."),
		agent.WithInstruction("You are Alice."),
		agent.WithInstructionLayers(func(context.Context) []ctxengine.Layer {
			return []ctxengine.Layer{{Name: "static", Text: "Be brief."}}
		}),
		agent.WithModels(cat),
		agent.WithTools(reg),
		agent.WithAllow(allowOnly("lookup_order")),
		agent.WithSessions(sessions),
		agent.WithMemory(mem),
		agent.WithServedTools(st),
		agent.WithUnqueued(),
		agent.WithLimits(limits),
		agent.WithProject("support"),
		agent.WithClock(clk),
		agent.WithLogger(logger),
		agent.WithLogContent(true),
		agent.WithRedactor(func(s string) string { return "[" + s + "]" }),
	)
	if s.Name != "Alice" || s.Description != "Answers Bob's questions." || s.Instruction != "You are Alice." {
		t.Fatalf("Name, Description, Instruction = %q, %q, %q", s.Name, s.Description, s.Instruction)
	}
	if s.InstructionLayers == nil || s.InstructionLayers(context.Background())[0].Text != "Be brief." {
		t.Fatal("WithInstructionLayers did not set the layers")
	}
	if s.Models != cat.Default() {
		t.Fatal("WithModels(catalogue) did not set its default set")
	}
	if s.Tools != toolkit.Registry(reg) || s.Sessions != sessions {
		t.Fatal("WithTools or WithSessions did not set its value")
	}
	if s.Allow == nil || !s.Allow.IsToolAllowed("lookup_order") || s.Allow.IsToolAllowed("refund") {
		t.Fatal("WithAllow did not set the checker")
	}
	if s.Memory != hook.Memory(mem) || s.Served != hook.ServedTools(st) {
		t.Fatal("WithMemory or WithServedTools did not set its value")
	}
	if !s.Unqueued || s.Limits != limits || s.Project != "support" {
		t.Fatalf("Unqueued, Limits, Project = %v, %+v, %q", s.Unqueued, s.Limits, s.Project)
	}
	if s.Clock != clock.Clock(clk) || s.Logger != logger {
		t.Fatal("WithClock or WithLogger did not set its value")
	}
	if s.LogContent == nil || !s.LogContent() {
		t.Fatal("WithLogContent(true) does not report true")
	}
	if s.Redactor == nil || s.Redactor("key") != "[key]" {
		t.Fatal("WithRedactor did not set the redactor")
	}
	if off := spec(agent.WithLogContent(false)); off.LogContent == nil || off.LogContent() {
		t.Fatal("WithLogContent(false) does not report false")
	}
}

// WithModels takes a set as it is, and leaves the models alone when given a
// nil catalogue or set; WithContext adds to the options already given.
func TestOptions_ModelsAndContext(t *testing.T) {
	cat := catalog(t, modelstest.New())
	if s := spec(agent.WithModels(cat.Default())); s.Models != cat.Default() {
		t.Fatal("WithModels(set) did not set the set")
	}
	if s := spec(agent.WithModels(cat), agent.WithModels((*models.Catalog)(nil))); s.Models != cat.Default() {
		t.Fatal("WithModels(nil catalogue) replaced the models")
	}
	if s := spec(agent.WithModels(cat), agent.WithModels((*models.Set)(nil))); s.Models != cat.Default() {
		t.Fatal("WithModels(nil set) replaced the models")
	}
	s := spec(
		agent.WithContext(ctxengine.WithMinPercent(10)),
		agent.WithContext(ctxengine.WithNormalPercent(50), ctxengine.WithSafetyPercent(90)),
	)
	if len(s.Context) != 3 {
		t.Fatalf("WithContext kept %d options, want 3", len(s.Context))
	}
}

// Each callback option registers a plugin holding that callback alone, in
// the order given.
func TestOptions_Callbacks(t *testing.T) {
	var got []string
	mark := func(name string) { got = append(got, name) }
	ctx := context.Background()
	s := spec(
		agent.WithBeforeTurn(func(context.Context, *agent.Turn) (*agent.TurnResult, error) {
			mark("BeforeTurn")
			return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
		}),
		agent.WithAfterTurn(func(context.Context, *agent.Turn, *agent.TurnResult) { mark("AfterTurn") }),
		agent.WithBeforeModel(func(context.Context, *agent.ModelInfo) (*spawnllm.LLMResponse, error) {
			mark("BeforeModel")
			return nil, nil //nolint:nilnil // (nil, nil): call the model
		}),
		agent.WithAfterModel(func(context.Context, *agent.ModelInfo, *agent.ModelResponse) { mark("AfterModel") }),
		agent.WithOnModelError(func(context.Context, *agent.ModelInfo, error) (*spawnllm.LLMResponse, error) {
			mark("OnModelError")
			return nil, nil //nolint:nilnil // (nil, nil): the error stands
		}),
		agent.WithBeforeTool(func(context.Context, *agent.ToolCall) (*agent.ToolResult, error) {
			mark("BeforeTool")
			return nil, nil //nolint:nilnil // (nil, nil): run the tool
		}),
		agent.WithAfterTool(func(context.Context, *agent.ToolCall, *agent.ToolResult) { mark("AfterTool") }),
		agent.WithOnToolError(func(_ context.Context, _ *agent.ToolCall, r *agent.ToolResult) *agent.ToolResult {
			mark("OnToolError")
			return r
		}),
		agent.WithOnFallback(func(context.Context, agent.Fallback) { mark("OnFallback") }),
		agent.WithOnCompaction(func(context.Context, agent.Compaction) { mark("OnCompaction") }),
		agent.WithOnTextDelta(func(context.Context, agent.TextDelta) { mark("OnTextDelta") }),
		agent.WithOnInterrupt(func(context.Context, agent.Interrupted) { mark("OnInterrupt") }),
		agent.WithOnEvent(func(context.Context, agent.Event) { mark("OnEvent") }),
	)
	want := []string{
		"BeforeTurn", "AfterTurn", "BeforeModel", "AfterModel", "OnModelError", "BeforeTool",
		"AfterTool", "OnToolError", "OnFallback", "OnCompaction", "OnTextDelta", "OnInterrupt", "OnEvent",
	}
	if len(s.Plugins) != len(want) {
		t.Fatalf("%d plugins, want %d", len(s.Plugins), len(want))
	}
	for i, p := range s.Plugins {
		set := 0
		for _, ok := range []bool{
			p.BeforeTurn != nil, p.AfterTurn != nil, p.BeforeModel != nil, p.AfterModel != nil,
			p.OnModelError != nil, p.BeforeTool != nil, p.AfterTool != nil, p.OnToolError != nil, p.OnFallback != nil,
			p.OnCompaction != nil, p.OnTextDelta != nil, p.OnInterrupt != nil, p.OnEvent != nil,
		} {
			if ok {
				set++
			}
		}
		if set != 1 {
			t.Fatalf("plugin %d (%s) holds %d callbacks, want 1", i, want[i], set)
		}
		switch {
		case p.BeforeTurn != nil:
			if _, err := p.BeforeTurn(ctx, nil); err != nil {
				t.Fatal(err)
			}
		case p.AfterTurn != nil:
			p.AfterTurn(ctx, nil, nil)
		case p.BeforeModel != nil:
			if _, err := p.BeforeModel(ctx, nil); err != nil {
				t.Fatal(err)
			}
		case p.AfterModel != nil:
			p.AfterModel(ctx, nil, nil)
		case p.OnModelError != nil:
			if _, err := p.OnModelError(ctx, nil, nil); err != nil {
				t.Fatal(err)
			}
		case p.BeforeTool != nil:
			if _, err := p.BeforeTool(ctx, nil); err != nil {
				t.Fatal(err)
			}
		case p.AfterTool != nil:
			p.AfterTool(ctx, nil, nil)
		case p.OnToolError != nil:
			_ = p.OnToolError(ctx, nil, nil)
		case p.OnFallback != nil:
			p.OnFallback(ctx, agent.Fallback{})
		case p.OnCompaction != nil:
			p.OnCompaction(ctx, agent.Compaction{})
		case p.OnTextDelta != nil:
			p.OnTextDelta(ctx, agent.TextDelta{})
		case p.OnInterrupt != nil:
			p.OnInterrupt(ctx, agent.Interrupted{})
		case p.OnEvent != nil:
			p.OnEvent(ctx, nil)
		}
		if len(got) != i+1 || got[i] != want[i] {
			t.Fatalf("plugin %d ran %v, want %s", i, got, want[i])
		}
	}
}

// The run options set their field of the run's options.
func TestRunOptions(t *testing.T) {
	o := hook.ApplyRunOptions(
		agent.AlreadyStored(),
		agent.ResetSession(),
		agent.TurnBudget(time.Minute),
		agent.WithTurnID("turn-1"),
		agent.WithoutTurnID(),
		agent.WithoutEvents(),
		agent.AtDepth(2),
	)
	want := agent.RunOptions{
		AlreadyStored: true, ResetSession: true, TurnBudget: time.Minute,
		TurnID: "turn-1", NoTurnID: true, NoEvents: true, Depth: 2,
	}
	if o != want {
		t.Fatalf("run options = %+v, want %+v", o, want)
	}
}

// Refusal marks an error IsRefusal recognises, through wrapping; other
// errors are not refusals.
func TestRefusal(t *testing.T) {
	err := agent.Refusal(errors.New("refunds need approval"))
	if !agent.IsRefusal(err) || !agent.IsRefusal(errors.Join(errors.New("tool refund"), err)) {
		t.Fatal("a refusal is not recognised")
	}
	if agent.IsRefusal(errors.New("database down")) || agent.IsRefusal(nil) {
		t.Fatal("a plain error is taken for a refusal")
	}
}

// New skips a nil option.
func TestNew_SkipsNilOption(t *testing.T) {
	cat := catalog(t, modelstest.New())
	a, err := agent.New("alice", nil, agent.WithModels(cat), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
