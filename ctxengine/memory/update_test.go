/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
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
// so the next write on the handle, and one on another handle, succeed, and
// the connection that replaces it still waits for a lock another connection
// holds instead of failing at once.
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
	assertWaitsForLock(t, ArchivePath(dir, "alice"), a)
}

// assertWaitsForLock holds the write lock of the database at path on another
// connection for a moment and checks that a write on a waits for it and
// succeeds.
func assertWaitsForLock(t *testing.T, path string, a *ArchiveStore) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	noErr(t, err)
	t.Cleanup(func() { noErr(t, db.Close()) })
	conn, err := db.Conn(context.Background())
	noErr(t, err)
	t.Cleanup(func() { noErr(t, conn.Close()) })
	_, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	noErr(t, err)
	released := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, err := conn.ExecContext(context.Background(), "COMMIT")
		released <- err
	}()
	noErr(t, setState(a, SessionState{Key: "alice", NextSeq: 3}))
	noErr(t, <-released)
}
