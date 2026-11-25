/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Record is one concept as Get returns it.
type Record struct {
	KB          string
	ID          string
	Frontmatter Frontmatter
	Body        string
	// Includes are the concepts the frontmatter lists under IncludeKey,
	// one level deep, each once, in the order listed. Their own includes
	// are not followed.
	Includes []Record
	// Missing lists the include entries that are not concepts in the
	// bundle (absent, not a concept, unreadable, or not a concept ID; an
	// entry that is not a string is given as its JSON text). Get does not
	// fail for them.
	Missing []string
	// Omitted lists the include entries beyond the first Limits.Includes,
	// which are not loaded.
	Omitted []string
}

// Get returns the concept id of knowledge base kb, with its inclusions.
func (s *Store) Get(ctx context.Context, r Resolver, kb, id string) (*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := s.resolve(ctx, r, kb)
	if err != nil {
		return nil, err
	}
	if reason := idProblem(id, s.limits); reason != "" {
		return nil, invalid(b.name, id, FieldError{Field: "concept_id", Reason: reason})
	}
	root, err := s.openForRead(b)
	if errors.Is(err, errNoRoot) {
		return nil, notFound(b.name, id)
	}
	if err != nil {
		return nil, err
	}
	defer closeQuietly(s.log, root)
	c, problems, err := s.readConcept(root, id)
	if err != nil {
		return nil, s.readError(err, b.name, id)
	}
	if problems != nil {
		return nil, invalid(b.name, id, problems...)
	}
	rec := &Record{KB: b.name, ID: id, Frontmatter: c.fm, Body: c.body}
	seen := map[string]bool{id: true}
	for _, entry := range asList(c.fm[IncludeKey]) {
		inc, isString := entry.(string)
		if !isString {
			inc = clip(canonicalJSON(entry), maxFieldPath)
		}
		if seen[inc] {
			continue
		}
		seen[inc] = true
		switch {
		case len(rec.Includes)+len(rec.Missing) >= s.limits.Includes:
			rec.Omitted = append(rec.Omitted, inc)
			continue
		case !isString:
			rec.Missing = append(rec.Missing, inc)
			continue
		}
		ic, ok, err := s.includedConcept(root, inc)
		if err != nil {
			return nil, err
		}
		if !ok {
			rec.Missing = append(rec.Missing, inc)
			continue
		}
		rec.Includes = append(rec.Includes, Record{KB: b.name, ID: inc, Frontmatter: ic.fm, Body: ic.body})
	}
	return rec, nil
}

// includedConcept reads an included concept. ok is false when it is not a
// readable concept; err is set only for an unexpected failure.
func (s *Store) includedConcept(root *os.Root, id string) (*concept, bool, error) {
	if root == nil || idProblem(id, s.limits) != "" {
		return nil, false, nil
	}
	c, problems, err := s.readConcept(root, id)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTooLarge):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case problems != nil:
		s.log.Warn("included concept is not valid", "id", id, "problem", problems[0].Field+" "+problems[0].Reason)
		return nil, false, nil
	}
	return c, true, nil
}

// resolve asks the resolver for a bundle.
func (s *Store) resolve(ctx context.Context, r Resolver, name string) (Bundle, error) {
	if r == nil {
		return Bundle{}, fmt.Errorf("%w: %s", ErrUnknownKB, quote(name))
	}
	b, err := r.Resolve(ctx, name)
	if errors.Is(err, ErrUnknownKB) {
		return Bundle{}, fmt.Errorf("%w: %s", ErrUnknownKB, quote(name))
	}
	if err != nil {
		return Bundle{}, fmt.Errorf("kb: resolve %s: %w", quote(name), err)
	}
	b.name = name
	return b, nil
}

// openForRead opens a bundle root without creating it. A missing root is
// errNoRoot when the bundle may be created (it reads as empty), and
// ErrUnknownKB when it may not.
func (s *Store) openForRead(b Bundle) (*os.Root, error) {
	root, err := s.openRoot(b)
	if errors.Is(err, errNoRoot) && !b.Create {
		return nil, fmt.Errorf("%w: %s has no directory", ErrUnknownKB, quote(b.name))
	}
	return root, err
}

func notFound(kb, id string) error {
	return fmt.Errorf("%w: %s in %s", ErrNotFound, quote(id), quote(kb))
}

// readError names the concept in a not-found or too-large error.
func (s *Store) readError(err error, kb, id string) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return notFound(kb, id)
	case errors.Is(err, ErrTooLarge):
		return fmt.Errorf("%w: concept %s in %s is over %d bytes", ErrTooLarge, quote(id), quote(kb), s.readCap())
	}
	return err
}

// readCap is the most bytes a concept file may have.
func (s *Store) readCap() int { return s.limits.Body + s.limits.Frontmatter + 64 }

// readConcept reads and parses a concept. The error matches ErrNotFound
// when there is no concept by that exact ID; problems are set for one that
// does not parse.
func (s *Store) readConcept(root *os.Root, id string) (*concept, []FieldError, error) {
	file := fileName(id)
	exact, _, err := s.lookup(root, file)
	if err != nil {
		return nil, nil, err
	}
	if !exact {
		return nil, nil, ErrNotFound
	}
	data, err := s.readCapped(root, file, s.readCap())
	if err != nil {
		return nil, nil, err
	}
	c, problems := parseConcept(data)
	if problems != nil {
		return nil, problems, nil
	}
	if c == nil {
		return nil, nil, ErrNotFound
	}
	return c, nil, nil
}

// readCapped reads a regular file of at most limit bytes. A missing or
// non-regular file is ErrNotFound; a longer one ErrTooLarge.
func (s *Store) readCapped(root *os.Root, name string, limit int) ([]byte, error) {
	// Symbolic links inside a bundle are not followed.
	if info, err := root.Lstat(filepath.FromSlash(name)); err != nil || !info.Mode().IsRegular() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer closeQuietly(s.log, f)
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

// lookup checks a slash-separated path in root segment by segment against
// the directory entries, so a name matches only exactly, whatever the file
// system's case rules. exact is true when every segment exists under
// exactly its name and every parent is a directory, not a symbolic link
// (links inside a bundle are not followed). Otherwise conflict, when set,
// says why the path cannot be created: an entry differing only in case,
// or a parent that is not a directory.
func (s *Store) lookup(root *os.Root, name string) (exact bool, conflict string, err error) {
	parts := strings.Split(name, "/")
	dir := "."
	for i, part := range parts {
		names, listErr := s.dirNames(root, dir)
		if listErr != nil {
			return false, "", listErr
		}
		if !slices.Contains(names, part) {
			for _, n := range names {
				if strings.EqualFold(n, part) {
					return false, "differs only in case from the existing " + quote(path.Join(dir, n)), nil
				}
			}
			return false, "", nil
		}
		dir = path.Join(dir, part)
		if i < len(parts)-1 {
			info, statErr := root.Lstat(filepath.FromSlash(dir))
			if statErr != nil {
				return false, "", statErr
			}
			if !info.IsDir() {
				return false, fmt.Sprintf("has a parent %s that is not a directory", quote(dir)), nil
			}
		}
	}
	return true, "", nil
}

// dirNames lists the names in a directory of root.
func (s *Store) dirNames(root *os.Root, dir string) ([]string, error) {
	f, err := root.Open(filepath.FromSlash(dir))
	if err != nil {
		return nil, err
	}
	defer closeQuietly(s.log, f)
	return f.Readdirnames(-1)
}
