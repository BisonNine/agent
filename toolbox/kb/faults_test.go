/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sentinel reports whether err matches one of the errors a caller acts on.
func sentinel(err error) bool {
	for _, s := range []error{ErrUnknownKB, ErrNotFound, ErrReadOnly, ErrInvalid, ErrTooLarge} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

// skipAsRoot skips a test that relies on permissions being enforced.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
}

// chmod sets a mode for the test and restores 0700 after it.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Error(err)
		}
	})
}

func TestUnreadableDirectoryIsAFailure(t *testing.T) {
	skipAsRoot(t)
	f := newFixture(t)
	// "zsub" is walked after "y", so Verify reaches y's include first.
	f.put("zsub/x", Frontmatter{"type": "Note"}, "text\n")
	f.put("y", Frontmatter{"type": "Note", IncludeKey: []any{"zsub/x"}}, "")
	chmod(t, filepath.Join(f.notes, "zsub"), 0o000)
	checks := map[string]func() error{
		"get":     func() error { _, err := f.s.Get(f.ctx, f.r, "notes", "zsub/x"); return err },
		"include": func() error { _, err := f.s.Get(f.ctx, f.r, "notes", "y"); return err },
		"put": func() error {
			_, err := f.s.Put(f.ctx, f.r, "notes", "zsub/z", Frontmatter{"type": "Note"}, "")
			return err
		},
		"put include": func() error {
			_, err := f.s.Put(f.ctx, f.r, "notes", "z", Frontmatter{"type": "Note", IncludeKey: []any{"zsub/x"}}, "")
			return err
		},
		"search": func() error { _, err := f.s.Search(f.ctx, f.r, "notes", "text", 0); return err },
		"verify": func() error { _, err := f.s.Verify(f.ctx, f.r, "notes"); return err },
	}
	for name, op := range checks {
		if err := op(); err == nil || sentinel(err) {
			t.Errorf("%s over an unreadable directory = %v, want a failure", name, err)
		}
	}
}

func TestUnreadableFileIsAFailure(t *testing.T) {
	skipAsRoot(t)
	f := newFixture(t)
	f.put("x", Frontmatter{"type": "Note"}, "")
	chmod(t, filepath.Join(f.notes, "x.md"), 0o000)
	if _, err := f.s.Get(f.ctx, f.r, "notes", "x"); err == nil || sentinel(err) {
		t.Fatalf("Get of an unreadable file = %v", err)
	}
	// A store that has not indexed it yet must read it.
	if _, err := newStore(t, Options{}).Search(f.ctx, f.r, "notes", "x", 0); err == nil || sentinel(err) {
		t.Fatalf("Search over an unreadable file = %v", err)
	}
	if _, err := f.s.Verify(f.ctx, f.r, "notes"); err == nil || sentinel(err) {
		t.Fatalf("Verify over an unreadable file = %v", err)
	}
	// A put replaces it whole.
	chmod(t, filepath.Join(f.notes, "x.md"), 0o200)
	f.put("x", Frontmatter{"type": "Note", "title": "t"}, "")
}

func TestUnwritableBundleLeavesNoTrace(t *testing.T) {
	skipAsRoot(t)
	f := newFixture(t)
	f.put("x", Frontmatter{"type": "Note"}, "old\n")
	chmod(t, f.notes, 0o500)
	if _, err := f.s.Put(f.ctx, f.r, "notes", "x", Frontmatter{"type": "Note"}, "new\n"); err == nil || sentinel(err) {
		t.Fatalf("Put into a read-only directory = %v", err)
	}
	if _, err := f.s.Put(f.ctx, f.r, "notes", "sub/y", Frontmatter{"type": "Note"}, ""); err == nil || sentinel(err) {
		t.Fatalf("Put needing a directory = %v", err)
	}
	if got := f.read("x.md"); got != "---\ntype: Note\n---\nold\n" {
		t.Fatalf("file = %q", got)
	}
	entries, err := os.ReadDir(f.notes)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
}

func TestDirectoryCreationFailures(t *testing.T) {
	skipAsRoot(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "file"), "x")
	locked := filepath.Join(dir, "locked")
	mkdir(t, locked)
	chmod(t, locked, 0o500)
	s := newStore(t, Options{})
	for name, b := range map[string]Bundle{
		"under a file":                  {Root: filepath.Join(dir, "file", "x", "y")},
		"in a locked directory":         {Root: filepath.Join(locked, "x", "y")},
		"confined under a file":         {Root: filepath.Join(dir, "file", "x"), Confine: dir},
		"confined in a locked folder":   {Root: filepath.Join(locked, "x"), Confine: locked},
		"confine is a file, no create":  {Root: filepath.Join(dir, "file", "x"), Confine: filepath.Join(dir, "file")},
		"relative root":                 {Root: "relative"},
		"relative confine":              {Root: dir, Confine: "relative"},
		"root outside confine":          {Root: dir, Confine: locked},
		"confine is a file, and create": {Root: filepath.Join(dir, "file", "x"), Confine: filepath.Join(dir, "file")},
	} {
		t.Run(name, func(t *testing.T) {
			b.Writable = true
			b.Create = !strings.Contains(name, "no create")
			r := mapResolver{"b": b}
			_, err := s.Put(context.Background(), r, "b", "a", Frontmatter{"type": "Note"}, "")
			if err == nil || sentinel(err) || !strings.Contains(err.Error(), "bundle") {
				t.Fatalf("Put = %v, want a bundle failure", err)
			}
			if !b.Create {
				if _, err := s.Get(context.Background(), r, "b", "a"); err == nil || sentinel(err) {
					t.Fatalf("Get = %v, want a bundle failure", err)
				}
			}
		})
	}
}

func TestIndexFailures(t *testing.T) {
	f := newFixture(t)
	f.put("a", Frontmatter{"type": "Note"}, "alpha\n")
	if _, err := f.s.Search(f.ctx, f.r, "notes", "alpha", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.index.db.Exec(`ALTER TABLE sections RENAME TO parked`); err != nil {
		t.Fatal(err)
	}
	// A write stands when the index cannot follow it; the next search
	// reads the file again.
	f.put("a", Frontmatter{"type": "Note"}, "beta\n")
	f.put("b", Frontmatter{"type": "Note"}, "beta\n")
	if _, err := f.s.Search(f.ctx, f.r, "notes", "beta", 0); err == nil || sentinel(err) {
		t.Fatalf("Search without its table = %v", err)
	}
	if err := os.Remove(filepath.Join(f.notes, "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Search(f.ctx, f.r, "notes", "beta", 0); err == nil {
		t.Fatal("Search dropped a row without its table")
	}
	if _, err := f.s.index.db.Exec(`ALTER TABLE parked RENAME TO sections`); err != nil {
		t.Fatal(err)
	}
	hits, err := f.s.Search(f.ctx, f.r, "notes", "beta", 0)
	if err != nil || len(hits) != 1 || hits[0].ID != "b" {
		t.Fatalf("Search after the index recovered = %+v, %v", hits, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Search(f.ctx, f.r, "notes", "beta", 0); err == nil || sentinel(err) {
		t.Fatalf("Search after Close = %v", err)
	}
}

func TestOddValues(t *testing.T) {
	for _, in := range []string{"x: !!bool maybe", "x: !!int many", "x: !!float much"} {
		if _, problems := parseConcept([]byte("---\ntype: a\n" + in + "\n---\n")); !hasField(problems, "frontmatter.x") {
			t.Errorf("%s: problems = %+v", in, problems)
		}
	}
	if _, ok := jsonNumber(json.Number("1e999")); ok {
		t.Error("jsonNumber accepted an out-of-range number")
	}
	if got := numbers(json.Number("1e999")); got != "1e999" {
		t.Errorf("numbers = %#v", got)
	}
	if got := canonicalJSON(map[string]any{"f": func() {}}); !strings.HasPrefix(got, "\x00") {
		t.Errorf("canonicalJSON of a func = %q", got)
	}
	// A value that is not JSON-shaped cannot read back the same.
	if _, err := renderFrontmatter(Frontmatter{"type": "a", "x": struct{}{}}, nil); err == nil {
		t.Error("renderFrontmatter accepted a value that does not read back")
	}
	text, err := renderFrontmatter(Frontmatter{"type": "a", "x": " lead\nnext", "y": "tab\tand\nline", "l": []any{"a\nb", "c"}}, nil)
	if err != nil || !strings.Contains(text, `x: " lead\nnext"`) || !strings.Contains(text, `y: "tab\tand\nline"`) {
		t.Errorf("render = %q, %v", text, err)
	}
	if n := stringNode("a\nb", true); n.Style != yaml.DoubleQuotedStyle {
		t.Errorf("safe multi-line style = %v", n.Style)
	}
	e := invalid("kb", "id", FieldError{Field: "a", Reason: "is bad"}, FieldError{Field: "b", Reason: "is worse"})
	if got := e.Error(); got != `invalid concept "id" in "kb": a is bad; b is worse` {
		t.Errorf("Error = %q", got)
	}
	if got := (&ValidationError{}).Error(); got != "invalid concept" {
		t.Errorf("Error = %q", got)
	}
}

func TestResolverFailureOnEveryOperation(t *testing.T) {
	s := newStore(t, Options{})
	r := errResolver{errors.New("boom")}
	ctx := context.Background()
	if _, err := s.Put(ctx, r, "x", "a", Frontmatter{"type": "a"}, ""); err == nil || sentinel(err) {
		t.Errorf("Put = %v", err)
	}
	if _, err := s.Search(ctx, r, "x", "a", 0); err == nil || sentinel(err) {
		t.Errorf("Search = %v", err)
	}
	if _, err := s.Verify(ctx, r, "x"); err == nil || sentinel(err) {
		t.Errorf("Verify = %v", err)
	}
}
