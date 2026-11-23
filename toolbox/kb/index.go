/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // the database/sql driver "sqlite"
)

// indexSchema is the search index: one row per Markdown section of a
// concept. bundle identifies the bundle (its root path) and is not
// searched.
const indexSchema = `CREATE VIRTUAL TABLE sections USING fts5(bundle UNINDEXED, id, title, heading, text)`

// searchQuery finds a bundle's best sections. bm25 weighs the columns id >
// title > heading > text; the snippet asks for 48 tokens, and the passage
// is then cut to Limits.PassageRunes.
const searchQuery = `SELECT id, title, heading, snippet(sections, 4, '', '', '…', 48), bm25(sections, 0.0, 10.0, 5.0, 3.0, 1.0) AS score
 FROM sections WHERE sections MATCH ? AND bundle = ? ORDER BY score LIMIT ?`

// index is the in-memory FTS5 index of every bundle searched.
type index struct {
	mu      sync.Mutex // held for every use of db and bundles
	db      *sql.DB
	log     *slog.Logger
	bundles map[string]map[string]indexed // bundle key → concept ID → state
}

// indexed is what the index holds for one file.
type indexed struct {
	size int64
	mod  time.Time
	rows []int64
}

// section is one Markdown section of a body.
type section struct{ heading, text string }

func openIndex(log *slog.Logger) (*index, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	// Every connection to ":memory:" is a database of its own: keep one.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if _, err := db.Exec(indexSchema); err != nil {
		closeQuietly(log, db)
		return nil, err
	}
	return &index{db: db, log: log, bundles: make(map[string]map[string]indexed)}, nil
}

func (x *index) close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.db.Close()
}

// state returns the bundle's file states, creating the map. x.mu is held.
func (x *index) state(key string) map[string]indexed {
	m := x.bundles[key]
	if m == nil {
		m = make(map[string]indexed)
		x.bundles[key] = m
	}
	return m
}

// replace sets a concept's rows. sections nil removes the concept from
// the search (a file that is not a concept keeps its state, with no rows).
// x.mu is held.
func (x *index) replace(ctx context.Context, key, id string, st indexed, title string, sections []section) error {
	m := x.state(key)
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			x.log.Warn("search index rollback failed", "error", rbErr.Error())
		}
	}()
	for _, row := range m[id].rows {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sections WHERE rowid = ?`, row); err != nil {
			return err
		}
	}
	st.rows = nil
	for _, sec := range sections {
		res, err := tx.ExecContext(ctx, `INSERT INTO sections (bundle, id, title, heading, text) VALUES (?, ?, ?, ?, ?)`,
			key, id, title, sec.heading, sec.text)
		if err != nil {
			return err
		}
		row, err := res.LastInsertId()
		if err != nil {
			return err
		}
		st.rows = append(st.rows, row)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m[id] = st
	return nil
}

// remove drops a concept. x.mu is held.
func (x *index) remove(ctx context.Context, key, id string) error {
	m := x.state(key)
	for _, row := range m[id].rows {
		if _, err := x.db.ExecContext(ctx, `DELETE FROM sections WHERE rowid = ?`, row); err != nil {
			return err
		}
	}
	delete(m, id)
	return nil
}

// forget drops what the index knows of a concept's file, so the next
// search reads it again. x.mu is held.
func (x *index) forget(key, id string) {
	m := x.state(key)
	if st, ok := m[id]; ok {
		st.size = -1
		m[id] = st
	}
}

// query runs a match expression over one bundle. x.mu is held.
func (x *index) query(ctx context.Context, key, match string, limit, passage int) ([]Hit, error) {
	rows, err := x.db.QueryContext(ctx, searchQuery, match, key, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			x.log.Debug("close failed", "error", closeErr.Error())
		}
	}()
	hits := []Hit{}
	for rows.Next() {
		var h Hit
		var score float64
		if err := rows.Scan(&h.ID, &h.Title, &h.Section, &h.Passage, &score); err != nil {
			return nil, err
		}
		h.Score = -score
		h.Passage = cutRunes(strings.TrimSpace(h.Passage), passage)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// cutRunes shortens s to at most n characters, ending with "…" when cut.
func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// matchExpression turns a query into an FTS5 expression: each term quoted
// (so nothing the caller types is FTS syntax), any term matching. Terms
// with no letter or digit are dropped; "" means nothing to search for.
func matchExpression(query string) string {
	var terms []string
	for t := range strings.FieldsSeq(query) {
		if strings.IndexFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}

// splitSections splits a Markdown body at its ATX headings, outside fenced
// code. Text before the first heading is a section with no heading. A body
// with no text gives one empty section, so the concept's ID and title are
// still searched.
func splitSections(body string) []section {
	var out []section
	cur := section{}
	var text strings.Builder
	fence := ""
	flush := func() {
		cur.text = strings.TrimSpace(text.String())
		if cur.heading != "" || cur.text != "" {
			out = append(out, cur)
		}
		text.Reset()
	}
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if fence != "" {
			if isFenceClose(trimmed, fence) {
				fence = ""
			}
			text.WriteString(line)
			continue
		}
		if f := fenceOpen(trimmed); f != "" {
			fence = f
			text.WriteString(line)
			continue
		}
		if h, ok := atxHeading(trimmed); ok {
			flush()
			cur = section{heading: h}
			continue
		}
		text.WriteString(line)
	}
	flush()
	if len(out) == 0 {
		out = append(out, section{})
	}
	return out
}

// fenceOpen returns the fence a line opens (``` or ~~~, at least three, up
// to three spaces in), or "".
func fenceOpen(line string) string {
	l := strings.TrimLeft(line, " ")
	if len(line)-len(l) > 3 {
		return ""
	}
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(l) && l[n] == c {
			n++
		}
		if n >= 3 {
			if c == '`' && strings.Contains(l[n:], "`") {
				return "" // not a fence: a backtick in the info string
			}
			return l[:n]
		}
	}
	return ""
}

// isFenceClose reports whether a line closes the fence.
func isFenceClose(line, fence string) bool {
	l := strings.TrimLeft(line, " ")
	if len(line)-len(l) > 3 {
		return false
	}
	n := 0
	for n < len(l) && l[n] == fence[0] {
		n++
	}
	return n >= len(fence) && strings.TrimSpace(l[n:]) == ""
}

// atxHeading returns the text of an ATX heading line.
func atxHeading(line string) (string, bool) {
	l := strings.TrimLeft(line, " ")
	if len(line)-len(l) > 3 {
		return "", false
	}
	n := 0
	for n < len(l) && l[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(l) && l[n] != ' ' && l[n] != '\t') {
		return "", false
	}
	h := strings.TrimSpace(l[n:])
	// A closing sequence of #s, preceded by a space, is not part of it.
	if trimmed := strings.TrimRight(h, "#"); trimmed != h && (trimmed == "" || strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t")) {
		h = strings.TrimSpace(trimmed)
	}
	return h, true
}
