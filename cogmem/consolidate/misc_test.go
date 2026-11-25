/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every curated file is read into its own field.
func TestReadCuratedAllFiles(t *testing.T) {
	ws := t.TempDir()
	want := map[string]string{
		"AGENTS.md": "agents", "SOUL.md": "soul", "IDENTITY.md": "identity",
		"USER.md": "Alice", "MEMORY.md": "memory",
	}
	for name, body := range want {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	c := ReadCurated(ws)
	got := map[string]string{
		"AGENTS.md": c.AgentsMD, "SOUL.md": c.SoulMD, "IDENTITY.md": c.IdentityMD,
		"USER.md": c.UserMD, "MEMORY.md": c.MemoryMD,
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s read as %q, want %q", name, got[name], body)
		}
	}
}

// HTML comments in the per-agent file are guidance for the operator and never
// reach the model; an unterminated comment drops the rest of the file.
func TestBuildPromptStripsComments(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		appended bool
		want     []string
		notWant  []string
	}{
		{
			name: "only comments",
			file: "<!-- Write instructions for this assistant below. -->\n",
		},
		{
			name:     "comments around instructions",
			file:     "<!-- guidance -->\nRemember Bob's projects.\n<!-- more guidance -->\nIgnore small talk.",
			appended: true,
			want:     []string{"Remember Bob's projects.", "Ignore small talk."},
			notWant:  []string{"guidance", "<!--", "-->"},
		},
		{
			name:     "unterminated comment",
			file:     "Remember Bob's projects.\n<!-- half-written note\nsecret draft",
			appended: true,
			want:     []string{"Remember Bob's projects."},
			notWant:  []string{"half-written", "secret draft", "<!--"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), PromptFilename)
			if err := os.WriteFile(f, []byte(tc.file), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, res := BuildPrompt(f)
			if res.Appended != tc.appended || res.Ignored {
				t.Fatalf("result = %+v, want appended=%v", res, tc.appended)
			}
			if !tc.appended && got != DefaultPrompt() {
				t.Fatal("prompt changed although nothing was appended")
			}
			extra := strings.TrimPrefix(got, DefaultPrompt())
			for _, s := range tc.want {
				if !strings.Contains(extra, s) {
					t.Errorf("prompt is missing %q", s)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(extra, s) {
					t.Errorf("prompt contains %q", s)
				}
			}
		})
	}
}

// Zero batch options mean the documented defaults, not a zero budget.
func TestSelectBatchZeroOptionsUseDefaults(t *testing.T) {
	msgs := make([]Message, defaultMaxMessages+1)
	for i := range msgs {
		msgs[i] = Message{Seq: int64(i + 1), Role: "user", Text: "hi"}
	}
	batch, last, more := SelectBatch(msgs, BatchOptions{})
	if len(batch) != defaultMaxMessages || last != defaultMaxMessages || !more {
		t.Fatalf("len=%d last=%d more=%v, want %d messages and more", len(batch), last, more, defaultMaxMessages)
	}
	long := strings.Repeat("x", defaultPerMessageChars+1)
	batch, _, _ = SelectBatch([]Message{{Seq: 1, Role: "user", Text: long}}, BatchOptions{})
	if !strings.HasSuffix(batch[0].Text, "…[truncated]") {
		t.Fatal("a message over the default per-message cap was not truncated")
	}
}

// The evidence range is the batch's lowest to highest seq, whatever order the
// messages arrive in.
func TestValidateEvidenceRangeIgnoresMessageOrder(t *testing.T) {
	in := sampleInput()
	in.NewMessages = []Message{{Seq: 7, Role: "user", Text: "b"}, {Seq: 5, Role: "user", Text: "a"}}
	ok := Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "fact", Text: "ok", Confidence: 0.9, Evidence: ev(5, 7)}}}
	if err := ok.Validate(in); err != nil {
		t.Fatalf("evidence within [5,7] rejected: %v", err)
	}
	bad := Output{MemoryOps: []MemoryOp{{Op: "add", Domain: "d4", Type: "fact", Text: "ok", Confidence: 0.9, Evidence: ev(4, 7)}}}
	if err := bad.Validate(in); err == nil || !strings.Contains(err.Error(), "outside batch [5,7]") {
		t.Fatalf("err = %v, want evidence [4,7] rejected against [5,7]", err)
	}
}
