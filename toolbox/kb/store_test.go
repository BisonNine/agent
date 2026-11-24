/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/fsmode"
)

func TestNewLimits(t *testing.T) {
	s := newStore(t, Options{Limits: Limits{Body: 10}})
	got := s.Limits()
	want := DefaultLimits
	want.Body = 10
	if got != want {
		t.Fatalf("Limits = %+v, want %+v", got, want)
	}
	if _, err := New(Options{Limits: Limits{Includes: -1}}); err == nil || !strings.Contains(err.Error(), "Includes") {
		t.Fatalf("New with a negative limit = %v", err)
	}
	if _, err := New(Options{Limits: Limits{SearchDefault: 30}}); err == nil {
		t.Fatal("New accepted SearchDefault over SearchMax")
	}
}

func TestPutThenGet(t *testing.T) {
	f := newFixture(t)
	res := f.put("guidance/crypto-review", Frontmatter{
		"type": "Evaluation Guidance", "title": "Review cryptographic claims", "tags": []string{"cryptography"},
	}, "Record the evidence for each claimed algorithm and key size.\n")
	if !res.Created || res.KB != "notes" || res.ID != "guidance/crypto-review" {
		t.Fatalf("Put = %+v", res)
	}
	want := "---\ntype: Evaluation Guidance\ntitle: Review cryptographic claims\ntags: [cryptography]\n---\nRecord the evidence for each claimed algorithm and key size.\n"
	if got := f.read("guidance/crypto-review.md"); got != want {
		t.Fatalf("file =\n%s\nwant\n%s", got, want)
	}
	rec := f.get("guidance/crypto-review")
	if rec.KB != "notes" || rec.Frontmatter.Title() != "Review cryptographic claims" || rec.Body != "Record the evidence for each claimed algorithm and key size.\n" {
		t.Fatalf("Get = %+v", rec)
	}
	res = f.put("guidance/crypto-review", Frontmatter{"type": "Evaluation Guidance"}, "")
	if res.Created {
		t.Fatal("a replacement reported Created")
	}
	rec = f.get("guidance/crypto-review")
	if !reflect.DeepEqual(rec.Frontmatter, Frontmatter{"type": "Evaluation Guidance"}) || rec.Body != "" {
		t.Fatalf("after replacing the whole record: %+v", rec)
	}
}

func TestPutKeepsInternalDots(t *testing.T) {
	f := newFixture(t)
	f.put("SFR/FCS_COP.1", Frontmatter{"type": "SFR"}, "x\n")
	if _, err := os.Stat(filepath.Join(f.notes, "SFR", "FCS_COP.1.md")); err != nil {
		t.Fatal(err)
	}
}

func TestGetPutRoundTripKeepsExtensions(t *testing.T) {
	f := newFixture(t)
	original := "---\n# owner comment\ntype: Playbook\nx-acme-owner: {team: data, people: [alice, bob]}\ntags: oncall\ngenerated: { by: \"human:alice\", at: 2026-04-12T09:00:00Z }\n---\n# Trigger\n\nText.\r\n"
	writeFile(t, filepath.Join(f.notes, "playbooks", "alert.md"), original)
	rec := f.get("playbooks/alert")
	f.put("playbooks/alert", rec.Frontmatter, rec.Body)
	if got := f.read("playbooks/alert.md"); got != original {
		t.Fatalf("get then put changed the file:\n%q\nwant\n%q", got, original)
	}
	rec.Frontmatter["title"] = "Freshness alert"
	f.put("playbooks/alert", rec.Frontmatter, rec.Body)
	want := strings.Replace(original, "---\n# Trigger", "title: Freshness alert\n---\n# Trigger", 1)
	if got := f.read("playbooks/alert.md"); got != want {
		t.Fatalf("after adding a title:\n%q\nwant\n%q", got, want)
	}
}

func TestAccess(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.ref, "a.md"), "---\ntype: Note\n---\nshared\n")
	_, err := f.s.Put(f.ctx, f.r, "shared/ref", "a", Frontmatter{"type": "Note"}, "")
	if !errors.Is(err, ErrReadOnly) || !strings.Contains(err.Error(), `"shared/ref"`) {
		t.Fatalf("Put on a read-only bundle = %v", err)
	}
	if rec, err := f.s.Get(f.ctx, f.r, "shared/ref", "a"); err != nil || rec.KB != "shared/ref" {
		t.Fatalf("Get on a read-only bundle = %+v, %v", rec, err)
	}
	for _, r := range []Resolver{f.r, nil} {
		if _, err := f.s.Get(f.ctx, r, "nope", "a"); !errors.Is(err, ErrUnknownKB) || !strings.Contains(err.Error(), `"nope"`) {
			t.Fatalf("Get on an unknown bundle = %v", err)
		}
	}
	boom := errors.New("boom")
	if _, err := f.s.Get(f.ctx, errResolver{boom}, "x", "a"); !errors.Is(err, boom) || errors.Is(err, ErrUnknownKB) {
		t.Fatalf("Get with a failing resolver = %v", err)
	}
}

func TestCreate(t *testing.T) {
	f := newFixture(t)
	// A bundle that may be created reads as empty until it is.
	if _, err := f.s.Get(f.ctx, f.r, "notes", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before the bundle exists = %v", err)
	}
	if hits, err := f.s.Search(f.ctx, f.r, "notes", "x", 0); err != nil || hits == nil || len(hits) != 0 {
		t.Fatalf("Search before the bundle exists = %v, %v", hits, err)
	}
	if fields, err := f.s.Verify(f.ctx, f.r, "notes"); err != nil || fields != nil {
		t.Fatalf("Verify before the bundle exists = %v, %v", fields, err)
	}
	f.put("a", Frontmatter{"type": "Note"}, "")
	// One that may not be created is unknown without its directory.
	missing := filepath.Join(f.dir, "missing")
	f.r["fixed"] = Bundle{Root: missing, Writable: true}
	if _, err := f.s.Put(f.ctx, f.r, "fixed", "a", Frontmatter{"type": "Note"}, ""); !errors.Is(err, ErrUnknownKB) {
		t.Fatalf("Put without Create = %v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Put without Create made the directory: %v", err)
	}
	for name, op := range map[string]func() error{
		"get":    func() error { _, err := f.s.Get(f.ctx, f.r, "fixed", "a"); return err },
		"search": func() error { _, err := f.s.Search(f.ctx, f.r, "fixed", "a", 0); return err },
		"verify": func() error { _, err := f.s.Verify(f.ctx, f.r, "fixed"); return err },
	} {
		if err := op(); !errors.Is(err, ErrUnknownKB) {
			t.Errorf("%s without the directory = %v", name, err)
		}
	}
	// Create without Confine makes the root and its parents.
	f.r["fresh"] = Bundle{Root: filepath.Join(f.dir, "deep", "fresh"), Writable: true, Create: true}
	if _, err := f.s.Put(f.ctx, f.r, "fresh", "a", Frontmatter{"type": "Note"}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestIncludes(t *testing.T) {
	f := newFixture(t)
	f.put("b", Frontmatter{"type": "Note", "sources": []any{map[string]any{"resource": "https://example.com"}}}, "B\n")
	f.put("c", Frontmatter{"type": "Note", IncludeKey: []any{"b"}}, "C\n")
	f.put("a", Frontmatter{"type": "Note", IncludeKey: []any{"c", "b"}}, "A\n")
	rec := f.get("a")
	if len(rec.Includes) != 2 || rec.Includes[0].ID != "c" || rec.Includes[1].ID != "b" || rec.Missing != nil {
		t.Fatalf("includes = %+v, missing %v", rec.Includes, rec.Missing)
	}
	if rec.Includes[0].Includes != nil || rec.Includes[1].Frontmatter.Sources()[0]["resource"] != "https://example.com" {
		t.Fatalf("included records = %+v", rec.Includes)
	}
	// Put refuses a missing include, a non-concept and itself.
	writeFile(t, filepath.Join(f.notes, "README.md"), "# Not a concept\n")
	_, err := f.s.Put(f.ctx, f.r, "notes", "d", Frontmatter{"type": "Note", IncludeKey: []any{"b", "gone", "README"}}, "")
	fields := fieldsOf(t, err)
	for _, want := range []string{"frontmatter.x-tenebris-include[1]", "frontmatter.x-tenebris-include[2]"} {
		if !hasField(fields, want) {
			t.Errorf("fields %+v lack %s", fields, want)
		}
	}
	_, err = f.s.Put(f.ctx, f.r, "notes", "d", Frontmatter{"type": "Note", IncludeKey: []any{"d"}}, "")
	if fields := fieldsOf(t, err); !hasField(fields, "frontmatter.x-tenebris-include[0]") {
		t.Errorf("a self-include was accepted: %+v", fields)
	}
	// Get reports what is missing in the file, and does not fail.
	writeFile(t, filepath.Join(f.notes, "e.md"), "---\ntype: Note\nx-tenebris-include: [b, gone, e, b, ../x, README, broken, big]\n---\n")
	writeFile(t, filepath.Join(f.notes, "broken.md"), "---\ntype: [\n---\n")
	writeFile(t, filepath.Join(f.notes, "big.md"), "---\ntype: Note\n---\n"+strings.Repeat("x", f.s.readCap()))
	rec = f.get("e")
	if len(rec.Includes) != 1 || rec.Includes[0].ID != "b" || !reflect.DeepEqual(rec.Missing, []string{"gone", "../x", "README", "broken", "big"}) {
		t.Fatalf("includes %+v missing %v", rec.Includes, rec.Missing)
	}
}

func TestErrors(t *testing.T) {
	f := newFixture(t)
	f.put("a", Frontmatter{"type": "Note"}, "")
	writeFile(t, filepath.Join(f.notes, "broken.md"), "---\ntype: a\ntype: b\n---\n")
	writeFile(t, filepath.Join(f.notes, "big.md"), "---\ntype: Note\n---\n"+strings.Repeat("x", f.s.readCap()))
	writeFile(t, filepath.Join(f.notes, "plain.md"), "no frontmatter\n")
	tests := []struct {
		id   string
		want error
	}{
		{"missing", ErrNotFound},
		{"A", ErrNotFound},
		{"plain", ErrNotFound},
		{"broken", ErrInvalid},
		{"big", ErrTooLarge},
		{"bad/../id", ErrInvalid},
	}
	for _, tt := range tests {
		_, err := f.s.Get(f.ctx, f.r, "notes", tt.id)
		if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.id) {
			t.Errorf("Get %s = %v, want %v naming it", tt.id, err, tt.want)
		}
	}
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.s.Get(ctx, f.r, "notes", "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get with a cancelled context = %v", err)
	}
	if _, err := f.s.Put(ctx, f.r, "notes", "a", Frontmatter{"type": "Note"}, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Put with a cancelled context = %v", err)
	}
	if _, err := f.s.Search(ctx, f.r, "notes", "x", 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Search with a cancelled context = %v", err)
	}
	if _, err := f.s.Verify(ctx, f.r, "notes"); !errors.Is(err, context.Canceled) {
		t.Errorf("Verify with a cancelled context = %v", err)
	}
}

func TestPutRefusals(t *testing.T) {
	f := newFixture(t)
	s := newStore(t, Options{Limits: Limits{Body: 10, Frontmatter: 40, Includes: 1}})
	f.put("x", Frontmatter{"type": "Note"}, "")
	f.put("y", Frontmatter{"type": "Note"}, "")
	tests := []struct {
		name string
		id   string
		fm   Frontmatter
		body string
		want error
		in   string
	}{
		{"bad id", "a/../b", Frontmatter{"type": "Note"}, "", ErrInvalid, "concept_id"},
		{"no type", "a", Frontmatter{}, "", ErrInvalid, "frontmatter"},
		{"nil frontmatter", "a", nil, "", ErrInvalid, "frontmatter"},
		{"body not UTF-8", "a", Frontmatter{"type": "Note"}, "\xff", ErrInvalid, "body"},
		{"value not UTF-8", "a", Frontmatter{"type": "Note", "t": "\xff"}, "", ErrInvalid, "frontmatter.t"},
		{"not encodable", "a", Frontmatter{"type": "Note", "f": make(chan int)}, "", ErrInvalid, "frontmatter"},
		{"body too large", "a", Frontmatter{"type": "Note"}, strings.Repeat("b", 11), ErrTooLarge, "body"},
		{"frontmatter too large", "a", Frontmatter{"type": "Note", "title": strings.Repeat("t", 40)}, "", ErrTooLarge, "frontmatter"},
		{"too many includes", "a", Frontmatter{"type": "Note", IncludeKey: []any{"x", "y"}}, "", ErrTooLarge, IncludeKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Put(f.ctx, f.r, "notes", tt.id, tt.fm, tt.body)
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), tt.in) {
				t.Fatalf("Put = %v, want %v about %s", err, tt.want, tt.in)
			}
		})
	}
}

func TestCaseAndTargetType(t *testing.T) {
	f := newFixture(t)
	f.put("Guide/Alpha", Frontmatter{"type": "Note"}, "")
	for _, id := range []string{"guide/beta", "Guide/alpha"} {
		_, err := f.s.Put(f.ctx, f.r, "notes", id, Frontmatter{"type": "Note"}, "")
		if fields := fieldsOf(t, err); !strings.Contains(fields[0].Reason, "only in case") {
			t.Fatalf("Put %s = %v", id, err)
		}
	}
	mkdir(t, filepath.Join(f.notes, "dir.md"))
	_, err := f.s.Put(f.ctx, f.r, "notes", "dir", Frontmatter{"type": "Note"}, "")
	if fields := fieldsOf(t, err); !strings.Contains(fields[0].Reason, "not a regular file") {
		t.Fatalf("Put onto a directory = %v", err)
	}
	if _, getErr := f.s.Get(f.ctx, f.r, "notes", "dir"); !errors.Is(getErr, ErrNotFound) {
		t.Fatalf("Get of a directory = %v", getErr)
	}
	writeFile(t, filepath.Join(f.notes, "asset"), "binary")
	_, err = f.s.Put(f.ctx, f.r, "notes", "asset/x", Frontmatter{"type": "Note"}, "")
	if fields := fieldsOf(t, err); !strings.Contains(fields[0].Reason, "not a directory") {
		t.Fatalf("Put under a file = %v", err)
	}
	if _, err := f.s.Get(f.ctx, f.r, "notes", "asset/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get under a file = %v", err)
	}
}

func TestConfinement(t *testing.T) {
	f := newFixture(t)
	f.put("inside", Frontmatter{"type": "Note"}, "")
	outside := filepath.Join(f.dir, "outside")
	writeFile(t, filepath.Join(outside, "secret.md"), "---\ntype: Note\n---\nsecret\n")
	okf := filepath.Join(f.dir, "okf")

	// A bundle directory under Confine that links out.
	if err := os.Symlink(outside, filepath.Join(okf, "escape")); err != nil {
		t.Fatal(err)
	}
	f.r["escape"] = Bundle{Root: filepath.Join(okf, "escape"), Confine: okf, Writable: true, Create: true}
	if _, err := f.s.Get(f.ctx, f.r, "escape", "secret"); !errors.Is(err, ErrUnknownKB) {
		t.Fatalf("Get through a bundle link out of Confine = %v", err)
	} else {
		noHostPath(t, err, f.dir)
	}
	if _, err := f.s.Put(f.ctx, f.r, "escape", "planted", Frontmatter{"type": "Note"}, ""); err == nil {
		t.Fatal("Put wrote through a bundle link out of Confine")
	}
	if _, err := f.s.Search(f.ctx, f.r, "escape", "secret", 0); err == nil {
		t.Fatal("Search read through a bundle link out of Confine")
	}

	// A subdirectory and a file that link out of the bundle.
	if err := os.Symlink(outside, filepath.Join(f.notes, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(f.notes, "file.md")); err != nil {
		t.Fatal(err)
	}
	// A link that stays inside is not followed either.
	if err := os.Symlink(filepath.Join(f.notes, "inside.md"), filepath.Join(f.notes, "alias.md")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sub/secret", "file", "alias"} {
		if _, err := f.s.Get(f.ctx, f.r, "notes", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get %s = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := f.s.Put(f.ctx, f.r, "notes", "sub/planted", Frontmatter{"type": "Note"}, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("Put through a linked subdirectory = %v", err)
	}
	for _, id := range []string{"file", "alias"} {
		if _, err := f.s.Put(f.ctx, f.r, "notes", id, Frontmatter{"type": "Note"}, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("Put onto link %s = %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a file was written outside the bundle")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "secret.md")); err != nil || string(data) != "---\ntype: Note\n---\nsecret\n" {
		t.Fatal("a file outside the bundle was changed")
	}
	hits, err := f.s.Search(f.ctx, f.r, "notes", "secret", 0)
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search found through links: %v, %v", hits, err)
	}
	if fields, err := f.s.Verify(f.ctx, f.r, "notes"); err != nil || fields != nil {
		t.Fatalf("Verify = %v, %v", fields, err)
	}
}

func TestConcurrentPuts(t *testing.T) {
	f := newFixture(t)
	f.put("c", Frontmatter{"type": "Note"}, "first\n")
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			_, err := f.s.Put(f.ctx, f.r, "notes", "c", Frontmatter{"type": "Note", "title": fmt.Sprintf("v%d", i)}, fmt.Sprintf("body %d\n", i))
			errs <- err
		})
	}
	// Readers see a whole file, old or new, throughout.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			rec, err := f.s.Get(f.ctx, f.r, "notes", "c")
			if err != nil {
				t.Errorf("Get during puts: %v", err)
				return
			}
			if rec.Body != "first\n" && rec.Body != "body "+strings.TrimPrefix(rec.Frontmatter.Title(), "v")+"\n" {
				t.Errorf("torn read: %+v", rec)
				return
			}
		}
	})
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rec := f.get("c")
	i := strings.TrimPrefix(rec.Frontmatter.Title(), "v")
	if rec.Body != "body "+i+"\n" {
		t.Fatalf("final file mixes inputs: %+v", rec)
	}
	entries, err := os.ReadDir(f.notes)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temporary file left: %s", e.Name())
		}
	}
}

func TestFailedPutKeepsOldBytes(t *testing.T) {
	f := newFixture(t)
	f.put("c", Frontmatter{"type": "Note", "title": "kept"}, "old\n")
	before := f.read("c.md")
	_, err := f.s.Put(f.ctx, f.r, "notes", "c", Frontmatter{"type": "Note", "sources": []any{map[string]any{}}}, "new\n")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Put = %v", err)
	}
	if got := f.read("c.md"); got != before {
		t.Fatalf("file changed after a refused put: %q", got)
	}
}

func TestModes(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, Options{Modes: fsmode.Modes{Dir: 0o750, File: 0o640}})
	r := mapResolver{
		"own":    {Root: filepath.Join(dir, "own"), Writable: true, Create: true},
		"shared": {Root: filepath.Join(dir, "shared"), Writable: true, Create: true, Modes: fsmode.Modes{File: 0o644}},
	}
	ctx := context.Background()
	if _, err := s.Put(ctx, r, "own", "sub/a", Frontmatter{"type": "Note"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, r, "shared", "a", Frontmatter{"type": "Note"}, ""); err != nil {
		t.Fatal(err)
	}
	checks := map[string]fs.FileMode{
		"own": 0o750, "own/sub": 0o750, "own/sub/a.md": 0o640, "shared": 0o750, "shared/a.md": 0o644,
	}
	for rel, want := range checks {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", rel, got, want)
		}
	}
	// An update keeps the existing file's mode.
	if err := os.Chmod(filepath.Join(dir, "own", "sub", "a.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, r, "own", "sub/a", Frontmatter{"type": "Note", "title": "t"}, ""); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "own", "sub", "a.md")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("update changed the mode: %v, %v", info, err)
	}
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	f.put("SFR/FCS_COP.1", Frontmatter{"type": "SFR", "title": "Cryptographic operation"},
		"Intro paragraph.\n\n# Application note\nThe TOE shall perform encryption with AES-256 keys.\n\n# Evaluation\nCheck the key sizes.\n")
	f.put("notes/untitled", Frontmatter{"type": "Note"}, "Nothing about ciphers here.\n")
	hits, err := f.s.Search(f.ctx, f.r, "notes", "AES encryption", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "SFR/FCS_COP.1" || hits[0].Title != "Cryptographic operation" ||
		hits[0].Section != "Application note" || !strings.Contains(hits[0].Passage, "AES-256") || hits[0].Score <= 0 {
		t.Fatalf("hits = %+v", hits)
	}
	// The ID weighs: every section of the concept matches its ID.
	hits, err = f.s.Search(f.ctx, f.r, "notes", "FCS_COP.1", 2)
	if err != nil || len(hits) != 2 || hits[0].ID != "SFR/FCS_COP.1" {
		t.Fatalf("hits by ID = %+v, %v", hits, err)
	}
	hits, err = f.s.Search(f.ctx, f.r, "notes", "untitled", 0)
	if err != nil || len(hits) != 1 || hits[0].Title != "untitled" || hits[0].Section != "" {
		t.Fatalf("hit with no title = %+v, %v", hits, err)
	}
	for _, q := range []string{"zebra", `NEAR( "x" ) -- *`, "title:", "-- **"} {
		hits, err = f.s.Search(f.ctx, f.r, "notes", q, 0)
		if err != nil || hits == nil || len(hits) != 0 {
			t.Fatalf("Search %q = %v, %v; want an empty success", q, hits, err)
		}
	}
	for q, want := range map[string]error{"": ErrInvalid, "   ": ErrInvalid, "\xff": ErrInvalid, strings.Repeat("é", 257): ErrTooLarge} {
		if _, err := f.s.Search(f.ctx, f.r, "notes", q, 0); !errors.Is(err, want) {
			t.Errorf("Search %q = %v, want %v", q, err, want)
		}
	}
	if _, err := f.s.Search(f.ctx, f.r, "nope", "x", 0); !errors.Is(err, ErrUnknownKB) {
		t.Fatalf("Search of an unknown bundle = %v", err)
	}
}

func TestSearchLimit(t *testing.T) {
	f := newFixture(t)
	for i := range 25 {
		f.put(fmt.Sprintf("n%02d", i), Frontmatter{"type": "Note"}, "common word\n")
	}
	for limit, want := range map[int]int{0: 5, -1: 5, 3: 3, 20: 20, 50: 20} {
		hits, err := f.s.Search(f.ctx, f.r, "notes", "common", limit)
		if err != nil || len(hits) != want {
			t.Errorf("Search limit %d = %d hits, %v; want %d", limit, len(hits), err, want)
		}
	}
}

func TestSearchFollowsTheBundle(t *testing.T) {
	f := newFixture(t)
	f.put("a", Frontmatter{"type": "Note"}, "alpha\n")
	search := func(q string) []string {
		t.Helper()
		hits, err := f.s.Search(f.ctx, f.r, "notes", q, 20)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return ids
	}
	if got := search("alpha"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("after put: %v", got)
	}
	// A put is seen at once.
	f.put("a", Frontmatter{"type": "Note"}, "beta\n")
	if got := search("alpha"); len(got) != 0 {
		t.Fatalf("old text still found: %v", got)
	}
	if got := search("beta"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("new text not found: %v", got)
	}
	// External edit, add and delete.
	path := filepath.Join(f.notes, "a.md")
	writeFile(t, path, "---\ntype: Note\n---\ngamma, edited outside\n")
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.notes, "dir", "b.md"), "---\ntype: Note\n---\ngamma too\n")
	writeFile(t, filepath.Join(f.notes, ".hidden", "c.md"), "---\ntype: Note\n---\ngamma hidden\n")
	writeFile(t, filepath.Join(f.notes, ".d.md"), "---\ntype: Note\n---\ngamma hidden\n")
	writeFile(t, filepath.Join(f.notes, "index.md"), "---\ntype: Note\n---\ngamma index\n")
	writeFile(t, filepath.Join(f.notes, "dir", "log.md"), "---\ntype: Note\n---\ngamma log\n")
	writeFile(t, filepath.Join(f.notes, "README.md"), "gamma readme\n")
	writeFile(t, filepath.Join(f.notes, "broken.md"), "---\ntype: [\n---\ngamma\n")
	writeFile(t, filepath.Join(f.notes, "big.md"), "---\ntype: Note\n---\ngamma "+strings.Repeat("x", f.s.readCap()))
	writeFile(t, filepath.Join(f.notes, "Index_of.md"), "---\ntype: Note\n---\nunrelated\n")
	writeFile(t, filepath.Join(f.notes, "a.md.md"), "---\ntype: Note\n---\ngamma bad id\n")
	writeFile(t, filepath.Join(f.notes, "asset.txt"), "gamma asset\n")
	if got := search("gamma"); !reflect.DeepEqual(got, []string{"a", "dir/b"}) && !reflect.DeepEqual(got, []string{"dir/b", "a"}) {
		t.Fatalf("after external edits: %v", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := search("gamma"); !reflect.DeepEqual(got, []string{"dir/b"}) {
		t.Fatalf("after an external delete: %v", got)
	}
}

func TestVerify(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.notes, "rules", "s.json"), `{"required": ["title"]}`)
	f.r["notes"] = Bundle{Root: f.notes, Writable: true, Rules: []Rule{{PathPrefix: "ruled/", Schema: "rules/s.json"}}}
	files := map[string]string{
		"good.md":                       "---\ntype: Note\ntitle: t\n---\n",
		"README.md":                     "# not a concept\n",
		"dup.md":                        "---\ntype: a\ntype: b\n---\n",
		"nosource.md":                   "---\ntype: Note\nsources: [{id: x}]\n---\n",
		"ruled/x.md":                    "---\ntype: Note\n---\n",
		"inc.md":                        "---\ntype: Note\nx-tenebris-include: [gone]\n---\n",
		"self.md":                       "---\ntype: Note\nx-tenebris-include: [self]\n---\n",
		"many.md":                       "---\ntype: Note\nx-tenebris-include: [good, inc, nosource]\n---\n",
		"big.md":                        "---\ntype: Note\n---\n" + strings.Repeat("b", 30),
		"bigfm.md":                      "---\ntype: Note\ntitle: " + strings.Repeat("t", 60) + "\n---\n",
		"huge.md":                       "---\ntype: Note\n---\n" + strings.Repeat("h", 200),
		"x.md.md":                       "---\ntype: Note\n---\n",
		strings.Repeat("s", 31) + ".md": "---\ntype: Note\n---\n",
	}
	// Two folders that differ only in case are one folder on a
	// case-insensitive file system, so the collision exists only elsewhere.
	_, statErr := os.Stat(filepath.Join(f.notes, "RULES"))
	caseSensitive := statErr != nil
	if caseSensitive {
		files["Case/a.md"] = "---\ntype: Note\n---\n"
		files["case/b.md"] = "---\ntype: Note\n---\n"
	}
	for rel, content := range files {
		writeFile(t, filepath.Join(f.notes, filepath.FromSlash(rel)), content)
	}
	s := newStore(t, Options{Limits: Limits{Body: 20, Frontmatter: 60, Includes: 2, SegmentBytes: 30}})
	fields, err := s.Verify(f.ctx, f.r, "notes")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"dup":                   "frontmatter.type",
		"nosource":              "frontmatter.sources[0].resource",
		"ruled/x":               "frontmatter",
		"inc":                   "frontmatter.x-tenebris-include[0]",
		"self":                  "frontmatter.x-tenebris-include[0]",
		"many":                  "frontmatter.x-tenebris-include",
		"big":                   "body",
		"bigfm":                 "frontmatter",
		"huge":                  "concept_id",
		"x.md":                  "concept_id",
		strings.Repeat("s", 31): "concept_id",
	}
	if caseSensitive {
		want["case/b"] = "concept_id"
	}
	got := map[string]string{}
	for _, fe := range fields {
		if _, dup := got[fe.ID]; !dup {
			got[fe.ID] = fe.Field
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Verify = %+v\nwant %v", fields, want)
	}
}
