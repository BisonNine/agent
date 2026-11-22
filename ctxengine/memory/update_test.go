/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import (
	"errors"
	"testing"
)

// assertWritable checks that a and a second handle on the same file can both
// still write, so no connection kept the file's write lock.
func assertWritable(t *testing.T, dir string, a *ArchiveStore) {
	t.Helper()
	noErr(t, setState(a, SessionState{Key: "alice", NextSeq: 1}))
	b := openSessionArchive(t, dir, "alice")
	noErr(t, setState(b, SessionState{Key: "alice", NextSeq: 2}))
	st, err := a.State()
	noErr(t, err)
	if st.NextSeq != 2 {
		t.Errorf("NextSeq = %d, want 2", st.NextSeq)
	}
}

// TestUpdate_PanicRollsBack panics inside an Update: what it wrote is rolled
// back, the panic reaches the caller, and the file stays writable.
func TestUpdate_PanicRollsBack(t *testing.T) {
	dir := t.TempDir()
	a := openSessionArchive(t, dir, "alice")
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_ = a.Update(func(tx *StateTx) error { //nolint:errcheck // it panics
			noErr(t, tx.SetState(SessionState{Key: "alice", NextSeq: 9}))
			panic("boom")
		})
	}()
	st, err := a.State()
	noErr(t, err)
	if st.NextSeq != 0 {
		t.Errorf("the panicking Update was committed: NextSeq = %d", st.NextSeq)
	}
	assertWritable(t, dir, a)
}

// TestUpdate_FailedRollbackDiscardsConnection makes the rollback of a failed
// Update fail with the transaction still open: the connection is discarded,
// so the next write on the handle, and one on another handle, succeed.
func TestUpdate_FailedRollbackDiscardsConnection(t *testing.T) {
	rollbackStmt = "ROLLBACK_BROKEN"
	t.Cleanup(func() { rollbackStmt = "ROLLBACK" })

	dir := t.TempDir()
	a := openSessionArchive(t, dir, "alice")
	errFail := errors.New("fail")
	err := a.Update(func(tx *StateTx) error {
		noErr(t, tx.SetState(SessionState{Key: "alice", NextSeq: 9}))
		return errFail
	})
	if !errors.Is(err, errFail) {
		t.Fatalf("Update = %v, want %v", err, errFail)
	}
	rollbackStmt = "ROLLBACK"

	st, err := a.State()
	noErr(t, err)
	if st.NextSeq != 0 {
		t.Errorf("the failed Update was committed: NextSeq = %d", st.NextSeq)
	}
	assertWritable(t, dir, a)
}
