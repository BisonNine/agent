/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

// TestSQLiteStore_CollidingKeyRefused writes session a:b, then uses a/b,
// which maps to the same file: every write is refused with the typed error,
// every read is empty, and a:b is left as it was.
func TestSQLiteStore_CollidingKeyRefused(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	mustAdd(t, s, "a:b", spawnllm.Message{Role: "user", Content: "alice"})
	noErr(t, s.SetSummary("a:b", "alice's summary"))
	noErr(t, s.SetPendingTurn("a:b"))

	writes := map[string]func() error{
		"AddMessage":         func() error { return s.AddMessage("a/b", "user", "bob") },
		"SetSummary":         func() error { return s.SetSummary("a/b", "bob's summary") },
		"SetHistory":         func() error { return s.SetHistory("a/b", nil) },
		"TruncateHistory":    func() error { return s.TruncateHistory("a/b", 0) },
		"ClearPendingTurn":   func() error { return s.ClearPendingTurn("a/b") },
		"SetCompactionState": func() error { return s.SetCompactionState("a/b", memory.CompactionState{}) },
	}
	for name, write := range writes {
		if err := write(); !errors.Is(err, memory.ErrKeyCollision) {
			t.Errorf("%s(a/b) = %v, want ErrKeyCollision", name, err)
		}
	}
	var ce *memory.KeyCollisionError
	if err := s.AddMessage("a_b", "user", "bob"); !errors.As(err, &ce) || ce.Requested != "a_b" || ce.Stored != "a:b" {
		t.Errorf("AddMessage(a_b) = %v, want *KeyCollisionError{a_b, a:b}", err)
	}

	if h := s.GetHistory("a/b"); len(h) != 0 {
		t.Errorf("GetHistory(a/b) = %+v, want empty", h)
	}
	if got := s.GetSummary("a/b"); got != "" {
		t.Errorf("GetSummary(a/b) = %q, want empty", got)
	}
	if _, err := s.GetCompactionState("a/b"); !errors.Is(err, memory.ErrKeyCollision) {
		t.Errorf("GetCompactionState(a/b) = %v, want ErrKeyCollision", err)
	}

	// A fresh store reads a:b from disk unchanged.
	fresh := openStore(t, dir)
	if h := fresh.GetHistory("a:b"); len(h) != 1 || h[0].Content != "alice" {
		t.Errorf("GetHistory(a:b) = %+v", h)
	}
	if got := fresh.GetSummary("a:b"); got != "alice's summary" {
		t.Errorf("GetSummary(a:b) = %q", got)
	}
	pending, err := fresh.ListPendingSessions()
	noErr(t, err)
	if !slices.Equal(pending, []string{"a:b"}) {
		t.Errorf("ListPendingSessions = %v, want [a:b]", pending)
	}
	keys, err := memory.ListSessions(dir, nil)
	noErr(t, err)
	if !slices.Equal(keys, []string{"a:b"}) {
		t.Errorf("ListSessions = %v, want [a:b]", keys)
	}
}

// TestSQLiteStore_CollidingKeyReadFirst reads session a/b before anything is
// written, which creates its file and caches its handle, then writes a:b to
// that file: a/b must not see a:b's history through the cached handle.
func TestSQLiteStore_CollidingKeyReadFirst(t *testing.T) {
	s := newStore(t)
	if h := s.GetHistory("a/b"); len(h) != 0 {
		t.Fatalf("GetHistory(a/b) = %+v", h)
	}
	mustAdd(t, s, "a:b", spawnllm.Message{Role: "user", Content: "alice"})
	if h := s.GetHistory("a/b"); len(h) != 0 {
		t.Errorf("GetHistory(a/b) = %+v, want empty", h)
	}
	if err := s.AddMessage("a/b", "user", "bob"); !errors.Is(err, memory.ErrKeyCollision) {
		t.Errorf("AddMessage(a/b) = %v, want ErrKeyCollision", err)
	}
	if h := s.GetHistory("a:b"); len(h) != 1 || h[0].Content != "alice" {
		t.Errorf("GetHistory(a:b) = %+v", h)
	}
}

// TestSQLiteStore_CollidingKeyRaceOnNewFile writes two colliding keys to a
// brand-new file from two stores at once. Exactly one takes the file; the
// other is refused, and no message of one appears in the other's history.
func TestSQLiteStore_CollidingKeyRaceOnNewFile(t *testing.T) {
	const iterations = 50
	for i := range iterations {
		dir := t.TempDir()
		stores := []*SQLiteStore{openStore(t, dir), openStore(t, dir)}
		keys := []string{fmt.Sprintf("a:%d", i), fmt.Sprintf("a/%d", i)}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j := range stores {
			wg.Go(func() { errs[j] = stores[j].AddMessage(keys[j], "user", keys[j]) })
		}
		wg.Wait()

		winners := 0
		for j, err := range errs {
			switch {
			case err == nil:
				winners++
				if h := stores[j].GetHistory(keys[j]); len(h) != 1 || h[0].Content != keys[j] {
					t.Fatalf("iteration %d: %s history = %+v", i, keys[j], h)
				}
			case errors.Is(err, memory.ErrKeyCollision):
				if h := stores[j].GetHistory(keys[j]); len(h) != 0 {
					t.Fatalf("iteration %d: refused %s reads %+v", i, keys[j], h)
				}
			default:
				t.Fatalf("iteration %d: %s: %v", i, keys[j], err)
			}
		}
		if winners != 1 {
			t.Fatalf("iteration %d: %d keys took the file, want 1 (%v)", i, winners, errs)
		}
	}
}

// TestSQLiteStore_SimultaneousFirstOpens writes a brand-new session from two
// stores, and from a store beside the manager's own archive handle, at the
// same moment. Both opens must succeed and every write must land.
func TestSQLiteStore_SimultaneousFirstOpens(t *testing.T) {
	const iterations = 50
	t.Run("two stores", func(t *testing.T) {
		dir := t.TempDir()
		stores := []*SQLiteStore{openStore(t, dir), openStore(t, dir)}
		for i := range iterations {
			key := fmt.Sprintf("alice-%d", i)
			errs := make([]error, len(stores))
			var wg sync.WaitGroup
			for j, s := range stores {
				wg.Go(func() { errs[j] = s.AddMessage(key, "user", fmt.Sprintf("m%d", j)) })
			}
			wg.Wait()
			for j, err := range errs {
				if err != nil {
					t.Fatalf("iteration %d store %d: %v", i, j, err)
				}
			}
			if h := stores[0].GetHistoryWithSeqs(key); len(h) != 2 || h[0].Seq != 1 || h[1].Seq != 2 {
				t.Fatalf("iteration %d: window = %+v", i, h)
			}
		}
	})
	t.Run("store and archive handle", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		for i := range iterations {
			key := fmt.Sprintf("bob-%d", i)
			var storeErr, archiveErr error
			var wg sync.WaitGroup
			wg.Go(func() { storeErr = s.AddMessage(key, "user", "hello") })
			wg.Go(func() {
				a, err := memory.OpenSession(dir, key)
				if err == nil {
					err = a.Append(1, spawnllm.Message{Role: "user", Content: "hello"}, time.Now())
				}
				archiveErr = errors.Join(err, a.Close())
			})
			wg.Wait()
			if storeErr != nil || archiveErr != nil {
				t.Fatalf("iteration %d: store %v, archive %v", i, storeErr, archiveErr)
			}
		}
	})
}

// TestMigrateJSONL_CollidingKeyRefused migrates a JSONL session whose key
// maps to a file another session already holds: it is reported as an error
// and the file is left as it was.
func TestMigrateJSONL_CollidingKeyRefused(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	mustAdd(t, s, "a:b", spawnllm.Message{Role: "user", Content: "alice"})
	s.ForgetSession("a:b")
	writeFixture(t, dir, "a_b.meta.json", `{"key":"a/b","summary":"bob","next_seq":1}`)
	writeFixture(t, dir, "a_b.jsonl", `{"seq":1,"role":"user","content":"bob"}`+"\n")

	report, err := MigrateJSONL(dir, nil)
	noErr(t, err)
	if !errors.Is(report.Errors["a/b"], memory.ErrKeyCollision) || len(report.Migrated) != 0 {
		t.Fatalf("report = %+v, want a/b refused", report)
	}
	if h := s.GetHistory("a:b"); len(h) != 1 || h[0].Content != "alice" {
		t.Errorf("GetHistory(a:b) = %+v", h)
	}
	if !exists(t, dir, "a_b.meta.json") || !exists(t, dir, "a_b.jsonl") {
		t.Error("refused sources were renamed")
	}
}
