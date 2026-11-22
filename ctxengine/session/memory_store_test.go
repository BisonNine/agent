/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session

import (
	"fmt"
	"os"
	"sync"
	"testing"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

var _ SessionStore = (*MemoryStore)(nil)

func TestMemoryStore_DeleteSession(t *testing.T) {
	s := NewMemory()
	noErr(t, s.AddMessage("a", "user", "hello"))
	noErr(t, s.SetSummary("a", "sum"))
	noErr(t, s.SetPendingTurn("a"))
	noErr(t, s.SetCompactionState("a", memory.CompactionState{ActiveModelIndex: 3}))
	noErr(t, s.AddMessage("b", "user", "kept"))

	s.DeleteSession("a")
	s.DeleteSession("never-existed")

	if h := s.GetHistory("a"); len(h) != 0 {
		t.Errorf("history after delete = %+v", h)
	}
	if got := s.GetSummary("a"); got != "" {
		t.Errorf("summary after delete = %q", got)
	}
	if st, err := s.GetCompactionState("a"); err != nil || st != (memory.CompactionState{}) {
		t.Errorf("compaction state after delete = %+v, %v", st, err)
	}
	if keys, err := s.ListPendingSessions(); err != nil || len(keys) != 0 {
		t.Errorf("pending after delete = %v, %v", keys, err)
	}
	// A deleted session starts again from seq 1; others are untouched.
	if seq, err := s.AddFullMessage("a", spawnllm.Message{Role: "user", Content: "again"}); err != nil || seq != 1 {
		t.Errorf("seq after delete = %d, %v; want 1", seq, err)
	}
	if h := s.GetHistory("b"); len(h) != 1 || h[0].Content != "kept" {
		t.Errorf("other session = %+v", h)
	}
}

// TestMemoryStore_ForgetSessionConcurrent forgets a session while it is being
// written: no add is lost and no seq is reused.
func TestMemoryStore_ForgetSessionConcurrent(t *testing.T) {
	s := NewMemory()
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

// TestMemoryStore_WritesNothing runs every store operation with the working,
// temporary and home directories pointing at one empty, writable directory,
// so a write by relative or default path would land there, and checks that it
// is still empty.
func TestMemoryStore_WritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("HOME", dir)

	s := NewMemory()
	s.SetNoiseKey(func(string) (string, bool) { return "", false })
	msg := spawnllm.Message{Role: "user", Content: "x"}
	noErr(t, s.AddMessage("k", "user", "a"))
	_, err := s.AddFullMessage("k", msg)
	noErr(t, err)
	noErr(t, s.SetSummary("k", "s"))
	noErr(t, s.SetHistory("k", []spawnllm.Message{msg}))
	noErr(t, s.SetHistoryWithSeqs("k", s.GetHistoryWithSeqs("k")))
	summary := "s2"
	noErr(t, s.CommitCompaction("k", CompactionCommit{
		History:    s.GetHistoryWithSeqs("k"),
		Summary:    &summary,
		Checkpoint: &memory.SummaryRecord{Summary: summary},
	}))
	noErr(t, s.SetCompactionState("k", memory.CompactionState{MeaningfulCount: 1}))
	noErr(t, s.TruncateHistory("k", 1))
	noErr(t, s.SetPendingTurn("k"))
	_, err = s.ListPendingSessions()
	noErr(t, err)
	noErr(t, s.ClearPendingTurn("k"))
	s.GetArchiveBounds("k")
	noErr(t, s.Save("k"))
	s.ForgetSession("k")
	s.DeleteSession("k")
	noErr(t, s.Close())

	entries, err := os.ReadDir(dir)
	noErr(t, err)
	if len(entries) != 0 {
		t.Errorf("the store created %d entries, first %q", len(entries), entries[0].Name())
	}
}
