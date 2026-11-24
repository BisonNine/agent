// cogmem - Cognitive Memory
// License: MIT

package portable

import (
	"context"
	"strings"
	"testing"

	"go.bison9.com/agent/cogmem/store"
)

// refuse installs an SQLite trigger that aborts the given statement on the
// given table, so the store write it guards fails the way a full disk or a
// locked database would.
func refuse(t *testing.T, s *store.Store, name, when string) {
	t.Helper()
	q := `CREATE TRIGGER ` + name + ` ` + when + ` BEGIN SELECT RAISE(ABORT, 'refused by test'); END`
	if _, err := s.DB().ExecContext(context.Background(), q); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
}

// Every store failure during an import stops it with an error naming the
// step, and nothing after the failing write is applied.
func TestImportStopsOnStoreFailure(t *testing.T) {
	ctx := context.Background()
	retiredDoc := func(text string) Document {
		return Document{FormatVersion: FormatVersion, Domains: []Domain{{
			Name: "Craft", Status: "active",
			Memories: []Memory{{Type: "fact", Status: "retired", Confidence: 0.9, Text: text}},
		}}}
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, s *store.Store) // existing state and the fault
		doc   Document
		mode  ImportMode
		want  string
	}{
		{
			name: "replace cannot clear a domain",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_delete", "BEFORE DELETE ON domains")
			},
			doc:  Document{FormatVersion: FormatVersion},
			mode: ImportReplace,
			want: "cogmem: import: clear ",
		},
		{
			name: "matched domain cannot be updated",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				if _, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Craft"}); err != nil {
					t.Fatalf("create: %v", err)
				}
				refuse(t, s, "no_update", "BEFORE UPDATE ON domains") //nolint:contextcheck // the test helpers open and write the store without a context
			},
			doc:  Document{FormatVersion: FormatVersion, Domains: []Domain{{Name: "Craft", Status: "active", Summary: "new"}}},
			mode: ImportMerge,
			want: `cogmem: import: update domain "Craft": `,
		},
		{
			name: "domain cannot be created",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_insert", "BEFORE INSERT ON domains")
			},
			doc:  Document{FormatVersion: FormatVersion, Domains: []Domain{{Name: "Craft", Status: "active"}}},
			mode: ImportMerge,
			want: `cogmem: import: create domain "Craft": `,
		},
		{
			name: "matched domain's memories cannot be read",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Craft"})
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				if _, err = s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
					DomainID: d.ID, Type: store.TypeFact, Text: "Alice writes short.",
					Status: store.StatusActive, Confidence: 0.9,
				}); err != nil {
					t.Fatalf("add: %v", err)
				}
				if _, err = s.DB().ExecContext(ctx, `UPDATE memories SET created_at='not a time'`); err != nil {
					t.Fatalf("corrupt: %v", err)
				}
			},
			doc:  Document{FormatVersion: FormatVersion, Domains: []Domain{{Name: "Craft", Status: "active"}}},
			mode: ImportMerge,
			want: "cogmem: import: read ",
		},
		{
			name: "memory cannot be added",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_memory", "BEFORE INSERT ON memories")
			},
			doc:  retiredDoc("Bob prefers tables."),
			mode: ImportMerge,
			want: "cogmem: import: add memory to ",
		},
		{
			name: "added memory cannot be retired",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				refuse(t, s, "no_retire", "BEFORE UPDATE ON memories WHEN NEW.status='retired'")
			},
			doc:  retiredDoc("Bob prefers tables."),
			mode: ImportMerge,
			want: "cogmem: import: retire ",
		},
		{
			name: "matched memory cannot be retired",
			setup: func(t *testing.T, s *store.Store) {
				t.Helper()
				d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Craft"})
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				if _, err = s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
					DomainID: d.ID, Type: store.TypeFact, Text: "Bob prefers tables.",
					Status: store.StatusActive, Confidence: 0.9,
				}); err != nil {
					t.Fatalf("add: %v", err)
				}
				refuse(t, s, "no_retire", "BEFORE UPDATE ON memories WHEN NEW.status='retired'") //nolint:contextcheck // the test helpers open and write the store without a context
			},
			doc:  retiredDoc("Bob prefers tables."),
			mode: ImportMerge,
			want: "cogmem: import: retire ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t, "fail.cogmem.db") //nolint:contextcheck // the test helpers open and write the store without a context
			tc.setup(t, s)
			_, err := Import(ctx, s, tc.doc, tc.mode)
			if err == nil {
				t.Fatalf("import succeeded, want an error starting %q", tc.want)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to start %q", err, tc.want)
			}
		})
	}
}

// Export and a replacing import on a closed store fail with the read error
// rather than producing an empty document or an empty store.
func TestExportAndReplaceFailOnClosedStore(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "closed.cogmem.db")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Export(ctx, s); err == nil || !strings.HasPrefix(err.Error(), "cogmem: export domains: ") {
		t.Fatalf("export err = %v, want the domain read error", err)
	}
	_, err := Import(ctx, s, Document{FormatVersion: FormatVersion}, ImportReplace)
	if err == nil || !strings.HasPrefix(err.Error(), "cogmem: import: list existing: ") {
		t.Fatalf("replace err = %v, want the list error", err)
	}
}

// A memory that cannot be read fails the export: a backup that silently
// leaves memories out is worse than none.
func TestExportFailsOnUnreadableMemory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "badmem.cogmem.db")
	d, err := s.CreateDomain(ctx, s.DB(), store.CreateDomainParams{Name: "Craft"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err = s.AddMemory(ctx, s.DB(), store.AddMemoryParams{
		DomainID: d.ID, Type: store.TypeFact, Text: "Alice writes short.",
		Status: store.StatusActive, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err = s.DB().ExecContext(ctx, `UPDATE memories SET created_at='not a time'`); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err = Export(ctx, s); err == nil || !strings.HasPrefix(err.Error(), "cogmem: export memories of "+d.ID) {
		t.Fatalf("export err = %v, want the memory read error for %s", err, d.ID)
	}
}

// A domain with a blank name is skipped, and a retired memory without a
// reason is retired with the default one.
func TestImportSkipsBlankDomainAndDefaultsRetireReason(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, "blank.cogmem.db")
	before, err := s.ListDomains(ctx, s.DB())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	doc := Document{FormatVersion: FormatVersion, Domains: []Domain{
		{Name: "   ", Status: "active", Memories: []Memory{{Type: "fact", Status: "active", Text: "lost"}}},
		{Name: "Craft", Status: "active", Memories: []Memory{{Type: "fact", Status: "retired", Confidence: 0.5, Text: "Old rule."}}},
	}}
	res, err := Import(ctx, s, doc, ImportMerge)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if want := (ImportResult{DomainsCreated: 1, MemoriesCreated: 1}); res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	after, err := s.ListDomains(ctx, s.DB())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("domains = %d, want %d (the blank one skipped)", len(after), len(before)+1)
	}
	d, err := s.DomainByName(ctx, s.DB(), "Craft")
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	mems, err := s.ListMemories(ctx, s.DB(), d.ID)
	if err != nil {
		t.Fatalf("memories: %v", err)
	}
	if len(mems) != 1 || mems[0].Status != store.StatusRetired ||
		mems[0].RetireReason == nil || *mems[0].RetireReason != "retired before export" {
		t.Fatalf("memories = %+v, want one retired with the default reason", mems)
	}
}
