/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// Each overflow retry halves every model's own max_tokens, down to that
// model's floor: a larger model keeps halving while a smaller one stops at
// or below minHalvedMaxTokens. The options the turn shares across calls
// (what BeforeModel sees, and every ModelCall's request) never carry a
// model's max_tokens.
func TestEngine_OverflowHalvesMaxTokensPerModel(t *testing.T) {
	overflow := func() modelstest.Step {
		return modelstest.Fail(errors.New("context_length_exceeded: request too long"))
	}
	alice := modelstest.New().Script("alice-wire", overflow(), overflow(), overflow())
	bob := modelstest.New().Script("bob-wire", overflow(), overflow(), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk,
		engineModel{name: "Alice Model", wire: "alice-wire", client: alice},
		engineModel{name: "Bob Model", wire: "bob-wire", client: bob, maxTokens: 1000})
	f := newEngineFixture(t, chain, clk)
	f.cm.assembled = []spawnllm.Message{{Role: "user", Content: "hello"}} // nothing to compact away
	var shared []map[string]any
	f.spec.hooks.add(hook.Plugin{Name: "tap", BeforeModel: func(_ context.Context, info *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
		shared = append(shared, info.Request.Options)
		return nil, nil //nolint:nilnil // (nil, nil): go on with the step
	}})

	if _, res, err := f.run(t); err != nil || res.content != "done" {
		t.Fatalf("run = %q, %v", res.content, err)
	}
	maxTokens := func(p *modelstest.Provider) []any {
		calls := p.Calls()
		out := make([]any, 0, len(calls))
		for _, c := range calls {
			out = append(out, c.Options["max_tokens"])
		}
		return out
	}
	if got := maxTokens(alice); !slices.Equal(got, []any{4096, 2048, 1024}) {
		t.Fatalf("Alice's max_tokens = %v, want 4096, 2048, 1024", got)
	}
	if got := maxTokens(bob); !slices.Equal(got, []any{1000, 500, 500}) {
		t.Fatalf("Bob's max_tokens = %v, want 1000, 500, 500 (halved to his floor)", got)
	}
	if len(shared) != 3 {
		t.Fatalf("BeforeModel saw %d requests, want 3", len(shared))
	}
	for i, opts := range shared {
		if _, ok := opts["max_tokens"]; ok {
			t.Fatalf("shared options of attempt %d were changed: %v", i, opts)
		}
	}
	for _, ev := range f.rec.all() {
		if c, ok := ev.(hook.ModelCall); ok {
			if _, has := c.Request.Options["max_tokens"]; has {
				t.Fatalf("ModelCall request options carry a model's max_tokens: %v", c.Request.Options)
			}
		}
	}
}

// lineLog is a slog handler keeping each record as one text line.
type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *lineLog) Handle(ctx context.Context, r slog.Record) error {
	var buf bytes.Buffer
	if err := slog.NewTextHandler(&buf, nil).Handle(ctx, r); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, buf.String())
	return nil
}

func (l *lineLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *lineLog) WithGroup(string) slog.Handler      { return l }

func (l *lineLog) matching(msg string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.Contains(line, "msg="+`"`+msg+`"`) {
			out = append(out, line)
		}
	}
	return out
}

// The line for a candidate skipped in cooldown carries no text of the
// provider's error that parked it: the chain's skip error is built from the
// candidate's provider, model and remaining time alone. So it holds even
// with no redactor at all (the identity the engine defaults to), for a
// failure whose message carries a secret.
func TestEngine_SkippedCandidateLineCarriesNoProviderText(t *testing.T) {
	const secret = "sk-alice-0123456789"
	alice := modelstest.New().Script("alice-wire",
		modelstest.Fail(errors.New("429 Too Many Requests: rate limit exceeded for key "+secret)))
	bob := modelstest.New().Script("bob-wire", callStep(toolCall("c1", "noop", nil)), modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk,
		engineModel{name: "Alice Model", wire: "alice-wire", client: alice},
		engineModel{name: "Bob Model", wire: "bob-wire", client: bob})
	f := newEngineFixture(t, chain, clk)
	f.tool("noop", func(context.Context, map[string]any) *toolkit.Result { return toolkit.SilentResult("ok") })
	logs := &lineLog{}
	f.spec.log = slog.New(logs)
	var fallbacks []hook.Fallback
	f.spec.hooks.add(hook.Plugin{Name: "tap", OnFallback: func(_ context.Context, ev hook.Fallback) { fallbacks = append(fallbacks, ev) }})

	if _, _, err := f.run(t); err != nil {
		t.Fatal(err)
	}
	// The input is realistic: the failure that parked Alice carried the secret.
	if len(fallbacks) != 2 || fallbacks[0].Passed[0].Err == nil || !strings.Contains(fallbacks[0].Passed[0].Err.Error(), secret) {
		t.Fatalf("fallbacks = %+v, want Alice's failure with the provider's message first", fallbacks)
	}
	skipped := logs.matching("Fallback: skipped candidate (cooldown)")
	if len(skipped) != 1 {
		t.Fatalf("skip lines = %q, want one", skipped)
	}
	if strings.Contains(skipped[0], secret) || strings.Contains(skipped[0], "rate limit exceeded") {
		t.Fatalf("skip line carries the provider's text: %s", skipped[0])
	}
	if !strings.Contains(skipped[0], "in cooldown") {
		t.Fatalf("skip line = %s, want the cooldown it was skipped for", skipped[0])
	}
}

func TestBackfillReasoningContent(t *testing.T) {
	history := func() []spawnllm.Message {
		return []spawnllm.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "a1"},
			{Role: "assistant", Content: "a2", ReasoningContent: "thought"},
			{Role: "tool", Content: "r"},
		}
	}
	tests := []struct {
		name               string
		required, hasTools bool
		wantSame           bool
		wantReasoning      []string
	}{
		{name: "not required", hasTools: true, wantSame: true, wantReasoning: []string{"", "", "thought", ""}},
		{name: "no tools", required: true, wantSame: true, wantReasoning: []string{"", "", "thought", ""}},
		{name: "required with tools", required: true, hasTools: true, wantReasoning: []string{"", reasoningPlaceholder, "thought", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := history()
			got := backfillReasoningContent(in, tt.required, tt.hasTools)
			if same := &got[0] == &in[0]; same != tt.wantSame {
				t.Fatalf("returned the input slice: %v, want %v", same, tt.wantSame)
			}
			for i, m := range got {
				if m.ReasoningContent != tt.wantReasoning[i] {
					t.Fatalf("message %d reasoning = %q, want %q", i, m.ReasoningContent, tt.wantReasoning[i])
				}
			}
			if !slices.EqualFunc(in, history(), func(a, b spawnllm.Message) bool { return a.ReasoningContent == b.ReasoningContent }) {
				t.Fatal("the input was changed")
			}
		})
	}

	t.Run("nothing to fill", func(t *testing.T) {
		in := []spawnllm.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "a", ReasoningContent: "r"}}
		if got := backfillReasoningContent(in, true, true); &got[0] != &in[0] {
			t.Fatal("copied a history that needed no backfill")
		}
	})
}

func TestMessagesForModel(t *testing.T) {
	const note = "\n[1 attachment(s) on this message are hidden — the current model cannot view images]"
	withMedia := func() []spawnllm.Message {
		return []spawnllm.Message{
			{Role: "user", Content: "look\n", Media: []string{"data:image/png;base64,AAAA"}},
			{Role: "assistant", Content: "seen"},
		}
	}
	textOnly := func() []spawnllm.Message {
		return []spawnllm.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello", ReasoningContent: "r"}}
	}
	tests := []struct {
		name          string
		in            []spawnllm.Message
		vision        string
		reasoning     bool
		hasTools      bool
		wantSame      bool
		wantFirst     string
		wantMedia     int
		wantReasoning string // the second message's
	}{
		{name: "vision user message keeps media", in: withMedia(), vision: modelspec.VisionUserMessage, wantSame: true, wantFirst: "look\n", wantMedia: 1},
		{name: "vision tool response keeps media", in: withMedia(), vision: modelspec.VisionToolResponse, wantSame: true, wantFirst: "look\n", wantMedia: 1},
		{name: "vision off strips media", in: withMedia(), vision: modelspec.VisionOff, wantFirst: "look" + note},
		{name: "unknown model strips media", in: withMedia(), vision: "", wantFirst: "look" + note},
		{name: "unrecognised vision strips media", in: withMedia(), vision: "sometimes", wantFirst: "look" + note},
		{name: "no media is not copied", in: textOnly(), vision: "", wantSame: true, wantFirst: "hi", wantReasoning: "r"},
		{name: "no media, nothing to backfill", in: textOnly(), vision: modelspec.VisionOff, reasoning: true, hasTools: true, wantSame: true, wantFirst: "hi", wantReasoning: "r"},
		{name: "backfill and strip together", in: withMedia(), vision: "", reasoning: true, hasTools: true, wantFirst: "look" + note, wantReasoning: reasoningPlaceholder},
		{name: "empty", in: nil, vision: "", wantSame: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := slices.Clone(tt.in)
			got := messagesForModel(tt.in, tt.vision, tt.reasoning, tt.hasTools)
			if len(got) != len(tt.in) {
				t.Fatalf("got %d messages, want %d", len(got), len(tt.in))
			}
			if len(got) == 0 {
				return
			}
			if same := &got[0] == &tt.in[0]; same != tt.wantSame {
				t.Fatalf("returned the input slice: %v, want %v", same, tt.wantSame)
			}
			if got[0].Content != tt.wantFirst || len(got[0].Media) != tt.wantMedia {
				t.Fatalf("first message = %q with %d media, want %q with %d", got[0].Content, len(got[0].Media), tt.wantFirst, tt.wantMedia)
			}
			if got[1].ReasoningContent != tt.wantReasoning {
				t.Fatalf("second message reasoning = %q, want %q", got[1].ReasoningContent, tt.wantReasoning)
			}
			for i := range orig {
				if tt.in[i].Content != orig[i].Content || len(tt.in[i].Media) != len(orig[i].Media) || tt.in[i].ReasoningContent != orig[i].ReasoningContent {
					t.Fatalf("input message %d was changed: %+v, was %+v", i, tt.in[i], orig[i])
				}
			}
		})
	}
}
