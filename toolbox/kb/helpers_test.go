/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// mapResolver resolves names from a map.
type mapResolver map[string]Bundle

func (m mapResolver) Resolve(_ context.Context, name string) (Bundle, error) {
	b, ok := m[name]
	if !ok {
		return Bundle{}, ErrUnknownKB
	}
	return b, nil
}

func (m mapResolver) Bundles(context.Context) ([]Info, error) {
	out := make([]Info, 0, len(m))
	for name, b := range m {
		out = append(out, Info{Name: name, Writable: b.Writable})
	}
	return out, nil
}

// errResolver fails every call.
type errResolver struct{ err error }

func (e errResolver) Resolve(context.Context, string) (Bundle, error) { return Bundle{}, e.err }
func (e errResolver) Bundles(context.Context) ([]Info, error)         { return nil, e.err }

// newStore returns a Store closed at the end of the test.
func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// fixture is a Store over one writable bundle "notes" in a temporary
// directory, and one read-only bundle "ref".
type fixture struct {
	t     *testing.T
	s     *Store
	dir   string
	notes string // root of "notes"
	ref   string // root of "ref"
	r     mapResolver
	ctx   context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, s: newStore(t, Options{}), dir: dir, ctx: context.Background()}
	f.notes = filepath.Join(dir, "okf", "notes")
	f.ref = filepath.Join(dir, "ref")
	mkdir(t, f.ref)
	f.r = mapResolver{
		"notes":      {Root: f.notes, Confine: filepath.Join(dir, "okf"), Writable: true, Create: true},
		"shared/ref": {Root: f.ref},
	}
	return f
}

// put puts a concept and fails the test on an error.
func (f *fixture) put(id string, fm Frontmatter, body string) PutResult {
	f.t.Helper()
	res, err := f.s.Put(f.ctx, f.r, "notes", id, fm, body)
	if err != nil {
		f.t.Fatalf("Put %s: %v", id, err)
	}
	return res
}

// get gets a concept and fails the test on an error.
func (f *fixture) get(id string) *Record {
	f.t.Helper()
	rec, err := f.s.Get(f.ctx, f.r, "notes", id)
	if err != nil {
		f.t.Fatalf("Get %s: %v", id, err)
	}
	return rec
}

// read returns a file of the notes bundle.
func (f *fixture) read(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.notes, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fieldsOf returns the fields of a validation error, failing the test for
// any other error.
func fieldsOf(t *testing.T, err error) []FieldError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error %v is not ErrInvalid", err)
	}
	return ve.Fields
}

// hasField reports whether fields name the field path.
func hasField(fields []FieldError, field string) bool {
	for _, f := range fields {
		if f.Field == field {
			return true
		}
	}
	return false
}

// removeFile removes a file of the notes bundle.
func removeFile(f *fixture, rel string) error {
	return os.Remove(filepath.Join(f.notes, filepath.FromSlash(rel)))
}

// noHostPath fails the test when an error's text holds a path under dir.
func noHostPath(t *testing.T, err error, dir string) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), dir) {
		t.Errorf("error holds a host path: %v", err)
	}
}

// lockedWriter serialises writes for a log handler shared by goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
