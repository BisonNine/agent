/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package kb reads, searches and writes knowledge bases.
//
// Knowledge bases are stored as Open Knowledge Format (OKF) v0.3 bundles
// and are OKF compatible: a bundle written by these tools can be read by
// any OKF tool, and an OKF bundle can be served by these tools. The README
// states what that covers, where the package is stricter than OKF, and its
// one extension (x-tenebris-include). OKF v0.3:
// https://github.com/open-knowledge-format/open-knowledge-format/blob/527e81aff55992be21507e97004286789126f88c/SPEC.md
//
// A bundle is a directory of Markdown concepts with YAML frontmatter. A
// host maps knowledge-base names to bundle directories with a Resolver;
// the Store does the rest: exact reads with declared
// inclusions, ranked full-text search, validated atomic writes, and a check
// of a whole bundle.
//
// Every file is reached through an os.Root on the bundle (or on its
// Bundle.Confine ancestor), so no path and no symbolic link leads out of
// it. The search index is an in-memory SQLite FTS5 table, rebuilt as
// needed: nothing is written to a bundle except the concepts Put writes.
//
// The tools that publish this to a model are in the tools subpackage.
package kb

import (
	"fmt"
	"log/slog"
	"sync"

	"go.bison9.com/agent/fsmode"
)

// Limits bound what the Store accepts and returns. A zero field takes its
// default (DefaultLimits); a negative one is refused by New.
type Limits struct {
	SearchDefault int // hits when the caller gives no limit (5)
	SearchMax     int // most hits a search returns (20)
	Body          int // body bytes (128 KiB)
	Frontmatter   int // rendered frontmatter bytes (32 KiB)
	Includes      int // x-tenebris-include entries per concept (16)
	IDBytes       int // concept ID bytes (512)
	SegmentBytes  int // bytes per ID segment (200)
	QueryRunes    int // search query characters (256)
	PassageRunes  int // characters of a hit's passage (about 240)
	GetResult     int // bytes of a get tool result; the Store does not use it (512 KiB)
	// IndexedBundles is how many bundles the search index keeps; past it
	// the least recently searched or written one is dropped, and rebuilt
	// when it is next searched (32).
	IndexedBundles int
}

// DefaultLimits are the limits a zero Limits field takes.
var DefaultLimits = Limits{
	SearchDefault: 5,
	SearchMax:     20,
	Body:          128 << 10,
	Frontmatter:   32 << 10,
	Includes:      16,
	IDBytes:       512,
	SegmentBytes:  200,
	QueryRunes:    256,
	PassageRunes:  240,
	GetResult:     512 << 10,

	IndexedBundles: 32,
}

// withDefaults returns l with each zero field taken from DefaultLimits, or
// an error naming a negative field.
func (l Limits) withDefaults() (Limits, error) {
	fields := []struct {
		name string
		v    *int
		def  int
	}{
		{"SearchDefault", &l.SearchDefault, DefaultLimits.SearchDefault},
		{"SearchMax", &l.SearchMax, DefaultLimits.SearchMax},
		{"Body", &l.Body, DefaultLimits.Body},
		{"Frontmatter", &l.Frontmatter, DefaultLimits.Frontmatter},
		{"Includes", &l.Includes, DefaultLimits.Includes},
		{"IDBytes", &l.IDBytes, DefaultLimits.IDBytes},
		{"SegmentBytes", &l.SegmentBytes, DefaultLimits.SegmentBytes},
		{"QueryRunes", &l.QueryRunes, DefaultLimits.QueryRunes},
		{"PassageRunes", &l.PassageRunes, DefaultLimits.PassageRunes},
		{"GetResult", &l.GetResult, DefaultLimits.GetResult},
		{"IndexedBundles", &l.IndexedBundles, DefaultLimits.IndexedBundles},
	}
	for _, f := range fields {
		if *f.v < 0 {
			return l, fmt.Errorf("kb: limit %s must not be negative", f.name)
		}
		if *f.v == 0 {
			*f.v = f.def
		}
	}
	if l.SearchDefault > l.SearchMax {
		return l, fmt.Errorf("kb: limit SearchDefault %d exceeds SearchMax %d", l.SearchDefault, l.SearchMax)
	}
	return l, nil
}

// Options configure a Store.
type Options struct {
	// Modes are the modes of the directories and files Put creates; a zero
	// field is owner-only (fsmode.DefaultDir, fsmode.DefaultFile). A
	// bundle's own Bundle.Modes take precedence.
	Modes fsmode.Modes
	// Limits; a zero field takes its default.
	Limits Limits
	// Logger receives diagnostics, tagged component=kb. Nil: discarded.
	Logger *slog.Logger
}

// Store serves knowledge bases. One Store serves every caller of a
// process: the bundles a call may reach come from the Resolver passed to
// each method. Its methods are safe for concurrent use.
type Store struct {
	modes  fsmode.Modes
	limits Limits
	log    *slog.Logger

	locks   keyedMutex
	schemas schemaCache
	index   *index

	closeOnce sync.Once
	closeErr  error
}

// New returns a Store. It opens the in-memory search index.
func New(opts Options) (*Store, error) {
	limits, err := opts.Limits.withDefaults()
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "kb")
	idx, err := openIndex(log, limits.IndexedBundles)
	if err != nil {
		return nil, fmt.Errorf("kb: open search index: %w", err)
	}
	return &Store{
		modes:  opts.Modes.Or(fsmode.Modes{Dir: fsmode.DefaultDir, File: fsmode.DefaultFile}),
		limits: limits,
		log:    log,
		index:  idx,
	}, nil
}

// Limits returns the limits in force, defaults applied.
func (s *Store) Limits() Limits { return s.limits }

// Close releases the search index. The Store must not be used afterwards.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.index.close() })
	return s.closeErr
}
