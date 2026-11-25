/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

// The turn engine driven through its hooks with a scripted model and fake
// tools: each behaviour plan 4.7 keeps from llmTurn, and each guarantee the
// hook package documents for its events.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

func newFake() *clock.Fake { return clock.NewFake(time.Unix(1_800_000_000, 0)) }

// The turn's model is chosen once: every iteration goes to the model the
// chain pinned, even when the selection changes mid-turn.
func TestEngine_ModelStickyForTheTurn(t *testing.T) {
	p := modelstest.New().
		Script("alice-wire", callStep(toolCall("c1", "pick_bob", nil)), modelstest.Reply("done"))
	clk := newFake()
	cat, chain := testChain(t, clk,
		engineModel{name: "Alice Model", wire: "alice-wire", client: p},
		engineModel{name: "Bob Model", wire: "bob-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.tool("pick_bob", func(context.Context, map[string]any) *toolkit.Result {
		if err := cat.Select(models.Chat, "Bob Model"); err != nil {
			return toolkit.ErrorResult(err.Error())
		}
		return toolkit.SilentResult("ok")
	})
	_, res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Models(); !slices.Equal(got, []string{"alice-wire", "alice-wire"}) {
		t.Fatalf("models called = %v, want Alice's model for both steps", got)
	}
	if res.content != "done" {
		t.Fatalf("content = %q", res.content)
	}
	if got := cat.Begin(models.Chat).Candidates()[0].Name; got != "Bob Model" {
		t.Fatalf("the next turn starts on %q, want Bob Model", got)
	}
}

// A model that fails falls back to the next within the call (OnFallback and a
// Fallback event, after the failed call's ModelCall); on the next iteration
// the failed model, now parked, is skipped without a call.
func TestEngine_FallbackWithCooldownSkip(t *testing.T) {
	alice := modelstest.New().Script("alice-wire", modelstest.Status(429, 0))
	bob := modelstest.New().Script("bob-wire", callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk,
		engineModel{name: "Alice Model", wire: "alice-wire", client: alice},
		engineModel{name: "Bob Model", wire: "bob-wire", client: bob})
	f := newEngineFixture(t, chain, clk)
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	var taps []hook.Fallback
	f.spec.hooks.add(hook.Plugin{Name: "tap", OnFallback: func(_ context.Context, ev hook.Fallback) { taps = append(taps, ev) }})

	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if got := alice.Models(); len(got) != 1 {
		t.Fatalf("Alice's model was called %d times, want once (then parked)", len(got))
	}
	if len(taps) != 2 || taps[0].Passed[0].Skipped || !taps[1].Passed[0].Skipped || taps[1].Next.Name != "Bob Model" {
		t.Fatalf("fallbacks = %+v, want Alice's failure then her skip, each to Bob", taps)
	}
	kinds := f.rec.kinds()
	first := slices.Index(kinds, "ModelCall")
	if first < 0 || kinds[first+1] != "Fallback" {
		t.Fatalf("events = %v, want the failed call's ModelCall then Fallback", kinds)
	}
	var calls []hook.ModelCall
	for _, ev := range f.rec.all() {
		if c, ok := ev.(hook.ModelCall); ok {
			calls = append(calls, c)
		}
	}
	if len(calls) != 3 || calls[0].Err == nil || calls[0].Model.Name != "Alice Model" || calls[2].Model.Name != "Bob Model" {
		t.Fatalf("model calls = %+v, want Alice failing, then Bob twice", calls)
	}
}

// A timed-out dispatch is retried after the backoff, on the turn's clock.
func TestEngine_TimeoutRetryOnTheClock(t *testing.T) {
	p := modelstest.New().Script("alice-wire",
		modelstest.Fail(fmt.Errorf("request: %w", context.DeadlineExceeded)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)

	done := make(chan error, 1)
	go func() { _, _, err := f.run(t); done <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("the retry never waited on the clock: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("the turn ended before the backoff: %v", err)
	default:
	}
	clk.Advance(2 * f.spec.retryBackoff) // past the jittered first step
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := len(p.Calls()); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

// The jitter spreads each backoff step by 20% either way.
func TestEngine_BackoffJitter(t *testing.T) {
	for range 200 {
		if got := withJitter(time.Second); got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("withJitter(1s) = %v", got)
		}
	}
}

// A context overflow tells the turn (OnCompaction and an event, phase
// overflow), forces a compaction and, when history did not shrink, retries
// with max_tokens halved.
func TestEngine_OverflowCompactsAndLowersMaxTokens(t *testing.T) {
	p := modelstest.New().Script("alice-wire",
		modelstest.Fail(errors.New("context_length_exceeded: request too long")), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.cm.assembled = []spawnllm.Message{{Role: "user", Content: "hello"}} // nothing to compact away
	var phases []hook.CompactionPhase
	f.spec.hooks.add(hook.Plugin{Name: "tap", OnCompaction: func(_ context.Context, ev hook.Compaction) { phases = append(phases, ev.Phase) }})

	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(phases, []hook.CompactionPhase{hook.CompactionOverflow}) {
		t.Fatalf("OnCompaction = %v, want one overflow", phases)
	}
	if !slices.Contains(f.cm.events(), "force") {
		t.Fatalf("context manager = %v, want a forced compaction", f.cm.events())
	}
	calls := p.Calls()
	if len(calls) != 2 || calls[0].Options["max_tokens"] != 4096 || calls[1].Options["max_tokens"] != 2048 {
		t.Fatalf("max_tokens = %v then %v, want 4096 then 2048", calls[0].Options["max_tokens"], calls[len(calls)-1].Options["max_tokens"])
	}
}

// A batch's calls run in parallel, each with its own timeout; the results
// are stored in call order whatever order they finish in.
func TestEngine_ParallelToolsWithTimeout(t *testing.T) {
	p := modelstest.New().Script("alice-wire",
		callStep(toolCall("c1", "slow", nil), toolCall("c2", "fast", nil), toolCall("c3", "stuck", nil)),
		modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.toolTimeout = func() time.Duration { return 200 * time.Millisecond }
	fastDone := make(chan struct{})
	f.tool("slow", func(ctx context.Context, _ map[string]any) *toolkit.Result {
		select { // finishes only once fast has: the calls run in parallel
		case <-fastDone:
			return toolkit.SilentResult("slow result")
		case <-ctx.Done():
			return toolkit.ErrorResult("slow never saw fast finish")
		}
	})
	f.tool("fast", func(context.Context, map[string]any) *toolkit.Result {
		defer close(fastDone)
		return toolkit.SilentResult("fast result")
	})
	f.tool("stuck", func(ctx context.Context, _ map[string]any) *toolkit.Result {
		<-ctx.Done()
		return toolkit.ErrorResult("timed out: " + ctx.Err().Error())
	})

	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, m := range f.cm.stored {
		if m.Role == "tool" {
			results = append(results, m.ToolCallID+"="+m.Content)
		}
	}
	want := []string{"c1=slow result", "c2=fast result", "c3=timed out: context deadline exceeded"}
	if !slices.Equal(results, want) {
		t.Fatalf("stored results = %v, want %v", results, want)
	}
}

// The same batch four times in a row stops the turn; each repeat before that
// is steered, with the spec's hint for the tool repeated.
func TestEngine_RepeatedBatchStops(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "file_edit", map[string]any{"path": "a.md"})))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.repeatHint = func(names []string) string {
		if slices.Contains(names, "file_edit") {
			return "Re-read the file."
		}
		return ""
	}
	f.tool("file_edit", func(context.Context, map[string]any) *toolkit.Result { return toolkit.ErrorResult("no match") })

	turn, res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.finishReason != "loop_detected" || !strings.HasPrefix(res.content, "Stopped: I kept making the same file_edit call (4 times)") {
		t.Fatalf("result = %+v", res)
	}
	steering := 0
	for _, m := range turn.messages {
		if m.Role == "user" && strings.Contains(m.Content, "exact same tool call") {
			steering++
			if !strings.HasSuffix(m.Content, " Re-read the file.") {
				t.Errorf("steering %q lacks the hint", m.Content)
			}
		}
	}
	if steering != 2 || len(p.Calls()) != 4 {
		t.Fatalf("steering = %d, calls = %d; want 2 and 4", steering, len(p.Calls()))
	}
}

// An empty reply with reasoning is poked twice, then marked degenerate.
func TestEngine_EmptyReplyPoke(t *testing.T) {
	empty := modelstest.Step{Response: &spawnllm.LLMResponse{ReasoningContent: "thinking", FinishReason: "stop", Normal: true}}
	tests := []struct {
		name       string
		steps      []modelstest.Step
		calls      int
		degenerate bool
		content    string
	}{
		{"answers after a poke", []modelstest.Step{empty, modelstest.Reply("hi Bob")}, 2, false, "hi Bob"},
		{"never answers", []modelstest.Step{empty}, 3, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := modelstest.New().Script("alice-wire", tt.steps...)
			clk := newFake()
			_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
			f := newEngineFixture(t, chain, clk)
			_, res, err := f.run(t)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Calls()) != tt.calls || res.degenerate != tt.degenerate || res.content != tt.content {
				t.Fatalf("calls = %d, result = %+v", len(p.Calls()), res)
			}
		})
	}
}

// On the last step the request forbids tool calls (tool_choice none) while
// the tools stay declared, and carries the low-steps note.
func TestEngine_LastStepForbidsTools(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.maxIterations = 2
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	calls := p.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if _, set := calls[0].Options[spawnllm.ToolChoiceOption]; set {
		t.Error("the first step forbids tools")
	}
	last := calls[1]
	if last.Options[spawnllm.ToolChoiceOption] != spawnllm.ToolChoiceNone || len(last.Tools) != 1 {
		t.Fatalf("last step: tool_choice = %v, tools = %d; want none with the tool declared", last.Options[spawnllm.ToolChoiceOption], len(last.Tools))
	}
	if note := last.Messages[len(last.Messages)-1]; note.Content != lowStepsNote(2, 2) {
		t.Fatalf("last message = %q, want the low-steps note", note.Content)
	}
}

// The provider's prompt-token count calibrates the context engine's estimate.
func TestEngine_PromptTokenCalibration(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Step{Response: &spawnllm.LLMResponse{
		Content: "done", FinishReason: "stop", Usage: &spawnllm.UsageInfo{PromptTokens: 1234},
	}})
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.cm.estimate = 1000
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	if len(f.cm.observed) != 1 || f.cm.observed[0] != [2]int{1000, 1234} {
		t.Fatalf("observed = %v, want (1000, 1234)", f.cm.observed)
	}
}

// An async tool's immediate result is what the turn stores; its late result
// arrives after the turn, outside the engine (the host routes it).
func TestEngine_AsyncToolLateResult(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "spawn", nil)), modelstest.Reply("started"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	late := make(chan string, 1)
	release := make(chan struct{})
	f.tool("spawn", func(context.Context, map[string]any) *toolkit.Result {
		go func() {
			<-release
			late <- "Bob's report"
		}()
		return toolkit.AsyncResult("Work started, will report back")
	})
	turn, res, err := f.run(t)
	if err != nil || res.content != "started" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	close(release)
	if got := <-late; got != "Bob's report" {
		t.Fatalf("late result = %q", got)
	}
	select {
	case <-turn.handle.Done():
	default:
		t.Fatal("the turn had not finished")
	}
	if f.cm.stored[1].Content != "Work started, will report back" {
		t.Fatalf("stored = %q, want the immediate result", f.cm.stored[1].Content)
	}
}

// cliProvider is a scripted model that runs as a CLI.
type cliProvider struct{ *modelstest.Provider }

func (cliProvider) IsCLI() bool { return true }

// A CLI model answers the turn in one call with no low-steps note, no
// tool_choice and no temperature.
func TestEngine_CLIModelOneCall(t *testing.T) {
	p := cliProvider{modelstest.New().Script("cli-wire", modelstest.Reply("done"))}
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "CLI Model", wire: "cli-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.maxIterations = 1
	temp := 0.3
	f.spec.temperature = &temp
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	calls := p.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if _, set := calls[0].Options[spawnllm.ToolChoiceOption]; set {
		t.Error("a CLI call carries tool_choice")
	}
	if len(calls[0].Messages) != 1 {
		t.Errorf("messages = %d, want the user's only (no low-steps note)", len(calls[0].Messages))
	}
}

// A CLI provider with a CLI protocol takes no temperature.
func TestEngine_CLIProtocolTakesNoTemperature(t *testing.T) {
	f := newEngineFixture(t, nil, newFake())
	f.spec.candidates = []models.Candidate{{Name: "Claude", Provider: "Claude CLI", Protocol: "claude-cli", Model: "sonnet"}}
	temp := 0.3
	f.spec.temperature = &temp
	turn := newEngineTurn(f.spec, nil)
	if _, set := turn.newDispatch(context.Background(), nil, 0).llmOpts["temperature"]; set {
		t.Fatal("temperature sent to a CLI model")
	}
}

// Events: TurnStarted first, TurnFinished last and once; every
// ToolCallStarted (all of a batch before the first BeforeTool) has one
// ToolCallFinished, a panicking tool's included.
func TestEngine_EventGuarantees(t *testing.T) {
	p := modelstest.New().Script("alice-wire",
		callStep(toolCall("c1", "ok", nil), toolCall("c2", "boom", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.tool("ok", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("fine") })
	f.tool("boom", func(context.Context, map[string]any) *toolkit.Result { panic("Bob's bug") })
	var startedAtBefore atomic.Int64
	f.spec.hooks.add(hook.Plugin{Name: "tap", BeforeTool: func(context.Context, *hook.ToolCall) (*toolkit.Result, error) {
		n := 0
		for _, k := range f.rec.kinds() {
			if k == "ToolCallStarted" {
				n++
			}
		}
		startedAtBefore.Store(int64(n))
		return nil, nil //nolint:nilnil // (nil, nil): go on with the step
	}})

	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	kinds := f.rec.kinds()
	if kinds[0] != "TurnStarted" || kinds[len(kinds)-1] != "TurnFinished" || countOf(kinds, "TurnFinished") != 1 {
		t.Fatalf("events = %v", kinds)
	}
	if startedAtBefore.Load() != 2 {
		t.Fatalf("BeforeTool saw %d ToolCallStarted, want the whole batch (2)", startedAtBefore.Load())
	}
	finished := map[string]*toolkit.Result{}
	for _, ev := range f.rec.all() {
		if fin, ok := ev.(hook.ToolCallFinished); ok {
			finished[fin.Call.ID] = fin.Result
		}
	}
	if len(finished) != 2 || finished["c2"] == nil {
		t.Fatalf("finished = %v, want both calls", finished)
	}
	var perr *hook.ToolPanicError
	if !errors.As(finished["c2"].Err, &perr) || perr.Tool != "boom" || perr.Value != "Bob's bug" {
		t.Fatalf("c2 error = %v, want a *hook.ToolPanicError for boom", finished["c2"].Err)
	}
	if got, want := finished["c2"].Err.Error(), "tool boom panicked: Bob's bug"; got != want {
		t.Fatalf("c2 error text = %q, want %q", got, want)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

// A failed turn still ends with exactly one TurnFinished, carrying the error.
func TestEngine_TurnFinishedOnError(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Fail(errors.New("model broke")))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	if _, _, err := f.run(t); err == nil {
		t.Fatal("no error")
	}
	evs := f.rec.all()
	fin, ok := evs[len(evs)-1].(hook.TurnFinished)
	if !ok || fin.Err == nil || fin.Outcome != hook.OutcomeError || countOf(f.rec.kinds(), "TurnFinished") != 1 {
		t.Fatalf("last event = %+v", evs[len(evs)-1])
	}
}

// Cancelling the turn through its handle ends the model call in flight: its
// ModelCall carries the context's error and the turn ends cancelled, with the
// cause.
func TestEngine_CancelThroughHandle(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{})
	p := modelstest.New().Script("alice-wire", modelstest.Step{Hold: hold, Started: started})
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	turn := newEngineTurn(f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
	done := make(chan error, 1)
	go func() { _, err := turn.run(context.Background()); done <- err }()
	<-started
	turn.handle.Cancel(hook.ErrInterrupted)
	if err := <-done; err == nil {
		t.Fatal("the cancelled turn reported no error")
	}
	result := turn.handle.Wait()
	if result.Outcome != hook.OutcomeCancelled || !errors.Is(result.Cause, hook.ErrInterrupted) {
		t.Fatalf("result = %+v, want cancelled by an interrupt", result)
	}
	var call hook.ModelCall
	for _, ev := range f.rec.all() {
		if c, ok := ev.(hook.ModelCall); ok {
			call = c
		}
	}
	if !errors.Is(call.Err, context.Canceled) {
		t.Fatalf("model call error = %v, want the context's", call.Err)
	}
}

// Partial text reaches OnTextDelta and TextDelta events, all before
// TurnFinished; without an OnTextDelta no streaming is asked for.
func TestEngine_TextDeltas(t *testing.T) {
	f := newEngineFixture(t, nil, newFake())
	if _, set := newEngineTurn(f.spec, nil).newDispatch(context.Background(), nil, 0).llmOpts[spawnllm.TextDeltaOption]; set {
		t.Fatal("streaming asked for with no OnTextDelta")
	}
	var got []string
	f.spec.hooks.add(hook.Plugin{Name: "tap", OnTextDelta: func(_ context.Context, ev hook.TextDelta) { got = append(got, ev.Text) }})
	turn := newEngineTurn(f.spec, nil)
	// As BeforeLoop leaves it by default with an OnTextDelta (startLoop).
	turn.textDeltas = true
	d := turn.newDispatch(context.Background(), nil, 0)
	deliver, ok := d.llmOpts[spawnllm.TextDeltaOption].(spawnllm.TextDeltaFunc)
	if !ok {
		t.Fatal("no delta function on the request")
	}
	deliver("Hi ")
	deliver("Bob")
	if !slices.Equal(got, []string{"Hi ", "Bob"}) || countOf(f.rec.kinds(), "TextDelta") != 2 {
		t.Fatalf("deltas = %v, events = %v", got, f.rec.kinds())
	}
}

// AfterModel runs before the assistant message is stored: the notes it adds
// (what became of generated images) are in the stored tool-call message, and
// its reply lines in the turn's reply; the ModelCall event keeps the
// response as the model gave it.
func TestEngine_AfterModelBeforeStorage(t *testing.T) {
	img := spawnllm.GeneratedImage{URL: "https://example.com/cat.png"}
	p := modelstest.New().Script("alice-wire",
		modelstest.Step{Response: &spawnllm.LLMResponse{
			Content: "here", Images: []spawnllm.GeneratedImage{img},
			ToolCalls: []spawnllm.ToolCall{toolCall("c1", "noop", nil)}, FinishReason: "tool_calls",
		}},
		modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	var storedAtAfterModel []string
	f.spec.hooks.add(hook.Plugin{Name: "images", AfterModel: func(_ context.Context, _ *hook.ModelInfo, resp *hook.ModelResponse) {
		if len(resp.Response.Images) == 0 {
			return
		}
		storedAtAfterModel = f.cm.events()
		resp.Notes = append(resp.Notes, "[Image saved.]")
		resp.ReplyLines = append(resp.ReplyLines, "Image: cat.png")
		resp.Response.Content = "changed"
	}})
	_, res, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(storedAtAfterModel, "add:toolcall") {
		t.Fatal("AfterModel ran after the tool-call message was stored")
	}
	if f.cm.stored[0].Content != "changed\n\n[Image saved.]" {
		t.Fatalf("stored tool-call message = %q", f.cm.stored[0].Content)
	}
	if !slices.Equal(res.replyLines, []string{"Image: cat.png"}) || res.images != 1 {
		t.Fatalf("result = %+v", res)
	}
	for _, ev := range f.rec.all() {
		if c, ok := ev.(hook.ModelCall); ok && c.Response != nil && len(c.Response.Images) > 0 && c.Response.Content != "here" {
			t.Fatalf("ModelCall response = %q, want the model's own", c.Response.Content)
		}
	}
	if countOf(f.rec.kinds(), "Image") != 1 {
		t.Fatalf("events = %v, want one Image", f.rec.kinds())
	}
}

// Evictions before a dispatch are a Compaction event (phase evicted).
func TestEngine_EvictionEvent(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.cm.evictOnce = true
	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	for _, ev := range f.rec.all() {
		if c, ok := ev.(hook.Compaction); ok {
			if c.Phase != hook.CompactionEvicted || len(c.Evictions) != 1 || c.Evictions[0].Resource != "notes.md" {
				t.Fatalf("compaction = %+v", c)
			}
			return
		}
	}
	t.Fatal("no Compaction event")
}

// A message the store refuses ends the turn with the store's error.
func TestEngine_StoreRefusalEndsTurn(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "noop", nil)))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.cm.failAdd = errors.New("disk full")
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	_, _, err := f.run(t)
	se, ok := errors.AsType[*hook.SessionStoreError](err)
	if !ok || se.What != "tool call message" || !errors.Is(se.Err, f.cm.failAdd) || !errors.Is(err, hook.ErrSessionStore) {
		t.Fatalf("err = %v, want the store's refusal of the tool call message", err)
	}
}

// The turn's scope is on the context of every tool call, and its identity
// is that of every model call.
func TestEngine_ScopeOnCallsAndIdentity(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "who", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	var seen hook.Scope
	var session string
	f.tool("who", func(ctx context.Context, _ map[string]any) *toolkit.Result {
		seen, _ = hook.ScopeOf(ctx)
		session = toolkit.SessionKey(ctx)
		return toolkit.SilentResult("ok")
	})
	ctx := beginTurnContext(context.Background(), f.spec.scope, f.spec.hooks, clk)
	turn := newEngineTurn(f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
	if _, err := turn.run(ctx); err != nil {
		t.Fatal(err)
	}
	if seen != f.spec.scope || session != "s" {
		t.Fatalf("scope = %+v, session = %q", seen, session)
	}
	if id := p.Calls()[0].Identity; id != engineIdentity {
		t.Fatalf("identity = %+v, want %+v", id, engineIdentity)
	}
}

// A panicking event consumer only watches: it is logged at error level with
// its name, and the turn goes on.
func TestEngine_ConsumerPanicIsLogged(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	var buf strings.Builder
	f.spec.hooks.log = slog.New(slog.NewTextHandler(&buf, nil))
	f.spec.hooks.add(hook.Plugin{Name: "bobs-watcher", OnEvent: func(context.Context, hook.Event) { panic("Bob's bug") }})
	_, res, err := f.run(t)
	if err != nil || res.content != "done" {
		t.Fatalf("run = %+v, %v; want the turn to go on", res, err)
	}
	if out := buf.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "consumer=bobs-watcher") || !strings.Contains(out, "stack=") {
		t.Fatalf("log = %s", out)
	}
}

// A panic in a turn or model step ends the turn with a *hook.PanicError
// naming the callback and its plugin: exactly one TurnFinished with
// OutcomeError, the later callbacks of the kind not run, nothing of the step
// stored.
func TestEngine_StepPanicEndsTheTurn(t *testing.T) {
	tests := []struct {
		kind  string
		setup func(f *engineFixture, later *atomic.Bool)
	}{
		{"BeforeModel", func(f *engineFixture, later *atomic.Bool) {
			f.spec.hooks.add(hook.Plugin{Name: "bobs", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) { panic("Bob's bug") }})
			f.spec.hooks.add(hook.Plugin{Name: "later", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
				later.Store(true)
				return nil, nil //nolint:nilnil // (nil, nil): go on with the step
			}})
		}},
		{"AfterModel", func(f *engineFixture, later *atomic.Bool) {
			f.spec.hooks.add(hook.Plugin{Name: "bobs", AfterModel: func(context.Context, *hook.ModelInfo, *hook.ModelResponse) { panic("Bob's bug") }})
			f.spec.hooks.add(hook.Plugin{Name: "later", AfterModel: func(context.Context, *hook.ModelInfo, *hook.ModelResponse) { later.Store(true) }})
		}},
		{"OnModelError", func(f *engineFixture, later *atomic.Bool) {
			f.spec.hooks.add(hook.Plugin{Name: "fail", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
				return nil, errors.New("refused")
			}})
			f.spec.hooks.add(hook.Plugin{Name: "bobs", OnModelError: func(context.Context, *hook.ModelInfo, error) (*spawnllm.LLMResponse, error) { panic("Bob's bug") }})
			f.spec.hooks.add(hook.Plugin{Name: "later", OnModelError: func(context.Context, *hook.ModelInfo, error) (*spawnllm.LLMResponse, error) {
				later.Store(true)
				return nil, nil //nolint:nilnil // (nil, nil): go on with the step
			}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
			clk := newFake()
			_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
			f := newEngineFixture(t, chain, clk)
			f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
			var later atomic.Bool
			tt.setup(f, &later)
			_, _, err := f.run(t)
			perr, ok := errors.AsType[*hook.PanicError](err)
			if !ok || perr.Callback != tt.kind || perr.Plugin != "bobs" || perr.Value != "Bob's bug" {
				t.Fatalf("err = %v, want the %s panic of bobs", err, tt.kind)
			}
			if later.Load() {
				t.Error("a later callback of the kind ran after the panic")
			}
			if len(f.cm.stored) != 0 {
				t.Errorf("stored %v after the panic", f.cm.stored)
			}
			kinds := f.rec.kinds()
			evs := f.rec.all()
			fin, isFin := evs[len(evs)-1].(hook.TurnFinished)
			if countOf(kinds, "TurnFinished") != 1 || !isFin || fin.Outcome != hook.OutcomeError || !errors.Is(fin.Err, perr) {
				t.Fatalf("events = %v, last = %+v", kinds, evs[len(evs)-1])
			}
		})
	}
}

// A panic in the loop itself, not in a callback, reaches run's caller after
// exactly one TurnFinished with OutcomeError.
func TestEngine_LoopPanicReachesTheCaller(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.env = panickyEnv{f.env}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_, _, err := f.run(t)
		t.Errorf("run returned (%v) instead of panicking", err)
	}()
	kinds := f.rec.kinds()
	evs := f.rec.all()
	if fin, ok := evs[len(evs)-1].(hook.TurnFinished); !ok || fin.Outcome != hook.OutcomeError || countOf(kinds, "TurnFinished") != 1 {
		t.Fatalf("events = %v", kinds)
	}
}

// panickyEnv is a host whose tool list panics.
type panickyEnv struct{ *fakeEnv }

func (panickyEnv) toolDefs() []spawnllm.ToolDefinition { panic("Bob's bug") }

// A panic in a tool callback fails only its call: the result is the
// internal-error result with the *hook.PanicError, the call's later callbacks
// do not run, it still gets its ToolCallFinished, and the turn goes on.
func TestEngine_ToolCallbackPanicFailsTheCall(t *testing.T) {
	tests := []struct {
		kind  string
		setup func(f *engineFixture, later *atomic.Bool)
	}{
		{"BeforeTool", func(f *engineFixture, later *atomic.Bool) {
			f.spec.hooks.add(hook.Plugin{Name: "bobs", BeforeTool: func(context.Context, *hook.ToolCall) (*toolkit.Result, error) { panic("Bob's bug") }})
			f.spec.hooks.add(hook.Plugin{Name: "later", AfterTool: func(context.Context, *hook.ToolCall, *toolkit.Result) { later.Store(true) }})
		}},
		{"OnToolError", func(f *engineFixture, later *atomic.Bool) {
			f.env.tools["noop"] = func(context.Context, map[string]any) *toolkit.Result { return toolkit.ErrorResult("no") }
			f.spec.hooks.add(hook.Plugin{Name: "bobs", OnToolError: func(context.Context, *hook.ToolCall, *toolkit.Result) *toolkit.Result { panic("Bob's bug") }})
			f.spec.hooks.add(hook.Plugin{Name: "later", AfterTool: func(context.Context, *hook.ToolCall, *toolkit.Result) { later.Store(true) }})
		}},
		{"AfterTool", func(f *engineFixture, later *atomic.Bool) {
			f.spec.hooks.add(hook.Plugin{Name: "bobs", AfterTool: func(_ context.Context, _ *hook.ToolCall, r *toolkit.Result) {
				r.ForLLM = "half-changed"
				panic("Bob's bug")
			}})
			f.spec.hooks.add(hook.Plugin{Name: "later", AfterTool: func(context.Context, *hook.ToolCall, *toolkit.Result) { later.Store(true) }})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
			clk := newFake()
			_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
			f := newEngineFixture(t, chain, clk)
			f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
			var later atomic.Bool
			tt.setup(f, &later)
			_, res, err := f.run(t)
			if err != nil || res.content != "done" {
				t.Fatalf("run = %+v, %v; want the turn to go on", res, err)
			}
			if later.Load() {
				t.Error("a later callback of the call ran after the panic")
			}
			var fin hook.ToolCallFinished
			for _, ev := range f.rec.all() {
				if e, ok := ev.(hook.ToolCallFinished); ok {
					fin = e
				}
			}
			perr, ok := errors.AsType[*hook.PanicError](fin.Result.Err)
			if !ok || perr.Callback != tt.kind || fin.Result.ForLLM != "Tool noop failed with an internal error." || !fin.Result.IsError {
				t.Fatalf("finished result = %+v", fin.Result)
			}
			if got := f.cm.stored[1].Content; got != "Tool noop failed with an internal error." {
				t.Fatalf("stored result = %q", got)
			}
		})
	}
}

// An observer that panics during the turn is logged and skipped: the turn
// goes on.
func TestEngine_ObserverPanicIsSkipped(t *testing.T) {
	first := modelstest.New().Script("alice-wire", modelstest.Fail(errors.New("503 overloaded")))
	second := modelstest.New().Script("bob-wire", modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk,
		engineModel{name: "Alice Model", wire: "alice-wire", client: first},
		engineModel{name: "Bob Model", wire: "bob-wire", client: second})
	f := newEngineFixture(t, chain, clk)
	var buf strings.Builder
	f.spec.hooks.log = slog.New(slog.NewTextHandler(&buf, nil))
	f.spec.hooks.add(hook.Plugin{Name: "bobs-notice", OnFallback: func(context.Context, hook.Fallback) { panic("Bob's bug") }})
	_, res, err := f.run(t)
	if err != nil || res.content != "done" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if out := buf.String(); !strings.Contains(out, "consumer=bobs-notice") {
		t.Fatalf("log = %s", out)
	}
}

// A provider call that panics still gets its ModelCall, and a turn whose
// loop panics still runs AfterTurn and clears its pending marker before its
// one TurnFinished with OutcomeError.
func TestEngine_LoopPanicEndsTheSpan(t *testing.T) {
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: panickyClient{}})
	f := newEngineFixture(t, chain, clk)
	var afterTurn bool
	f.spec.hooks.add(hook.Plugin{Name: "watch", AfterTurn: func(context.Context, *hook.Turn, *hook.TurnResult) { afterTurn = true }})
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_, _, err := f.run(t)
		t.Errorf("run returned (%v) instead of panicking", err)
	}()
	kinds := f.rec.kinds()
	last, isFin := f.rec.all()[len(kinds)-1].(hook.TurnFinished)
	if countOf(kinds, "TurnFinished") != 1 || !isFin || last.Outcome != hook.OutcomeError {
		t.Fatalf("events = %v", kinds)
	}
	started, calls := 0, 0
	for _, ev := range f.rec.all() {
		switch ev.(type) {
		case hook.ModelCallStarted:
			started++
		case hook.ModelCall:
			calls++
		}
	}
	if started != 1 || calls != 1 {
		t.Fatalf("%d ModelCallStarted, %d ModelCall", started, calls)
	}
	if !afterTurn || !slices.Equal(f.store.events(), []string{"pending", "cleared"}) {
		t.Fatalf("AfterTurn ran %v, store %v", afterTurn, f.store.events())
	}
}

// panickyClient is a provider client that panics.
type panickyClient struct{}

func (panickyClient) Chat(context.Context, accounting.Identity, []spawnllm.Message, []spawnllm.ToolDefinition, string, map[string]any) (*spawnllm.LLMResponse, error) {
	panic("Bob's bug")
}
func (panickyClient) GetDefaultModel() string { return "alice-wire" }

// An AfterModel that sets the response to nil changes nothing: the turn goes
// on with the response it was given.
func TestEngine_AfterModelNilResponseIsNoChange(t *testing.T) {
	p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.hooks.add(hook.Plugin{Name: "drop", AfterModel: func(_ context.Context, _ *hook.ModelInfo, resp *hook.ModelResponse) { resp.Response = nil }})
	_, res, err := f.run(t)
	if err != nil || res.content != "done" {
		t.Fatalf("run = %+v, %v", res, err)
	}
}

// A spec without memory runs without it; one without a context manager or
// host services is refused, not run into a nil.
func TestEngine_IncompleteSpec(t *testing.T) {
	p := modelstest.New().Script("alice-wire", callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	f.spec.mem = nil
	if _, res, err := f.run(t); err != nil || res.content != "done" {
		t.Fatalf("without memory: %+v, %v", res, err)
	}
	for name, mutate := range map[string]func(*engineSpec){
		"no context manager": func(s *engineSpec) { s.cm = nil },
		"no host services":   func(s *engineSpec) { s.env = nil },
	} {
		spec := f.spec
		mutate(&spec)
		if _, err := newEngineTurn(spec, nil).run(context.Background()); !errors.Is(err, errEngineIncomplete) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// pcHandler records the function each record was logged from.
type pcHandler struct{ funcs *[]string }

func (pcHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h pcHandler) Handle(_ context.Context, r slog.Record) error {
	frame, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
	*h.funcs = append(*h.funcs, r.Message+"@"+frame.Function)
	return nil
}
func (h pcHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h pcHandler) WithGroup(string) slog.Handler      { return h }

// An engine line names the engine function that logged it, not the logging
// helper.
func TestEngine_LogLinesNameTheirCallSite(t *testing.T) {
	var funcs []string
	f := newEngineFixture(t, nil, newFake())
	f.spec.log = slog.New(pcHandler{funcs: &funcs})
	newEngineTurn(f.spec, nil).logDispatchTarget()
	if len(funcs) != 1 || !strings.HasSuffix(funcs[0], ".(*engineTurn).logDispatchTarget") || !strings.HasPrefix(funcs[0], "Dispatching to model@") {
		t.Fatalf("logged from %v", funcs)
	}
}

// A compaction reported on a turn's context reaches that turn's hooks, as
// OnCompaction and a Compaction event; one on a context of no turn reaches
// none.
func TestEngine_CompactionReachesTheTurnsHooks(t *testing.T) {
	var seen []string
	h := &turnHooks{}
	h.add(hook.Plugin{
		Name: "tap", OnCompaction: func(context.Context, hook.Compaction) { seen = append(seen, "callback") },
		OnEvent: func(_ context.Context, ev hook.Event) {
			if c, ok := ev.(hook.Compaction); ok && c.Phase == hook.CompactionStarted {
				seen = append(seen, "event")
			}
		},
	})
	ctx := beginTurnContext(context.Background(), hook.Scope{AgentID: "alice", Session: "s"}, h, newFake())
	start := ctxengine.CompactionStart{SessionKey: "s", Blocking: true}
	CompactionStart(ctx, start)
	CompactionStart(context.Background(), start) // no turn: reaches none
	if !slices.Equal(seen, []string{"callback", "event"}) {
		t.Fatalf("reached %v", seen)
	}
}

// BeforeModel is told a request was rebuilt from the session store on the
// turn's first request and after an eviction or an overflow retry replaced
// the working request, and not on a request kept from the step before,
// retried after a timeout, or kept because an overflow's reassembly failed.
func TestEngine_RebuiltOnStoreRequests(t *testing.T) {
	tests := []struct {
		name  string
		steps []modelstest.Step
		setup func(f *engineFixture)
		// advance runs the turn past a timeout's backoff.
		advance bool
		want    []bool
	}{
		{"kept", []modelstest.Step{callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done")}, nil, false, []bool{true, false}},
		{
			"eviction",
			[]modelstest.Step{callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done")},
			func(f *engineFixture) {
				// The second step's assembly evicts.
				f.spec.hooks.add(hook.Plugin{Name: "evict", OnEvent: func(_ context.Context, ev hook.Event) {
					if _, ok := ev.(hook.ToolBatchStarted); ok {
						f.cm.mu.Lock()
						f.cm.evictOnce = true
						f.cm.mu.Unlock()
					}
				}})
			}, false,
			[]bool{true, true},
		},
		{
			"overflow",
			[]modelstest.Step{modelstest.Fail(errors.New("context_length_exceeded: request too long")), modelstest.Reply("done")},
			nil, false,
			[]bool{true, true},
		},
		{
			"overflow, reassembly failed",
			[]modelstest.Step{modelstest.Fail(errors.New("context_length_exceeded: request too long")), modelstest.Reply("done")},
			func(f *engineFixture) { f.cm.failAfterForce = true }, false,
			[]bool{true, false},
		},
		{
			"timeout",
			[]modelstest.Step{modelstest.Fail(fmt.Errorf("request: %w", context.DeadlineExceeded)), modelstest.Reply("done")},
			nil, true,
			[]bool{true, false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := modelstest.New().Script("alice-wire", tt.steps...)
			clk := newFake()
			_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
			f := newEngineFixture(t, chain, clk)
			f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.NewResult("ok") })
			if tt.setup != nil {
				tt.setup(f)
			}
			var rebuilt []bool
			f.spec.hooks.add(hook.Plugin{Name: "tap", BeforeModel: func(_ context.Context, info *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
				rebuilt = append(rebuilt, info.Rebuilt)
				return nil, nil //nolint:nilnil // (nil, nil): go on with the step
			}})
			done := make(chan error, 1)
			go func() { _, _, err := f.run(t); done <- err }()
			if tt.advance {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := clk.BlockUntil(ctx, 1); err != nil {
					t.Fatalf("the retry never waited on the clock: %v", err)
				}
				clk.Advance(2 * f.spec.retryBackoff)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rebuilt, tt.want) {
				t.Fatalf("Rebuilt = %v, want %v", rebuilt, tt.want)
			}
		})
	}
}

// A turn that reaches its model loop has one LoopStarted, after the pending
// marker was set and before its first ModelCallStarted; a turn BeforeTurn
// answered, or whose message the store refused, has none.
func TestEngine_LoopStarted(t *testing.T) {
	loopStarts := func(f *engineFixture) (n int, firstCall int, at int) {
		firstCall, at = -1, -1
		for i, ev := range f.rec.all() {
			switch ev.(type) {
			case hook.LoopStarted:
				n++
				at = i
			case hook.ModelCallStarted:
				if firstCall < 0 {
					firstCall = i
				}
			}
		}
		return n, firstCall, at
	}
	t.Run("loop", func(t *testing.T) {
		p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
		clk := newFake()
		_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
		f := newEngineFixture(t, chain, clk)
		var pendingAtStart []string
		f.spec.hooks.add(hook.Plugin{Name: "tap", OnEvent: func(_ context.Context, ev hook.Event) {
			if _, ok := ev.(hook.LoopStarted); ok {
				pendingAtStart = f.store.events()
			}
		}})
		if _, _, err := f.run(t); err != nil {
			t.Fatal(err)
		}
		n, firstCall, at := loopStarts(f)
		if n != 1 || firstCall < 0 || at > firstCall {
			t.Fatalf("LoopStarted %d times at %d, first ModelCallStarted at %d", n, at, firstCall)
		}
		if !slices.Equal(pendingAtStart, []string{"pending"}) {
			t.Fatalf("store at LoopStarted = %v, want the marker set", pendingAtStart)
		}
	})
	t.Run("answered", func(t *testing.T) {
		f := newEngineFixture(t, nil, newFake())
		f.spec.hooks.add(hook.Plugin{Name: "person", BeforeTurn: func(context.Context, *hook.Turn) (*hook.TurnResult, error) {
			return &hook.TurnResult{Outcome: hook.OutcomeOK, Reply: "hi"}, nil
		}})
		if _, _, err := f.run(t); err != nil {
			t.Fatal(err)
		}
		if n, _, _ := loopStarts(f); n != 0 {
			t.Fatalf("LoopStarted %d times for a turn BeforeTurn answered", n)
		}
	})
	t.Run("refused", func(t *testing.T) {
		f := newEngineFixture(t, nil, newFake())
		f.spec.options.AlreadyStored = false
		f.cm.failAdd = errors.New("disk full")
		if _, _, err := f.run(t); err == nil {
			t.Fatal("the store's refusal did not end the turn")
		}
		if n, _, _ := loopStarts(f); n != 0 {
			t.Fatalf("LoopStarted %d times for a turn whose message was refused", n)
		}
	})
}
