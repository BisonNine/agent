/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
)

// damage runs stmts against the archive of key under dir through a
// connection of its own, the way a crash, a bug or another program could.
func damage(t *testing.T, dir, key string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+memory.ArchivePath(dir, key))
	noErr(t, err)
	defer func() { noErr(t, db.Close()) }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

// loggedHost is an archive host whose logger writes to the returned buffer.
func loggedHost(dir string) (Host, *bytes.Buffer) {
	var buf bytes.Buffer
	return Host{SessionsDir: dir, Logger: slog.New(slog.NewTextHandler(&buf, nil))}, &buf
}

func TestIntArg(t *testing.T) {
	tests := []struct {
		name   string
		v      any
		want   int64
		wantOK bool
	}{
		{"int", 7, 7, true},
		{"int64", int64(8), 8, true},
		{"float64", float64(9), 9, true},
		{"json number", json.Number("10"), 10, true},
		{"fractional json number", json.Number("1.5"), 0, false},
		{"string", "11", 0, false},
		{"null", nil, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := intArg(map[string]any{"n": tc.v}, "n")
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("intArg(%#v) = %d, %v; want %d, %v", tc.v, got, ok, tc.want, tc.wantOK)
			}
		})
	}
	if _, ok := intArg(map[string]any{}, "n"); ok {
		t.Error("intArg of an absent key reported ok")
	}
}

func TestSessionHistory_InvertedRange(t *testing.T) {
	r := run(t, archiveHost(t.TempDir()), "messages", "alice", map[string]any{"seq_start": 5, "seq_end": 2})
	if !r.IsError || r.ForLLM != "seq_start (5) > seq_end (2)" {
		t.Errorf("result = %q (isErr=%v), want the inverted-range error", r.ForLLM, r.IsError)
	}
}

func TestSessionHistory_EmptyArchive(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir, "alice", nil)
	r := run(t, archiveHost(dir), "messages", "alice", map[string]any{"seq": 1})
	if r.IsError || r.ForLLM != "not available in the current archive window" {
		t.Errorf("result = %q (isErr=%v), want not available", r.ForLLM, r.IsError)
	}
}

// TestSessionHistory_WindowFloor serves only the newest 5000 seqs: a range
// reaching further back is cut at the floor.
func TestSessionHistory_WindowFloor(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir, "alice", []memory.StoredMessage{
		archiveMsg(1, "user", "too old"),
		archiveMsg(1000, "user", "below the floor"),
		archiveMsg(1001, "user", "at the floor"),
		archiveMsg(6000, "assistant", "newest"),
	})
	r := run(t, archiveHost(dir), "messages", "alice", map[string]any{"seq_start": 1, "seq_end": 6000})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var got []struct {
		Seq int64 `json:"seq"`
	}
	noErr(t, json.Unmarshal([]byte(r.ForLLM), &got))
	if len(got) != 2 || got[0].Seq != 1001 || got[1].Seq != 6000 {
		t.Errorf("seqs = %+v, want 1001 and 6000", got)
	}
}

func TestSessionHistory_ArchiveErrors(t *testing.T) {
	t.Run("bounds", func(t *testing.T) {
		dir := t.TempDir()
		writeArchive(t, dir, "alice", []memory.StoredMessage{archiveMsg(1, "user", "hi")})
		// A messages table whose seq is not a number, as another program
		// could leave behind.
		damage(t, dir, "alice", `DROP TABLE messages_fts`, `DROP TABLE messages`,
			`CREATE TABLE messages (seq TEXT)`, `INSERT INTO messages VALUES ('abc')`)
		h, logs := loggedHost(dir)
		r := run(t, h, "messages", "alice", map[string]any{"seq": 1})
		if !r.IsError || !strings.HasPrefix(r.ForLLM, "archive bounds error: ") {
			t.Errorf("result = %q (isErr=%v), want an archive bounds error", r.ForLLM, r.IsError)
		}
		if out := logs.String(); !strings.Contains(out, "session archive error") || !strings.Contains(out, "component=tools") {
			t.Errorf("log = %q, want the failure logged with component=tools", out)
		}
	})
	t.Run("read", func(t *testing.T) {
		dir := t.TempDir()
		writeArchive(t, dir, "alice", []memory.StoredMessage{archiveMsg(1, "user", "hi")})
		damage(t, dir, "alice", `UPDATE messages SET payload = 'not json'`)
		r := run(t, archiveHost(dir), "messages", "alice", map[string]any{"seq": 1})
		if !r.IsError || !strings.HasPrefix(r.ForLLM, "archive read error: ") {
			t.Errorf("result = %q (isErr=%v), want an archive read error", r.ForLLM, r.IsError)
		}
	})
}

func TestSearchTool_QueryRequiredAndLimitFloor(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir, "alice", []memory.StoredMessage{
		archiveMsg(1, "user", "hello one"),
		archiveMsg(2, "user", "hello two"),
	})
	h := archiveHost(dir)
	for _, q := range []any{"   ", nil, 3} {
		if r := run(t, h, "search", "alice", map[string]any{"query": q}); !r.IsError || r.ForLLM != "query parameter is required" {
			t.Errorf("query %#v: result = %q (isErr=%v), want the required error", q, r.ForLLM, r.IsError)
		}
	}
	r := run(t, h, "search", "alice", map[string]any{"query": "hello", "limit": 0})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	var got []map[string]any
	noErr(t, json.Unmarshal([]byte(r.ForLLM), &got))
	if len(got) != 1 {
		t.Errorf("limit 0 returned %d results, want 1", len(got))
	}
}

func TestSessionCompactTool_DefaultOutcome(t *testing.T) {
	h := Host{Compact: func(context.Context, string) (string, string, error) { return "", "", nil }}
	r := run(t, h, "compact", "alice", nil)
	if r.IsError || r.ForLLM != "Session compacted successfully." {
		t.Errorf("result = %q (isErr=%v), want the plain success line", r.ForLLM, r.IsError)
	}
	if r := run(t, h, "compact", "", nil); !r.IsError || r.ForLLM != "session key not available" {
		t.Errorf("without a session key: %q (isErr=%v)", r.ForLLM, r.IsError)
	}
}

func TestSessionInfoTool_NoInformation(t *testing.T) {
	h := Host{SessionInfo: func(context.Context, string) (*SessionInfo, error) { return nil, nil }}
	r := run(t, h, "info", "alice", nil)
	if !r.IsError || r.ForLLM != "session info error: no session information returned" {
		t.Errorf("result = %q (isErr=%v), want the no-information error", r.ForLLM, r.IsError)
	}
	if r := run(t, h, "info", "", nil); !r.IsError || r.ForLLM != "session key not available" {
		t.Errorf("without a session key: %q (isErr=%v)", r.ForLLM, r.IsError)
	}
}

func TestSessionClearTool_WithoutNote(t *testing.T) {
	h := Host{Clear: func(context.Context, string, string) error { return nil }}
	r := run(t, h, "clear", "alice", nil)
	if r.IsError || !strings.HasSuffix(r.ForLLM, "a new turn will begin.") || strings.Contains(r.ForLLM, "handoff") {
		t.Errorf("result = %q (isErr=%v), want the clear notice without a handoff", r.ForLLM, r.IsError)
	}
}

func TestSessionSummaryList_OffsetAndLimit(t *testing.T) {
	const key = "alice"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := make([]memory.SummaryRecord, 0, 4)
	for i := range 4 {
		recs = append(recs, memory.SummaryRecord{GeneratedAt: base.Add(time.Duration(i) * time.Hour), CoveredSeqStart: 1, CoveredSeqEnd: int64(10 * (i + 1)), Summary: "s"})
	}
	h := archiveHost(writeArchiveSummaries(t, key, recs))

	// Newest first: ids 4, 3, 2, 1. Offset 1 and limit 2 give ids 3 and 2.
	r := run(t, h, "summary_list", key, map[string]any{"offset": 1, "limit": 2})
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.ForLLM)
	}
	lines := strings.Split(r.ForLLM, "\n")
	if len(lines) != 3 || lines[0] != "2 context summary checkpoint(s) (newest first):" ||
		!strings.HasPrefix(lines[1], "- id 3:") || !strings.HasPrefix(lines[2], "- id 2:") {
		t.Errorf("listing = %q, want ids 3 and 2", r.ForLLM)
	}

	r = run(t, h, "summary_list", key, map[string]any{"offset": 4})
	if r.IsError || r.ForLLM != "no context summaries in the requested range" {
		t.Errorf("offset past the end: %q (isErr=%v)", r.ForLLM, r.IsError)
	}
}

func TestSessionSummary_ArchiveErrors(t *testing.T) {
	dir := writeArchiveSummaries(t, "alice", []memory.SummaryRecord{{GeneratedAt: time.Now(), Summary: "s"}})
	damage(t, dir, "alice", `UPDATE summaries SET generated_at = 'abc'`)
	h := archiveHost(dir)
	if r := run(t, h, "summary_list", "alice", nil); !r.IsError || !strings.HasPrefix(r.ForLLM, "summary list error: ") {
		t.Errorf("summary_list = %q (isErr=%v), want a summary list error", r.ForLLM, r.IsError)
	}
	if r := run(t, h, "summary_get", "alice", map[string]any{"id": 1}); !r.IsError || !strings.HasPrefix(r.ForLLM, "summary get error: ") {
		t.Errorf("summary_get = %q (isErr=%v), want a summary get error", r.ForLLM, r.IsError)
	}
}

func TestSessionSummaryGet_ShowsProfile(t *testing.T) {
	dir := writeArchiveSummaries(t, "alice", []memory.SummaryRecord{{
		GeneratedAt: time.Date(2026, 2, 3, 4, 5, 0, 0, time.UTC), Model: "m1", Profile: "abc12345",
		CoveredSeqStart: 1, CoveredSeqEnd: 9, Summary: "Alice and Bob planned the release.",
	}})
	r := run(t, archiveHost(dir), "summary_get", "alice", map[string]any{"id": 1})
	want := "Context summary checkpoint id 1 (covers #1-#9, generated 2026-02-03 04:05 UTC, model m1, profile abc12345)\n\nAlice and Bob planned the release."
	if r.IsError || r.ForLLM != want {
		t.Errorf("result = %q (isErr=%v), want %q", r.ForLLM, r.IsError, want)
	}
}
