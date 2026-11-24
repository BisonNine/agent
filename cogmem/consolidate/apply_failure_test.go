// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/cogmem/store"
)

// refuse installs an SQLite trigger that aborts the statements it matches, so
// the store write it guards fails the way a full disk or a locked database
// would.
func refuse(t *testing.T, s *store.Store, name, when string) {
	t.Helper()
	q := `CREATE TRIGGER ` + name + ` ` + when + ` BEGIN SELECT RAISE(ABORT, 'refused by test'); END`
	if _, err := s.DB().Exec(q); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}

// corruptOtherDomain adds an active domain whose state cannot be read, so any
// read over all active domains (the keyword checks) fails.
func corruptOtherDomain(t *testing.T, s *store.Store) {
	t.Helper()
	d := createDomain(t, s, "Broken")
	if _, err := s.DB().Exec(`UPDATE domains SET state_json='{' WHERE id=?`, d.ID); err != nil {
		t.Fatalf("corrupt domain: %v", err)
	}
}

// storeSnapshot renders every domain, memory and audit event, so a test can
// prove a failed Apply left the store exactly as it was.
func storeSnapshot(t *testing.T, s *store.Store) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id||'|'||name||'|'||status||'|'||version||'|'||summary||'|'||auto_keywords FROM domains ORDER BY id`,
		`SELECT id||'|'||domain_id||'|'||status||'|'||text FROM memories ORDER BY id`,
		`SELECT id FROM memory_events ORDER BY id`,
	} {
		snapshotQuery(t, s, q, &b)
		b.WriteString("--\n")
	}
	return b.String()
}

// snapshotQuery appends one line per row of the single-column query q to b.
func snapshotQuery(t *testing.T, s *store.Store, q string, b *strings.Builder) {
	t.Helper()
	rows, err := s.DB().Query(q)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("snapshot scan: %v", err)
		}
		b.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
}

// Apply is all or nothing: when any store write fails, whether the operation
// itself or its audit event, Apply returns the error, reports nothing applied,
// and the operations before it in the same output are rolled back.
func TestApplyRollsBackOnStoreFailure(t *testing.T) {
	ev := store.Evidence{SeqStart: 1, SeqEnd: 1}
	const logRefused = "BEFORE INSERT ON memory_events WHEN NEW.actor='sleep_cycle'"
	type fixture struct{ domain, memory string }
	cases := []struct {
		name  string
		fault func(t *testing.T, s *store.Store)
		ops   func(f fixture) Output
	}{
		{
			name:  "create: keyword check cannot read domains",
			fault: corruptOtherDomain,
			ops: func(fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "Travel", KeywordTriggers: "flight booking", Evidence: ev}}}
			},
		},
		{
			name: "create: domain insert refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_domain", "BEFORE INSERT ON domains")
			},
			ops: func(fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "Travel", Evidence: ev}}}
			},
		},
		{
			name: "create: audit event refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_event", logRefused)
			},
			ops: func(fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "Travel", Evidence: ev}}}
			},
		},
		{
			name:  "update: keyword target does not exist",
			fault: func(*testing.T, *store.Store) {},
			ops: func(fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "update", ID: "no-such-domain", KeywordTriggers: "flight booking", Evidence: ev}}}
			},
		},
		{
			name:  "update: keyword check cannot read domains",
			fault: corruptOtherDomain,
			ops: func(f fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "update", ID: f.domain, KeywordTriggers: "flight booking", Evidence: ev}}}
			},
		},
		{
			name: "update: domain write refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_update", "BEFORE UPDATE ON domains")
			},
			ops: func(f fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "update", ID: f.domain, Summary: "new summary", Evidence: ev}}}
			},
		},
		{
			name: "update: audit event refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_event", logRefused)
			},
			ops: func(f fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "update", ID: f.domain, Summary: "new summary", Evidence: ev}}}
			},
		},
		{
			name: "archive: domain write refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_update", "BEFORE UPDATE ON domains")
			},
			ops: func(f fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "archive", ID: f.domain, Evidence: ev}}}
			},
		},
		{
			name: "archive: audit event refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_event", logRefused)
			},
			ops: func(f fixture) Output {
				return Output{DomainOps: []DomainOp{{Op: "archive", ID: f.domain, Evidence: ev}}}
			},
		},
		{
			name: "add: memory insert refused after a domain was created",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_memory", "BEFORE INSERT ON memories")
			},
			ops: func(fixture) Output {
				return Output{
					DomainOps: []DomainOp{{Op: "create", TmpID: "t1", Name: "Travel", Evidence: ev}},
					MemoryOps: []MemoryOp{{Op: "add", Domain: "t1", Type: "fact", Text: "Alice flies on Tuesdays.", Confidence: 0.9, Evidence: ev}},
				}
			},
		},
		{
			name: "add: audit event refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_event", logRefused)
			},
			ops: func(f fixture) Output {
				return Output{MemoryOps: []MemoryOp{{Op: "add", Domain: f.domain, Type: "fact", Text: "Alice flies on Tuesdays.", Confidence: 0.9, Evidence: ev}}}
			},
		},
		{
			name: "supersede: replacement refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_memory", "BEFORE INSERT ON memories")
			},
			ops: func(f fixture) Output {
				return Output{MemoryOps: []MemoryOp{{Op: "supersede", Domain: f.domain, OldID: f.memory, Type: "rule", Text: "Bob reviews every change.", Confidence: 0.9, Evidence: ev}}}
			},
		},
		{
			name: "supersede: audit event refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_event", logRefused)
			},
			ops: func(f fixture) Output {
				return Output{MemoryOps: []MemoryOp{{Op: "supersede", Domain: f.domain, OldID: f.memory, Type: "rule", Text: "Bob reviews every change.", Confidence: 0.9, Evidence: ev}}}
			},
		},
		{
			name: "retire: memory write refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_retire", "BEFORE UPDATE ON memories")
			},
			ops: func(f fixture) Output {
				return Output{MemoryOps: []MemoryOp{{Op: "retire", ID: f.memory, Reason: "obsolete", Evidence: ev}}}
			},
		},
		{
			name: "retire: audit event refused",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_event", logRefused)
			},
			ops: func(f fixture) Output {
				return Output{MemoryOps: []MemoryOp{{Op: "retire", ID: f.memory, Reason: "obsolete", Evidence: ev}}}
			},
		},
		{
			name: "conflict ledger event refused after a memory was added",
			fault: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_conflict", "BEFORE INSERT ON memory_events WHEN NEW.event_type='conflict_resolved'")
			},
			ops: func(f fixture) Output {
				return Output{
					MemoryOps:      []MemoryOp{{Op: "add", Domain: f.domain, Type: "fact", Text: "Alice flies on Tuesdays.", Confidence: 0.9, Evidence: ev}},
					ConflictLedger: []LedgerEntry{{Resolved: "kept Tuesday", Reason: "Alice said so", Evidence: ev}},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			d := createDomain(t, s, "Gateway")
			m := addMemory(t, s, d.ID, store.TypeRule, "Run make test after changes.")
			tc.fault(t, s)
			before := storeSnapshot(t, s)

			res, err := Apply(ctx, s, tc.ops(fixture{domain: d.ID, memory: m.ID}), ApplyContext{Actor: "sleep_cycle"})
			if err == nil {
				t.Fatal("apply succeeded, want the store error")
			}
			if res.Applied != 0 || len(res.DroppedKeywords) != 0 {
				t.Errorf("result = %+v, want nothing reported applied", res)
			}
			if after := storeSnapshot(t, s); after != before {
				t.Errorf("store changed by a failed apply:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A created domain takes the status the model gave it, and active when it
// gave none.
func TestApplyCreateHonoursStatus(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ev := store.Evidence{SeqStart: 1, SeqEnd: 1}
	out := Output{DomainOps: []DomainOp{
		{Op: "create", TmpID: "t1", Name: "Old trips", Status: "archived", Evidence: ev},
		{Op: "create", TmpID: "t2", Name: "Trips", Evidence: ev},
	}}
	if _, err := Apply(ctx, s, out, ApplyContext{Actor: "sleep_cycle"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for name, want := range map[string]store.Status{"Old trips": store.StatusArchived, "Trips": store.StatusActive} {
		d, err := s.DomainByNameAny(ctx, s.DB(), name)
		if err != nil {
			t.Fatalf("domain %q: %v", name, err)
		}
		if d.Status != want {
			t.Errorf("domain %q status = %q, want %q", name, d.Status, want)
		}
	}
}
