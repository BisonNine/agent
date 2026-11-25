/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// errInjected is what faultDB returns for an Exec or Query it fails.
var errInjected = errors.New("injected failure")

// faultDB wraps a DBTX and intercepts the statements containing match. Skip
// lets that many matching statements through first. With before nil the
// intercepted statement fails: Exec and Query return errInjected, and QueryRow
// runs under a cancelled context, so its Scan reports context.Canceled. With
// before set, before runs and the statement then proceeds normally, which lets
// a test change the database between a method's read and its write.
type faultDB struct {
	DBTX
	match  string
	skip   int
	before func()

	mu   sync.Mutex
	hits int
}

// intercept reports whether query is the one to act on, running before when
// it is set (in which case the statement is not failed).
func (f *faultDB) intercept(query string) (fail bool) {
	if !strings.Contains(query, f.match) {
		return false
	}
	f.mu.Lock()
	f.hits++
	n := f.hits
	f.mu.Unlock()
	if n <= f.skip {
		return false
	}
	if f.before != nil {
		f.before()
		return false
	}
	return true
}

func (f *faultDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if f.intercept(query) {
		return nil, errInjected
	}
	return f.DBTX.ExecContext(ctx, query, args...)
}

func (f *faultDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if f.intercept(query) {
		return nil, errInjected
	}
	return f.DBTX.QueryContext(ctx, query, args...)
}

func (f *faultDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if f.intercept(query) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		return f.DBTX.QueryRowContext(cctx, query, args...)
	}
	return f.DBTX.QueryRowContext(ctx, query, args...)
}

// failOn returns a DBTX over s.DB() that fails the first statement containing
// match.
func failOn(s *Store, match string) *faultDB { return &faultDB{DBTX: s.DB(), match: match} }

// isInjected reports whether err is a failure faultDB produced.
func isInjected(err error) bool {
	return errors.Is(err, errInjected) || errors.Is(err, context.Canceled)
}

// Every store method reports a closed database as an error instead of
// returning empty results that look like an empty store.
func TestClosedStoreReportsErrors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, err := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := add(t, s, d.ID, TypeFact, "Alice likes tea")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	calls := map[string]func() error{
		"CreateDomain": func() error { _, err := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Bob"}); return err },
		"UpdateDomain": func() error { return s.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{Summary: strptr("x")}) },
		"DeleteDomain": func() error { return s.DeleteDomain(ctx, db, d.ID) },
		"MigrateDomain": func() error {
			_, err := s.MigrateDomain(ctx, db, d.ID, "dOTHER")
			return err
		},
		"ArchiveDomain":   func() error { return s.ArchiveDomain(ctx, db, d.ID) },
		"GetDomain":       func() error { _, err := s.GetDomain(ctx, db, d.ID, true); return err },
		"DomainByName":    func() error { _, err := s.DomainByName(ctx, db, "Alice"); return err },
		"DomainByNameAny": func() error { _, err := s.DomainByNameAny(ctx, db, "Alice"); return err },
		"GeneralDomain":   func() error { _, err := s.GeneralDomain(ctx, db); return err },
		"ListDomains":     func() error { _, err := s.ListDomains(ctx, db); return err },
		"Counts":          func() error { _, err := s.Counts(ctx, db); return err },
		"AddMemory": func() error {
			_, err := s.AddMemory(ctx, db, AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: "x"})
			return err
		},
		"RetireMemory": func() error { return s.RetireMemory(ctx, db, m.ID, "r") },
		"DeleteMemory": func() error { return s.DeleteMemory(ctx, db, m.ID) },
		"SupersedeMemory": func() error {
			_, err := s.SupersedeMemory(ctx, db, m.ID, AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: "y"})
			return err
		},
		"SetMemoryFileRef":    func() error { _, err := s.SetMemoryFileRef(ctx, db, m.ID, "a.md"); return err },
		"SetMemoryType":       func() error { _, err := s.SetMemoryType(ctx, db, m.ID, TypeRule); return err },
		"RestoreMemory":       func() error { return s.RestoreMemory(ctx, db, m.ID) },
		"GetMemory":           func() error { _, err := s.GetMemory(ctx, db, m.ID); return err },
		"ListMemories":        func() error { _, err := s.ListMemories(ctx, db, d.ID); return err },
		"ListPromptMemories":  func() error { _, err := s.ListPromptMemories(ctx, db, d.ID); return err },
		"CountEvents":         func() error { _, err := s.CountEvents(ctx, db, d.ID); return err },
		"SearchMemories":      func() error { _, err := s.SearchMemories(ctx, db, "tea", 0, false); return err },
		"MatchActiveMemories": func() error { _, err := s.MatchActiveMemories(ctx, db, "tea", ""); return err },
		"CheckKeywords":       func() error { _, _, err := s.CheckKeywords(ctx, db, d.ID, []string{"tea"}); return err },
		"KeywordPatch":        func() error { _, _, err := s.KeywordPatch(ctx, db, d.ID, []string{"tea"}); return err },
		"LogEvent":            func() error { return s.LogEvent(ctx, db, Event{Type: "create", Actor: "test"}) },
		"RecordRun":           func() error { return s.RecordRun(ctx, db, Run{Trigger: "manual", Model: "m", Status: "ok"}) },
		"LastRun":             func() error { _, _, err := s.LastRun(ctx, db); return err },
		"GetState":            func() error { _, err := s.GetState(ctx, db, InboxStateKey); return err },
		"SetWatermark":        func() error { return s.SetWatermark(ctx, db, InboxStateKey, 1, 1) },
		"IncMeaningful":       func() error { return s.IncMeaningful(ctx, db, InboxStateKey, 1) },
		"AcquireLease": func() error {
			_, err := s.AcquireLease(ctx, db, "worker", "alice", time.Minute)
			return err
		},
		"ReleaseLease":         func() error { return s.ReleaseLease(ctx, db, "worker", "alice") },
		"ListEvents":           func() error { _, err := s.ListEvents(ctx, db); return err },
		"AppendInbox":          func() error { return s.AppendInbox(ctx, db, 1, "user", "hi") },
		"InboxBounds":          func() error { _, _, err := s.InboxBounds(ctx, db); return err },
		"InboxRange":           func() error { _, err := s.InboxRange(ctx, db, 0, 10); return err },
		"InboxCount":           func() error { _, err := s.InboxCount(ctx, db); return err },
		"DeleteInboxThrough":   func() error { _, err := s.DeleteInboxThrough(ctx, db, 10); return err },
		"InboxBackfilled":      func() error { _, err := s.InboxBackfilled(ctx); return err },
		"SetInboxBackfilled":   func() error { return s.SetInboxBackfilled(ctx) },
		"PurgeExpiredEvents":   func() error { _, err := s.PurgeExpiredEvents(ctx, db, 30); return err },
		"PurgeRetiredMemories": func() error { _, err := s.PurgeRetiredMemories(ctx, db, 30); return err },
		"DedupeActiveMemories": func() error { _, err := s.DedupeActiveMemories(ctx); return err },
		"PurgeNonActive":       func() error { _, err := s.PurgeNonActive(ctx, true); return err },
		"StableRev":            func() error { _, err := s.StableRev(ctx); return err },
		"Vacuum":               func() error { return s.Vacuum(ctx) },
		"WithTx": func() error {
			return s.WithTx(ctx, func(*sql.Tx) error { t.Error("WithTx ran fn on a closed store"); return nil })
		},
		// The migration steps run on every Open; each must report a database it
		// cannot read rather than treat it as already migrated.
		"migrate":                    func() error { return s.migrate(ctx) },
		"renameLegacyTables":         func() error { return s.renameLegacyTables(ctx) },
		"ensureDomainColumns":        func() error { return s.ensureDomainColumns(ctx) },
		"ensureRunColumns":           func() error { return s.ensureRunColumns(ctx) },
		"ensureMemoryColumns":        func() error { return s.ensureMemoryColumns(ctx) },
		"normalizeStickyColumn":      func() error { return s.normalizeStickyColumn(ctx) },
		"dropLegacyMemoryColumns":    func() error { return s.dropLegacyMemoryColumns(ctx) },
		"dropLegacyDomainColumns":    func() error { return s.dropLegacyDomainColumns(ctx) },
		"collapseConsolidationState": func() error { return s.collapseConsolidationState(ctx) },
		"seedGeneralOnce":            func() error { return s.seedGeneralOnce(ctx) },
		"snapshotBeforeMigration":    func() error { return s.snapshotBeforeMigration(ctx, 0) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Errorf("%s on a closed store returned no error", name)
			}
		})
	}
}

// A failure at any statement inside a domain mutator is returned, never
// swallowed, and leaves the stable block alone when it happens before the
// write.
func TestDomainMutatorsReportStatementFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		match string
		skip  int
		run   func(s *Store, q DBTX, d, other Domain) error
	}{
		{"create: name check", "lower(trim(name))=lower(trim(?)) AND id<>?", 0, func(s *Store, q DBTX, _, _ Domain) error {
			_, err := s.CreateDomain(ctx, q, CreateDomainParams{Name: "Bob"})
			return err
		}},
		{"create: id allocation", "SELECT 1 FROM domains", 0, func(s *Store, q DBTX, _, _ Domain) error {
			_, err := s.CreateDomain(ctx, q, CreateDomainParams{Name: "Bob"})
			return err
		}},
		{"create: insert", "INSERT INTO domains", 0, func(s *Store, q DBTX, _, _ Domain) error {
			_, err := s.CreateDomain(ctx, q, CreateDomainParams{Name: "Bob"})
			return err
		}},
		{"create: stable rev", "UPDATE meta SET value", 0, func(s *Store, q DBTX, _, _ Domain) error {
			_, err := s.CreateDomain(ctx, q, CreateDomainParams{Name: "Bob"})
			return err
		}},
		{"update: name check", "AND id<>?", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.UpdateDomain(ctx, q, d.ID, UpdateDomainParams{Name: strptr("Bob")})
		}},
		{"update: write", "UPDATE domains SET name=?", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.UpdateDomain(ctx, q, d.ID, UpdateDomainParams{Summary: strptr("x")})
		}},
		{"update: stable rev", "UPDATE meta SET value", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.UpdateDomain(ctx, q, d.ID, UpdateDomainParams{Summary: strptr("x")})
		}},
		{"delete: memories", "DELETE FROM memories WHERE domain_id", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.DeleteDomain(ctx, q, d.ID)
		}},
		{"delete: domain", "DELETE FROM domains WHERE id", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.DeleteDomain(ctx, q, d.ID)
		}},
		{"delete: stable rev", "UPDATE meta SET value", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.DeleteDomain(ctx, q, d.ID)
		}},
		{"migrate: read destination", "WHERE id=?", 1, func(s *Store, q DBTX, d, o Domain) error {
			_, err := s.MigrateDomain(ctx, q, d.ID, o.ID)
			return err
		}},
		{"migrate: move memories", "UPDATE memories SET domain_id", 0, func(s *Store, q DBTX, d, o Domain) error {
			_, err := s.MigrateDomain(ctx, q, d.ID, o.ID)
			return err
		}},
		{"migrate: delete source", "DELETE FROM domains WHERE id", 0, func(s *Store, q DBTX, d, o Domain) error {
			_, err := s.MigrateDomain(ctx, q, d.ID, o.ID)
			return err
		}},
		{"migrate: stable rev", "UPDATE meta SET value", 0, func(s *Store, q DBTX, d, o Domain) error {
			_, err := s.MigrateDomain(ctx, q, d.ID, o.ID)
			return err
		}},
		{"archive: write", "UPDATE domains SET status=?", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.ArchiveDomain(ctx, q, d.ID)
		}},
		{"archive: stable rev", "UPDATE meta SET value", 0, func(s *Store, q DBTX, d, _ Domain) error {
			return s.ArchiveDomain(ctx, q, d.ID)
		}},
		{"get with memories", "FROM memories", 0, func(s *Store, q DBTX, d, _ Domain) error {
			_, err := s.GetDomain(ctx, q, d.ID, true)
			return err
		}},
		{"keyword patch: check", "FROM domains WHERE status IN", 0, func(s *Store, q DBTX, d, _ Domain) error {
			_, _, err := s.KeywordPatch(ctx, q, d.ID, []string{"tea"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t) //nolint:contextcheck // the test helpers open and write the store without a context
			d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Alice", Sticky: true})
			other, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Other"})
			add(t, s, d.ID, TypeFact, "Alice likes tea") //nolint:contextcheck // the test helpers open and write the store without a context
			f := &faultDB{DBTX: s.DB(), match: tc.match, skip: tc.skip}
			if err := tc.run(s, f, d, other); !isInjected(err) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
		})
	}
}

// A domain mutator that fails part-way inside WithTx leaves nothing behind:
// DeleteDomain removes the memories first, and a failure deleting the domain
// itself must roll that back.
func TestDeleteDomainFailureRollsBackInTx(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Alice"})
	m := add(t, s, d.ID, TypeFact, "Alice likes tea")

	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		return s.DeleteDomain(ctx, &faultDB{DBTX: tx, match: "DELETE FROM domains WHERE id"}, d.ID)
	})
	if !isInjected(err) {
		t.Fatalf("delete err = %v, want the injected failure", err)
	}
	if _, err := s.GetDomain(ctx, s.DB(), d.ID, false); err != nil {
		t.Errorf("domain gone after a rolled-back delete: %v", err)
	}
	if got, err := s.GetMemory(ctx, s.DB(), m.ID); err != nil || got.Text != m.Text {
		t.Errorf("memory after a rolled-back delete = %+v, %v; want it kept", got, err)
	}
}

// A domain deleted between UpdateDomain's read and its write is reported as
// not found, not as a successful update.
func TestUpdateDomainDeletedMidway(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Alice"})
	f := &faultDB{DBTX: s.DB(), match: "UPDATE domains SET name=?", before: func() {
		if _, err := s.DB().Exec(`DELETE FROM domains WHERE id=?`, d.ID); err != nil {
			t.Errorf("delete: %v", err)
		}
	}}
	if err := s.UpdateDomain(ctx, f, d.ID, UpdateDomainParams{Summary: strptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A rename changes the name, keeps the id, may change only the case of the
// domain's own name, and is refused when another active domain has the name.
func TestUpdateDomainRename(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice"})
	if _, err := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Bob"}); err != nil {
		t.Fatalf("create Bob: %v", err)
	}
	before, _ := s.StableRev(ctx)

	if err := s.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{Name: strptr(" bob ")}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("rename onto Bob: %v, want ErrDuplicateName", err)
	}
	if err := s.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{Name: strptr("ALICE")}); err != nil {
		t.Fatalf("rename to own name in other case: %v", err)
	}
	state := DomainState{NextActions: []string{"call Bob"}}
	if err := s.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{Name: strptr("Alice's notes"), State: &state}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := s.DomainByName(ctx, db, "alice's notes")
	if err != nil {
		t.Fatalf("by new name: %v", err)
	}
	if got.ID != d.ID || got.Name != "Alice's notes" || len(got.State.NextActions) != 1 || got.State.NextActions[0] != "call Bob" {
		t.Errorf("renamed domain = %+v", got)
	}
	if got.Version != d.Version+2 {
		t.Errorf("version = %d, want %d (two successful updates)", got.Version, d.Version+2)
	}
	if after, _ := s.StableRev(ctx); after != before+2 {
		t.Errorf("stable_rev %d -> %d, want +2 (a rename changes the index)", before, after)
	}
}

// A domain state that cannot be stored as JSON is refused before anything is
// written.
func TestDomainStateThatCannotBeEncoded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	bad := DomainState{Fields: map[string]any{"score": math.NaN()}}
	if _, err := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice", State: bad}); err == nil {
		t.Fatal("create with an unencodable state succeeded")
	}
	if _, err := s.DomainByName(ctx, db, "Alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("domain written despite the error: %v", err)
	}
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Bob"})
	if err := s.UpdateDomain(ctx, db, d.ID, UpdateDomainParams{State: &bad}); err == nil {
		t.Fatal("update with an unencodable state succeeded")
	}
	if got, _ := s.GetDomain(ctx, db, d.ID, false); got.Version != d.Version {
		t.Errorf("version %d -> %d after a refused update", d.Version, got.Version)
	}
}

// A domain row whose state_json is corrupt is reported, naming the domain, by
// every read that meets it.
func TestCorruptDomainStateIsReported(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice"})
	if _, err := db.Exec(`UPDATE domains SET state_json='{not json' WHERE id=?`, d.ID); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := s.GetDomain(ctx, db, d.ID, false); err == nil || !strings.Contains(err.Error(), "bad state_json for "+d.ID) {
		t.Errorf("GetDomain err = %v", err)
	}
	if _, err := s.ListDomains(ctx, db); err == nil || !strings.Contains(err.Error(), d.ID) {
		t.Errorf("ListDomains err = %v", err)
	}
}

// Rows holding values of the wrong type (a corrupted file, or one written by
// something other than cogmem) are reported by the list reads, not skipped.
func TestCorruptRowsAreReported(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice"})
	m := add(t, s, d.ID, TypeFact, "Alice likes tea")
	if err := s.AppendInbox(ctx, db, 1, "user", "hi"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.LogEvent(ctx, db, Event{Type: "create", Actor: "test"}); err != nil {
		t.Fatalf("log: %v", err)
	}
	for _, q := range []string{
		`UPDATE memories SET created_at='yesterday' WHERE id='` + m.ID + `'`,
		`UPDATE inbox SET created_at='yesterday'`,
		`UPDATE memory_events SET id=NULL`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("corrupt (%s): %v", q, err)
		}
	}
	if _, err := s.ListMemories(ctx, db, d.ID); err == nil {
		t.Error("ListMemories over a corrupt row returned no error")
	}
	if _, err := s.InboxRange(ctx, db, 0, 10); err == nil {
		t.Error("InboxRange over a corrupt row returned no error")
	}
	if _, err := s.ListEvents(ctx, db); err == nil {
		t.Error("ListEvents over a corrupt row returned no error")
	}
	if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('inbox_backfilled','yes')`); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}
	if _, err := s.InboxBackfilled(ctx); err == nil {
		t.Error("InboxBackfilled with a non-numeric flag returned no error")
	}
}

// A failure at any statement inside a memory mutator is returned, never
// swallowed.
func TestMemoryMutatorsReportStatementFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		match string
		run   func(s *Store, q DBTX, d Domain, m, retired Memory) error
	}{
		{"add: id allocation", "SELECT 1 FROM memories", func(s *Store, q DBTX, d Domain, _, _ Memory) error {
			_, err := s.AddMemory(ctx, q, AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: "x"})
			return err
		}},
		{"add: insert", "INSERT INTO memories", func(s *Store, q DBTX, d Domain, _, _ Memory) error {
			_, err := s.AddMemory(ctx, q, AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: "x"})
			return err
		}},
		{"add: stable rev", "UPDATE meta SET value", func(s *Store, q DBTX, d Domain, _, _ Memory) error {
			_, err := s.AddMemory(ctx, q, AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: "x"})
			return err
		}},
		{"retire: domain", "SELECT type FROM domains", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			return s.RetireMemory(ctx, q, m.ID, "r")
		}},
		{"retire: write", "UPDATE memories SET status=?", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			return s.RetireMemory(ctx, q, m.ID, "r")
		}},
		{"delete: domain", "SELECT type FROM domains", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			return s.DeleteMemory(ctx, q, m.ID)
		}},
		{"delete: write", "DELETE FROM memories", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			return s.DeleteMemory(ctx, q, m.ID)
		}},
		{"delete: stable rev", "UPDATE meta SET value", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			return s.DeleteMemory(ctx, q, m.ID)
		}},
		{"file ref: domain", "SELECT type FROM domains", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			_, err := s.SetMemoryFileRef(ctx, q, m.ID, "a.md")
			return err
		}},
		{"file ref: write", "UPDATE memories SET file_ref", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			_, err := s.SetMemoryFileRef(ctx, q, m.ID, "a.md")
			return err
		}},
		{"file ref: stable rev", "UPDATE meta SET value", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			_, err := s.SetMemoryFileRef(ctx, q, m.ID, "a.md")
			return err
		}},
		{"type: write", "UPDATE memories SET type", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			_, err := s.SetMemoryType(ctx, q, m.ID, TypeRule)
			return err
		}},
		{"type: stable rev", "UPDATE meta SET value", func(s *Store, q DBTX, _ Domain, m, _ Memory) error {
			_, err := s.SetMemoryType(ctx, q, m.ID, TypeRule)
			return err
		}},
		{"restore: domain", "SELECT type FROM domains", func(s *Store, q DBTX, _ Domain, _, r Memory) error {
			return s.RestoreMemory(ctx, q, r.ID)
		}},
		{"restore: write", "UPDATE memories SET status=?", func(s *Store, q DBTX, _ Domain, _, r Memory) error {
			return s.RestoreMemory(ctx, q, r.ID)
		}},
		{"restore: stable rev", "UPDATE meta SET value", func(s *Store, q DBTX, _ Domain, _, r Memory) error {
			return s.RestoreMemory(ctx, q, r.ID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t) //nolint:contextcheck // the test helpers open and write the store without a context
			d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Alice", Sticky: true})
			m := add(t, s, d.ID, TypeFact, "Alice likes tea")    //nolint:contextcheck // the test helpers open and write the store without a context
			r := add(t, s, d.ID, TypeFact, "Alice liked coffee") //nolint:contextcheck // the test helpers open and write the store without a context
			if err := s.RetireMemory(ctx, s.DB(), r.ID, "outdated"); err != nil {
				t.Fatalf("retire: %v", err)
			}
			if err := tc.run(s, failOn(s, tc.match), d, m, r); !isInjected(err) {
				t.Fatalf("err = %v, want the injected failure", err)
			}
		})
	}
}

// Superseding a memory that does not exist fails without adding the
// replacement.
func TestSupersedeUnknownMemory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Alice"})
	_, err := s.SupersedeMemory(ctx, s.DB(), "hNOPE1", AddMemoryParams{DomainID: d.ID, Type: TypeFact, Text: "Bob likes tea"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got, _ := s.ListMemories(ctx, s.DB(), d.ID); len(got) != 0 {
		t.Errorf("replacement added despite the failure: %+v", got)
	}
}

// Restoring a retired memory in a sticky domain puts it back in the prompt,
// so the stable block is rebuilt; restoring an active memory is a no-op.
func TestRestoreMemoryInStickyDomainRebuildsStableBlock(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	db := s.DB()
	d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice", Sticky: true})
	m := add(t, s, d.ID, TypeRule, "Always greet Bob")
	if err := s.RetireMemory(ctx, db, m.ID, "outdated"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	before, _ := s.StableRev(ctx)
	if err := s.RestoreMemory(ctx, db, m.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	after, _ := s.StableRev(ctx)
	if after != before+1 {
		t.Errorf("stable_rev %d -> %d, want +1", before, after)
	}
	got, _ := s.GetMemory(ctx, db, m.ID)
	if got.Status != StatusActive || got.RetireReason != nil {
		t.Errorf("restored memory = %+v", got)
	}
	if err := s.RestoreMemory(ctx, db, m.ID); err != nil {
		t.Fatalf("restore active: %v", err)
	}
	if again, _ := s.StableRev(ctx); again != after {
		t.Errorf("stable_rev %d -> %d on restoring an active memory", after, again)
	}
}

// When every candidate id collides, allocation gives up with an error and
// nothing is written.
func TestIDAllocationGivesUpAfterCollisions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	q := collidingDB{DBTX: s.DB()}
	_, err := s.CreateDomain(ctx, q, CreateDomainParams{Name: "Alice"})
	if err == nil || !strings.Contains(err.Error(), "id allocation failed") {
		t.Fatalf("err = %v, want the allocation failure", err)
	}
	if _, err := s.DomainByName(ctx, s.DB(), "Alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("domain written despite the failure: %v", err)
	}
}

// collidingDB answers every id-existence probe with "taken".
type collidingDB struct{ DBTX }

func (c collidingDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.HasPrefix(query, "SELECT 1 FROM ") {
		return c.DBTX.QueryRowContext(ctx, `SELECT 1 WHERE ? IS NOT NULL`, args...)
	}
	return c.DBTX.QueryRowContext(ctx, query, args...)
}

// Purging events reports a failure to rebuild the stable block along with
// how many it removed, so the caller knows the delete happened.
func TestPurgeExpiredEventsStableRevFailure(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Alice"})
	e := add(t, s, d.ID, TypeEvent, "Alice went to the shop")
	age(t, s, e.ID, 40)
	n, err := s.PurgeExpiredEvents(ctx, failOn(s, "UPDATE meta SET value"), 30)
	if !isInjected(err) || n != 1 {
		t.Fatalf("purge = %d, %v; want 1 and the injected failure", n, err)
	}
	if _, err := s.GetMemory(ctx, s.DB(), e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("event still there: %v", err)
	}
}

// DedupeActiveMemories and PurgeNonActive write in one transaction: a write
// the database refuses leaves every row as it was.
func TestStoreLevelWritesRollBackOnFailure(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		trigger string
		run     func(s *Store) error
	}{
		{
			"dedupe", `CREATE TRIGGER t BEFORE UPDATE ON memories BEGIN SELECT RAISE(ABORT, 'refused'); END`,
			func(s *Store) error { _, err := s.DedupeActiveMemories(ctx); return err },
		},
		{
			"purge memories", `CREATE TRIGGER t BEFORE DELETE ON memories BEGIN SELECT RAISE(ABORT, 'refused'); END`,
			func(s *Store) error { _, err := s.PurgeNonActive(ctx, true); return err },
		},
		{
			"purge domains", `CREATE TRIGGER t BEFORE DELETE ON domains BEGIN SELECT RAISE(ABORT, 'refused'); END`,
			func(s *Store) error { _, err := s.PurgeNonActive(ctx, true); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t) //nolint:contextcheck // the test helpers open and write the store without a context
			db := s.DB()
			d, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Alice"})
			gone, _ := s.CreateDomain(ctx, db, CreateDomainParams{Name: "Bob"})
			add(t, s, d.ID, TypeFact, "Alice likes tea")         //nolint:contextcheck // the test helpers open and write the store without a context
			add(t, s, d.ID, TypeFact, "Alice likes tea")         //nolint:contextcheck // the test helpers open and write the store without a context
			r := add(t, s, d.ID, TypeFact, "Alice liked coffee") //nolint:contextcheck // the test helpers open and write the store without a context
			_ = s.RetireMemory(ctx, db, r.ID, "outdated")
			_ = s.ArchiveDomain(ctx, db, gone.ID)
			before, _ := s.Counts(ctx, db)
			if _, err := db.Exec(tc.trigger); err != nil {
				t.Fatalf("trigger: %v", err)
			}
			if err := tc.run(s); err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("err = %v, want the refusal", err)
			}
			if after, _ := s.Counts(ctx, db); after != before {
				t.Errorf("counts %+v -> %+v after a refused write", before, after)
			}
		})
	}
}

func TestPlaceholdersEmpty(t *testing.T) {
	if got := placeholders(0); got != "" {
		t.Errorf("placeholders(0) = %q, want empty", got)
	}
	if got := placeholders(3); got != "?,?,?" {
		t.Errorf("placeholders(3) = %q", got)
	}
}

// A store with no recorded run reports ok=false, not an error.
func TestLastRunEmpty(t *testing.T) {
	s := openTest(t)
	r, ok, err := s.LastRun(context.Background(), s.DB())
	if err != nil || ok || r != (Run{}) {
		t.Errorf("LastRun = %+v, %v, %v; want zero, false, nil", r, ok, err)
	}
}

// Restoring a memory in a non-sticky domain does not touch the stable block.
func TestRestoreMemoryInPlainDomainKeepsStableBlock(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.CreateDomain(ctx, s.DB(), CreateDomainParams{Name: "Bob"})
	m := add(t, s, d.ID, TypeFact, "Bob likes tea")
	_ = s.RetireMemory(ctx, s.DB(), m.ID, "outdated")
	before, _ := s.StableRev(ctx)
	if err := s.RestoreMemory(ctx, s.DB(), m.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if after, _ := s.StableRev(ctx); after != before {
		t.Errorf("stable_rev %d -> %d", before, after)
	}
	if got, _ := s.GetMemory(ctx, s.DB(), m.ID); got.Status != StatusActive {
		t.Errorf("status = %s, want active", got.Status)
	}
}

// The agent's own keyword phrases are kept apart from consolidation's, and an
// empty phrase matches nothing.
func TestAgentKeywordPhrasesAndEmptyPhrase(t *testing.T) {
	d := Domain{KeywordTriggers: " Tea , alice,tea", AutoKeywords: "coffee"}
	if got := d.AgentKeywordPhrases(); len(got) != 2 || got[0] != "tea" || got[1] != "alice" {
		t.Errorf("AgentKeywordPhrases = %q", got)
	}
	if phraseMatch("alice likes tea", "") {
		t.Error("empty phrase matched")
	}
}
