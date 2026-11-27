/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// oldSchemaFile writes a session file as a version without the project
// column left it: one state row, then the column dropped.
func oldSchemaFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alice.archive.db")
	a, err := Open(path)
	noErr(t, err)
	st := SessionState{
		Key: "alice", Summary: "Bob asked about the garden.", NextSeq: 3, CreatedAt: time.Unix(10, 0).UTC(),
		Compaction: CompactionState{MeaningfulCount: 4, ActiveModelIndex: 2, ExposeReasoning: true, BillingCode: "dropped"},
	}
	noErr(t, setState(a, st))
	noErr(t, a.Close())
	db, err := sql.Open("sqlite", archiveDSN(path, ""))
	noErr(t, err)
	if _, err := db.Exec("ALTER TABLE session_state DROP COLUMN project"); err != nil {
		t.Fatal(err)
	}
	noErr(t, db.Close())
	return path
}

// An old file opened read-only reads its billing code as "" and the rest as
// written; opened writable, it gains the column, keeps its row, and stores a
// billing code from then on. The column is added only once.
func TestProjectColumn_OldSchema(t *testing.T) {
	path := oldSchemaFile(t)

	ro, err := OpenReadOnly(path, nil)
	noErr(t, err)
	st, err := ro.State()
	noErr(t, err)
	if st.Key != "alice" || st.Compaction.MeaningfulCount != 4 || st.Compaction.ActiveModelIndex != 2 ||
		!st.Compaction.ExposeReasoning || st.Compaction.BillingCode != "" {
		t.Fatalf("read-only state = %+v", st)
	}

	a, err := Open(path)
	noErr(t, err)
	st, err = a.State()
	noErr(t, err)
	if st.Key != "alice" || st.Summary != "Bob asked about the garden." || st.Compaction.BillingCode != "" {
		t.Fatalf("state after upgrade = %+v", st)
	}
	st.Compaction.BillingCode = "pentest-acme"
	noErr(t, setState(a, st))
	noErr(t, a.Close())

	// Opened again: the column is there, nothing is added twice.
	a, err = Open(path)
	noErr(t, err)
	st, err = a.State()
	noErr(t, err)
	if st.Compaction.BillingCode != "pentest-acme" {
		t.Fatalf("billing code = %q", st.Compaction.BillingCode)
	}
	noErr(t, a.Close())
	ro, err = OpenReadOnly(path, nil)
	noErr(t, err)
	if st, err = ro.State(); err != nil || st.Compaction.BillingCode != "pentest-acme" {
		t.Fatalf("read-only state = %+v, %v", st, err)
	}
}

// A new file has the column, and a state row round-trips the billing code.
func TestProjectColumn_NewFile(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "bob.archive.db"))
	noErr(t, err)
	defer func() { noErr(t, a.Close()) }()
	noErr(t, setState(a, SessionState{Key: "bob", Compaction: CompactionState{BillingCode: "garden"}}))
	st, err := a.State()
	noErr(t, err)
	if st.Compaction.BillingCode != "garden" {
		t.Fatalf("billing code = %q", st.Compaction.BillingCode)
	}
}

// A column that cannot be added fails the open, and the transaction that
// tried is rolled back: the file opens once the column is fixed.
func TestProjectColumn_UpgradeFails(t *testing.T) {
	path := oldSchemaFile(t)
	saved := addedColumns
	t.Cleanup(func() { addedColumns = saved })
	// NOT NULL without a default cannot be added to a table with rows.
	addedColumns = append(append([]struct{ name, def string }{}, saved...), struct{ name, def string }{"broken", "TEXT NOT NULL"})
	if _, err := Open(path); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("Open = %v, want ErrArchiveUnavailable", err)
	}
	addedColumns = saved
	a, err := Open(path)
	noErr(t, err)
	st, err := a.State()
	noErr(t, err)
	if st.Key != "alice" || st.Compaction.BillingCode != "" {
		t.Fatalf("state = %+v", st)
	}
	noErr(t, a.Close())
}

// A database that cannot be queried fails the column upgrade and a state
// read, before either reads a row.
func TestProjectColumn_Closed(t *testing.T) {
	db, err := sql.Open("sqlite", archiveDSN(filepath.Join(t.TempDir(), "x.db"), ""))
	noErr(t, err)
	noErr(t, db.Close())
	if err := addMissingColumns(db); err == nil {
		t.Error("addMissingColumns on a closed database succeeded")
	}
	if _, err := readState(db); err == nil {
		t.Error("readState on a closed database succeeded")
	}
}
