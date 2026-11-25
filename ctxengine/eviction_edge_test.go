/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

// TestToInt64 coerces every numeric form a decoded argument can take, and
// treats anything unparseable as 0 (the start of the file).
func TestToInt64(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int64
	}{
		{"float64", float64(42), 42},
		{"int64", int64(43), 43},
		{"int", 44, 44},
		{"json number", json.Number("45"), 45},
		{"bad json number", json.Number("4.5e"), 0},
		{"string", " 46 ", 46},
		{"bad string", "page two", 0},
		{"bool", true, 0},
		{"nil", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt64(tc.in); got != tc.want {
				t.Errorf("toInt64(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormToolCall_UnparseableArgs treats arguments that are not valid JSON
// as no arguments rather than a half-decoded map.
func TestNormToolCall_UnparseableArgs(t *testing.T) {
	name, args := normToolCall(spawnllm.ToolCall{Function: &spawnllm.FunctionCall{Name: "mcp__fs__file_read_bytes", Arguments: `{"path":`}})
	if name != "file_read_bytes" || args != nil {
		t.Errorf("normToolCall = %q, %v; want file_read_bytes, nil", name, args)
	}
}

// TestEvictLargeArgs_NoArguments leaves a call without arguments alone.
func TestEvictLargeArgs_NoArguments(t *testing.T) {
	tc := spawnllm.ToolCall{ID: "w1", Function: &spawnllm.FunctionCall{Name: "file_write", Arguments: ""}}
	if got := evictLargeArgs(&tc, 10, DefaultEvictionRoles()); got != nil {
		t.Errorf("evictLargeArgs = %+v, want nil", got)
	}
	if tc.Function.Arguments != "" {
		t.Errorf("arguments rewritten to %q", tc.Function.Arguments)
	}
}

// TestEvictionBudgetAndThreshold_Resolution derives the budget and the batch
// threshold from the window, using the default chars-per-token when none is
// set, and disables both without a window.
func TestEvictionBudgetAndThreshold_Resolution(t *testing.T) {
	noWindow := &Manager{cfg: managerConfig{eviction: EvictionPolicy{MinSweepPercent: 5}}}
	if got := noWindow.evictionBudgetBytes(); got != 0 {
		t.Errorf("budget without a window = %d, want 0", got)
	}
	if got := noWindow.evictionMinSweepBytes(); got != 0 {
		t.Errorf("threshold without a window = %d, want 0", got)
	}
	m := &Manager{cfg: managerConfig{contextWindow: 1000, eviction: EvictionPolicy{MinSweepPercent: 10}}}
	if got, want := m.evictionBudgetBytes(), int(1000*defaultCharsPerToken*0.40); got != want {
		t.Errorf("derived budget = %d, want %d", got, want)
	}
	if got, want := m.evictionMinSweepBytes(), int(1000*defaultCharsPerToken*10/100); got != want {
		t.Errorf("derived threshold = %d, want %d", got, want)
	}
}

// TestSweep_EmptyWindow returns nothing on a session with no history.
func TestSweep_EmptyWindow(t *testing.T) {
	if got := newEvictMgr(t, newSeqStore(nil), basePolicy()).SweepEvictions(context.Background()); got != nil {
		t.Errorf("events on an empty window = %+v", got)
	}
}

// TestSweep_EmptyReaderResultKept never evicts an empty reader result: there
// is nothing to reclaim.
func TestSweep_EmptyReaderResultKept(t *testing.T) {
	specs := make([]turnSpec, 0, 13)
	specs = append(specs, turnSpec{tool: "file_read_bytes", id: "r", args: map[string]any{"path": "a.md"}, content: ""})
	for range 12 {
		specs = append(specs, turnSpec{text: "t"})
	}
	store := newSeqStore(buildHistory(specs...))
	if got := newEvictMgr(t, store, basePolicy()).SweepEvictions(context.Background()); len(got) != 0 {
		t.Errorf("empty result evicted: %+v", got)
	}
}

// TestSweep_BudgetStopsOnceUnder evicts largest-first only until the reader
// bytes are back under the budget, keeping the rest.
func TestSweep_BudgetStopsOnceUnder(t *testing.T) {
	p := basePolicy()
	p.ProtectTurns = 0
	p.EvictTurns = 100
	p.BudgetBytes = 2500
	store := newSeqStore(buildHistory(
		turnSpec{tool: "file_read_bytes", id: "big", args: map[string]any{"path": "a.md"}, content: strings.Repeat("a", 2000)},
		turnSpec{tool: "file_read_bytes", id: "mid", args: map[string]any{"path": "b.md"}, content: strings.Repeat("b", 1000)},
		turnSpec{tool: "file_read_bytes", id: "small", args: map[string]any{"path": "c.md"}, content: strings.Repeat("c", 500)},
		turnSpec{text: "t"},
	))
	events := newEvictMgr(t, store, p).SweepEvictions(context.Background())
	if len(events) != 1 || events[0].Resource != "a.md" {
		t.Fatalf("events = %+v, want only the largest read evicted", events)
	}
	if isEvicted(findToolResult(store, "mid")) || isEvicted(findToolResult(store, "small")) {
		t.Error("reads under the budget were evicted")
	}
}

// TestSweep_BudgetEvictsSupersededFirst sheds a superseded read inside the
// protect window before a larger read that is still current.
func TestSweep_BudgetEvictsSupersededFirst(t *testing.T) {
	p := basePolicy()
	p.EvictTurns = 100
	p.BudgetBytes = 3600
	store := newSeqStore(buildHistory(
		turnSpec{tool: "file_read_bytes", id: "a", args: map[string]any{"path": "a.md"}, content: strings.Repeat("a", 3000)},
		turnSpec{text: "1"}, turnSpec{text: "2"}, turnSpec{text: "3"}, turnSpec{text: "4"},
		turnSpec{tool: "file_read_bytes", id: "b1", args: map[string]any{"path": "b.md"}, content: strings.Repeat("b", 500)},
		turnSpec{tool: "file_read_bytes", id: "b2", args: map[string]any{"path": "b.md"}, content: strings.Repeat("b", 500)},
	))
	events := newEvictMgr(t, store, p).SweepEvictions(context.Background())
	if len(events) != 1 || events[0].Reason != "superseded" || events[0].Resource != "b.md" {
		t.Fatalf("events = %+v, want only the superseded read of b.md", events)
	}
	if isEvicted(findToolResult(store, "a")) || isEvicted(findToolResult(store, "b2")) {
		t.Error("a current read was evicted")
	}
}

// TestSweep_ArgBytesCountTowardBatch counts evictable argument bytes toward
// the batch threshold: a large aged write payload alone clears it.
func TestSweep_ArgBytesCountTowardBatch(t *testing.T) {
	p := argPolicy()
	p.MinSweepPercent = 1 // 1% of a 10k-token window
	store := newSeqStore(bigWriteHistory(strings.Repeat("x", 60_000), 12))
	mgr := asManager(t, New("sess", store, WithContextWindow(10_000), WithEvictionPolicy(p)))
	asm, err := mgr.Assemble(context.Background(), AssembleRequest{})
	noErr(t, err)
	if len(asm.Evictions) != 1 || asm.Evictions[0].Reason != "args" {
		t.Fatalf("evictions = %+v, want the write payload", asm.Evictions)
	}
	if got := stringArgT(t, storedArgs(t, store, "w1"), "content"); !isEvicted(got) {
		t.Errorf("payload not evicted: %d bytes", len(got))
	}
}

// plainHistoryStore is a seqStore without SetHistoryWithSeqs, optionally
// failing its writes.
type plainHistoryStore struct {
	inner   *seqStore
	setErr  error
	saveErr error
}

func (s *plainHistoryStore) GetHistory(k string) []spawnllm.Message { return s.inner.GetHistory(k) }
func (s *plainHistoryStore) GetHistoryWithSeqs(k string) []memory.StoredMessage {
	return s.inner.GetHistoryWithSeqs(k)
}
func (s *plainHistoryStore) GetSummary(k string) string      { return s.inner.GetSummary(k) }
func (s *plainHistoryStore) SetSummary(k, v string) error    { return s.inner.SetSummary(k, v) }
func (s *plainHistoryStore) Save(string) error               { return s.saveErr }
func (s *plainHistoryStore) AddMessage(_, _, _ string) error { return nil }
func (s *plainHistoryStore) AddFullMessage(string, spawnllm.Message) (int64, error) {
	return 0, nil
}
func (s *plainHistoryStore) TruncateHistory(string, int) error      { return nil }
func (s *plainHistoryStore) SetPendingTurn(string) error            { return nil }
func (s *plainHistoryStore) ClearPendingTurn(string) error          { return nil }
func (s *plainHistoryStore) GetArchiveBounds(string) (int64, int64) { return 0, 0 }
func (s *plainHistoryStore) ListPendingSessions() ([]string, error) { return nil, nil }
func (s *plainHistoryStore) Close() error                           { return nil }
func (s *plainHistoryStore) SetHistory(k string, h []spawnllm.Message) error {
	if s.setErr != nil {
		return s.setErr
	}
	return s.inner.SetHistory(k, h)
}

// TestSweep_PlainStore persists the evictions through SetHistory on a store
// that cannot keep seqs, and reports no events when that write fails.
func TestSweep_PlainStore(t *testing.T) {
	history := func() *seqStore { return newSeqStore(staleReads(1, 5000)) }

	t.Run("persisted", func(t *testing.T) {
		store := &plainHistoryStore{inner: history()}
		mgr := asManager(t, New("sess", store, WithContextWindow(0), WithEvictionPolicy(basePolicy())))
		if events := mgr.SweepEvictions(context.Background()); len(events) != 1 {
			t.Fatalf("events = %+v, want 1", events)
		}
		if !isEvicted(findToolResult(store.inner, "ra")) {
			t.Error("eviction not written through SetHistory")
		}
	})

	t.Run("save fails", func(t *testing.T) {
		logs := &logCapture{}
		store := &plainHistoryStore{inner: history(), saveErr: errors.New("disk full")}
		mgr := asManager(t, New("sess", store, WithContextWindow(0), WithEvictionPolicy(basePolicy()), WithLogger(logs.logger())))
		if events := mgr.SweepEvictions(context.Background()); len(events) != 1 {
			t.Fatalf("events = %+v, want 1: the history write succeeded", events)
		}
		if !strings.Contains(logs.String(), "eviction save failed") {
			t.Errorf("save failure not logged:\n%s", logs.String())
		}
	})

	t.Run("write fails", func(t *testing.T) {
		logs := &logCapture{}
		store := &plainHistoryStore{inner: history(), setErr: errors.New("disk full")}
		mgr := asManager(t, New("sess", store, WithContextWindow(0), WithEvictionPolicy(basePolicy()), WithLogger(logs.logger())))
		if events := mgr.SweepEvictions(context.Background()); events != nil {
			t.Fatalf("events = %+v, want nil when nothing was persisted", events)
		}
		if isEvicted(findToolResult(store.inner, "ra")) {
			t.Error("content evicted although the write failed")
		}
		if !strings.Contains(logs.String(), "eviction persist failed") {
			t.Errorf("persist failure not logged:\n%s", logs.String())
		}
	})
}
