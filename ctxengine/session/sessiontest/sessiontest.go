/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package sessiontest is the conformance suite for session stores: the
// behaviour the ctxengine manager relies on from session.SessionStore and the
// optional interfaces it uses when a store has them. session.SQLiteStore and
// session.MemoryStore both run it; a third implementation should too.
package sessiontest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"
)

// Store is what the suite exercises: the SessionStore contract plus the
// optional methods the ctxengine manager uses when a store has them
// (ctxengine.CompactionStateStore, ctxengine.CompactionCommitter and the
// seq-preserving window rewrite), and the noise and cache controls a host
// calls.
type Store interface {
	session.SessionStore
	GetCompactionState(sessionKey string) (memory.CompactionState, error)
	SetCompactionState(sessionKey string, state memory.CompactionState) error
	SetHistoryWithSeqs(sessionKey string, history []memory.StoredMessage) error
	CommitCompaction(sessionKey string, c session.CompactionCommit) error
	SetNoiseKey(fn memory.NoiseKeyFunc)
	ForgetSession(key string)
}

// Run runs the conformance suite against stores from newStore, which must
// return a fresh, empty store for each call and arrange its cleanup with t.
func Run(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(check)
	}{
		{"AddAndGetHistory", check.testAddAndGetHistory},
		{"MessageRoundTrip", check.testMessageRoundTrip},
		{"CallerMemoryNotShared", check.testCallerMemoryNotShared},
		{"SeqMinting", check.testSeqMinting},
		{"CreatedAtStamped", check.testCreatedAtStamped},
		{"UnknownSession", check.testUnknownSession},
		{"SessionIsolation", check.testSessionIsolation},
		{"KeysNeverShareHistory", check.testKeysNeverShareHistory},
		{"Summary", check.testSummary},
		{"TruncateHistory", check.testTruncateHistory},
		{"SetHistoryFreshSeqs", check.testSetHistoryFreshSeqs},
		{"SetHistoryWithSeqsPreserves", check.testSetHistoryWithSeqsPreserves},
		{"SetHistoryWithSeqsMonotonic", check.testSetHistoryWithSeqsMonotonic},
		{"SetHistoryWithSeqsOrdersBySeq", check.testSetHistoryWithSeqsOrdersBySeq},
		{"UnencodableMessageRejected", check.testUnencodableMessageRejected},
		{"MeaningfulCount", check.testMeaningfulCount},
		{"NoiseKey", check.testNoiseKey},
		{"ForgetSession", check.testForgetSession},
		{"ForgetSessionConcurrent", check.testForgetSessionConcurrent},
		{"CompactionStateRoundTrip", check.testCompactionStateRoundTrip},
		{"CommitCompaction", check.testCommitCompaction},
		{"PendingTurn", check.testPendingTurn},
		{"ArchiveBoundsWithoutArchive", check.testArchiveBoundsWithoutArchive},
		{"SaveAndClose", check.testSaveAndClose},
		{"ConcurrentSessions", check.testConcurrentSessions},
		{"ConcurrentReadersDuringWrites", check.testConcurrentReadersDuringWrites},
		{"ConcurrentRewrites", check.testConcurrentRewrites},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(check{t: t, s: newStore(t)})
		})
	}
}

// check is one test of the suite: its test and the store under test.
type check struct {
	t *testing.T
	s Store
}

// noErr fails the test when err is not nil. It must be called from the test's
// own goroutine.
func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// mustAdd appends msg and returns its seq, failing the test on a write error.
func mustAdd(t *testing.T, s Store, key string, msg spawnllm.Message) int64 {
	t.Helper()
	seq, err := s.AddFullMessage(key, msg)
	if err != nil {
		t.Fatalf("AddFullMessage(%s): %v", key, err)
	}
	return seq
}

func user(content string) spawnllm.Message {
	return spawnllm.Message{Role: "user", Content: content}
}

func seqsOf(stored []memory.StoredMessage) []int64 {
	out := make([]int64, len(stored))
	for i, sm := range stored {
		out[i] = sm.Seq
	}
	return out
}

func (c check) testAddAndGetHistory() {
	t, s := c.t, c.s
	noErr(t, s.AddMessage("s1", "user", "hello"))
	noErr(t, s.AddMessage("s1", "assistant", "hi"))

	history := s.GetHistory("s1")
	if len(history) != 2 {
		t.Fatalf("got %d messages, want 2", len(history))
	}
	if history[0].Role != "user" || history[0].Content != "hello" {
		t.Errorf("msg[0] = %+v", history[0])
	}
	if history[1].Role != "assistant" || history[1].Content != "hi" {
		t.Errorf("msg[1] = %+v", history[1])
	}
}

// testMessageRoundTrip stores a message with every persisted field set and
// reads it back unchanged.
func (c check) testMessageRoundTrip() {
	t, s := c.t, c.s
	call := spawnllm.Message{
		Role:             "assistant",
		Content:          "done",
		Source:           "web",
		Type:             "chat",
		Media:            []string{"media://1"},
		Attachments:      []spawnllm.MessageAttachment{{Filename: "a.txt", SHA256: "ab", Size: 3}},
		ReasoningContent: "because",
		ToolCalls: []spawnllm.ToolCall{
			{ID: "tc1", Type: "function", Function: &spawnllm.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`}},
		},
		ResponsesReasoning: []json.RawMessage{json.RawMessage(`{"id":"r1"}`)},
		AnthropicThinking:  []json.RawMessage{json.RawMessage(`{"type":"thinking"}`)},
	}
	result := spawnllm.Message{Role: "tool", Content: "ok", ToolCallID: "tc1", ToolReferences: []string{"web_fetch"}}
	if seq := mustAdd(t, s, "s1", call); seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	mustAdd(t, s, "s1", result)

	history := s.GetHistory("s1")
	if len(history) != 2 {
		t.Fatalf("got %d, want 2", len(history))
	}
	if !reflect.DeepEqual(history[0], call) {
		t.Errorf("tool call message:\n got %+v\nwant %+v", history[0], call)
	}
	if !reflect.DeepEqual(history[1], result) {
		t.Errorf("tool result = %+v, want %+v", history[1], result)
	}

	// Fields that are not persisted (the parsed form of a tool call) are not
	// kept: a message reads back as it would after a restart.
	parsed := spawnllm.Message{Role: "assistant", ToolCalls: []spawnllm.ToolCall{
		{ID: "tc2", Name: "f", Arguments: map[string]any{"a": 1.0}, ThoughtSignature: "sig"},
	}}
	mustAdd(t, s, "s1", parsed)
	got := s.GetHistory("s1")[2].ToolCalls[0]
	if got.ID != "tc2" || got.Name != "" || got.Arguments != nil || got.ThoughtSignature != "" {
		t.Errorf("unpersisted tool call fields kept: %+v", got)
	}
}

// testCallerMemoryNotShared checks that neither the message a caller stored
// nor the history it read aliases the store's copy.
func (c check) testCallerMemoryNotShared() {
	t, s := c.t, c.s
	msg := spawnllm.Message{
		Role:      "assistant",
		ToolCalls: []spawnllm.ToolCall{{ID: "tc1", Function: &spawnllm.FunctionCall{Name: "f", Arguments: "{}"}}},
		Media:     []string{"m1"},
	}
	mustAdd(t, s, "k", msg)
	msg.ToolCalls[0].ID = "changed"
	msg.ToolCalls[0].Function.Name = "changed"
	msg.Media[0] = "changed"

	got := s.GetHistory("k")
	if got[0].ToolCalls[0].ID != "tc1" || got[0].ToolCalls[0].Function.Name != "f" || got[0].Media[0] != "m1" {
		t.Fatalf("stored message changed with the caller's: %+v", got[0])
	}
	got[0].ToolCalls[0].ID = "changed"
	got[0].Media[0] = "changed"
	again := s.GetHistory("k")
	if again[0].ToolCalls[0].ID != "tc1" || again[0].Media[0] != "m1" {
		t.Errorf("stored message changed with the history read: %+v", again[0])
	}
}

func (c check) testSeqMinting() {
	t, s := c.t, c.s
	for i := 1; i <= 5; i++ {
		if seq := mustAdd(t, s, "seq", user("msg")); seq != int64(i) {
			t.Errorf("AddFullMessage(%d) seq = %d", i, seq)
		}
	}
	stored := s.GetHistoryWithSeqs("seq")
	if got := seqsOf(stored); !slices.Equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("seqs = %v, want 1..5", got)
	}

	// Truncation preserves the seqs of the survivors and the counter.
	noErr(t, s.TruncateHistory("seq", 3))
	if got := seqsOf(s.GetHistoryWithSeqs("seq")); !slices.Equal(got, []int64{3, 4, 5}) {
		t.Fatalf("after truncate seqs = %v, want 3, 4, 5", got)
	}
	if seq := mustAdd(t, s, "seq", user("after")); seq != 6 {
		t.Errorf("seq after truncate = %d, want 6", seq)
	}
}

func (c check) testCreatedAtStamped() {
	t, s := c.t, c.s
	before := time.Now().UTC()
	noErr(t, s.AddMessage("stamp", "user", "hello"))
	mustAdd(t, s, "stamp", spawnllm.Message{Role: "assistant", Content: "hi"})
	after := time.Now().UTC()

	stored := s.GetHistoryWithSeqs("stamp")
	if len(stored) != 2 {
		t.Fatalf("expected 2 stored messages, got %d", len(stored))
	}
	for i, m := range stored {
		if m.CreatedAt.Before(before) || m.CreatedAt.After(after) {
			t.Errorf("stored[%d].CreatedAt = %v, want in [%v, %v]", i, m.CreatedAt, before, after)
		}
		if m.CreatedAt.Location() != time.UTC {
			t.Errorf("stored[%d].CreatedAt location = %v, want UTC", i, m.CreatedAt.Location())
		}
	}
}

// testUnknownSession checks what every read returns for a session nothing was
// written to: empty, never nil, slices and zero values.
func (c check) testUnknownSession() {
	t, s := c.t, c.s
	if h := s.GetHistory("nonexistent"); h == nil || len(h) != 0 {
		t.Errorf("GetHistory = %#v, want empty slice", h)
	}
	if h := s.GetHistoryWithSeqs("nonexistent"); h == nil || len(h) != 0 {
		t.Errorf("GetHistoryWithSeqs = %#v, want empty slice", h)
	}
	if got := s.GetSummary("nonexistent"); got != "" {
		t.Errorf("GetSummary = %q, want empty", got)
	}
	st, err := s.GetCompactionState("nonexistent")
	if err != nil || st != (memory.CompactionState{}) {
		t.Errorf("GetCompactionState = %+v, %v; want zero, nil", st, err)
	}
	if lo, hi := s.GetArchiveBounds("nonexistent"); lo != 0 || hi != 0 {
		t.Errorf("GetArchiveBounds = %d, %d; want 0, 0", lo, hi)
	}
	keys, err := s.ListPendingSessions()
	if err != nil || len(keys) != 0 {
		t.Errorf("ListPendingSessions = %v, %v; want none", keys, err)
	}
	s.ForgetSession("nonexistent")
}

func (c check) testSessionIsolation() {
	t, s := c.t, c.s
	noErr(t, s.AddMessage("s1", "user", "session1"))
	noErr(t, s.AddMessage("telegram:1/2", "user", "session2"))
	noErr(t, s.SetSummary("s1", "one"))

	if h := s.GetHistoryWithSeqs("s1"); len(h) != 1 || h[0].Content != "session1" || h[0].Seq != 1 {
		t.Errorf("s1: %+v", h)
	}
	if h := s.GetHistoryWithSeqs("telegram:1/2"); len(h) != 1 || h[0].Content != "session2" || h[0].Seq != 1 {
		t.Errorf("telegram:1/2: %+v", h)
	}
	if got := s.GetSummary("telegram:1/2"); got != "" {
		t.Errorf("summary leaked across sessions: %q", got)
	}
}

// testKeysNeverShareHistory writes keys that one file-name rule maps to one
// name (a:b, a/b, a_b). Two different keys never share a history: a store
// either keeps them apart or refuses the later ones, and a refused key reads
// empty. The first key is never refused.
func (c check) testKeysNeverShareHistory() {
	t, s := c.t, c.s
	keys := []string{"a:b", "a/b", "a_b"}
	noErr(t, s.AddMessage(keys[0], "user", keys[0]))
	noErr(t, s.SetSummary(keys[0], keys[0]))
	accepted := []bool{true, false, false}
	for i, key := range keys[1:] {
		accepted[i+1] = s.AddMessage(key, "user", key) == nil
	}
	for i, key := range keys {
		h := s.GetHistory(key)
		switch {
		case accepted[i] && (len(h) != 1 || h[0].Content != key):
			t.Errorf("%s history = %+v, want only its own message", key, h)
		case !accepted[i] && len(h) != 0:
			t.Errorf("refused %s reads %+v, want nothing", key, h)
		}
		if got := s.GetSummary(key); i > 0 && got != "" {
			t.Errorf("%s summary = %q, want none", key, got)
		}
	}
}

func (c check) testSummary() {
	t, s := c.t, c.s
	if got := s.GetSummary("s1"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	noErr(t, s.SetSummary("s1", "test summary"))
	if got := s.GetSummary("s1"); got != "test summary" {
		t.Errorf("got %q, want %q", got, "test summary")
	}
	noErr(t, s.SetSummary("s1", "replaced"))
	if got := s.GetSummary("s1"); got != "replaced" {
		t.Errorf("got %q, want %q", got, "replaced")
	}
	// SetSummary on a fresh session leaves its compaction state zero.
	st, err := s.GetCompactionState("s1")
	noErr(t, err)
	if st != (memory.CompactionState{}) {
		t.Errorf("fresh compaction state = %+v", st)
	}
}

func (c check) testTruncateHistory() {
	t, s := c.t, c.s
	for i := range 10 {
		noErr(t, s.AddMessage("trunc", "user", string(rune('a'+i))))
	}

	noErr(t, s.TruncateHistory("trunc", 4))
	history := s.GetHistory("trunc")
	if len(history) != 4 {
		t.Fatalf("expected 4, got %d", len(history))
	}
	if history[0].Content != "g" || history[3].Content != "j" {
		t.Errorf("kept = %+v", history)
	}

	// Keep more than exists keeps all.
	noErr(t, s.TruncateHistory("trunc", 100))
	if got := len(s.GetHistory("trunc")); got != 4 {
		t.Errorf("keep 100: got %d, want 4", got)
	}

	// A partial truncate leaves the counters alone.
	st, err := s.GetCompactionState("trunc")
	noErr(t, err)
	if st.MeaningfulCount != 10 {
		t.Errorf("MeaningfulCount after partial truncate = %d, want 10", st.MeaningfulCount)
	}

	// keepLast <= 0 empties the window and resets the compression counters.
	st.CompressedAtMeaningfulCount = 3
	st.Cooling = true
	st.ActiveModelIndex = 2
	st.Project = "garden"
	noErr(t, s.SetCompactionState("trunc", st))
	noErr(t, s.TruncateHistory("trunc", 0))
	if got := len(s.GetHistory("trunc")); got != 0 {
		t.Errorf("keep 0: got %d, want 0", got)
	}
	st, err = s.GetCompactionState("trunc")
	noErr(t, err)
	if st.MeaningfulCount != 0 || st.CompressedAtMeaningfulCount != 0 || st.Cooling {
		t.Errorf("counters not reset: %+v", st)
	}
	if st.ActiveModelIndex != 2 || st.Project != "garden" {
		t.Errorf("a reset cleared the host's model index or project: %+v", st)
	}
	// Seqs keep counting after a full reset, and a negative keep is a reset.
	if seq := mustAdd(t, s, "trunc", user("next")); seq != 11 {
		t.Errorf("seq after reset = %d, want 11", seq)
	}
	noErr(t, s.TruncateHistory("trunc", -1))
	if got := len(s.GetHistory("trunc")); got != 0 {
		t.Errorf("keep -1: got %d, want 0", got)
	}
}

func (c check) testSetHistoryFreshSeqs() {
	t, s := c.t, c.s
	for range 5 {
		noErr(t, s.AddMessage("replace", "user", "old"))
	}
	noErr(t, s.TruncateHistory("replace", 2))

	before := time.Now().UTC()
	noErr(t, s.SetHistory("replace", []spawnllm.Message{
		{Role: "user", Content: "new1"},
		{Role: "assistant", Content: "new2"},
	}))
	after := time.Now().UTC()

	stored := s.GetHistoryWithSeqs("replace")
	if len(stored) != 2 || stored[0].Content != "new1" || stored[1].Content != "new2" {
		t.Fatalf("history = %+v", stored)
	}
	// Fresh seqs continue after the old counter (5), never reuse.
	if got := seqsOf(stored); !slices.Equal(got, []int64{6, 7}) {
		t.Errorf("seqs = %v, want 6, 7", got)
	}
	for i, m := range stored {
		if m.CreatedAt.Before(before) || m.CreatedAt.After(after) {
			t.Errorf("stored[%d].CreatedAt = %v not stamped now", i, m.CreatedAt)
		}
	}
	if seq := mustAdd(t, s, "replace", user("x")); seq != 8 {
		t.Errorf("next seq = %d, want 8", seq)
	}

	// An empty history empties the window and keeps the counter.
	noErr(t, s.SetHistory("replace", nil))
	if got := len(s.GetHistory("replace")); got != 0 {
		t.Errorf("after empty SetHistory: %d messages", got)
	}
	if seq := mustAdd(t, s, "replace", user("y")); seq != 9 {
		t.Errorf("seq after empty SetHistory = %d, want 9", seq)
	}
}

func (c check) testSetHistoryWithSeqsPreserves() {
	t, s := c.t, c.s
	for i := range 5 {
		noErr(t, s.AddMessage("preserve", "user", string(rune('a'+i))))
	}
	tail := s.GetHistoryWithSeqs("preserve")[3:]
	noErr(t, s.SetHistoryWithSeqs("preserve", tail))

	stored := s.GetHistoryWithSeqs("preserve")
	if got := seqsOf(stored); !slices.Equal(got, []int64{4, 5}) {
		t.Fatalf("seqs not preserved: %v", got)
	}
	if !stored[0].CreatedAt.Equal(tail[0].CreatedAt) {
		t.Errorf("CreatedAt not carried over: %v vs %v", stored[0].CreatedAt, tail[0].CreatedAt)
	}
	if seq := mustAdd(t, s, "preserve", spawnllm.Message{Role: "assistant", Content: "next"}); seq != 6 {
		t.Errorf("next seq = %d, want 6", seq)
	}
}

// testSetHistoryWithSeqsMonotonic covers the two ways the counter could go
// backwards: a rewrite with lower seqs than the counter, and messages with no
// seq that must be minted above everything seen.
func (c check) testSetHistoryWithSeqsMonotonic() {
	t, s := c.t, c.s
	for range 5 {
		noErr(t, s.AddMessage("mono", "user", "m"))
	}
	// Only low seqs retained: the counter must stay at 5.
	noErr(t, s.SetHistoryWithSeqs("mono", []memory.StoredMessage{
		memory.NewStoredMessage(1, user("one")),
	}))
	if seq := mustAdd(t, s, "mono", user("six")); seq != 6 {
		t.Errorf("seq after low rewrite = %d, want 6", seq)
	}

	// Unnumbered messages are minted after the highest seen (here 9 > counter 6).
	noErr(t, s.SetHistoryWithSeqs("mono", []memory.StoredMessage{
		memory.NewStoredMessage(9, user("nine")),
		{Role: "assistant", Content: "unnumbered"},
	}))
	stored := s.GetHistoryWithSeqs("mono")
	if got := seqsOf(stored); !slices.Equal(got, []int64{9, 10}) {
		t.Fatalf("seqs = %v, want 9, 10", got)
	}
	if stored[1].CreatedAt.IsZero() {
		t.Error("minted message should be stamped")
	}
	if seq := mustAdd(t, s, "mono", user("x")); seq != 11 {
		t.Errorf("seq after mint = %d, want 11", seq)
	}
}

// testSetHistoryWithSeqsOrdersBySeq checks that the window reads back in seq
// order whatever order the rewrite supplied it in.
func (c check) testSetHistoryWithSeqsOrdersBySeq() {
	t, s := c.t, c.s
	noErr(t, s.SetHistoryWithSeqs("order", []memory.StoredMessage{
		memory.NewStoredMessage(7, user("seven")),
		memory.NewStoredMessage(3, user("three")),
		memory.NewStoredMessage(5, user("five")),
	}))
	stored := s.GetHistoryWithSeqs("order")
	if got := seqsOf(stored); !slices.Equal(got, []int64{3, 5, 7}) {
		t.Fatalf("seqs = %v, want 3, 5, 7", got)
	}
	if stored[0].Content != "three" || stored[2].Content != "seven" {
		t.Errorf("contents do not follow their seqs: %+v", stored)
	}
	if seq := mustAdd(t, s, "order", user("x")); seq != 8 {
		t.Errorf("next seq = %d, want 8", seq)
	}
}

// testUnencodableMessageRejected checks that a message that cannot be encoded
// fails each write with an error and leaves the session as it was.
func (c check) testUnencodableMessageRejected() {
	t, s := c.t, c.s
	const key = "bad"
	mustAdd(t, s, key, user("hello"))
	summary := "kept"
	noErr(t, s.SetSummary(key, summary))
	before, beforeErr := s.GetCompactionState(key)
	noErr(t, beforeErr)

	// A raw reasoning item that is not JSON cannot be marshalled.
	bad := spawnllm.Message{Role: "assistant", Content: "x", ResponsesReasoning: []json.RawMessage{json.RawMessage("{not json")}}
	newSummary := "replaced"
	writes := map[string]func() error{
		"AddFullMessage": func() error {
			seq, err := s.AddFullMessage(key, bad)
			if err == nil && seq != 0 {
				t.Errorf("AddFullMessage seq = %d, want 0", seq)
			}
			return err
		},
		"SetHistory": func() error { return s.SetHistory(key, []spawnllm.Message{user("ok"), bad}) },
		"SetHistoryWithSeqs": func() error {
			return s.SetHistoryWithSeqs(key, []memory.StoredMessage{memory.NewStoredMessage(5, bad)})
		},
		"CommitCompaction": func() error {
			return s.CommitCompaction(key, session.CompactionCommit{
				History:    []memory.StoredMessage{memory.NewStoredMessage(5, bad)},
				Summary:    &newSummary,
				Compaction: func(st *memory.CompactionState) { st.Cooling = true },
			})
		},
	}
	for name, write := range writes {
		if err := write(); err == nil {
			t.Errorf("%s accepted a message that cannot be encoded", name)
		}
	}

	stored := s.GetHistoryWithSeqs(key)
	if len(stored) != 1 || stored[0].Seq != 1 || stored[0].Content != "hello" {
		t.Errorf("window = %+v, want only seq 1 hello", stored)
	}
	if got := s.GetSummary(key); got != summary {
		t.Errorf("summary = %q, want %q", got, summary)
	}
	after, err := s.GetCompactionState(key)
	noErr(t, err)
	if after != before {
		t.Errorf("compaction state = %+v, want unchanged %+v", after, before)
	}
	if seq := mustAdd(t, s, key, user("next")); seq != 2 {
		t.Errorf("next seq = %d, want 2", seq)
	}
}

func (c check) testMeaningfulCount() {
	t, s := c.t, c.s
	for i := range 3 {
		noErr(t, s.AddMessage("mc", "user", string(rune('a'+i))))
	}
	// Two duplicates of the last message (same role and content) are noise.
	noErr(t, s.AddMessage("mc", "user", "c"))
	noErr(t, s.AddMessage("mc", "user", "c"))
	// Same content from another role is not.
	noErr(t, s.AddMessage("mc", "assistant", "c"))
	// Empty content is never noise.
	noErr(t, s.AddMessage("mc", "assistant", ""))
	noErr(t, s.AddMessage("mc", "assistant", ""))

	if got := len(s.GetHistory("mc")); got != 8 {
		t.Errorf("window = %d, want 8", got)
	}
	st, err := s.GetCompactionState("mc")
	noErr(t, err)
	if st.MeaningfulCount != 6 {
		t.Errorf("MeaningfulCount = %d, want 6", st.MeaningfulCount)
	}
}

// fireKey is a noise key recognising "fire <ts>: <job>" messages by job.
func fireKey(content string) (string, bool) {
	rest, ok := strings.CutPrefix(content, "fire ")
	if !ok {
		return "", false
	}
	_, job, ok := strings.Cut(rest, ": ")
	return job, ok
}

// testNoiseKey covers the store-level switch: with no noise key two fires of
// one job (different timestamps) are both meaningful; with the key installed
// the second is noise.
func (c check) testNoiseKey() {
	t, s := c.t, c.s
	fires := []string{"fire 00:00: check disk", "fire 01:00: check disk"}
	for _, f := range fires {
		noErr(t, s.AddMessage("off", "user", f))
	}
	st, err := s.GetCompactionState("off")
	noErr(t, err)
	if st.MeaningfulCount != 2 {
		t.Errorf("without a noise key MeaningfulCount = %d, want 2", st.MeaningfulCount)
	}

	s.SetNoiseKey(fireKey)
	for _, f := range fires {
		noErr(t, s.AddMessage("on", "user", f))
	}
	st, err = s.GetCompactionState("on")
	noErr(t, err)
	if st.MeaningfulCount != 1 {
		t.Errorf("with the noise key MeaningfulCount = %d, want 1", st.MeaningfulCount)
	}
}

// testForgetSession checks that ForgetSession keeps the session's data and
// drops only its noise cache, so a duplicate after it counts again.
func (c check) testForgetSession() {
	t, s := c.t, c.s
	const key = "telegram_42"
	noErr(t, s.AddMessage(key, "user", "hello"))
	noErr(t, s.SetSummary(key, "sum"))
	noErr(t, s.SetPendingTurn(key))

	s.ForgetSession(key)

	noErr(t, s.AddMessage(key, "user", "hello"))
	if hist := s.GetHistoryWithSeqs(key); len(hist) != 2 || hist[1].Seq != 2 {
		t.Fatalf("history altered by ForgetSession: %+v", hist)
	}
	if got := s.GetSummary(key); got != "sum" {
		t.Errorf("summary after ForgetSession = %q", got)
	}
	keys, err := s.ListPendingSessions()
	noErr(t, err)
	if !slices.Equal(keys, []string{key}) {
		t.Errorf("pending after ForgetSession = %v", keys)
	}
	st, err := s.GetCompactionState(key)
	noErr(t, err)
	if st.MeaningfulCount != 2 {
		t.Errorf("MeaningfulCount = %d, want 2 (cache dropped)", st.MeaningfulCount)
	}
}

// testForgetSessionConcurrent forgets a session while it is being written: no
// add is lost and no seq is reused.
func (c check) testForgetSessionConcurrent() {
	t, s := c.t, c.s
	const goroutines, perGoroutine = 10, 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				if err := s.AddMessage("k", "user", fmt.Sprintf("m%d", i)); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			s.ForgetSession("k")
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	stored := s.GetHistoryWithSeqs("k")
	if len(stored) != goroutines*perGoroutine {
		t.Fatalf("window = %d messages, want %d", len(stored), goroutines*perGoroutine)
	}
	for i, sm := range stored {
		if sm.Seq != int64(i+1) {
			t.Fatalf("stored[%d].Seq = %d", i, sm.Seq)
		}
	}
}

func (c check) testCompactionStateRoundTrip() {
	t, s := c.t, c.s
	const key = "agent:alice:main"
	noErr(t, s.AddMessage(key, "user", "hi"))

	// A local time with a monotonic reading reads back as the same instant in
	// UTC.
	generated := time.Now().Add(-time.Hour).In(time.FixedZone("X", 3600))
	want := memory.CompactionState{
		MeaningfulCount:             7,
		CompressedAtMeaningfulCount: 5,
		Cooling:                     true,
		CoolingSinceCount:           6,
		SummaryGeneratedAt:          generated,
		SummaryModel:                "m1",
		ActiveModelIndex:            2,
		ExposeReasoning:             true,
		ShowToolActivity:            true,
		Project:                     "pentest-acme",
	}
	noErr(t, s.SetCompactionState(key, want))
	got, err := s.GetCompactionState(key)
	noErr(t, err)
	if !got.SummaryGeneratedAt.Equal(generated) || got.SummaryGeneratedAt.Location() != time.UTC {
		t.Errorf("SummaryGeneratedAt = %v, want %v in UTC", got.SummaryGeneratedAt, generated)
	}
	want.SummaryGeneratedAt = time.Unix(0, generated.UnixNano()).UTC()
	if got != want {
		t.Errorf("compaction state round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	// The counters live beside the window, which they do not disturb.
	if h := s.GetHistory(key); len(h) != 1 || h[0].Content != "hi" {
		t.Errorf("history = %+v", h)
	}
	// The store owns the meaningful count from here on.
	noErr(t, s.AddMessage(key, "user", "more"))
	got, err = s.GetCompactionState(key)
	noErr(t, err)
	if got.MeaningfulCount != 8 {
		t.Errorf("MeaningfulCount after add = %d, want 8", got.MeaningfulCount)
	}
	if got.Project != "pentest-acme" {
		t.Errorf("Project after add = %q, want pentest-acme", got.Project)
	}
	// A fresh session's state can be written before any message.
	noErr(t, s.SetCompactionState("fresh", memory.CompactionState{ActiveModelIndex: 1, Project: "garden"}))
	if got, err := s.GetCompactionState("fresh"); err != nil || got.ActiveModelIndex != 1 || got.Project != "garden" {
		t.Errorf("fresh session state = %+v, %v", got, err)
	}
}

// testCommitCompaction writes a compaction result through the store and reads
// back the window with its seqs preserved, the summary, the compaction
// counters, and a seq counter that carries on.
func (c check) testCommitCompaction() {
	t, s := c.t, c.s
	for i := range 5 {
		mustAdd(t, s, "c", user(string(rune('a'+i))))
	}
	tail := s.GetHistoryWithSeqs("c")[3:] // seqs 4, 5
	summary := "the summary"
	err := s.CommitCompaction("c", session.CompactionCommit{
		History: tail,
		Summary: &summary,
		Checkpoint: &memory.SummaryRecord{
			Model: "m", SourceSeqStart: 1, SourceSeqEnd: 3, CoveredSeqStart: 1, CoveredSeqEnd: 3, Summary: summary,
		},
		Compaction: func(st *memory.CompactionState) {
			if st.MeaningfulCount != 5 {
				t.Errorf("Compaction saw MeaningfulCount %d, want the current 5", st.MeaningfulCount)
			}
			st.CompressedAtMeaningfulCount = 5
			st.Cooling = true
		},
	})
	noErr(t, err)

	if got := seqsOf(s.GetHistoryWithSeqs("c")); !slices.Equal(got, []int64{4, 5}) {
		t.Fatalf("window seqs = %v, want 4, 5", got)
	}
	if got := s.GetSummary("c"); got != summary {
		t.Errorf("summary = %q, want %q", got, summary)
	}
	st, err := s.GetCompactionState("c")
	noErr(t, err)
	if st.MeaningfulCount != 5 || st.CompressedAtMeaningfulCount != 5 || !st.Cooling {
		t.Errorf("compaction state = %+v, want meaningful 5, compressed_at 5, cooling", st)
	}
	if seq := mustAdd(t, s, "c", user("f")); seq != 6 {
		t.Errorf("seq after commit = %d, want 6", seq)
	}

	// A drop-only commit (no summary, no counters) leaves the summary and the
	// counters alone.
	noErr(t, s.CommitCompaction("c", session.CompactionCommit{History: s.GetHistoryWithSeqs("c")[1:]}))
	if got := s.GetSummary("c"); got != summary {
		t.Errorf("summary after drop-only commit = %q, want unchanged", got)
	}
	if got := seqsOf(s.GetHistoryWithSeqs("c")); !slices.Equal(got, []int64{5, 6}) {
		t.Errorf("window after drop-only commit = %v, want 5, 6", got)
	}
	st, err = s.GetCompactionState("c")
	noErr(t, err)
	if st.MeaningfulCount != 6 || st.CompressedAtMeaningfulCount != 5 || !st.Cooling {
		t.Errorf("compaction state after drop-only commit = %+v", st)
	}
}

func (c check) testPendingTurn() {
	t, s := c.t, c.s
	keys, err := s.ListPendingSessions()
	if err != nil || len(keys) != 0 {
		t.Fatalf("empty store: keys=%v err=%v", keys, err)
	}

	// Session A: set and left set.
	noErr(t, s.AddMessage("session-a", "user", "hello"))
	noErr(t, s.SetPendingTurn("session-a"))
	// Session B: set then cleared.
	noErr(t, s.AddMessage("session-b", "user", "hi"))
	noErr(t, s.SetPendingTurn("session-b"))
	noErr(t, s.ClearPendingTurn("session-b"))
	// Session C: never marked.
	noErr(t, s.AddMessage("session-c", "user", "hey"))
	// Session D: marked pending before any message exists, twice.
	noErr(t, s.SetPendingTurn("agent:bob:main"))
	noErr(t, s.SetPendingTurn("agent:bob:main"))
	// Session E: cleared without ever being set.
	noErr(t, s.ClearPendingTurn("session-e"))

	keys, err = s.ListPendingSessions()
	noErr(t, err)
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"agent:bob:main", "session-a"}) {
		t.Errorf("pending = %v, want agent:bob:main and session-a", keys)
	}

	// The flag does not touch the window or the counters.
	if h := s.GetHistoryWithSeqs("session-a"); len(h) != 1 || h[0].Seq != 1 {
		t.Errorf("window after SetPendingTurn = %+v", h)
	}
	if seq := mustAdd(t, s, "agent:bob:main", user("first")); seq != 1 {
		t.Errorf("first seq after a pending mark = %d, want 1", seq)
	}
	noErr(t, s.ClearPendingTurn("session-a"))
	noErr(t, s.ClearPendingTurn("agent:bob:main"))
	keys, err = s.ListPendingSessions()
	if err != nil || len(keys) != 0 {
		t.Errorf("after clearing all: keys=%v err=%v", keys, err)
	}
}

// testArchiveBoundsWithoutArchive checks that window writes are not the
// archive: the bounds stay (0, 0) until something archives.
func (c check) testArchiveBoundsWithoutArchive() {
	t, s := c.t, c.s
	noErr(t, s.AddMessage("s1", "user", "a"))
	noErr(t, s.SetHistoryWithSeqs("s1", []memory.StoredMessage{memory.NewStoredMessage(4, user("b"))}))
	if lo, hi := s.GetArchiveBounds("s1"); lo != 0 || hi != 0 {
		t.Errorf("bounds with empty archive = %d, %d; want 0, 0", lo, hi)
	}
}

func (c check) testSaveAndClose() {
	t, s := c.t, c.s
	noErr(t, s.AddMessage("k", "user", "a"))
	noErr(t, s.Save("k"))
	noErr(t, s.Save("unknown"))
	if got := len(s.GetHistory("k")); got != 1 {
		t.Errorf("after Save: %d messages, want 1", got)
	}
	noErr(t, s.Close())
	noErr(t, s.Close())
}

// testConcurrentSessions adds to many sessions at once: each session's seqs
// are its own, contiguous from 1, and its messages are all there in order.
func (c check) testConcurrentSessions() {
	t, s := c.t, c.s
	const sessions, perSession = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, sessions*perSession)
	for i := range sessions {
		key := fmt.Sprintf("s%d", i)
		wg.Go(func() {
			for j := range perSession {
				seq, err := s.AddFullMessage(key, user(fmt.Sprintf("%s-%d", key, j)))
				if err != nil {
					errs <- err
					return
				}
				if seq != int64(j+1) {
					errs <- fmt.Errorf("%s: message %d got seq %d", key, j, seq)
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := range sessions {
		key := fmt.Sprintf("s%d", i)
		stored := s.GetHistoryWithSeqs(key)
		if len(stored) != perSession {
			t.Fatalf("%s: %d messages, want %d", key, len(stored), perSession)
		}
		for j, sm := range stored {
			if sm.Seq != int64(j+1) || sm.Content != fmt.Sprintf("%s-%d", key, j) {
				t.Errorf("%s[%d] = seq %d %q", key, j, sm.Seq, sm.Content)
			}
		}
	}
}

// testConcurrentReadersDuringWrites reads one session while it is written:
// every read is a consistent prefix of the final window.
func (c check) testConcurrentReadersDuringWrites() {
	t, s := c.t, c.s
	const writes, readers = 60, 4
	done := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				stored := s.GetHistoryWithSeqs("rw")
				for i, sm := range stored {
					if sm.Seq != int64(i+1) || sm.Content != fmt.Sprintf("m%d", i+1) {
						errs <- fmt.Errorf("read saw %d: seq %d %q", i, sm.Seq, sm.Content)
						return
					}
				}
				_ = s.GetSummary("rw")
				_, _ = s.GetCompactionState("rw") //nolint:errcheck // only racing the writer
			}
		})
	}
	for i := 1; i <= writes; i++ {
		mustAdd(t, s, "rw", user(fmt.Sprintf("m%d", i)))
		if i%10 == 0 {
			noErr(t, s.SetSummary("rw", fmt.Sprintf("after %d", i)))
		}
	}
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := len(s.GetHistory("rw")); got != writes {
		t.Errorf("final window = %d, want %d", got, writes)
	}
}

// testConcurrentRewrites adds, reads, summarises and truncates one session
// from several goroutines, as an agent loop and a summariser do: no add is
// lost, so the counter ends at the number of adds.
func (c check) testConcurrentRewrites() {
	t, s := c.t, c.s
	var wg sync.WaitGroup
	const goroutines, perGoroutine = 10, 20
	errs := make(chan error, goroutines*perGoroutine+20)
	for range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				if err := s.AddMessage("concurrent", "user", fmt.Sprintf("msg %d", i)); err != nil {
					errs <- err
				}
				_ = s.GetHistory("concurrent")
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			if err := s.SetSummary("concurrent", "summary"); err != nil {
				errs <- err
			}
			if err := s.TruncateHistory("concurrent", 50); err != nil {
				errs <- err
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	stored := s.GetHistoryWithSeqs("concurrent")
	for i := 1; i < len(stored); i++ {
		if stored[i].Seq <= stored[i-1].Seq {
			t.Fatalf("seqs not increasing at %d: %v", i, seqsOf(stored))
		}
	}
	if seq := mustAdd(t, s, "concurrent", user("last")); seq != goroutines*perGoroutine+1 {
		t.Errorf("seq after the run = %d, want %d", seq, goroutines*perGoroutine+1)
	}
	if s.GetSummary("concurrent") != "summary" {
		t.Error("summary lost")
	}
}
