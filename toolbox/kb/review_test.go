/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/fsmode"
)

// TestOversizedArgumentsAreRefusedCheaply: sizes are checked on the
// arguments before any work in proportion to them.
func TestOversizedArgumentsAreRefusedCheaply(t *testing.T) {
	f := newFixture(t)
	tags := make([]any, 400000)
	for i := range tags {
		tags[i] = "tag"
	}
	includes := make([]string, 1000000)
	tests := map[string]struct {
		id   string
		fm   Frontmatter
		body string
		want error
	}{
		"huge list":     {"a", Frontmatter{"type": "Note", "tags": tags}, "", ErrTooLarge},
		"huge string":   {"a", Frontmatter{"type": "Note", "title": strings.Repeat("t", 50<<20)}, "", ErrTooLarge},
		"many includes": {"a", Frontmatter{"type": "Note", IncludeKey: includes}, "", ErrTooLarge},
		"huge body":     {"a", Frontmatter{"type": "Note", "tags": tags}, strings.Repeat("b", 50<<20), ErrTooLarge},
		"huge id":       {strings.Repeat("\xff", 50<<20), Frontmatter{"type": "Note"}, "", ErrInvalid},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var err error
			bytes, took := measure(func() { _, err = f.s.Put(f.ctx, f.r, "notes", tt.id, tt.fm, tt.body) })
			if !errors.Is(err, tt.want) {
				t.Fatalf("Put = %v, want %v", err, tt.want)
			}
			if bytes > 1<<20 || took > time.Second {
				t.Fatalf("Put allocated %d bytes in %v", bytes, took)
			}
			if len(err.Error()) > maxErrorText {
				t.Fatalf("error text is %d bytes", len(err.Error()))
			}
		})
	}
	// The cheap bound never refuses what fits.
	if _, err := f.s.Put(f.ctx, f.r, "notes", "fits", Frontmatter{"type": "Note", "title": strings.Repeat("t", 30<<10)}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestControlCharactersInQuery(t *testing.T) {
	f := newFixture(t)
	f.put("a", Frontmatter{"type": "Note"}, "alpha\n")
	for _, q := range []string{"al\x00pha", "\x1b[31m", "a\u0085"} {
		if _, err := f.s.Search(f.ctx, f.r, "notes", q, 0); !hasField(fieldsOf(t, err), "query") {
			t.Errorf("Search %q = %v", q, err)
		}
	}
	if hits, err := f.s.Search(f.ctx, f.r, "notes", "alpha\tbeta\n", 0); err != nil || len(hits) != 1 {
		t.Fatalf("Search with a tab and newline = %v, %v", hits, err)
	}
}

func TestGetIncludesAreBounded(t *testing.T) {
	f := newFixture(t)
	s := newStore(t, Options{Limits: Limits{Includes: 2}})
	for _, id := range []string{"a", "b", "c"} {
		f.put(id, Frontmatter{"type": "Note"}, "")
	}
	writeFile(t, filepath.Join(f.notes, "x.md"), "---\ntype: Note\nx-tenebris-include: [a, 7, gone, b, {k: v}, c, a]\n---\n")
	rec, err := s.Get(f.ctx, f.r, "notes", "x")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rec.Includes))
	for _, inc := range rec.Includes {
		ids = append(ids, inc.ID)
	}
	// The first entries up to the limit are resolved, loaded or missing;
	// entries that are not strings are reported as their JSON.
	if !reflect.DeepEqual(ids, []string{"a"}) || !reflect.DeepEqual(rec.Missing, []string{"7"}) ||
		!reflect.DeepEqual(rec.Omitted, []string{"gone", "b", `{"k":"v"}`, "c"}) {
		t.Fatalf("includes %v missing %v omitted %v", ids, rec.Missing, rec.Omitted)
	}
}

func TestRefusedPutCreatesNothing(t *testing.T) {
	skipAsRoot(t)
	f := newFixture(t)
	okf := filepath.Join(f.dir, "okf")
	before := tree(t, f.dir)
	// Refused by the checks: the bundle is never created.
	_, err := f.s.Put(f.ctx, f.r, "notes", "sub/a", Frontmatter{"type": "Note", IncludeKey: []any{"gone"}}, "")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Put = %v", err)
	}
	f.r["ruled"] = Bundle{Root: filepath.Join(okf, "ruled"), Confine: okf, Writable: true, Create: true, Rules: []Rule{{Schema: "s.json"}}}
	if _, err := f.s.Put(f.ctx, f.r, "ruled", "a", Frontmatter{"type": "Note"}, ""); err == nil || sentinel(err) {
		t.Fatalf("Put with a rule in a missing bundle = %v", err)
	}
	if after := tree(t, f.dir); after != before {
		t.Fatalf("tree changed:\n%s\nwas\n%s", after, before)
	}
	// Failing after directories were made: they are removed again.
	f.r["locked"] = Bundle{
		Root: filepath.Join(okf, "deep", "locked"), Confine: okf, Writable: true, Create: true,
		Modes: fsmode.Modes{Dir: 0o500},
	}
	if _, err := f.s.Put(f.ctx, f.r, "locked", "a", Frontmatter{"type": "Note"}, ""); err == nil {
		t.Fatal("Put into an unwritable new bundle succeeded")
	}
	if after := tree(t, f.dir); after != before {
		t.Fatalf("tree changed:\n%s\nwas\n%s", after, before)
	}
	f.put("x", Frontmatter{"type": "Note"}, "")
	before = tree(t, f.dir)
	f.r["notes"] = Bundle{Root: f.notes, Writable: true, Modes: fsmode.Modes{Dir: 0o500}}
	if _, err := f.s.Put(f.ctx, f.r, "notes", "a/b/c", Frontmatter{"type": "Note"}, ""); err == nil {
		t.Fatal("Put under an unwritable new directory succeeded")
	}
	if after := tree(t, f.dir); after != before {
		t.Fatalf("tree changed:\n%s\nwas\n%s", after, before)
	}
}

func TestIndexKeepsRecentBundles(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, Options{Limits: Limits{IndexedBundles: 2}})
	r := mapResolver{}
	for _, name := range []string{"a", "b", "c"} {
		r[name] = Bundle{Root: filepath.Join(dir, name), Writable: true, Create: true}
		if _, err := s.Put(context.Background(), r, name, "x", Frontmatter{"type": "Note"}, "word "+name+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	s.index.mu.Lock()
	kept := len(s.index.bundles)
	_, hasA := s.index.bundles[filepath.Join(dir, "a")]
	s.index.mu.Unlock()
	if kept != 2 || hasA {
		t.Fatalf("index keeps %d bundles (a kept: %v)", kept, hasA)
	}
	var rows int
	if err := s.index.db.QueryRow(`SELECT count(*) FROM sections WHERE bundle = ?`, filepath.Join(dir, "a")).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows of the dropped bundle = %d, %v", rows, err)
	}
	// A dropped bundle is rebuilt when it is searched.
	hits, err := s.Search(context.Background(), r, "a", "word", 0)
	if err != nil || len(hits) != 1 || hits[0].ID != "x" {
		t.Fatalf("Search of a dropped bundle = %v, %v", hits, err)
	}
}

func TestBundlesDoNotWaitForEachOther(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, Options{})
	r := mapResolver{
		"a": {Root: filepath.Join(dir, "a"), Writable: true, Create: true},
		"b": {Root: filepath.Join(dir, "b"), Writable: true, Create: true},
	}
	held := s.index.acquire(context.Background(), filepath.Join(dir, "a")) // a long search of a
	done := make(chan error, 1)
	go func() {
		_, err := s.Put(context.Background(), r, "b", "x", Frontmatter{"type": "Note"}, "")
		if err == nil {
			_, err = s.Search(context.Background(), r, "b", "x", 0)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a put and search of one bundle waited for another's index")
	}
	held.mu.Unlock()
}

func TestNamesAreTheCallers(t *testing.T) {
	f := newFixture(t)
	f.r["alias"] = f.r["notes"]
	res, err := f.s.Put(f.ctx, f.r, "alias", "a", Frontmatter{"type": "Note"}, "")
	if err != nil || res.KB != "alias" {
		t.Fatalf("Put = %+v, %v", res, err)
	}
	if rec, getErr := f.s.Get(f.ctx, f.r, "alias", "a"); getErr != nil || rec.KB != "alias" {
		t.Fatalf("Get = %+v, %v", rec, getErr)
	}
	_, err = f.s.Put(f.ctx, f.r, "alias", "b", Frontmatter{"type": "Note", IncludeKey: []any{"gone"}}, "")
	if ve, ok := errors.AsType[*ValidationError](err); !ok || ve.KB != "alias" || !strings.Contains(err.Error(), fmt.Sprintf("%q", "alias")) {
		t.Fatalf("Put = %v", err)
	}
}

func TestPutSizeChecks(t *testing.T) {
	f := newFixture(t)
	f.put("b", Frontmatter{"type": "Note"}, "")
	// One include given alone counts as one entry.
	s := newStore(t, Options{Limits: Limits{Includes: 1, Frontmatter: 64}})
	if _, err := s.Put(f.ctx, f.r, "notes", "a", Frontmatter{"type": "Note", IncludeKey: "b"}, ""); err != nil {
		t.Fatal(err)
	}
	keys := map[string]any{}
	for i := range 100 {
		keys[fmt.Sprintf("key%03d", i)] = 1
	}
	long := []string{strings.Repeat("t", 40), strings.Repeat("u", 40)}
	// A lower bound under the limit, but more once written: refused after
	// rendering, before anything is written.
	fits := Frontmatter{"type": "Note", "title": strings.Repeat("t", 40)}
	for name, fm := range map[string]Frontmatter{
		"many keys":    {"type": "Note", "x": keys},
		"long tags":    {"type": "Note", "tags": long},
		"when written": fits,
	} {
		limit := 64
		if name == "when written" {
			limit = minBytes(map[string]any(fits), 1<<20) + 1
		}
		s := newStore(t, Options{Limits: Limits{Frontmatter: limit}})
		if _, err := s.Put(f.ctx, f.r, "notes", "c", fm, ""); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: Put = %v", name, err)
		}
	}
	if _, err := f.s.Put(f.ctx, f.r, "notes", "c", Frontmatter{"type": "Note", "n": json.Number("1e400")}, ""); !hasField(fieldsOf(t, err), "frontmatter.n") {
		t.Errorf("Put with an out-of-range number = %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.notes, "c.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused put wrote: %v", err)
	}
}
