/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"
)

// TestMergeSeqRanges drops invalid ranges, fills an omitted end, and merges
// overlapping and adjacent ranges.
func TestMergeSeqRanges(t *testing.T) {
	in := []SeqRange{
		{SeqStart: 0, SeqEnd: 5},   // invalid start
		{SeqStart: 9, SeqEnd: 3},   // inverted
		{SeqStart: 1, SeqEnd: 4},   // first
		{SeqStart: 3, SeqEnd: 2},   // inverted
		{SeqStart: 2, SeqEnd: 3},   // inside the first
		{SeqStart: 5},              // adjacent, end omitted
		{SeqStart: 10, SeqEnd: 12}, // gap
	}
	want := []SeqRange{{1, 5}, {10, 12}}
	if got := mergeSeqRanges(in); !reflect.DeepEqual(got, want) {
		t.Errorf("mergeSeqRanges = %v, want %v", got, want)
	}
}

// TestStripHTMLComments removes closed comments and drops an unterminated
// one to the end of the text.
func TestStripHTMLComments(t *testing.T) {
	tests := map[string]string{
		"keep <!-- note --> this":           "keep  this",
		"keep\n<!-- a\nb -->this":           "keep\nthis",
		"keep this <!-- never closed\nmore": "keep this ",
		"no comments":                       "no comments",
	}
	for in, want := range tests {
		if got := stripHTMLComments(in); got != want {
			t.Errorf("stripHTMLComments(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApplySummaryCoverage_ExistingRange extends the coverage over an
// earlier summary that recorded only a start and end, keeping its start time.
func TestApplySummaryCoverage_ExistingRange(t *testing.T) {
	applySummaryCoverage(nil, nil, 1, 2, nil) // nil summary is a no-op

	t0 := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	existing := &Summary{CoveredSeqStart: 3, CoveredSeqEnd: 9, CoveredSeqStartAt: t0}
	s := &Summary{}
	summarized := []memory.StoredMessage{
		{Seq: 10, Role: "user", CreatedAt: t0.Add(time.Hour)},
		{Seq: 14, Role: "assistant", CreatedAt: t0.Add(2 * time.Hour)},
	}
	applySummaryCoverage(s, existing, 10, 14, summarized)

	if s.CoveredSeqStart != 3 || s.CoveredSeqEnd != 14 {
		t.Errorf("covered = %d..%d, want 3..14", s.CoveredSeqStart, s.CoveredSeqEnd)
	}
	if want := []SeqRange{{3, 14}}; !reflect.DeepEqual(s.CoveredRanges, want) {
		t.Errorf("covered ranges = %v, want %v", s.CoveredRanges, want)
	}
	if !s.CoveredSeqStartAt.Equal(t0) || !s.CoveredSeqEndAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("covered times = %v..%v", s.CoveredSeqStartAt, s.CoveredSeqEndAt)
	}
}

// TestFormatStoredMessagesForSummary_RuntimeArgs shows a tool call's
// arguments from the runtime map when it has no serialized form.
func TestFormatStoredMessagesForSummary_RuntimeArgs(t *testing.T) {
	stored := []memory.StoredMessage{{
		Seq:       4,
		Role:      "assistant",
		ToolCalls: []spawnllm.ToolCall{{ID: "c1", Name: "file_read_bytes", Arguments: map[string]any{"path": "alice.md"}}},
	}}
	out := formatStoredMessagesForSummary(stored, nil)
	for _, want := range []string{"[#4] [assistant]", "content: <empty>", "name: file_read_bytes", `arguments: {"path":"alice.md"}`} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript lacks %q:\n%s", want, out)
		}
	}
}

// TestSplitSystemMessage separates a leading system message.
func TestSplitSystemMessage(t *testing.T) {
	stored := []memory.StoredMessage{
		{Seq: 1, Role: "system", Content: "rules"},
		{Seq: 2, Role: "user", Content: "hi"},
	}
	sys, conv := splitSystemMessage(stored)
	if sys == nil || sys.Content != "rules" || len(conv) != 1 || conv[0].Seq != 2 {
		t.Errorf("split = %+v, %+v", sys, conv)
	}
	sys, conv = splitSystemMessage(stored[1:])
	if sys != nil || len(conv) != 1 {
		t.Errorf("split without system = %+v, %+v", sys, conv)
	}
}

// TestSelectTail_NothingFits retains nothing when even the newest group is
// over the budget and no floor applies.
func TestSelectTail_NothingFits(t *testing.T) {
	stored := []memory.StoredMessage{
		{Seq: 1, Role: "user", Content: strings.Repeat("a", 400)},
		{Seq: 2, Role: "assistant", Content: strings.Repeat("b", 400)},
	}
	tail, start := selectTail(stored, 1, 0, 0, time.Now(), estimateTokens, nil)
	if tail != nil || start != len(stored) {
		t.Errorf("selectTail = %v, %d; want nil, %d", tail, start, len(stored))
	}
	if tail, start := selectTail(nil, 100, 1, 0, time.Now(), estimateTokens, nil); tail != nil || start != 0 {
		t.Errorf("selectTail(nil) = %v, %d", tail, start)
	}
	if got := collapseStoredNoise(nil, noNoiseKey); len(got) != 0 {
		t.Errorf("collapseStoredNoise(nil) = %v", got)
	}
}

// TestCountMeaningfulMessages does not count a message that repeats the one
// before it.
func TestCountMeaningfulMessages(t *testing.T) {
	msgs := []spawnllm.Message{
		{Role: "user", Content: "ping"},
		{Role: "user", Content: "ping"},
		{Role: "assistant", Content: "pong"},
	}
	if got := countMeaningfulMessages(msgs, noNoiseKey); got != 2 {
		t.Errorf("countMeaningfulMessages = %d, want 2", got)
	}
}

// TestSettingsFromOptions reports what the options asked for, without New's
// clamps.
func TestSettingsFromOptions(t *testing.T) {
	got := SettingsFromOptions(WithMinPercent(90), WithNormalPercent(50), WithContextWindow(4096))
	if got.MinPercent != 90 || got.NormalPercent != 50 || got.ContextWindow != 4096 {
		t.Errorf("settings = %+v", got)
	}
}

// TestWithArchiveContentMaxBytes caps tool results stored in the archive at
// the configured size, and ignores a non-positive value.
func TestWithArchiveContentMaxBytes(t *testing.T) {
	if got := (&Manager{cfg: applyOptions(WithArchiveContentMaxBytes(0))}).archiveContentLimit(); got != archiveContentMaxBytes {
		t.Errorf("limit with 0 = %d, want the default %d", got, archiveContentMaxBytes)
	}

	dir := t.TempDir()
	store, err := session.NewSQLiteStore(dir)
	noErr(t, err)
	defer func() { noErr(t, store.Close()) }()
	ctx := context.Background()
	mgr := asManager(t, New("alice", store, WithArchiveDir(dir), WithContextWindow(10_000_000),
		WithMessageThreshold(0), WithArchiveContentMaxBytes(1000)))
	defer func() { noErr(t, mgr.Close(ctx)) }()

	_, err = mgr.AddToolCallMessage(ctx, spawnllm.Message{Role: "assistant", ToolCalls: []spawnllm.ToolCall{
		{ID: "r1", Function: &spawnllm.FunctionCall{Name: "file_read_bytes", Arguments: `{"path":"bob.md"}`}},
	}})
	noErr(t, err)
	content := strings.Repeat("z", 5000)
	seq, err := mgr.AddToolResult(ctx, spawnllm.Message{Role: "tool", ToolCallID: "r1", Content: content})
	noErr(t, err)

	a, err := memory.OpenReadOnly(memory.ArchivePath(dir, "alice"), nil)
	noErr(t, err)
	defer func() { noErr(t, a.Close()) }()
	rows, err := a.QueryRange(seq, seq)
	if err != nil || len(rows) != 1 {
		t.Fatalf("QueryRange: rows=%d err=%v", len(rows), err)
	}
	if !strings.HasPrefix(rows[0].Content, content[:1000]) || !strings.Contains(rows[0].Content, "[content truncated: 5000 bytes total, first 1000 shown]") {
		t.Errorf("archived content = %d bytes, want the first 1000 and the marker", len(rows[0].Content))
	}
}

// applyOptions resolves opts over the package defaults.
func applyOptions(opts ...Option) managerConfig {
	cfg := defaultManagerConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// TestApplyLargeMsgChecks truncates an oversized message but never the
// newest user message, which is only logged when past the hard limit.
func TestApplyLargeMsgChecks(t *testing.T) {
	logs := &logCapture{}
	mgr := asManager(t, New("alice", newMockStore(), WithContextWindow(1000), WithLogger(logs.logger())))
	big := strings.Repeat("w", 10_000)
	conv := []spawnllm.Message{
		{Role: "assistant", Content: big},
		{Role: "user", Content: big},
	}
	mgr.applyLargeMsgChecks(conv)

	threshold := 1000 * (defaultSafetyPercent - defaultLargeMsgOffset) / 100
	if want := strings.Repeat("w", threshold*5/2) + " [**TRUNCATED DUE TO SIZE**]"; conv[0].Content != want {
		t.Errorf("assistant message = %d bytes, want truncated to %d runes plus the marker", len(conv[0].Content), threshold*5/2)
	}
	if conv[1].Content != big {
		t.Error("the newest user message was truncated")
	}
	if !strings.Contains(logs.String(), "last user message exceeds hard context limit") {
		t.Errorf("oversized user message not logged:\n%s", logs.String())
	}

	// Without a window there is no limit to apply.
	noWindow := asManager(t, New("bob", newMockStore(), WithContextWindow(0)))
	stored := []memory.StoredMessage{{Seq: 1, Role: "assistant", Content: big}}
	noWindow.applyLargeMsgChecksStored(stored)
	if stored[0].Content != big {
		t.Error("message truncated without a context window")
	}
}

// TestCompact_OnlySystemMessage reports nothing to compress when the window
// holds only the system message.
func TestCompact_OnlySystemMessage(t *testing.T) {
	store := session.NewMemory()
	_, err := store.AddFullMessage("alice", spawnllm.Message{Role: "system", Content: "rules"})
	noErr(t, err)
	mgr := asManager(t, New("alice", store, WithModelCaller(chainOf([]*mockLLM{{}}))))
	noErr(t, mgr.Compact(context.Background()))
	if r := mgr.LastCompactionReport(); r == nil || r.Outcome != "nothing" {
		t.Errorf("report = %+v, want outcome nothing", r)
	}
	if h := store.GetHistory("alice"); len(h) != 1 || h[0].Role != "system" {
		t.Errorf("history changed: %+v", h)
	}
}
