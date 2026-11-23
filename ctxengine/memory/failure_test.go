/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm"
)

// execRaw runs stmts on the store's write connection, bypassing the API, to
// damage the database the way a crash, a bug or another program could.
func execRaw(t *testing.T, a *ArchiveStore, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := a.db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// writeRawDB creates a SQLite database at path holding only what stmts create.
func writeRawDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", archiveDSN(path, ""))
	noErr(t, err)
	defer func() { noErr(t, db.Close()) }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// readOnlyDir makes dir unwritable for the test and restores it afterwards.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	noErr(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { noErr(t, os.Chmod(dir, 0o700)) })
}

// badMessage is a message json.Marshal refuses: its raw reasoning item is not
// JSON.
func badMessage() spawnllm.Message {
	return spawnllm.Message{Role: "assistant", Content: "x", ResponsesReasoning: []json.RawMessage{json.RawMessage("{not json")}}
}

func TestReads_CorruptPayloadFails(t *testing.T) {
	a := openTestArchive(t)
	execRaw(t, a,
		`INSERT INTO messages (seq, role, payload, text, created_at) VALUES (1, 'user', 'not json', 'hello', 1)`,
		`INSERT INTO window (seq, role, payload, created_at) VALUES (1, 'user', 'not json', 1)`)

	var syntaxErr *json.SyntaxError
	if _, err := a.QueryRange(1, 1); !errors.As(err, &syntaxErr) {
		t.Errorf("QueryRange error = %v, want a JSON syntax error", err)
	}
	if _, err := a.Search(context.Background(), "hello", "", 10); !errors.As(err, &syntaxErr) {
		t.Errorf("Search error = %v, want a JSON syntax error", err)
	}
	if _, err := a.Window(); !errors.As(err, &syntaxErr) {
		t.Errorf("Window error = %v, want a JSON syntax error", err)
	}
}

func TestReads_NonIntegerColumnFails(t *testing.T) {
	a := openTestArchive(t)
	execRaw(t, a,
		`INSERT INTO messages (seq, role, payload, text, created_at) VALUES (1, 'user', '{"role":"user"}', 'hello', 'abc')`,
		`INSERT INTO window (seq, role, payload, created_at) VALUES (1, 'user', '{"role":"user"}', 'abc')`,
		`INSERT INTO summaries (id, generated_at, summary) VALUES (1, 'abc', 'body')`)

	reads := map[string]func() error{
		"QueryRange":    func() error { return errOf(a.QueryRange(1, 1)) },
		"Search":        func() error { return errOf(a.Search(context.Background(), "hello", "", 10)) },
		"Stats":         func() error { _, _, _, err := a.Stats(); return err },
		"Window":        func() error { return errOf(a.Window()) },
		"ListSummaries": func() error { return errOf(a.ListSummaries()) },
		"GetSummary":    func() error { return errOf2(a.GetSummary(1)) },
	}
	for name, read := range reads {
		if err := read(); err == nil {
			t.Errorf("%s succeeded on a non-integer timestamp", name)
		}
	}
}

func TestMissingTables_Fail(t *testing.T) {
	a := openTestArchive(t)
	noErr(t, a.Append(1, sampleMsg("user", "hello"), time.Now()))
	execRaw(t, a, `DROP TABLE messages_fts`, `DROP TABLE messages`, `DROP TABLE summaries`, `DROP TABLE window`)

	calls := map[string]func() error{
		"QueryRange":    func() error { return errOf(a.QueryRange(1, 1)) },
		"Search":        func() error { return errOf(a.Search(context.Background(), "hello", "", 10)) },
		"Bounds":        func() error { return errOf2(a.Bounds()) },
		"Stats":         func() error { _, _, _, err := a.Stats(); return err },
		"MinSeqAfter":   func() error { return errOf(a.MinSeqAfter(time.Time{})) },
		"ListSummaries": func() error { return errOf(a.ListSummaries()) },
		"GetSummary":    func() error { return errOf2(a.GetSummary(1)) },
		"AppendSummary": func() error { return errOf(a.AppendSummary(SummaryRecord{Summary: "s"})) },
		"Window":        func() error { return errOf(a.Window()) },
		"ReplaceWindow": func() error { return replaceWindowWith(a, nil, SessionState{}) },
	}
	for name, call := range calls {
		if err := call(); err == nil || !strings.Contains(err.Error(), "no such table") {
			t.Errorf("%s error = %v, want no such table", name, err)
		}
	}
	if got := a.MaxSeq(); got != 0 {
		t.Errorf("MaxSeq = %d, want 0 when the archive cannot be read", got)
	}
}

func TestMaxSeq(t *testing.T) {
	a := openTestArchive(t)
	if got := a.MaxSeq(); got != 0 {
		t.Errorf("MaxSeq of an empty archive = %d, want 0", got)
	}
	for _, seq := range []int64{3, 1, 7} {
		noErr(t, a.Append(seq, sampleMsg("user", "m"), time.Now()))
	}
	if got := a.MaxSeq(); got != 7 {
		t.Errorf("MaxSeq = %d, want 7", got)
	}
}

func TestUnmarshalableMessage_NothingStored(t *testing.T) {
	a := openTestArchive(t)
	if err := a.Append(1, badMessage(), time.Now()); err == nil {
		t.Error("Append stored a message that cannot be encoded")
	}
	err := appendWindow(a, NewStoredMessage(1, badMessage()), SessionState{Key: "alice", NextSeq: 1})
	if err == nil || !strings.Contains(err.Error(), "marshal message") {
		t.Errorf("AppendWindow error = %v, want a marshal error", err)
	}
	got, err := a.QueryRange(0, 10)
	noErr(t, err)
	w, err := a.Window()
	noErr(t, err)
	st, err := a.State()
	noErr(t, err)
	if len(got) != 0 || len(w) != 0 || st.Key != "" {
		t.Errorf("after failed writes: archive %d, window %d, state %+v; want all empty", len(got), len(w), st)
	}
}

func TestOpen_NotADatabase(t *testing.T) {
	dir := t.TempDir()
	path := ArchivePath(dir, "alice")
	junk := bytes.Repeat([]byte("this is not a sqlite database "), 40)
	noErr(t, os.WriteFile(path, junk, 0o600))

	a, openErr := Open(path)
	if !errors.Is(openErr, ErrArchiveUnavailable) {
		t.Fatalf("Open error = %v, want ErrArchiveUnavailable", openErr)
	}
	// Every operation on the unavailable store is a no-op or reports it.
	if err := a.Append(1, sampleMsg("user", "x"), time.Now()); err != nil {
		t.Errorf("Append = %v, want nil", err)
	}
	if id, err := a.AppendSummary(SummaryRecord{Summary: "s"}); id != 0 || err != nil {
		t.Errorf("AppendSummary = %d, %v; want 0, nil", id, err)
	}
	for name, prune := range map[string]func() error{
		"PruneMessagesToCount":  func() error { return a.PruneMessagesToCount(1) },
		"PruneMessagesBefore":   func() error { return a.PruneMessagesBefore(time.Now()) },
		"PruneSummariesToCount": func() error { return a.PruneSummariesToCount(1) },
		"PruneSummariesBefore":  func() error { return a.PruneSummariesBefore(time.Now()) },
	} {
		if err := prune(); err != nil {
			t.Errorf("%s = %v, want nil", name, err)
		}
	}
	if got := a.MaxSeq(); got != 0 {
		t.Errorf("MaxSeq = %d, want 0", got)
	}
	reads := map[string]func() error{
		"ListSummaries": func() error { return errOf(a.ListSummaries()) },
		"GetSummary":    func() error { return errOf2(a.GetSummary(1)) },
		"Stats":         func() error { _, _, _, err := a.Stats(); return err },
		"MinSeqAfter":   func() error { return errOf(a.MinSeqAfter(time.Time{})) },
		"Search":        func() error { return errOf(a.Search(context.Background(), "x", "", 1)) },
		"Window":        func() error { return errOf(a.Window()) },
		"Update":        func() error { return setState(a, SessionState{Key: "alice"}) },
	}
	for name, read := range reads {
		if err := read(); !errors.Is(err, ErrArchiveUnavailable) {
			t.Errorf("%s = %v, want ErrArchiveUnavailable", name, err)
		}
	}
	noErr(t, a.Close())

	if _, err := OpenSessionReadOnly(dir, "alice", nil); err == nil || errors.Is(err, ErrKeyCollision) {
		t.Errorf("OpenSessionReadOnly error = %v, want the database error", err)
	}
	got, readErr := os.ReadFile(path)
	noErr(t, readErr)
	if !bytes.Equal(got, junk) {
		t.Error("opening a file that is not a database changed it")
	}
}

func TestOpen_SchemaConflictRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.archive.db")
	// A messages table without created_at makes the index in the schema fail.
	writeRawDB(t, path, `CREATE TABLE messages (seq INTEGER PRIMARY KEY)`)

	a, err := Open(path)
	if !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("Open error = %v, want ErrArchiveUnavailable", err)
	}
	noErr(t, a.Close())

	db, err := openReadOnly(path)
	noErr(t, err)
	defer func() { noErr(t, db.Close()) }()
	ok, err := hasTable(db, "summaries")
	noErr(t, err)
	if ok {
		t.Error("a failed schema left part of it behind")
	}
}

func TestOpenSession_UnreadableKey(t *testing.T) {
	dir := t.TempDir()
	writeRawDB(t, ArchivePath(dir, "alice"), `CREATE TABLE session_state (id INTEGER PRIMARY KEY)`)

	a, err := OpenSession(dir, "alice")
	if err == nil || errors.Is(err, ErrKeyCollision) || !strings.Contains(err.Error(), "read session key") {
		t.Fatalf("OpenSession error = %v, want a key read error", err)
	}
	if err := setState(a, SessionState{Key: "alice"}); !errors.Is(err, ErrArchiveUnavailable) {
		t.Errorf("Update on the refused store = %v, want ErrArchiveUnavailable", err)
	}
}

// logBuffer returns a logger writing text records to the returned buffer.
func logBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestOpen_RebuildsDamagedFTSIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.archive.db")
	a, err := Open(path)
	noErr(t, err)
	noErr(t, a.Append(1, sampleMsg("user", "hello alice"), time.Now()))
	// Lose the index's segment blocks, as a damaged file could.
	execRaw(t, a, `DELETE FROM messages_fts_data WHERE id > 10`)
	noErr(t, a.Close())

	log, buf := logBuffer()
	a, err = Open(path, WithLogger(log))
	noErr(t, err)
	defer func() { noErr(t, a.Close()) }()

	if !strings.Contains(buf.String(), "rebuilding") || !strings.Contains(buf.String(), "component=memory") {
		t.Errorf("log = %q, want a rebuild warning tagged component=memory", buf.String())
	}
	hits, err := a.Search(context.Background(), "alice", "", 10)
	noErr(t, err)
	if len(hits) != 1 || hits[0].Seq != 1 {
		t.Errorf("search alice = %+v, want seq 1", hits)
	}
}

func TestSearch_InvalidQuery(t *testing.T) {
	a := openTestArchive(t)
	noErr(t, a.Append(1, sampleMsg("user", "hello"), time.Now()))
	if _, err := a.Search(context.Background(), `"unterminated`, "", 10); err == nil {
		t.Error("Search accepted an FTS5 syntax error")
	}
	if _, err := a.Search(context.Background(), strings.Repeat("a", 501), "", 10); err == nil {
		t.Error("Search accepted a query over 500 characters")
	}
}

func TestSearch_LimitClamped(t *testing.T) {
	a := openTestArchive(t)
	for seq := int64(1); seq <= 101; seq++ {
		noErr(t, a.Append(seq, sampleMsg("user", "hello"), time.Now()))
	}
	for _, tt := range []struct{ limit, want int }{{0, 1}, {-5, 1}, {2, 2}, {1000, 100}} {
		got, err := a.Search(context.Background(), "hello", "", tt.limit)
		noErr(t, err)
		if len(got) != tt.want {
			t.Errorf("Search limit %d returned %d results, want %d", tt.limit, len(got), tt.want)
		}
	}
}

func TestDelete_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.archive.db")
	a, err := Open(path)
	noErr(t, err)
	noErr(t, a.Delete())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat after Delete = %v, want not exist", err)
	}
	noErr(t, a.Delete())

	var zero ArchiveStore
	noErr(t, zero.Delete())
}

func TestLegacySummaries_SkipsBlankAndCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.archive.db")
	first, err := json.Marshal(SummaryCheckpoint{GeneratedAt: time.Unix(100, 0), Summary: "first"})
	noErr(t, err)
	second, err := json.Marshal(SummaryCheckpoint{GeneratedAt: time.Unix(200, 0), Summary: "second"})
	noErr(t, err)
	body := string(first) + "\n\n{corrupt\n" + string(second) + "\n"
	legacy := strings.TrimSuffix(path, ".archive.db") + ".summaries.jsonl"
	noErr(t, os.WriteFile(legacy, []byte(body), 0o600))

	a, err := Open(path)
	noErr(t, err)
	defer func() { noErr(t, a.Close()) }()
	metas, err := a.ListSummaries()
	noErr(t, err)
	if len(metas) != 2 {
		t.Fatalf("imported %d summaries, want 2", len(metas))
	}
	for i, want := range []string{"first", "second"} {
		rec, ok, err := a.GetSummary(metas[i].ID)
		noErr(t, err)
		if !ok || rec.Summary != want {
			t.Errorf("summary %d = %q (found %v), want %q", i, rec.Summary, ok, want)
		}
	}
}

func TestLegacySummaries_UnreadableFileIgnored(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, legacy string)
	}{
		{"directory", func(t *testing.T, legacy string) {
			t.Helper()
			noErr(t, os.Mkdir(legacy, 0o700))
		}},
		{"no read permission", func(t *testing.T, legacy string) {
			t.Helper()
			if os.Getuid() == 0 {
				t.Skip("root ignores file permissions")
			}
			noErr(t, os.WriteFile(legacy, []byte("{}\n"), 0o000))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.archive.db")
			tt.setup(t, strings.TrimSuffix(path, ".archive.db")+".summaries.jsonl")
			log, buf := logBuffer()
			a, err := Open(path, WithLogger(log))
			noErr(t, err)
			defer func() { noErr(t, a.Close()) }()
			metas, err := a.ListSummaries()
			noErr(t, err)
			if len(metas) != 0 {
				t.Errorf("imported %d summaries from an unreadable file", len(metas))
			}
			if !strings.Contains(buf.String(), "legacy summaries") {
				t.Errorf("log = %q, want a legacy summaries warning", buf.String())
			}
		})
	}
}

func TestDelete_ReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.archive.db")
	a, err := Open(path)
	noErr(t, err)
	readOnlyDir(t, dir)
	if err := a.Delete(); err == nil {
		t.Error("Delete reported success for a file it could not remove")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file gone after a failed Delete: %v", err)
	}
}

func TestDeleteSession_ReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "alice", "hello")
	readOnlyDir(t, dir)
	err := DeleteSession(dir, "alice")
	if err == nil || !strings.Contains(err.Error(), "delete session") {
		t.Errorf("DeleteSession error = %v, want a delete error", err)
	}
}

func TestOpen_UnwritableDirectory(t *testing.T) {
	dir := t.TempDir()
	readOnlyDir(t, dir)
	path := filepath.Join(dir, "s.archive.db")
	if _, err := Open(path); !errors.Is(err, ErrArchiveUnavailable) {
		t.Errorf("Open error = %v, want ErrArchiveUnavailable", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat = %v, want the file not created", err)
	}
}

func TestListSessions_Errors(t *testing.T) {
	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		noErr(t, os.WriteFile(file, nil, 0o600))
		if _, err := ListSessions(file, nil); err == nil {
			t.Error("ListSessions of a file succeeded")
		}
	})
	t.Run("dangling link skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeSession(t, dir, "alice", "hello")
		noErr(t, os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "bob"+archiveSuffix)))
		keys, err := ListSessions(dir, nil)
		noErr(t, err)
		if len(keys) != 1 || keys[0] != "alice" {
			t.Errorf("keys = %v, want [alice]", keys)
		}
	})
}

func TestReadOnlySession_FileRemoved(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "alice", "hello")
	ro, err := OpenSessionReadOnly(dir, "alice", nil)
	noErr(t, err)
	noErr(t, DeleteSession(dir, "alice"))
	if _, err := ro.Window(); err == nil {
		t.Error("Window read a removed file")
	}
}

func TestStateTx_StateReadModifyWrite(t *testing.T) {
	a := openTestArchive(t)
	noErr(t, setState(a, SessionState{Key: "alice", NextSeq: 4}))
	noErr(t, a.Update(func(tx *StateTx) error {
		st, err := tx.State()
		if err != nil {
			return err
		}
		st.NextSeq++
		return tx.SetState(st)
	}))
	st, err := a.State()
	noErr(t, err)
	if st.Key != "alice" || st.NextSeq != 5 {
		t.Errorf("state = %+v, want key alice, next seq 5", st)
	}
}

// TestUpdate_TransactionAlreadyEnded covers a failed transaction SQLite has
// already rolled back itself: the error is returned and the store stays
// usable.
func TestUpdate_TransactionAlreadyEnded(t *testing.T) {
	a := openTestArchive(t)
	boom := errors.New("boom")
	err := a.Update(func(tx *StateTx) error {
		if err := tx.SetState(SessionState{Key: "alice", NextSeq: 1}); err != nil {
			return err
		}
		if _, err := tx.tx.Exec("ROLLBACK"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update error = %v, want boom", err)
	}
	st, err := a.State()
	noErr(t, err)
	if st.Key != "" {
		t.Errorf("state = %+v, want none written", st)
	}
	noErr(t, setState(a, SessionState{Key: "bob", NextSeq: 2}))
	st, err = a.State()
	noErr(t, err)
	if st.Key != "bob" {
		t.Errorf("state after a later Update = %+v, want key bob", st)
	}
}

func TestNoiseCache_Nil(t *testing.T) {
	var c *NoiseCache
	msg := StoredMessage{Role: "user", Content: "hello"}
	c.Record(msg, nil)
	if c.IsNoise(msg, nil) {
		t.Error("a nil cache classified a message as noise")
	}
}

func TestNewStoredMessageAt_ZeroTimeStamped(t *testing.T) {
	before := time.Now().UTC()
	m := NewStoredMessageAt(1, spawnllm.Message{Role: "user"}, time.Time{})
	if m.CreatedAt.Before(before) || m.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want now in UTC", m.CreatedAt)
	}
}

func TestLogger_NilDiscards(t *testing.T) {
	if (&ArchiveStore{}).logger() == nil {
		t.Error("logger of a store without one is nil")
	}
}
