/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
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

// index is the in-memory FTS5 index of the bundles searched or written
// recently. Each bundle has its own lock, so work on one does not wait for
// another (the single database connection is held only per statement or
// short transaction). At most max bundles are kept: when another is added,
// the least recently used one is dropped, and rebuilt on its next search.
type index struct {
	db  *sql.DB
	log *slog.Logger
	max int

	mu      sync.Mutex // guards bundles and tick
	bundles map[string]*bundleIndex
	tick    uint64
}

// bundleIndex is the index state of one bundle. Its mu is held while the
// bundle's files are compared with the index and its rows are used.
type bundleIndex struct {
	mu      sync.Mutex
	x       *index
	key     string
	files   map[string]indexed // concept ID → state
	used    uint64             // index.tick at the last acquire; under index.mu
	dropped bool               // evicted: acquire again
}

// indexed is what the index holds for one file.
type indexed struct {
	size int64
	mod  time.Time
	rows []int64
}

// section is one Markdown section of a body.
type section struct{ heading, text string }

func openIndex(log *slog.Logger, maxBundles int) (*index, error) {
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
	return &index{db: db, log: log, max: maxBundles, bundles: make(map[string]*bundleIndex)}, nil
}

func (x *index) close() error { return x.db.Close() }

// acquire returns the bundle's index state, locked; the caller unlocks it.
// It may drop the least recently used bundle to stay within x.max.
func (x *index) acquire(ctx context.Context, key string) *bundleIndex {
	for {
		x.mu.Lock()
		bi := x.bundles[key]
		if bi == nil {
			bi = &bundleIndex{x: x, key: key, files: make(map[string]indexed)}
			x.bundles[key] = bi
		}
		x.tick++
		bi.used = x.tick
		var victim *bundleIndex
		if len(x.bundles) > x.max {
			for _, other := range x.bundles {
				if other != bi && (victim == nil || other.used < victim.used) {
					victim = other
				}
			}
			delete(x.bundles, victim.key)
		}
		x.mu.Unlock()
		if victim != nil {
			victim.drop(context.WithoutCancel(ctx))
		}
		bi.mu.Lock()
		if !bi.dropped {
			return bi
		}
		bi.mu.Unlock() // evicted meanwhile: start again
	}
}

// drop deletes an evicted bundle's rows.
func (bi *bundleIndex) drop(ctx context.Context) {
	bi.mu.Lock()
	defer bi.mu.Unlock()
	bi.dropped = true
	for id := range bi.files {
		if err := bi.remove(ctx, id); err != nil {
			bi.x.log.Warn("search index rows not dropped", "error", err.Error())
			return
		}
	}
}

// replace sets a concept's rows. sections nil removes the concept from
// the search (a file that is not a concept keeps its state, with no rows).
// bi.mu is held.
func (bi *bundleIndex) replace(ctx context.Context, id string, st indexed, title string, sections []section) error {
	tx, err := bi.x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			bi.x.log.Warn("search index rollback failed", "error", rbErr.Error())
		}
	}()
	for _, row := range bi.files[id].rows {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sections WHERE rowid = ?`, row); err != nil {
			return err
		}
	}
	st.rows = nil
	for _, sec := range sections {
		res, err := tx.ExecContext(ctx, `INSERT INTO sections (bundle, id, title, heading, text) VALUES (?, ?, ?, ?, ?)`,
			bi.key, id, title, sec.heading, sec.text)
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
	bi.files[id] = st
	return nil
}

// remove drops a concept. bi.mu is held.
func (bi *bundleIndex) remove(ctx context.Context, id string) error {
	for _, row := range bi.files[id].rows {
		if _, err := bi.x.db.ExecContext(ctx, `DELETE FROM sections WHERE rowid = ?`, row); err != nil {
			return err
		}
	}
	delete(bi.files, id)
	return nil
}

// forget makes the next search read a concept's file again. bi.mu is held.
func (bi *bundleIndex) forget(id string) {
	if st, ok := bi.files[id]; ok {
		st.size = -1
		bi.files[id] = st
	}
}

// query runs a match expression over the bundle. bi.mu is held.
func (bi *bundleIndex) query(ctx context.Context, match string, limit, passage int) ([]Hit, error) {
	rows, err := bi.x.db.QueryContext(ctx, searchQuery, match, bi.key, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			bi.x.log.Debug("close failed", "error", closeErr.Error())
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
