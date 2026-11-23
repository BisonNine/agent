/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// PutResult reports a successful Put.
type PutResult struct {
	KB      string
	ID      string
	Created bool // false: an existing concept was replaced
}

// Put creates or replaces concept id of knowledge base kb with fm and
// body: the whole record, so keys absent from fm are removed. fm may hold
// any values encoding/json can encode; they are stored as their JSON
// values. A one-item list field may be given as the item.
//
// The record is checked (the frontmatter schema, OKF's fields, the
// bundle's rules, that every include is a concept of the bundle), written
// to a temporary file, read back and compared, then renamed over the
// concept, so a reader sees the old file or the new one and a failed Put
// leaves the old one as it was. Puts to one concept are serialised; the
// last one wins. When a concept is replaced, the text of each unchanged
// frontmatter key is kept as it was.
func (s *Store) Put(ctx context.Context, r Resolver, kb, id string, fm Frontmatter, body string) (PutResult, error) {
	if err := ctx.Err(); err != nil {
		return PutResult{}, err
	}
	b, err := s.resolve(ctx, r, kb)
	if err != nil {
		return PutResult{}, err
	}
	if reason := idProblem(id, s.limits); reason != "" {
		return PutResult{}, invalid(b.name, id, FieldError{Field: "concept_id", Reason: reason})
	}
	if !b.Writable {
		return PutResult{}, fmt.Errorf("%w: %s", ErrReadOnly, quote(b.name))
	}
	record, err := s.checkRecord(b.name, id, fm, body)
	if err != nil {
		return PutResult{}, err
	}
	// Nothing is created until the record has passed every check: a
	// bundle that may be created but is missing is checked as empty.
	key := bundleKey(b)
	fields, err := s.bundleChecks(b, key, id, record)
	if err != nil {
		return PutResult{}, err
	}
	if fields != nil {
		return PutResult{}, invalid(b.name, id, fields...)
	}
	unlock := s.locks.lock(key + "\x00" + strings.ToLower(id))
	defer unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return PutResult{}, ctxErr
	}
	created, err := s.publish(ctx, b, key, id, record, body)
	if err != nil {
		return PutResult{}, err
	}
	return PutResult{KB: b.name, ID: id, Created: created}, nil
}

// checkRecord checks what needs no bundle and returns the frontmatter as
// it will be stored.
func (s *Store) checkRecord(kb, id string, fm Frontmatter, body string) (Frontmatter, error) {
	// Sizes first, on the arguments as given, before any work in
	// proportion to them.
	if len(body) > s.limits.Body {
		return nil, fmt.Errorf("%w: the body is %d bytes; the limit is %d", ErrTooLarge, len(body), s.limits.Body)
	}
	if n := countItems(fm[IncludeKey]); n > s.limits.Includes {
		return nil, fmt.Errorf("%w: %s has %d entries; the limit is %d", ErrTooLarge, IncludeKey, n, s.limits.Includes)
	}
	if minBytes(map[string]any(fm), s.limits.Frontmatter) > s.limits.Frontmatter {
		return nil, fmt.Errorf("%w: the frontmatter is over the %d-byte limit", ErrTooLarge, s.limits.Frontmatter)
	}
	fields := textFields(map[string]any(fm), "frontmatter")
	if !utf8.ValidString(body) {
		fields = append(fields, FieldError{Field: "body", Reason: "is not valid UTF-8"})
	}
	if fields != nil {
		return nil, invalid(kb, id, fields...)
	}
	record, err := canonicalize(fm)
	if ve, ok := errors.AsType[*ValidationError](err); ok {
		return nil, invalid(kb, id, ve.Fields...)
	}
	if err != nil {
		return nil, invalid(kb, id, FieldError{Field: "frontmatter", Reason: "cannot be stored: " + err.Error()})
	}
	if fields := s.recordFields(id, record); fields != nil {
		return nil, invalid(kb, id, fields...)
	}
	return record, nil
}

// bundleFields applies the checks that need the bundle: its rules, and
// that every include is a concept in it.
func (s *Store) bundleChecks(b Bundle, key, id string, fm Frontmatter) ([]FieldError, error) {
	root, err := s.openForRead(b)
	if errors.Is(err, errNoRoot) {
		return s.bundleFields(nil, key, b, id, fm)
	}
	if err != nil {
		return nil, err
	}
	defer closeQuietly(s.log, root)
	return s.bundleFields(root, key, b, id, fm)
}

// bundleFields applies the checks that need the bundle; root nil is a
// bundle that does not exist yet.
func (s *Store) bundleFields(root *os.Root, key string, b Bundle, id string, fm Frontmatter) ([]FieldError, error) {
	fields, err := s.ruleFields(root, key, b, id, fm)
	if err != nil {
		return nil, err
	}
	for i, inc := range fm.Includes() {
		if inc == id || idProblem(inc, s.limits) != "" {
			continue // reported by recordFields
		}
		_, ok, err := s.includedConcept(root, inc)
		if err != nil {
			return nil, err
		}
		if !ok {
			fields = append(fields, FieldError{
				Field:  fmt.Sprintf("frontmatter.%s[%d]", IncludeKey, i),
				Reason: fmt.Sprintf("names %s, which is not a concept in %s", quote(inc), quote(b.name)),
			})
		}
	}
	return fields, nil
}

// publish writes the concept atomically and updates the index. The
// concept's lock is held. created reports that no file was there.
func (s *Store) publish(ctx context.Context, b Bundle, key, id string, fm Frontmatter, body string) (created bool, err error) {
	kb := b.name
	modes := b.Modes.Or(s.modes)
	file := fileName(id)
	mode := modes.File
	root, err := s.openForRead(b)
	if err != nil && !errors.Is(err, errNoRoot) {
		return false, err
	}
	exact := false
	if root != nil {
		defer closeQuietly(s.log, root)
		var conflict string
		if exact, conflict, err = s.lookup(root, file); err != nil {
			return false, err
		}
		if conflict != "" {
			return false, invalid(kb, id, FieldError{Field: "concept_id", Reason: conflict})
		}
	}
	var old *concept
	created = !exact
	if exact {
		info, statErr := root.Lstat(filepath.FromSlash(file))
		if statErr != nil {
			return false, statErr
		}
		if !info.Mode().IsRegular() {
			return false, invalid(kb, id, FieldError{Field: "concept_id", Reason: "names something that is not a regular file"})
		}
		mode = info.Mode().Perm()
		if data, readErr := s.readCapped(root, file, s.readCap()); readErr == nil {
			old, _ = parseConcept(data) // an unreadable old file is replaced whole
		}
	}
	text, err := renderFrontmatter(fm, old)
	if err != nil {
		return false, invalid(kb, id, FieldError{Field: "frontmatter", Reason: "cannot be written so that it reads back the same: " + err.Error()})
	}
	if len(text) > s.limits.Frontmatter {
		return false, fmt.Errorf("%w: the frontmatter is %d bytes written; the limit is %d", ErrTooLarge, len(text), s.limits.Frontmatter)
	}
	// From here on, what a failure leaves behind is removed: the root and
	// the directories made for this write, when empty.
	var undo []func()
	defer func() {
		if err != nil {
			for _, u := range slices.Backward(undo) {
				u()
			}
		}
	}()
	if root == nil {
		var undoRoot func()
		if root, undoRoot, err = s.createRoot(b, modes.Dir); err != nil {
			return false, err
		}
		undo = append(undo, undoRoot)
		defer closeQuietly(s.log, root)
	}
	dir := path.Dir(file)
	if dir != "." {
		made, mkErr := mkdirAllIn(root, dir, modes.Dir)
		undo = append(undo, func() { s.removeDirsIn(root, made) })
		if mkErr != nil {
			return false, fmt.Errorf("kb: create directory for %s: %w", quote(id), mkErr)
		}
	}
	tmp, back, err := s.writeTemp(root, file, renderDocument(text, body), mode)
	if err != nil {
		return false, fmt.Errorf("kb: write %s: %w", quote(id), err)
	}
	if err = s.commit(root, tmp, file, back, fm, body); err != nil {
		s.removeTemp(root, tmp)
		return false, fmt.Errorf("kb: write %s: %w", quote(id), err)
	}
	if syncErr := s.syncDir(root, dir); syncErr != nil {
		s.log.Warn("directory not synced after a write", "id", id, "error", syncErr.Error())
	}
	s.reindex(ctx, root, key, id, fm, body)
	return created, nil
}

// writeTemp writes data to a new dot-prefixed temporary file beside file
// (outside the bundle and the index, OKF §3.2), synced, with mode set
// regardless of the umask. It returns the temporary file's name and its
// contents as read back through the same handle, which the mode (an
// existing file's, kept) may not allow to be reopened for.
func (s *Store) writeTemp(root *os.Root, file string, data []byte, mode fs.FileMode) (string, []byte, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, err
	}
	tmp := path.Join(path.Dir(file), "."+path.Base(file)+"."+hex.EncodeToString(rnd[:])+".tmp")
	f, err := root.OpenFile(filepath.FromSlash(tmp), os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", nil, err
	}
	var back []byte
	err = f.Chmod(mode)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err == nil {
		back, err = io.ReadAll(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		s.removeTemp(root, tmp)
		return "", nil, err
	}
	return tmp, back, nil
}

// removeTemp removes a temporary file after a failure.
func (s *Store) removeTemp(root *os.Root, tmp string) {
	if err := root.Remove(filepath.FromSlash(tmp)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("temporary file not removed", "file", tmp, "error", err.Error())
	}
}

// commit checks that the temporary file read back holds exactly the
// record, and renames it over the concept.
func (s *Store) commit(root *os.Root, tmp, file string, back []byte, fm Frontmatter, body string) error {
	c, problems := parseConcept(back)
	if problems != nil || c == nil || !sameValues(c.fm, fm) || c.body != body {
		return errors.New("the written file does not read back as the record")
	}
	return root.Rename(filepath.FromSlash(tmp), filepath.FromSlash(file))
}

// syncDir syncs a directory of root, so a rename in it is durable.
func (s *Store) syncDir(root *os.Root, dir string) error {
	d, err := root.Open(filepath.FromSlash(dir))
	if err != nil {
		return err
	}
	defer closeQuietly(s.log, d)
	return d.Sync()
}

// reindex updates the index after a write. A failure is logged and the
// concept left for the next search to read again: the write stands.
func (s *Store) reindex(ctx context.Context, root *os.Root, key, id string, fm Frontmatter, body string) {
	bi := s.index.acquire(ctx, key)
	defer bi.mu.Unlock()
	info, err := root.Lstat(filepath.FromSlash(fileName(id)))
	if err == nil {
		st := indexed{size: info.Size(), mod: info.ModTime()}
		err = bi.replace(context.WithoutCancel(ctx), id, st, displayTitle(id, fm), splitSections(body))
	}
	if err != nil {
		s.log.Warn("search index not updated after a write", "id", id, "error", err.Error())
		bi.forget(id)
	}
}

// countItems is the number of entries a list field holds as given: the
// length of a slice, 0 for nil, 1 for anything else.
func countItems(v any) int {
	if v == nil {
		return 0
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		return rv.Len()
	}
	return 1
}

// minBytes returns a lower bound of the bytes v takes written as YAML (its
// strings and keys, and a separator per item), or a value over limit as
// soon as the bound passes it, so an oversized value costs work in
// proportion to the limit, not to its size. Values that are not
// JSON-shaped count as one byte here; the exact size is checked when the
// record is written.
func minBytes(v any, limit int) int {
	n := 0
	var walk func(v any) bool
	walk = func(v any) bool {
		if n > limit {
			return false
		}
		switch v := v.(type) {
		case string:
			n += len(v)
		case map[string]any:
			for k, item := range v {
				n += len(k) + 2
				if !walk(item) {
					return false
				}
			}
		case []any:
			for _, item := range v {
				n += 2
				if !walk(item) {
					return false
				}
			}
		case []string:
			for _, item := range v {
				n += 2 + len(item)
				if n > limit {
					return false
				}
			}
		default:
			n++
		}
		return n <= limit
	}
	walk(v)
	return n
}
