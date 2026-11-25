/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm"
)

// TestCompactionReport_String_Outcomes renders the closing line for each
// outcome, choosing the failure explanation from the attempts.
func TestCompactionReport_String_Outcomes(t *testing.T) {
	tests := []struct {
		name     string
		outcome  string
		attempts []CompactionAttempt
		want     string
	}{
		{"partial", "partial", nil, "Partially compacted (still above safety threshold) — 3 messages (2 KB)"},
		{"nothing", "nothing", nil, "Nothing to compress — already compact."},
		{"refused", "failed", []CompactionAttempt{{Model: "m1", Status: "refused"}}, "model(s) refused to summarize"},
		{"rate limited", "failed", []CompactionAttempt{{Model: "m1", Status: "error", Detail: "rate limited"}}, "it is in cooldown"},
		{"plain failure", "failed", []CompactionAttempt{{Model: "m1", Status: "error", Detail: "invalid JSON"}}, "Failed — no model produced an acceptable summary."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &CompactionReport{BeforeMsgs: 10, BeforeBytes: 4096, AfterMsgs: 3, AfterBytes: 2048, Outcome: tc.outcome, Attempts: tc.attempts}
			if got := r.String(); !strings.Contains(got, tc.want) {
				t.Errorf("report lacks %q:\n%s", tc.want, got)
			}
		})
	}
	var nilReport *CompactionReport
	if got := nilReport.String(); got != "" {
		t.Errorf("nil report = %q, want empty", got)
	}
}

// TestCompactionReport_String_DateRange puts the span of the compacted
// messages under the header: one day, or a from-to range.
func TestCompactionReport_String_DateRange(t *testing.T) {
	day1 := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		to   time.Time
		want string
	}{
		{"same day", day1.Add(5 * time.Hour), "Compaction: 4 messages (3.0 MB)\nMar 4, 2026\n"},
		{"several days", day1.AddDate(0, 0, 2), "Compaction: 4 messages (3.0 MB)\nMar 4, 2026 to Mar 6, 2026\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &CompactionReport{BeforeMsgs: 4, BeforeBytes: 3 << 20, DateFrom: day1, DateTo: tc.to, Outcome: "nothing"}
			if got := r.String(); !strings.HasPrefix(got, tc.want) {
				t.Errorf("report = %q, want prefix %q", got, tc.want)
			}
		})
	}
}

// TestShortErr flattens and bounds an error for display.
func TestShortErr(t *testing.T) {
	if got := shortErr(nil); got != "" {
		t.Errorf("shortErr(nil) = %q", got)
	}
	if got := shortErr(errors.New("line one\nline two")); got != "line one line two" {
		t.Errorf("shortErr = %q, want newlines flattened", got)
	}
	long := shortErr(errors.New(strings.Repeat("x", 200)))
	if len(long) != 120 || !strings.HasSuffix(long, "...") {
		t.Errorf("shortErr of 200 chars = %d chars %q, want 120 ending in ...", len(long), long[len(long)-5:])
	}
}

// TestDumpJSON_Unencodable records a value JSON cannot encode as a JSON
// string naming the error, so the dump stays valid JSON.
func TestDumpJSON_Unencodable(t *testing.T) {
	out := dumpJSON(func() {})
	var s string
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("dumpJSON output %q is not a JSON string: %v", out, err)
	}
	if !strings.HasPrefix(s, "unencodable: ") {
		t.Errorf("dumpJSON = %q, want an unencodable note", s)
	}
}

// TestCompactionRecorder_DumpSinkFailureLogged logs a warning when the
// host's failure-dump sink cannot write, and still records the attempt.
func TestCompactionRecorder_DumpSinkFailureLogged(t *testing.T) {
	logs := &logCapture{}
	rec := &compactionRecorder{
		sessionKey:  "alice",
		log:         logs.logger(),
		failureDump: func(string, map[string]any, string, string) error { return errors.New("disk full") },
	}
	rec.record("m1", "error", "bad JSON", time.Second, nil, "x")
	if len(rec.attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(rec.attempts))
	}
	if out := logs.String(); !strings.Contains(out, "failed-compression dump: write failed") || !strings.Contains(out, "disk full") {
		t.Errorf("sink failure not logged:\n%s", out)
	}
}

// TestCompactionRecorder_UnencodableRequestDump hands the sink a valid JSON
// note for a request that cannot be encoded, and writes no capture line
// rather than a broken one, logging that the capture was skipped.
func TestCompactionRecorder_UnencodableRequestDump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compact.jsonl")
	sink := &dumpSink{}
	logs := &logCapture{}
	rec := &compactionRecorder{sessionKey: "alice", debugPath: path, debugPerm: 0o600, failureDump: sink.write, log: logs.logger()}
	req := []spawnllm.Message{{Role: "assistant", ResponsesReasoning: []json.RawMessage{json.RawMessage("{broken")}}}
	rec.record("m1", "error", "bad JSON", time.Second, req, "resp")

	if len(sink.inputs) != 1 {
		t.Fatalf("dumps = %d, want 1", len(sink.inputs))
	}
	var note string
	if err := json.Unmarshal([]byte(sink.inputs[0]), &note); err != nil || !strings.HasPrefix(note, "unencodable: ") {
		t.Errorf("dump input = %q (%v), want an unencodable note", sink.inputs[0], err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("capture file stat = %v, want no file written", err)
	}
	if out := logs.String(); !strings.Contains(out, "compaction debug capture: encode failed") {
		t.Errorf("skipped capture not logged:\n%s", out)
	}
}

// TestCompactionRecorder_CaptureOpenFailureLogged logs a warning when the
// debug capture file cannot be opened and keeps the attempt.
func TestCompactionRecorder_CaptureOpenFailureLogged(t *testing.T) {
	logs := &logCapture{}
	path := filepath.Join(t.TempDir(), "missing", "compact.jsonl")
	rec := &compactionRecorder{sessionKey: "alice", debugPath: path, debugPerm: 0o600, log: logs.logger()}
	rec.record("m1", "ok", "", time.Second, nil, "{}")
	if len(rec.attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(rec.attempts))
	}
	if out := logs.String(); !strings.Contains(out, "compaction debug capture: open failed") {
		t.Errorf("open failure not logged:\n%s", out)
	}
}

// TestCompactionRecorder_CaptureAppends writes one JSON line per attempt and
// appends to an existing capture file, resetting its mode.
func TestCompactionRecorder_CaptureAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compact.jsonl")
	rec := &compactionRecorder{sessionKey: "alice", debugPath: path, debugPerm: 0o600}
	rec.record("m1", "error", "bad JSON", time.Second, nil, "first")
	noErr(t, os.Chmod(path, 0o644))
	rec.record("m2", "ok", "", time.Second, nil, "second")

	data, err := os.ReadFile(path)
	noErr(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("capture lines = %d, want 2:\n%s", len(lines), data)
	}
	for i, want := range []string{"first", "second"} {
		var row map[string]any
		if jsonErr := json.Unmarshal([]byte(lines[i]), &row); jsonErr != nil || row["response"] != want || row["session"] != "alice" {
			t.Errorf("line %d = %s (%v), want response %q", i, lines[i], jsonErr, want)
		}
	}
	info, err := os.Stat(path)
	noErr(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("capture mode = %#o, want 0600", info.Mode().Perm())
	}
}
