/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session

import (
	"os"
	"testing"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

// TestCommitCompaction_Checkpoint checks the part of a commit the
// conformance suite cannot see: the checkpoint lands in the archive's summary
// log, and a drop-only commit adds none.
func TestCommitCompaction_Checkpoint(t *testing.T) {
	s := newStore(t)
	for i := range 5 {
		mustAdd(t, s, "c", spawnllm.Message{Role: "user", Content: string(rune('a' + i))})
	}
	summary := "the summary"
	err := s.CommitCompaction("c", CompactionCommit{
		History: s.GetHistoryWithSeqs("c")[3:],
		Summary: &summary,
		Checkpoint: &memory.SummaryRecord{
			Model: "m", SourceSeqStart: 1, SourceSeqEnd: 3, CoveredSeqStart: 1, CoveredSeqEnd: 3, Summary: summary,
		},
	})
	noErr(t, err)
	a, err := memory.OpenReadOnly(memory.ArchivePath(s.dir, "c"), nil)
	noErr(t, err)
	metas, err := a.ListSummaries()
	if err != nil || len(metas) != 1 || metas[0].Model != "m" || metas[0].SourceSeqEnd != 3 {
		t.Fatalf("checkpoints = %+v, %v; want one from model m covering #1-#3", metas, err)
	}

	noErr(t, s.CommitCompaction("c", CompactionCommit{History: s.GetHistoryWithSeqs("c")[1:]}))
	metas, err = a.ListSummaries()
	noErr(t, err)
	if len(metas) != 1 {
		t.Errorf("checkpoints after drop-only commit = %d, want still 1", len(metas))
	}
}

// TestWriteErrorsAreReturned replaces the store directory with a file so
// every write fails (a missing directory would be recreated), and checks each
// write method reports it instead of swallowing it.
func TestWriteErrorsAreReturned(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	msg := spawnllm.Message{Role: "user", Content: "x"}
	if seq, err := s.AddFullMessage("w", msg); err == nil || seq != 0 {
		t.Errorf("AddFullMessage = %d, %v; want 0 and an error", seq, err)
	}
	if err := s.AddMessage("w", "user", "x"); err == nil {
		t.Error("AddMessage returned nil on a failed write")
	}
	if err := s.SetSummary("w", "s"); err == nil {
		t.Error("SetSummary returned nil on a failed write")
	}
	if err := s.SetHistory("w", []spawnllm.Message{msg}); err == nil {
		t.Error("SetHistory returned nil on a failed write")
	}
	if err := s.SetHistoryWithSeqs("w", []memory.StoredMessage{memory.NewStoredMessage(1, msg)}); err == nil {
		t.Error("SetHistoryWithSeqs returned nil on a failed write")
	}
	if err := s.TruncateHistory("w", 0); err == nil {
		t.Error("TruncateHistory returned nil on a failed write")
	}
	if err := s.CommitCompaction("w", CompactionCommit{}); err == nil {
		t.Error("CommitCompaction returned nil on a failed write")
	}
}
