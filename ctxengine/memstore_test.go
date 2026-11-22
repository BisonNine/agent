/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

// The manager over session.MemoryStore: it runs the same conversation as over
// session.SQLiteStore with the same result, and touches no disk.

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"
)

// The manager finds these optional interfaces on the in-memory store.
var (
	_ CompactionStateStore = (*session.MemoryStore)(nil)
	_ CompactionCommitter  = (*session.MemoryStore)(nil)
	_ interface {
		SetHistoryWithSeqs(sessionKey string, history []memory.StoredMessage) error
	} = (*session.MemoryStore)(nil)
)

// summaryTimesRE matches the timestamps in a stored summary.
var summaryTimesRE = regexp.MustCompile(`"(\w+_at)":"[^"]*"`)

// renderedTimeRE matches the times a rendered summary prints, to the minute.
var renderedTimeRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC`)

// scenarioStore is what the scenario needs from a store besides the manager's
// use of it.
type scenarioStore interface {
	session.SessionStore
	CompactionStateStore
}

// scenarioResult is everything observable from one run of runScenario, with
// the wall-clock timestamps cleared once checked to be set.
type scenarioResult struct {
	Seqs       []int64
	Assemblies [][]spawnllm.Message
	Evictions  []EvictionEvent
	Compacted  bool
	Pending    []string
	History    []memory.StoredMessage
	Summary    string
	Rendered   string
	State      memory.CompactionState
	Stats      ContextStats
}

// runScenario drives one session through the manager: a pending-turn mark, a
// file read that goes stale and is evicted by Assemble, enough conversation
// for an on-demand compaction with a scripted summariser, the turn cleared,
// and Close.
func runScenario(t *testing.T, store scenarioStore) scenarioResult {
	t.Helper()
	ctx := context.Background()
	var r scenarioResult
	mgr := asManager(t, New(e2eKey, store,
		WithContextWindow(20_000),
		WithMessageThreshold(0),
		WithModelCaller(&e2eSummarizer{model: "e2e-model"}),
	))
	add := func(seq int64, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Add*: %v", err)
		}
		r.Seqs = append(r.Seqs, seq)
	}
	assemble := func() {
		t.Helper()
		asm, err := mgr.Assemble(ctx, e2eRequest())
		noErr(t, err)
		for i := range asm.Messages {
			asm.Messages[i].Content = renderedTimeRE.ReplaceAllString(asm.Messages[i].Content, "TIME")
		}
		r.Assemblies = append(r.Assemblies, asm.Messages)
		r.Evictions = append(r.Evictions, asm.Evictions...)
		r.Compacted = r.Compacted || asm.Compacted
	}

	noErr(t, store.SetPendingTurn(e2eKey))
	add(mgr.AddUserMessage(ctx, spawnllm.Message{Role: "user", Content: "read chapter one"}))
	add(mgr.AddToolCallMessage(ctx, spawnllm.Message{Role: "assistant", ToolCalls: []spawnllm.ToolCall{
		e2eCall("read-1", "file_read_bytes", `{"path":"notes/ch1.md"}`),
	}}))
	add(mgr.AddToolResult(ctx, spawnllm.Message{
		Role: "tool", ToolCallID: "read-1", Content: "chapter one begins here " + strings.Repeat("x", 4976),
	}))
	add(mgr.AddAssistantMessage(ctx, spawnllm.Message{Role: "assistant", Content: "chapter one is about a journey"}))
	assemble()
	for i := 1; i <= 12; i++ {
		add(mgr.AddUserMessage(ctx, spawnllm.Message{Role: "user", Content: fmt.Sprintf("follow-up %d %s", i, strings.Repeat("q", 600))}))
		add(mgr.AddAssistantMessage(ctx, spawnllm.Message{Role: "assistant", Content: fmt.Sprintf("answer %d %s", i, strings.Repeat("a", 600))}))
		assemble()
	}
	noErr(t, mgr.Compact(ctx))
	add(mgr.AddUserMessage(ctx, spawnllm.Message{Role: "user", Content: "and now?"}))
	assemble()
	r.Rendered = renderedTimeRE.ReplaceAllString(mgr.RenderedSummary(), "TIME")
	r.Stats = mgr.Stats()
	if r.Stats.LastCompressedAt.IsZero() {
		t.Error("Stats.LastCompressedAt is zero after a compaction")
	}
	r.Stats.LastCompressedAt = time.Time{}
	// The summary's size includes its timestamps, whose printed length varies.
	if r.Stats.SummaryTokens == 0 {
		t.Error("Stats.SummaryTokens is zero after a compaction")
	}
	r.Stats.SummaryTokens = 0
	noErr(t, store.ClearPendingTurn(e2eKey))
	noErr(t, mgr.Close(ctx))

	pending, err := store.ListPendingSessions()
	noErr(t, err)
	r.Pending = pending
	r.History = store.GetHistoryWithSeqs(e2eKey)
	for i := range r.History {
		if r.History[i].CreatedAt.IsZero() {
			t.Errorf("history[%d] has no CreatedAt", i)
		}
		r.History[i].CreatedAt = time.Time{}
	}
	r.Summary = summaryTimesRE.ReplaceAllString(store.GetSummary(e2eKey), `"$1":""`)
	r.State, err = store.GetCompactionState(e2eKey)
	noErr(t, err)
	if r.State.SummaryGeneratedAt.IsZero() {
		t.Error("SummaryGeneratedAt is zero after a compaction")
	}
	r.State.SummaryGeneratedAt = time.Time{}
	return r
}

// TestMemoryStore_SameAsSQLite runs the scenario over both stores and checks
// that it exercised eviction and compaction and came out the same.
func TestMemoryStore_SameAsSQLite(t *testing.T) {
	sqlite, err := session.NewSQLiteStore(t.TempDir())
	noErr(t, err)
	t.Cleanup(func() { noErr(t, sqlite.Close()) })

	want := runScenario(t, sqlite)
	got := runScenario(t, session.NewMemory())

	if len(want.Evictions) == 0 {
		t.Error("the scenario evicted nothing")
	}
	if want.Summary == "" || want.Rendered == "" {
		t.Error("the scenario produced no summary")
	}
	if len(want.Pending) != 0 {
		t.Errorf("pending after clear = %v", want.Pending)
	}
	v := reflect.ValueOf(want)
	g := reflect.ValueOf(got)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if !reflect.DeepEqual(g.Field(i).Interface(), v.Field(i).Interface()) {
			t.Errorf("%s differs:\nmemory: %+v\nsqlite: %+v", name, g.Field(i).Interface(), v.Field(i).Interface())
		}
	}
}

// TestMemoryStore_NoDisk runs the scenario over the in-memory store with the
// working, temporary and home directories all pointing at one empty, writable
// directory, so a write by relative or default path would land there, and
// checks that it is still empty.
func TestMemoryStore_NoDisk(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("HOME", dir)

	r := runScenario(t, session.NewMemory())
	if len(r.Evictions) == 0 || r.Summary == "" {
		t.Errorf("scenario incomplete: %d evictions, summary %q", len(r.Evictions), r.Summary)
	}
	entries, err := os.ReadDir(dir)
	noErr(t, err)
	if len(entries) != 0 {
		t.Errorf("the run created %d entries, first %q", len(entries), entries[0].Name())
	}
}
