/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import "testing"

// noErr fails the test when err is not nil. It must be called from the test's
// own goroutine.
func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// setState writes st in its own Update.
func setState(a *ArchiveStore, st SessionState) error {
	return a.Update(func(tx *StateTx) error { return tx.SetState(st) })
}

// appendWindow inserts msg and writes st in one Update.
func appendWindow(a *ArchiveStore, msg StoredMessage, st SessionState) error {
	return a.Update(func(tx *StateTx) error {
		if err := tx.AppendWindow(msg); err != nil {
			return err
		}
		return tx.SetState(st)
	})
}

// replaceWindowWith replaces the window with msgs and writes st in one Update.
func replaceWindowWith(a *ArchiveStore, msgs []StoredMessage, st SessionState) error {
	return a.Update(func(tx *StateTx) error {
		if err := tx.ReplaceWindow(msgs); err != nil {
			return err
		}
		return tx.SetState(st)
	})
}

// truncateWindowTo keeps the newest keepLast window rows and writes st in one
// Update.
func truncateWindowTo(a *ArchiveStore, keepLast int, st SessionState) error {
	return a.Update(func(tx *StateTx) error {
		if err := tx.TruncateWindow(keepLast); err != nil {
			return err
		}
		return tx.SetState(st)
	})
}

// commitCompaction replaces the window, writes st and appends checkpoint when
// set, in one Update, as session.SQLiteStore.CommitCompaction does.
func commitCompaction(a *ArchiveStore, msgs []StoredMessage, st SessionState, checkpoint *SummaryRecord) error {
	return a.Update(func(tx *StateTx) error {
		if err := tx.ReplaceWindow(msgs); err != nil {
			return err
		}
		if err := tx.SetState(st); err != nil {
			return err
		}
		if checkpoint == nil {
			return nil
		}
		return tx.AppendSummary(*checkpoint)
	})
}
