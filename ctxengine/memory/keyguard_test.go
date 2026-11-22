/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm"
)

// writeSession opens key's file under dir, appends one window message and
// writes the state with key, and closes the file.
func writeSession(t *testing.T, dir, key, content string) {
	t.Helper()
	a, err := OpenSession(dir, key)
	noErr(t, err)
	noErr(t, appendWindow(a, NewStoredMessage(1, spawnllm.Message{Role: "user", Content: content}),
		SessionState{Key: key, NextSeq: 1}))
	noErr(t, a.Close())
}

// readSession returns the state and window of the file at path, opened by
// path so no key check applies.
func readSession(t *testing.T, path string) (SessionState, []StoredMessage) {
	t.Helper()
	a, err := OpenReadOnly(path, nil)
	noErr(t, err)
	st, err := a.State()
	noErr(t, err)
	w, err := a.Window()
	noErr(t, err)
	return st, w
}

// errOf returns the error of a call that returns one value and an error.
func errOf[T any](_ T, err error) error { return err }

// errOf2 returns the error of a call that returns two values and an error.
func errOf2[T, U any](_ T, _ U, err error) error { return err }

// wantCollision fails unless err is a *KeyCollisionError for requested
// against stored, also matched by ErrKeyCollision.
func wantCollision(t *testing.T, err error, requested, stored string) {
	t.Helper()
	var ce *KeyCollisionError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v, want *KeyCollisionError", err)
	}
	if !errors.Is(err, ErrKeyCollision) {
		t.Errorf("errors.Is(%v, ErrKeyCollision) = false", err)
	}
	if ce.Requested != requested || ce.Stored != stored {
		t.Errorf("collision = {Requested %q, Stored %q}, want {%q, %q}", ce.Requested, ce.Stored, requested, stored)
	}
	if strings.Contains(err.Error(), stored) && !strings.Contains(requested, stored) {
		t.Errorf("error text %q names the other session's key", err.Error())
	}
}

func TestOpenSession_RefusesFileOfAnotherKey(t *testing.T) {
	for _, other := range []string{"a/b", "a_b", `a\b`} {
		t.Run(other, func(t *testing.T) {
			dir := t.TempDir()
			writeSession(t, dir, "a:b", "alice")
			path := ArchivePath(dir, "a:b")
			if ArchivePath(dir, other) != path {
				t.Fatalf("%q and a:b do not share a file", other)
			}

			a, err := OpenSession(dir, other)
			wantCollision(t, err, other, "a:b")
			if ce := (*KeyCollisionError)(nil); errors.As(err, &ce) && ce.Path != path {
				t.Errorf("Path = %q, want %q", ce.Path, path)
			}
			if updErr := a.Update(func(*StateTx) error { return nil }); !errors.Is(updErr, ErrArchiveUnavailable) {
				t.Errorf("Update on the refused store = %v, want ErrArchiveUnavailable", updErr)
			}

			ro, err := OpenSessionReadOnly(dir, other, nil)
			wantCollision(t, err, other, "a:b")
			if _, readErr := ro.Window(); !errors.Is(readErr, ErrArchiveUnavailable) {
				t.Errorf("Window on the refused store = %v, want ErrArchiveUnavailable", readErr)
			}

			st, w := readSession(t, path)
			if st.Key != "a:b" || len(w) != 1 || w[0].Content != "alice" {
				t.Errorf("file changed: state %+v, window %+v", st, w)
			}
		})
	}
}

// TestKeyWrittenMeanwhile opens one new file for two colliding keys before
// either has written: both opens pass, the first write takes the file, and
// every later read and write through the second key's handles is refused,
// each inside its own transaction.
func TestKeyWrittenMeanwhile(t *testing.T) {
	dir := t.TempDir()
	alice, err := OpenSession(dir, "a:b")
	noErr(t, err)
	defer func() { noErr(t, alice.Close()) }()
	bob, err := OpenSession(dir, "a/b")
	noErr(t, err)
	defer func() { noErr(t, bob.Close()) }()
	bobRO, err := OpenSessionReadOnly(dir, "a/b", nil)
	noErr(t, err)

	noErr(t, appendWindow(alice, NewStoredMessage(1, spawnllm.Message{Role: "user", Content: "alice"}),
		SessionState{Key: "a:b", NextSeq: 1}))
	noErr(t, alice.Append(1, spawnllm.Message{Role: "user", Content: "alice"}, time.Now()))
	_, err = alice.AppendSummary(SummaryRecord{Summary: "alice"})
	noErr(t, err)

	for _, h := range []*ArchiveStore{bob, bobRO} {
		reads := map[string]func() error{
			"Window":        func() error { return errOf(h.Window()) },
			"State":         func() error { return errOf(h.State()) },
			"Bounds":        func() error { return errOf2(h.Bounds()) },
			"QueryRange":    func() error { return errOf(h.QueryRange(1, 1)) },
			"Search":        func() error { return errOf(h.Search(context.Background(), "alice", "", 10)) },
			"Stats":         func() error { _, _, _, statsErr := h.Stats(); return statsErr },
			"MinSeqAfter":   func() error { return errOf(h.MinSeqAfter(time.Time{})) },
			"ListSummaries": func() error { return errOf(h.ListSummaries()) },
			"GetSummary":    func() error { return errOf2(h.GetSummary(1)) },
		}
		for name, read := range reads {
			if readErr := read(); !errors.Is(readErr, ErrKeyCollision) {
				t.Errorf("%s = %v, want ErrKeyCollision", name, readErr)
			}
		}
	}
	if got := bob.MaxSeq(); got != 0 {
		t.Errorf("MaxSeq = %d, want 0", got)
	}
	writes := map[string]func() error{
		"Append": func() error {
			return bob.Append(2, spawnllm.Message{Role: "user", Content: "bob"}, time.Now())
		},
		"AppendSummary":         func() error { return errOf(bob.AppendSummary(SummaryRecord{Summary: "bob"})) },
		"PruneMessagesToCount":  func() error { return bob.PruneMessagesToCount(1) },
		"PruneMessagesBefore":   func() error { return bob.PruneMessagesBefore(time.Now().Add(time.Hour)) },
		"PruneSummariesToCount": func() error { return bob.PruneSummariesToCount(1) },
		"PruneSummariesBefore":  func() error { return bob.PruneSummariesBefore(time.Now().Add(time.Hour)) },
	}
	for name, write := range writes {
		if writeErr := write(); !errors.Is(writeErr, ErrKeyCollision) {
			t.Errorf("%s = %v, want ErrKeyCollision", name, writeErr)
		}
	}
	minSeq, maxSeq, err := alice.Bounds()
	if err != nil || minSeq != 1 || maxSeq != 1 {
		t.Errorf("alice's archive = [%d, %d] %v, want [1, 1]", minSeq, maxSeq, err)
	}
	metas, err := alice.ListSummaries()
	if err != nil || len(metas) != 1 {
		t.Errorf("alice's summaries = %+v %v, want 1", metas, err)
	}

	ran := false
	err = bob.Update(func(tx *StateTx) error {
		ran = true
		return tx.SetState(SessionState{Key: "a/b", NextSeq: 9})
	})
	wantCollision(t, err, "a/b", "a:b")
	if ran {
		t.Error("the refused transaction ran its function")
	}
	st, err := alice.State()
	noErr(t, err)
	if st.Key != "a:b" || st.NextSeq != 1 {
		t.Errorf("state = %+v, want key a:b, next seq 1", st)
	}
}

// TestOpenSession_ExistingSameKeyFile opens a file written by path, as older
// versions did, for the key it holds.
func TestOpenSession_ExistingSameKeyFile(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(ArchivePath(dir, "telegram:1/2"))
	noErr(t, err)
	noErr(t, appendWindow(a, NewStoredMessage(1, spawnllm.Message{Role: "user", Content: "hi"}),
		SessionState{Key: "telegram:1/2", NextSeq: 1}))
	noErr(t, a.Close())

	a, err = OpenSession(dir, "telegram:1/2")
	noErr(t, err)
	noErr(t, appendWindow(a, NewStoredMessage(2, spawnllm.Message{Role: "user", Content: "again"}),
		SessionState{Key: "telegram:1/2", NextSeq: 2}))
	noErr(t, a.Close())

	ro, err := OpenSessionReadOnly(dir, "telegram:1/2", nil)
	noErr(t, err)
	w, err := ro.Window()
	noErr(t, err)
	if len(w) != 2 {
		t.Errorf("window = %+v, want 2 messages", w)
	}
}

// TestOpenSession_AdoptsFileWithoutKey opens files that hold no key — an
// archive with no state row (written only by the manager's archive handle)
// and one whose row has an empty key — for a key, which then takes them.
func TestOpenSession_AdoptsFileWithoutKey(t *testing.T) {
	cases := map[string]func(a *ArchiveStore) error{
		"no state row": func(a *ArchiveStore) error {
			return a.Append(1, spawnllm.Message{Role: "user", Content: "archived"}, time.Now())
		},
		"empty key": func(a *ArchiveStore) error { return setState(a, SessionState{NextSeq: 3}) },
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			a, err := Open(ArchivePath(dir, "a:b"))
			noErr(t, err)
			noErr(t, prepare(a))
			noErr(t, a.Close())

			_, err = OpenSessionReadOnly(dir, "a/b", nil)
			noErr(t, err)
			a, err = OpenSession(dir, "a:b")
			noErr(t, err)
			noErr(t, setState(a, SessionState{Key: "a:b", NextSeq: 4}))
			noErr(t, a.Close())

			st, _ := readSession(t, ArchivePath(dir, "a:b"))
			if st.Key != "a:b" {
				t.Errorf("stored key = %q, want a:b", st.Key)
			}
			_, err = OpenSession(dir, "a/b")
			wantCollision(t, err, "a/b", "a:b")
		})
	}
}

// TestOpenSession_FileNames writes keys holding characters a SQLite URI
// treats specially and checks each lands in exactly the file ArchivePath
// names, and nothing else is created.
func TestOpenSession_FileNames(t *testing.T) {
	for _, key := range []string{"alice", "telegram:1/2", "a?b", "a?mode=ro", "a#b", "a%41b", "a%3Fb"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			writeSession(t, dir, key, "hello "+key)

			want := SanitizeSessionKey(key) + ".archive.db"
			entries, err := os.ReadDir(dir)
			noErr(t, err)
			for _, e := range entries {
				if name := e.Name(); name != want && name != want+"-wal" && name != want+"-shm" {
					t.Errorf("unexpected file %q beside %q", name, want)
				}
			}

			ro, err := OpenSessionReadOnly(dir, key, nil)
			noErr(t, err)
			w, err := ro.Window()
			noErr(t, err)
			if len(w) != 1 || w[0].Content != "hello "+key {
				t.Errorf("window = %+v", w)
			}
		})
	}
}

func TestDeleteSession_RefusesFileOfAnotherKey(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "a:b", "alice")

	wantCollision(t, DeleteSession(dir, "a/b"), "a/b", "a:b")
	if _, err := os.Stat(ArchivePath(dir, "a:b")); err != nil {
		t.Fatalf("file removed: %v", err)
	}
	noErr(t, DeleteSession(dir, "a:b"))
	if _, err := os.Stat(ArchivePath(dir, "a:b")); !os.IsNotExist(err) {
		t.Errorf("file kept after its own key deleted it: %v", err)
	}
}

// TestListSessions_CollidingFile lists a directory where a colliding key was
// refused: the file is listed once, under the key it holds.
func TestListSessions_CollidingFile(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "a:b", "alice")
	writeSession(t, dir, "bob", "bob")
	_, err := OpenSession(dir, "a/b")
	wantCollision(t, err, "a/b", "a:b")

	keys, err := ListSessions(dir, nil)
	noErr(t, err)
	if !slices.Equal(keys, []string{"a:b", "bob"}) {
		t.Errorf("ListSessions = %v, want [a:b bob]", keys)
	}
}

// TestOpen_SimultaneousFirstOpens opens brand-new files from several
// handles at once, as the store and the manager's archive do. Every open
// must succeed and every handle must be able to write.
func TestOpen_SimultaneousFirstOpens(t *testing.T) {
	const iterations, handles = 100, 3
	dir := t.TempDir()
	for i := range iterations {
		key := fmt.Sprintf("s%d", i)
		var wg sync.WaitGroup
		errs := make([]error, handles)
		for h := range handles {
			wg.Go(func() {
				a, err := OpenSession(dir, key)
				if err == nil {
					err = a.Update(func(tx *StateTx) error {
						return tx.SetState(SessionState{Key: key, NextSeq: int64(h + 1)})
					})
				}
				if closeErr := a.Close(); err == nil {
					err = closeErr
				}
				errs[h] = err
			})
		}
		wg.Wait()
		for h, err := range errs {
			if err != nil {
				t.Fatalf("iteration %d handle %d: %v", i, h, err)
			}
		}
	}
}

// TestOpenSessionReadOnly_DuringFirstOpen reads a session over and over
// while its file is being created. Each read sees either no archive yet or
// the whole schema, never a half-made file or a lock it did not wait out.
func TestOpenSessionReadOnly_DuringFirstOpen(t *testing.T) {
	const iterations = 100
	dir := t.TempDir()
	for i := range iterations {
		key := fmt.Sprintf("s%d", i)
		done := make(chan struct{})
		var wg sync.WaitGroup
		var openErr, readErr error
		wg.Go(func() {
			defer close(done)
			a, err := OpenSession(dir, key)
			openErr = errors.Join(err, a.Close())
		})
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				ro, err := OpenSessionReadOnly(dir, key, nil)
				if errors.Is(err, ErrArchiveUnavailable) {
					continue
				}
				if err == nil {
					_, _, err = ro.Bounds()
				}
				if err == nil {
					_, err = ro.State()
				}
				if err != nil {
					readErr = err
					return
				}
			}
		})
		wg.Wait()
		if openErr != nil {
			t.Fatalf("iteration %d: open: %v", i, openErr)
		}
		if readErr != nil {
			t.Fatalf("iteration %d: read: %v", i, readErr)
		}
	}
}
