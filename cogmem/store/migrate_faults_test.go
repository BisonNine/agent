/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freshStorePath opens and closes a store, returning its path, so a test can
// doctor a current-schema database before opening it again.
func freshStorePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.cogmem.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// A migration step the database refuses makes Open fail with an error naming
// the step, instead of handing back a store on a half-migrated schema.
func TestOpenReportsTheFailingMigrationStep(t *testing.T) {
	refuse := func(when, table string) string {
		return `CREATE TRIGGER refuse BEFORE ` + when + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'refused'); END`
	}
	cases := []struct {
		name  string
		fresh bool // start from a current-schema store; otherwise from an empty file
		stmts []string
		want  string
	}{
		{"legacy rename", false, []string{
			`CREATE TABLE hooks (id TEXT PRIMARY KEY)`,
			`CREATE VIEW memories AS SELECT 1 AS id`,
		}, "cogmem: legacy rename:"},
		{"schema", false, []string{
			`CREATE VIEW domains AS SELECT 1 AS id, 'active' AS status`,
		}, "cogmem: schema:"},
		{"seed meta", true, []string{refuse("INSERT", "meta")}, "cogmem: seed meta"},
		{"normalize memory types", true, []string{
			`INSERT INTO memories(id, domain_id, type, text, status, confidence, created_at, updated_at)
			 SELECT 'hAAAAA', id, 'lesson', 'Alice learned', 'active', 1, 0, 0 FROM domains LIMIT 1`,
			refuse("UPDATE", "memories"),
		}, "cogmem: normalize memory types:"},
		{"normalize sticky column", true, []string{
			`UPDATE domains SET type='general'`,
			refuse("UPDATE", "domains"),
		}, "cogmem: normalize sticky column:"},
		{"retire review status", true, []string{
			`INSERT INTO memories(id, domain_id, type, text, status, confidence, created_at, updated_at)
			 SELECT 'hAAAAA', id, 'fact', 'Bob is unconfirmed', 'review', 1, 0, 0 FROM domains LIMIT 1`,
			refuse("UPDATE", "memories"),
		}, "cogmem: retire review status:"},
		{"drop legacy memory columns", true, []string{
			`ALTER TABLE memories ADD COLUMN priority INTEGER`,
			`CREATE INDEX idx_priority ON memories(priority)`,
		}, "cogmem: drop legacy memory columns: drop priority:"},
		{"drop legacy domain columns", true, []string{
			`ALTER TABLE domains ADD COLUMN agent_id TEXT`,
			`CREATE INDEX idx_agent ON domains(agent_id)`,
		}, "cogmem: drop legacy domain columns: drop agent_id:"},
		{"seed general domain", true, []string{
			`DELETE FROM domains`,
			`DELETE FROM meta WHERE key='seeded_general'`,
			refuse("INSERT", "domains"),
		}, "cogmem: seed general domain:"},
		{"collapse consolidation state", true, []string{
			`DELETE FROM consolidation_state`,
			`INSERT INTO consolidation_state(archive_path, consolidated_seq, updated_at) VALUES('/archive/alice', 7, 0)`,
			refuse("INSERT", "consolidation_state"),
		}, "cogmem: collapse consolidation state:"},
		{"record migration", true, []string{
			`DELETE FROM schema_migrations`,
			refuse("INSERT", "schema_migrations"),
		}, "cogmem: record migration:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.fresh {
				path = freshStorePath(t)
			} else {
				path = filepath.Join(t.TempDir(), "test.cogmem.db")
			}
			execAll(t, path, tc.stmts)
			s, err := Open(path)
			if err == nil {
				_ = s.Close()
				t.Fatalf("open succeeded, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("open err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A file that is not a SQLite database is refused by Open and by Migrate,
// which reports it without changing it.
func TestOpenRefusesAFileThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	path := DBPath(dir)
	junk := bytes.Repeat([]byte("this is not a database "), 512)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("open of a non-database succeeded")
	} else if !strings.Contains(err.Error(), "cogmem: read schema version:") {
		t.Errorf("open err = %v", err)
	}
	res := Migrate(dir)
	if res.Err == nil || res.From != 0 || res.To != 0 || res.Migrated() {
		t.Errorf("migrate = %+v, want an error and no versions", res)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, junk) {
		t.Error("the file was changed")
	}
}

// Open reports a database path it cannot create: under a regular file, or in
// a directory it may not write.
func TestOpenReportsUncreatablePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(filepath.Join(file, "test.cogmem.db")); err == nil || !strings.Contains(err.Error(), "create directory") {
		t.Errorf("under a file: %v", err)
	}

	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if _, err := Open(filepath.Join(ro, "test.cogmem.db")); err == nil || !strings.Contains(err.Error(), "create database file") {
		t.Errorf("read-only directory: %v", err)
	}

	// Writable but not listable: the file can be created, but the snapshots
	// beside it cannot be found to give them the file mode.
	wx := filepath.Join(dir, "wx")
	if err := os.Mkdir(wx, 0o300); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wx, 0o700) })
	if _, err := Open(filepath.Join(wx, "test.cogmem.db")); err == nil || !strings.Contains(err.Error(), "list snapshots") {
		t.Errorf("unlistable directory: %v", err)
	}
}

// A bare file name opens in the current directory, and a snapshot beside it
// is found there and given the file mode.
func TestOpenBareFileName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	snap := "test.cogmem.db.pre-v3.db"
	if err := os.WriteFile(snap, []byte("x"), 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	s, err := Open("test.cogmem.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
	if _, err := os.Stat(filepath.Join(dir, "test.cogmem.db")); err != nil {
		t.Errorf("database not in the current directory: %v", err)
	}
	if fi, _ := os.Stat(snap); fi.Mode().Perm() != DefaultFilePermissions {
		t.Errorf("snapshot mode = %v, want %v", fi.Mode().Perm(), DefaultFilePermissions)
	}
}

// A database whose schema_migrations table is empty counts as version 0: it
// is snapshotted as pre-v0 and migrated, and Migrate reports 0 -> current.
func TestEmptyMigrationTableReadsAsVersionZero(t *testing.T) {
	dir := t.TempDir()
	path := DBPath(dir)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
	execAll(t, path, []string{`DELETE FROM schema_migrations`})
	if v := peekVersion(path); v != 0 {
		t.Fatalf("peekVersion = %d, want 0", v)
	}
	res := Migrate(dir)
	if res.Err != nil || res.From != 0 || res.To != SchemaVersion() || !res.Migrated() {
		t.Fatalf("migrate = %+v, want 0 -> %d", res, SchemaVersion())
	}
	if _, err := os.Stat(path + ".pre-v0.db"); err != nil {
		t.Errorf("no pre-v0 snapshot: %v", err)
	}
}

// The first snapshot at a version is the one kept: a later open that finds a
// non-empty snapshot there does not replace it.
func TestExistingSnapshotIsKept(t *testing.T) {
	path := freshStorePath(t)
	execAll(t, path, []string{`DELETE FROM schema_migrations`})
	snap := path + ".pre-v0.db"
	if err := os.WriteFile(snap, []byte("the first snapshot"), DefaultFilePermissions); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
	if got, _ := os.ReadFile(snap); string(got) != "the first snapshot" {
		t.Errorf("snapshot replaced: %q", got)
	}
}

// When the pre-migration snapshot cannot be written, Open refuses to migrate.
func TestOpenRefusesToMigrateWithoutSnapshot(t *testing.T) {
	path := freshStorePath(t)
	execAll(t, path, []string{`DELETE FROM schema_migrations`})
	// A dangling symlink: not there to Stat, but in the way of creating it.
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing", "x"), path+".pre-v0.db"); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	s, err := Open(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("open migrated without a snapshot")
	}
	if !strings.Contains(err.Error(), "cogmem: pre-migration snapshot:") {
		t.Errorf("err = %v", err)
	}
	if v := peekVersion(path); v != 0 {
		t.Errorf("schema version = %d after a refused migration, want 0", v)
	}
}

// The migration steps report a database that lacks the tables they alter,
// rather than recording the migration as done.
func TestMigrationStepsOnAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	s := &Store{db: raw}
	steps := map[string]func() error{
		"ensureDomainColumns":        func() error { return s.ensureDomainColumns(ctx) },
		"ensureRunColumns":           func() error { return s.ensureRunColumns(ctx) },
		"ensureMemoryColumns":        func() error { return s.ensureMemoryColumns(ctx) },
		"normalizeStickyColumn":      func() error { return s.normalizeStickyColumn(ctx) },
		"collapseConsolidationState": func() error { return s.collapseConsolidationState(ctx) },
		"seedGeneralOnce":            func() error { return s.seedGeneralOnce(ctx) },
	}
	for name, step := range steps {
		if err := step(); err == nil || !strings.Contains(err.Error(), "no such table") {
			t.Errorf("%s: err = %v, want no such table", name, err)
		}
	}
	// No-ops when the columns they drop are absent.
	if err := s.dropLegacyMemoryColumns(ctx); err != nil {
		t.Errorf("dropLegacyMemoryColumns: %v", err)
	}
	if err := s.dropLegacyDomainColumns(ctx); err != nil {
		t.Errorf("dropLegacyDomainColumns: %v", err)
	}

	// seedGeneralOnce marks the seed first and counts domains second; each
	// failure is reported.
	if _, err := raw.Exec(`CREATE VIEW meta AS SELECT 'k' AS key, '0' AS value WHERE 0`); err != nil {
		t.Fatalf("view: %v", err)
	}
	if err := s.seedGeneralOnce(ctx); err == nil {
		t.Error("seedGeneralOnce with an unwritable meta: no error")
	}
	if _, err := raw.Exec(`DROP VIEW meta`); err != nil {
		t.Fatalf("drop view: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if err := s.seedGeneralOnce(ctx); err == nil || !strings.Contains(err.Error(), "no such table: domains") {
		t.Errorf("seedGeneralOnce without domains: %v", err)
	}
}

// Migrate on a store that is current reports no change.
func TestMigrateCurrentStoreReportsNoChange(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(DBPath(dir))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = s.Close()
	res := Migrate(dir)
	if res.Err != nil || res.From != SchemaVersion() || res.To != SchemaVersion() || res.Migrated() {
		t.Errorf("migrate = %+v, want no change at %d", res, SchemaVersion())
	}
}
