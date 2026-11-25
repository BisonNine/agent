/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package testcron

import (
	"strings"
	"testing"
	"time"
)

var fired = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func TestBuildParseRoundTrip(t *testing.T) {
	tests := []struct {
		name, fp, msg string
	}{
		{"fingerprinted", "abc123", "check the mail"},
		{"legacy", "", "check the mail"},
		{"multi-line payload", "ff", "line one\nline two"},
		{"empty payload", "0a", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := Build(tc.fp, fired, tc.msg)
			if !strings.Contains(wrapped, "2026-03-04 05:06 UTC") {
				t.Fatalf("fire time missing from %q", wrapped)
			}
			// The payload is everything after the first newline following the
			// prefix line, so it keeps the blank line's newline, as the host's
			// parser does.
			fp, payload, ok := Parse(wrapped)
			if !ok || fp != tc.fp || payload != "\n"+tc.msg {
				t.Fatalf("Parse = %q, %q, %v; want %q, %q, true", fp, payload, ok, tc.fp, "\n"+tc.msg)
			}
		})
	}
}

func TestParseRejectsAndTolerates(t *testing.T) {
	tests := []struct {
		name, content, fp, payload string
		ok                         bool
	}{
		{"plain text", "hello", "", "", false},
		{"uppercase hex marker is not a marker", "[ABC] " + prefix + "t:\n\nx", "", "", false},
		{"marker too long", "[0123456789abc] " + prefix + "t:\n\nx", "", "", false},
		{"empty marker", "[] " + prefix + "t:\n\nx", "", "", false},
		{"marker without space", "[ab]" + prefix + "t:\n\nx", "", "", false},
		{"prefix line without newline", prefix + "t", "", "", true},
		{"monitor event is not cron", BuildEvent(fired, "note", "", ""), "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fp, payload, ok := Parse(tc.content)
			if fp != tc.fp || payload != tc.payload || ok != tc.ok {
				t.Fatalf("Parse(%q) = %q, %q, %v; want %q, %q, %v",
					tc.content, fp, payload, ok, tc.fp, tc.payload, tc.ok)
			}
		})
	}
}

func TestCollapseKey(t *testing.T) {
	// Two fires of one fingerprinted job share a key despite different times.
	a, okA := CollapseKey(Build("beef", fired, "a"))
	b, okB := CollapseKey(Build("beef", fired.Add(time.Hour), "b"))
	if !okA || !okB || a != "beef" || b != "beef" {
		t.Fatalf("fingerprinted keys = %q/%v, %q/%v; want beef", a, okA, b, okB)
	}
	// Legacy fires fall back to the payload.
	if k, ok := CollapseKey(Build("", fired, "same job")); !ok || k != "\nsame job" {
		t.Fatalf("legacy key = %q, %v", k, ok)
	}
	if _, ok := CollapseKey("not cron"); ok {
		t.Fatal("non-cron content has a collapse key")
	}
}

func TestBuildEvent(t *testing.T) {
	note := BuildEvent(fired, "disk full", "", "")
	if !strings.HasPrefix(note, eventPrefix+"2026-03-04 05:06:07 UTC:\n\ndisk full") || strings.Contains(note, "returned") {
		t.Fatalf("note-only event = %q", note)
	}
	full := BuildEvent(fired, "disk full", "df", "95%")
	if !strings.HasSuffix(full, "disk full\n\ndf returned the following:\n95%") {
		t.Fatalf("event with result = %q", full)
	}
}
