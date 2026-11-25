/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
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
		err := op()
		if err == nil || sentinel(err) {
			t.Errorf("%s over an unreadable directory = %v, want a failure", name, err)
		}
		noHostPath(t, err, f.dir)
	}
}

func TestUnreadableFileIsAFailure(t *testing.T) {
	skipAsRoot(t)
	f := newFixture(t)
	f.put("x", Frontmatter{"type": "Note"}, "")
	chmod(t, filepath.Join(f.notes, "x.md"), 0o000)
	if _, err := f.s.Get(f.ctx, f.r, "notes", "x"); err == nil || sentinel(err) {
		t.Fatalf("Get of an unreadable file = %v", err)
	} else {
		noHostPath(t, err, f.dir)
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

// TestUnusableBundleRoots: a root that cannot be opened or created is an
// unknown knowledge base named as the caller asked, whose error holds no
// host path (it is logged instead), and a put leaves nothing behind.
func TestUnusableBundleRoots(t *testing.T) {
	skipAsRoot(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "file"), "x")
	locked := filepath.Join(dir, "locked")
	mkdir(t, locked)
	chmod(t, locked, 0o500)
	mkdir(t, filepath.Join(dir, "okf", "bob", "notes"))
	if err := os.Symlink(filepath.Join("..", "bob", "notes"), filepath.Join(dir, "okf", "alice")); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(dir, "okf", "carol"))
	if err := os.Symlink(filepath.Join("..", "bob", "notes"), filepath.Join(dir, "okf", "carol", "notes")); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	s := newStore(t, Options{Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil))})
	okf := filepath.Join(dir, "okf")
	for name, b := range map[string]Bundle{
		"under a file":                   {Root: filepath.Join(dir, "file", "x", "y")},
		"in a locked directory":          {Root: filepath.Join(locked, "x", "y")},
		"confined under a file":          {Root: filepath.Join(dir, "file", "x"), Confine: dir},
		"confined in a locked folder":    {Root: filepath.Join(locked, "x"), Confine: locked},
		"confine is a file, no create":   {Root: filepath.Join(dir, "file", "x"), Confine: filepath.Join(dir, "file")},
		"root is a file, no create":      {Root: filepath.Join(dir, "file"), Confine: dir},
		"root is a file, unconfined":     {Root: filepath.Join(dir, "file")},
		"relative root":                  {Root: "relative"},
		"relative confine":               {Root: dir, Confine: "relative"},
		"root outside confine":           {Root: dir, Confine: locked},
		"confine is a file, and create":  {Root: filepath.Join(dir, "file", "x"), Confine: filepath.Join(dir, "file")},
		"linked to a sibling":            {Root: filepath.Join(okf, "alice"), Confine: okf},
		"linked to a sibling, no create": {Root: filepath.Join(okf, "alice"), Confine: okf},
		"under a linked parent":          {Root: filepath.Join(okf, "alice", "x"), Confine: okf},
		"linked root below":              {Root: filepath.Join(okf, "carol", "notes"), Confine: okf},
	} {
		t.Run(name, func(t *testing.T) {
			b.Writable = true
			b.Create = !strings.Contains(name, "no create")
			r := mapResolver{"b": b}
			before := tree(t, dir)
			_, err := s.Put(context.Background(), r, "b", "sub/a", Frontmatter{"type": "Note"}, "")
			if !errors.Is(err, ErrUnknownKB) || !strings.Contains(err.Error(), `"b"`) {
				t.Fatalf("Put = %v, want an unusable bundle", err)
			}
			noHostPath(t, err, dir)
			if after := tree(t, dir); after != before {
				t.Fatalf("a refused put changed the tree:\n%s\nwas\n%s", after, before)
			}
			ops := map[string]func() error{
				"get":    func() error { _, err := s.Get(context.Background(), r, "b", "a"); return err },
				"search": func() error { _, err := s.Search(context.Background(), r, "b", "a", 0); return err },
				"verify": func() error { _, err := s.Verify(context.Background(), r, "b"); return err },
			}
			for op, run := range ops {
				err := run()
				if !b.Create && !errors.Is(err, ErrUnknownKB) {
					t.Errorf("%s = %v, want an unusable bundle", op, err)
				}
				noHostPath(t, err, dir)
			}
		})
	}
	if !strings.Contains(logs.String(), dir) {
		t.Fatalf("the detail was not logged: %s", logs.String())
	}
}

// tree lists the entries under dir, for comparing before and after.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable directory is listed as it is
		}
		b.WriteString(p + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
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
	_, err := canonicalize(Frontmatter{"type": "a", "deep": []any{map[string]any{"n": json.Number("1e400")}}})
	if !hasField(fieldsOf(t, err), "frontmatter.deep[0].n") {
		t.Errorf("an out-of-range number was accepted: %v", err)
	}
	if got := canonicalJSON(map[string]any{"f": func() {}}); !strings.HasPrefix(got, "\x00") {
		t.Errorf("canonicalJSON of a func = %q", got)
	}
	// A value that is not JSON-shaped cannot read back the same.
	if _, renderErr := renderFrontmatter(Frontmatter{"type": "a", "x": struct{}{}}, nil); renderErr == nil {
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
