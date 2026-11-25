/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session

import (
	"bytes"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

// execDB runs stmts on the database at path through a connection of its own,
// to damage it the way a crash, a bug or another program could.
func execDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2000)")
	noErr(t, err)
	defer func() { noErr(t, db.Close()) }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// fileState returns the session state stored in key's file under dir.
func fileState(t *testing.T, dir, key string) memory.SessionState {
	t.Helper()
	a, err := memory.OpenSessionReadOnly(dir, key, nil)
	noErr(t, err)
	st, err := a.State()
	noErr(t, err)
	return st
}

func TestSQLiteStore_CorruptStateFailsWrites(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	s, err := NewSQLiteStore(dir, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	noErr(t, err)
	t.Cleanup(func() { noErr(t, s.Close()) })
	mustAdd(t, s, "alice", spawnllm.Message{Role: "user", Content: "hello"})
	execDB(t, memory.ArchivePath(dir, "alice"), `UPDATE session_state SET created_at = 'abc'`)

	msg := spawnllm.Message{Role: "user", Content: "x"}
	writes := map[string]func() error{
		"AddFullMessage":     func() error { _, err := s.AddFullMessage("alice", msg); return err },
		"SetSummary":         func() error { return s.SetSummary("alice", "s") },
		"SetHistory":         func() error { return s.SetHistory("alice", []spawnllm.Message{msg}) },
		"SetHistoryWithSeqs": func() error { return s.SetHistoryWithSeqs("alice", nil) },
		"CommitCompaction":   func() error { return s.CommitCompaction("alice", CompactionCommit{}) },
		"TruncateHistory":    func() error { return s.TruncateHistory("alice", 0) },
		"SetPendingTurn":     func() error { return s.SetPendingTurn("alice") },
		"SetCompactionState": func() error { return s.SetCompactionState("alice", memory.CompactionState{Cooling: true}) },
	}
	for name, write := range writes {
		if err := write(); err == nil {
			t.Errorf("%s wrote over a state it cannot read", name)
		}
	}
	if _, err := s.GetCompactionState("alice"); err == nil {
		t.Error("GetCompactionState read a corrupt state")
	}
	if got := s.GetSummary("alice"); got != "" {
		t.Errorf("GetSummary = %q, want empty", got)
	}
	if !strings.Contains(logs.String(), "get summary") || !strings.Contains(logs.String(), "component=session") {
		t.Errorf("log = %q, want a get summary warning tagged component=session", logs.String())
	}
	stored := s.GetHistoryWithSeqs("alice")
	if len(stored) != 1 || stored[0].Content != "hello" {
		t.Errorf("window = %+v, want the one message written before", stored)
	}
}

func TestSQLiteStore_MissingTables(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	mustAdd(t, s, "alice", spawnllm.Message{Role: "user", Content: "hello"})
	execDB(t, memory.ArchivePath(dir, "alice"),
		`DROP TABLE window`, `DROP TABLE messages_fts`, `DROP TABLE messages`)

	if err := s.TruncateHistory("alice", 1); err == nil {
		t.Error("TruncateHistory succeeded without a window table")
	}
	if minSeq, maxSeq := s.GetArchiveBounds("alice"); minSeq != 0 || maxSeq != 0 {
		t.Errorf("GetArchiveBounds = (%d, %d), want (0, 0)", minSeq, maxSeq)
	}
	if got := s.GetHistoryWithSeqs("alice"); got == nil || len(got) != 0 {
		t.Errorf("GetHistoryWithSeqs = %#v, want empty, not nil", got)
	}
}

func TestSQLiteStore_RewriteOfNewSessionStampsCreatedAt(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	before := time.Now()
	noErr(t, s.SetHistory("alice", []spawnllm.Message{{Role: "user", Content: "a"}}))
	noErr(t, s.CommitCompaction("bob", CompactionCommit{
		History: []memory.StoredMessage{memory.NewStoredMessage(3, spawnllm.Message{Role: "user", Content: "b"})},
	}))
	after := time.Now()
	s.ForgetSession("alice")
	s.ForgetSession("bob")

	for _, key := range []string{"alice", "bob"} {
		st := fileState(t, dir, key)
		if st.CreatedAt.Before(before) || st.CreatedAt.After(after) {
			t.Errorf("%s CreatedAt = %v, want in [%v, %v]", key, st.CreatedAt, before, after)
		}
		if !st.UpdatedAt.Equal(st.CreatedAt) {
			t.Errorf("%s UpdatedAt = %v, want CreatedAt %v", key, st.UpdatedAt, st.CreatedAt)
		}
	}
}

func TestListPendingSessions_SkipsUnreadable(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	noErr(t, s.SetPendingTurn("alice"))
	noErr(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "bob.archive.db")))
	noErr(t, os.WriteFile(filepath.Join(dir, "carol.archive.db"), bytes.Repeat([]byte("junk "), 200), 0o600))

	keys, err := s.ListPendingSessions()
	noErr(t, err)
	if len(keys) != 1 || keys[0] != "alice" {
		t.Errorf("keys = %v, want [alice]", keys)
	}
}

func TestListPendingSessions_NotADirectory(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	noErr(t, os.Remove(dir))
	noErr(t, os.WriteFile(dir, nil, 0o600))
	if _, err := s.ListPendingSessions(); err == nil {
		t.Error("ListPendingSessions of a file succeeded")
	}
}

func TestMigrateJSONL_NotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	noErr(t, os.WriteFile(file, nil, 0o600))
	if _, err := MigrateJSONL(file, nil); err == nil {
		t.Error("MigrateJSONL of a file succeeded")
	}
}

// migrateOne runs MigrateJSONL on dir and returns the error reported for name.
func migrateOne(t *testing.T, dir, name string) error {
	t.Helper()
	report, err := MigrateJSONL(dir, nil)
	noErr(t, err)
	if len(report.Migrated) != 0 || len(report.Skipped) != 0 {
		t.Errorf("report = %+v, want only an error", report)
	}
	return report.Errors[name]
}

// wantErrContains fails unless err is set and its text contains want.
func wantErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want one containing %q", err, want)
	}
}

func TestMigrateJSONL_UnreadableFiles(t *testing.T) {
	t.Run("meta", func(t *testing.T) {
		dir := t.TempDir()
		noErr(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "alice.meta.json")))
		wantErrContains(t, migrateOne(t, dir, "alice.meta.json"), "read meta")
	})
	t.Run("window missing", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, "alice.meta.json", `{"key":"alice"}`)
		noErr(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "alice.jsonl")))
		wantErrContains(t, migrateOne(t, dir, "alice"), "open window")
	})
	t.Run("window is a directory", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, "alice.meta.json", `{"key":"alice"}`)
		noErr(t, os.Mkdir(filepath.Join(dir, "target"), 0o700))
		noErr(t, os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "alice.jsonl")))
		wantErrContains(t, migrateOne(t, dir, "alice"), "scan window")
		if !exists(t, dir, "alice.meta.json") {
			t.Error("meta renamed although the window could not be read")
		}
	})
	t.Run("archive state", func(t *testing.T) {
		dir := t.TempDir()
		a, err := memory.OpenSession(dir, "alice")
		noErr(t, err)
		noErr(t, a.Update(func(tx *memory.StateTx) error { return tx.SetState(memory.SessionState{Key: "alice"}) }))
		noErr(t, a.Close())
		execDB(t, memory.ArchivePath(dir, "alice"), `UPDATE session_state SET created_at = 'abc'`)
		writeFixture(t, dir, "alice.meta.json", `{"key":"alice"}`)
		wantErrContains(t, migrateOne(t, dir, "alice"), "read state")
	})
}

func TestMigrateJSONL_SkipsBlankLines(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "alice.meta.json", `{"key":"alice","skip":1}`)
	writeFixture(t, dir, "alice.jsonl", "\n"+`{"role":"user","content":"gone"}`+"\n\n"+
		`{"role":"user","content":"one"}`+"\n\n"+`{"role":"assistant","content":"two"}`+"\n")
	report, err := MigrateJSONL(dir, nil)
	noErr(t, err)
	if len(report.Migrated) != 1 || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	a, err := memory.OpenSessionReadOnly(dir, "alice", nil)
	noErr(t, err)
	w, err := a.Window()
	noErr(t, err)
	// Blank lines are not counted: "one" and "two" are non-empty lines 2 and
	// 3, so with skip 1 they take seqs skip+lineNo = 3 and 4, as in
	// TestMigrateJSONL_MetaOnlyAndLegacySeqs.
	if len(w) != 2 || w[0].Seq != 3 || w[0].Content != "one" || w[1].Seq != 4 || w[1].Content != "two" {
		t.Errorf("window = %+v, want seq 3 one, seq 4 two", w)
	}
}

func TestMigrateJSONL_RenameFails(t *testing.T) {
	for _, tt := range []struct{ blocked, want string }{
		{"alice.jsonl.migrated", "rename window"},
		{"alice.meta.json.migrated", "rename meta"},
	} {
		t.Run(tt.blocked, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, dir, "alice.meta.json", `{"key":"alice"}`)
			writeFixture(t, dir, "alice.jsonl", `{"seq":1,"role":"user","content":"hello"}`+"\n")
			// A non-empty directory cannot be replaced by a rename.
			noErr(t, os.MkdirAll(filepath.Join(dir, tt.blocked, "x"), 0o700))
			wantErrContains(t, migrateOne(t, dir, "alice"), tt.want)
		})
	}
}

// legacyStateColumns is session_state as the archive schema declares it.
const legacyStateColumns = `id INTEGER PRIMARY KEY CHECK (id = 1), key TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '', next_seq INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
	meaningful_count INTEGER NOT NULL DEFAULT 0, compressed_at_meaningful_count INTEGER NOT NULL DEFAULT 0,
	cooling INTEGER NOT NULL DEFAULT 0, cooling_since_count INTEGER NOT NULL DEFAULT 0,
	summary_generated_at INTEGER NOT NULL DEFAULT 0, summary_model TEXT NOT NULL DEFAULT '',
	active_model_index INTEGER NOT NULL DEFAULT 0, expose_reasoning INTEGER NOT NULL DEFAULT 0,
	show_tool_activity INTEGER NOT NULL DEFAULT 0, pending_turn INTEGER NOT NULL DEFAULT 0`

// TestMigrateJSONL_WriteFailsRollsBack makes the archive refuse the window,
// then the state, and checks the migration reports it, writes nothing and
// leaves the sources in place for another run.
func TestMigrateJSONL_WriteFailsRollsBack(t *testing.T) {
	for _, tt := range []struct{ name, table, want string }{
		{"window", `CREATE TABLE window (seq INTEGER PRIMARY KEY, role TEXT NOT NULL, payload TEXT NOT NULL,
			created_at INTEGER NOT NULL, CHECK (seq < 0))`, "write window"},
		{"state", `CREATE TABLE session_state (` + legacyStateColumns + `, CHECK (next_seq = 0))`, "write state"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			execDB(t, memory.ArchivePath(dir, "alice"), tt.table)
			writeFixture(t, dir, "alice.meta.json", `{"key":"alice","next_seq":1}`)
			writeFixture(t, dir, "alice.jsonl", `{"seq":1,"role":"user","content":"hello"}`+"\n")

			wantErrContains(t, migrateOne(t, dir, "alice"), tt.want)
			for _, name := range []string{"alice.meta.json", "alice.jsonl"} {
				if !exists(t, dir, name) {
					t.Errorf("%s moved after a failed migration", name)
				}
			}
			a, err := memory.OpenSessionReadOnly(dir, "alice", nil)
			noErr(t, err)
			w, err := a.Window()
			noErr(t, err)
			st, err := a.State()
			noErr(t, err)
			if len(w) != 0 || st.NextSeq != 0 {
				t.Errorf("after the failed migration: window %d rows, next seq %d; want nothing written", len(w), st.NextSeq)
			}
		})
	}
}

func TestErrorsIsUnwrapped(t *testing.T) {
	// The migration wraps the archive's errors, so a caller can still match
	// them.
	dir := t.TempDir()
	writeFixture(t, dir, "a_b.meta.json", `{"key":"a:b"}`)
	a, err := memory.OpenSession(dir, "a/b")
	noErr(t, err)
	noErr(t, a.Update(func(tx *memory.StateTx) error { return tx.SetState(memory.SessionState{Key: "a/b", NextSeq: 1}) }))
	noErr(t, a.Close())
	report, err := MigrateJSONL(dir, nil)
	noErr(t, err)
	if !errors.Is(report.Errors["a:b"], memory.ErrKeyCollision) {
		t.Errorf("error = %v, want ErrKeyCollision", report.Errors["a:b"])
	}
}
