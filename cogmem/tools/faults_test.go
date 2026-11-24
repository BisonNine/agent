// cogmem - Cognitive Memory
// License: MIT

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/cogmem/store"
)

// corrupt runs stmts against the handlers' store through a handle of its own,
// leaving values of the wrong type as a damaged file or another program could.
func (hs harness) corrupt(t *testing.T, stmts ...string) {
	t.Helper()
	db := hs.store(t).DB()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("corrupt (%s): %v", q, err)
		}
	}
}

// failsWith runs a tool that must fail with an error result whose text
// contains want.
func (hs harness) failsWith(t *testing.T, tool string, args map[string]any, want string) {
	t.Helper()
	res := hs.call(t, tool, args)
	if !res.IsError || !strings.Contains(res.ForLLM, want) {
		t.Errorf("%s(%v) = %q (isErr=%v), want an error containing %q", tool, args, res.ForLLM, res.IsError, want)
	}
}

// A memory directory that cannot be created fails every tool with a clear
// error instead of a panic.
func TestUnopenableStore(t *testing.T) {
	ws := t.TempDir()
	blocker := filepath.Join(ws, "cogmem")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hs := newHarness(t, func(h *Host) { h.Dir = blocker })
	res := hs.call(t, "status", nil)
	if !res.IsError || res.ForLLM != "failed to open memory store" || res.Err == nil {
		t.Errorf("status = %q (isErr=%v, err=%v), want the open failure", res.ForLLM, res.IsError, res.Err)
	}
}

// Damaged memory rows are reported by the tools that read them; forget
// retires nothing it could not read.
func TestCorruptMemoryRowsAreReported(t *testing.T) {
	hs := newHarness(t, nil)
	id := hs.createMemory(t, map[string]any{"type": "fact", "text": "Alice likes tea"})
	event := hs.createMemory(t, map[string]any{"type": "event", "text": "Bob flew to Paris"})
	hs.corrupt(t, `UPDATE memories SET created_at='yesterday' WHERE id IN ('`+id+`', '`+event+`')`)

	hs.failsWith(t, "memory_search", map[string]any{"query": "tea"}, "created_at")
	// Nothing standing matches, so the search falls back to events and
	// reports the damaged one rather than "no match".
	hs.failsWith(t, "memory_search", map[string]any{"query": "Paris"}, "created_at")
	hs.failsWith(t, "memory_forget", map[string]any{"query": "tea"}, "created_at")

	var status string
	if err := hs.store(t).DB().QueryRow(`SELECT status FROM memories WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(store.StatusActive) {
		t.Errorf("status = %q after a failed forget, want active", status)
	}
}

// A damaged domain row fails the tools that must read every domain, rather
// than leaving it out.
func TestCorruptDomainRowIsReported(t *testing.T) {
	hs := newHarness(t, nil)
	hs.createMemory(t, map[string]any{"type": "fact", "text": "Bob likes coffee"})
	hs.corrupt(t, `UPDATE domains SET state_json='{not json'`)

	hs.failsWith(t, "domain_list", nil, "bad state_json")
	hs.failsWith(t, "memory_create", map[string]any{"type": "fact", "text": "into General"}, "bad state_json")
	hs.failsWith(t, "memory_create", map[string]any{"type": "fact", "text": "hinted", "domain_hint": "General"}, "bad state_json")
	hs.failsWith(t, "export", nil, "bad state_json")
	if _, err := os.Stat(filepath.Join(hs.ws, "files", exportFilename)); !os.IsNotExist(err) {
		t.Errorf("export file stat = %v, want none written", err)
	}
}

// A damaged consolidation run is reported by status, not shown as none.
func TestStatusCorruptRun(t *testing.T) {
	hs := newHarness(t, nil)
	hs.corrupt(t, `INSERT INTO consolidation_runs (id, trigger, model, status, started_at) VALUES ('r1', 'manual', 'm', 'ok', 'yesterday')`)
	hs.failsWith(t, "status", nil, "started_at")
}

// Export reports a files directory it cannot create or a target it cannot
// write.
func TestExportWriteFailures(t *testing.T) {
	t.Run("files is a file", func(t *testing.T) {
		hs := newHarness(t, nil)
		if err := os.WriteFile(filepath.Join(hs.ws, "files"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		hs.failsWith(t, "export", nil, "failed to prepare export directory")
	})
	t.Run("target is a directory", func(t *testing.T) {
		hs := newHarness(t, nil)
		if err := os.MkdirAll(filepath.Join(hs.ws, "files", exportFilename), 0o700); err != nil {
			t.Fatal(err)
		}
		hs.failsWith(t, "export", nil, "failed to write export")
	})
}

// With nothing archived, listing archived domains says there are none.
func TestListDomainsNone(t *testing.T) {
	hs := newHarness(t, nil)
	if got := hs.ok(t, "domain_list", map[string]any{"status": "archived"}); got != "No domains." {
		t.Errorf("domain_list = %q, want No domains.", got)
	}
}

// A keyword list passed as []string (a Go host rather than decoded JSON) is
// accepted like a JSON array.
func TestKeywordTriggersAsStringSlice(t *testing.T) {
	hs := newHarness(t, nil)
	id := hs.createDomain(t, map[string]any{"name": "Travel", "keyword_triggers": []string{"flight booking"}})
	d, err := hs.store(t).GetDomain(ctx, hs.store(t).DB(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.KeywordTriggers != "flight booking" {
		t.Errorf("keyword triggers = %q, want flight booking", d.KeywordTriggers)
	}
}

// Setting keywords on a domain that does not exist reports it by id.
func TestUpdateKeywordsUnknownDomain(t *testing.T) {
	hs := newHarness(t, nil)
	hs.fail(t, "domain_update", map[string]any{"id": "dZZZZZ", "set_keyword_triggers": []any{"flight booking"}}, "dZZZZZ not found")
}
