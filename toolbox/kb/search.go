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
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hit is one search result: a section of a concept.
type Hit struct {
	ID      string
	Title   string // the concept's title, or the last segment of its ID
	Section string // the section's heading; "" for text before the first heading
	Passage string // a short passage of the section around the match
	Score   float64
}

// Search ranks the sections of knowledge base kb's concepts against query
// and returns the best limit (Limits.SearchDefault when limit < 1, at most
// Limits.SearchMax). Every term may match; IDs weigh most, then titles,
// headings and text. No hit is a successful empty result.
//
// The index follows the bundle: before each search, files whose size or
// modification time changed since they were indexed are read again, and
// deleted ones dropped, so edits made outside the Store are seen.
func (s *Store) Search(ctx context.Context, r Resolver, kb, query string, limit int) ([]Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := s.resolve(ctx, r, kb)
	if err != nil {
		return nil, err
	}
	switch {
	case !utf8.ValidString(query):
		return nil, invalid(b.name, "", FieldError{Field: "query", Reason: "is not valid UTF-8"})
	case strings.IndexFunc(query, func(r rune) bool { return unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' }) >= 0:
		return nil, invalid(b.name, "", FieldError{Field: "query", Reason: "must not contain control characters"})
	case strings.TrimSpace(query) == "":
		return nil, invalid(b.name, "", FieldError{Field: "query", Reason: "is empty"})
	case utf8.RuneCountInString(query) > s.limits.QueryRunes:
		return nil, fmt.Errorf("%w: the query is %d characters; the limit is %d", ErrTooLarge, utf8.RuneCountInString(query), s.limits.QueryRunes)
	}
	if limit < 1 {
		limit = s.limits.SearchDefault
	}
	limit = min(limit, s.limits.SearchMax)
	root, err := s.openForRead(b)
	if errors.Is(err, errNoRoot) {
		return []Hit{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer closeQuietly(s.log, root)
	match := matchExpression(query)
	if match == "" {
		return []Hit{}, nil
	}
	key := bundleKey(b)
	bi := s.index.acquire(ctx, key)
	defer bi.mu.Unlock()
	if refreshErr := s.refresh(ctx, root, bi); refreshErr != nil {
		return nil, fmt.Errorf("kb: index %s: %w", quote(b.name), refreshErr)
	}
	hits, err := bi.query(ctx, match, limit, s.limits.PassageRunes)
	if err != nil {
		return nil, fmt.Errorf("kb: search %s: %w", quote(b.name), err)
	}
	return hits, nil
}

// refresh brings the index of one bundle up to date with its files.
// bi.mu is held.
func (s *Store) refresh(ctx context.Context, root *os.Root, bi *bundleIndex) error {
	state := bi.files
	seen := make(map[string]bool, len(state))
	err := walkConcepts(ctx, root, func(id string, info fs.FileInfo) error {
		if idProblem(id, s.limits) != "" {
			return nil // not reachable by ID: not searched
		}
		seen[id] = true
		if st, ok := state[id]; ok && st.size == info.Size() && st.mod.Equal(info.ModTime()) {
			return nil
		}
		return s.indexFile(ctx, root, bi, id, info)
	})
	if err != nil {
		return err
	}
	for id := range state {
		if !seen[id] {
			if err := bi.remove(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// indexFile (re)indexes one file. A file that is not a valid concept is
// skipped and logged; its state is kept so it is not read again until it
// changes. bi.mu is held.
func (s *Store) indexFile(ctx context.Context, root *os.Root, bi *bundleIndex, id string, info fs.FileInfo) error {
	st := indexed{size: info.Size(), mod: info.ModTime()}
	c, problems, err := s.readConcept(root, id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.log.Debug("search skips a file that is not a concept", "id", id)
		return bi.replace(ctx, id, st, "", nil)
	case errors.Is(err, ErrTooLarge):
		s.log.Warn("search skips a concept over the size limit", "id", id)
		return bi.replace(ctx, id, st, "", nil)
	case err != nil:
		return err
	case problems != nil:
		s.log.Warn("search skips a concept that does not parse", "id", id, "problem", problems[0].Field+" "+problems[0].Reason)
		return bi.replace(ctx, id, st, "", nil)
	}
	return bi.replace(ctx, id, st, displayTitle(id, c.fm), splitSections(c.body))
}

// displayTitle is a concept's title, or the last segment of its ID.
func displayTitle(id string, fm Frontmatter) string {
	if t := fm.Title(); t != "" {
		return t
	}
	return path.Base(id)
}

// walkConcepts calls fn for every candidate concept file in a bundle: a
// regular ".md" file, not reserved (index.md, log.md), outside any entry
// whose name starts with ".". Symbolic links are not followed. The ID is
// passed as found; fn checks it.
func walkConcepts(ctx context.Context, root *os.Root, fn func(id string, info fs.FileInfo) error) error {
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if p == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		id := strings.TrimSuffix(p, ".md")
		if base := strings.ToLower(path.Base(id)); base == "index" || base == "log" {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if errors.Is(infoErr, fs.ErrNotExist) {
				return nil // removed during the walk
			}
			return infoErr
		}
		return fn(id, info)
	})
}
