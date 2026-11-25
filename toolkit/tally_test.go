/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// redactSecret stands in for a host's redactor.
func redactSecret(s string) string { return strings.ReplaceAll(s, "s3cret", "[REDACTED]") }

func TestTally_RoundTrip(t *testing.T) {
	tl := NewTally(nil)
	const key = "agent:alice-worker:main"
	tl.Begin(key)
	tl.Record(key, "file_read_lines", NewResult("ok"))
	tl.Record(key, "web_fetch", ErrorResult("first failure"))
	tl.Record(key, "task_get", nil)
	tl.Record(key, "file_write", &Result{IsError: true, Err: errors.New("disk full")})

	calls, errs, last := tl.End(key)
	if calls != 4 || errs != 3 {
		t.Errorf("calls=%d errors=%d, want 4 and 3", calls, errs)
	}
	if last != "file_write: disk full" {
		t.Errorf("last error = %q, want %q", last, "file_write: disk full")
	}
}

func TestTally_NilResultText(t *testing.T) {
	tl := NewTally(nil)
	const key = "k"
	tl.Begin(key)
	tl.Record(key, "task_get", nil)
	_, _, last := tl.End(key)
	if last != "task_get: tool returned nil result" {
		t.Errorf("last error = %q", last)
	}
}

func TestTally_UnregisteredKeyIsNoOp(t *testing.T) {
	tl := NewTally(nil)
	const key = "agent:bob:main"
	tl.Record(key, "web_fetch", ErrorResult("boom"))
	tl.Record("", "web_fetch", ErrorResult("boom"))
	tl.Begin("")
	if calls, errs, last := tl.End(key); calls != 0 || errs != 0 || last != "" {
		t.Errorf("unregistered key tallied: %d %d %q", calls, errs, last)
	}
}

func TestTally_ZerosAfterEnd(t *testing.T) {
	tl := NewTally(nil)
	const key = "k"
	tl.Begin(key)
	tl.Record(key, "web_fetch", ErrorResult("boom"))
	if calls, _, _ := tl.End(key); calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	// Recording after End must not resurrect the entry.
	tl.Record(key, "web_fetch", ErrorResult("boom"))
	if calls, errs, last := tl.End(key); calls != 0 || errs != 0 || last != "" {
		t.Errorf("after End: %d %d %q, want zeros", calls, errs, last)
	}
}

func TestTally_LastErrorRedactedAndTruncated(t *testing.T) {
	tl := NewTally(redactSecret)
	const key = "k"
	tl.Begin(key)
	tl.Record(key, "memory_status", ErrorResult("rejected s3cret "+strings.Repeat("x", 1000)))
	_, _, last := tl.End(key)
	if strings.Contains(last, "s3cret") {
		t.Errorf("secret not redacted: %q", last)
	}
	if !strings.Contains(last, "[REDACTED]") {
		t.Errorf("redaction marker missing: %q", last)
	}
	text := strings.TrimPrefix(last, "memory_status: ")
	if n := utf8.RuneCountInString(text); n != MaxLastErrorRunes+1 {
		t.Errorf("error text is %d runes, want %d plus the ellipsis", n, MaxLastErrorRunes)
	}
}

func TestTally_ConcurrentRecord(t *testing.T) {
	tl := NewTally(nil)
	const key = "k"
	const workers, perWorker = 16, 50
	tl.Begin(key)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				if i%2 == 0 {
					tl.Record(key, "web_fetch", ErrorResult(fmt.Sprintf("fail %d/%d", w, i)))
				} else {
					tl.Record(key, "web_fetch", NewResult("ok"))
				}
			}
		})
	}
	wg.Wait()
	calls, errs, last := tl.End(key)
	if calls != workers*perWorker || errs != workers*perWorker/2 {
		t.Errorf("calls=%d errors=%d, want %d and %d", calls, errs, workers*perWorker, workers*perWorker/2)
	}
	if !strings.HasPrefix(last, "web_fetch: fail ") {
		t.Errorf("last error = %q", last)
	}
}
