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
)

// Verify runs Put's checks over every concept of knowledge base kb, as
// before activating a revision edited outside the Store (a Git checkout):
// IDs (with entries that differ only in case), the frontmatter's syntax
// and duplicate keys, the frontmatter schema, OKF's fields, the bundle's
// rules, includes, and the size limits. It returns every rule broken, each
// naming its concept; none means the bundle passes. Files that are not
// concepts (no frontmatter, or no type) are skipped. The error is set only
// when the check could not be made.
func (s *Store) Verify(ctx context.Context, r Resolver, kb string) ([]FieldError, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := s.resolve(ctx, r, kb)
	if err != nil {
		return nil, err
	}
	root, err := s.openForRead(b)
	if errors.Is(err, errNoRoot) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closeQuietly(s.log, root)
	key := bundleKey(b)
	var out []FieldError
	names := make(map[string]string) // lower-case path → path, per bundle
	err = walkConcepts(ctx, root, func(id string, _ fs.FileInfo) error {
		fields, checkErr := s.verifyConcept(root, key, b, id, names)
		for i := range fields {
			fields[i].ID = id
		}
		out = append(out, fields...)
		return checkErr
	})
	if err != nil {
		return nil, fmt.Errorf("kb: verify %s: %w", quote(b.Name), err)
	}
	return out, nil
}

// verifyConcept checks one file. names records the paths seen, to report
// case conflicts.
func (s *Store) verifyConcept(root *os.Root, key string, b Bundle, id string, names map[string]string) ([]FieldError, error) {
	if reason := caseConflict(id, names); reason != "" {
		return []FieldError{{Field: "concept_id", Reason: reason}}, nil
	}
	if reason := idProblem(id, s.limits); reason != "" {
		return []FieldError{{Field: "concept_id", Reason: reason}}, nil
	}
	c, problems, err := s.readConcept(root, id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.log.Debug("verify skips a file that is not a concept", "id", id)
		return nil, nil
	case errors.Is(err, ErrTooLarge):
		return []FieldError{{Field: "concept_id", Reason: fmt.Sprintf("is a file over %d bytes", s.readCap())}}, nil
	case err != nil:
		return nil, err
	case problems != nil:
		return problems, nil
	}
	var fields []FieldError
	if len(c.body) > s.limits.Body {
		fields = append(fields, FieldError{Field: "body", Reason: fmt.Sprintf("is %d bytes; the limit is %d", len(c.body), s.limits.Body)})
	}
	if len(c.text) > s.limits.Frontmatter {
		fields = append(fields, FieldError{Field: "frontmatter", Reason: fmt.Sprintf("is %d bytes; the limit is %d", len(c.text), s.limits.Frontmatter)})
	}
	if n := len(asList(c.fm[IncludeKey])); n > s.limits.Includes {
		fields = append(fields, FieldError{Field: "frontmatter." + IncludeKey, Reason: fmt.Sprintf("has %d entries; the limit is %d", n, s.limits.Includes)})
	}
	fields = append(fields, s.recordFields(id, c.fm)...)
	more, err := s.bundleFields(root, key, b, id, c.fm)
	if err != nil {
		return nil, err
	}
	return append(fields, more...), nil
}

// caseConflict reports a path (or one of its directories) that differs
// only in case from one seen before.
func caseConflict(id string, names map[string]string) string {
	p := id + ".md"
	for {
		lower := strings.ToLower(p)
		if seen, ok := names[lower]; ok {
			if seen != p {
				return "differs only in case from " + quote(seen)
			}
		} else {
			names[lower] = p
		}
		p = path.Dir(p)
		if p == "." {
			return ""
		}
	}
}
