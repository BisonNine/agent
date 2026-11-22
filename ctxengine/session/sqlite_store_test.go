/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

// Compile-time interface satisfaction check.
var _ SessionStore = (*SQLiteStore)(nil)

func newStore(t *testing.T) *SQLiteStore {
	t.Helper()
	return openStore(t, t.TempDir())
}

// mustAdd appends msg and returns its seq, failing the test on a write error.
func mustAdd(t *testing.T, s *SQLiteStore, key string, msg spawnllm.Message) int64 {
	t.Helper()
	seq, err := s.AddFullMessage(key, msg)
	if err != nil {
		t.Fatalf("AddFullMessage(%s): %v", key, err)
	}
	return seq
}

func openStore(t *testing.T, dir string) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(dir)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { noErr(t, s.Close()) })
	return s
}

func TestNewSQLiteStore_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "sessions")
	openStore(t, dir)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected directory, got file")
	}
}

// TestSessionFiles checks the on-disk layout: each session is its own
// <sanitized key>.archive.db, and a summary-only session is listed by its key.
func TestSessionFiles(t *testing.T) {
	s := newStore(t)
	noErr(t, s.AddMessage("s1", "user", "hello"))
	noErr(t, s.AddMessage("telegram:1/2", "user", "session2"))
	noErr(t, s.SetSummary("summary-only", "test summary"))

	for _, name := range []string{"s1.archive.db", "telegram_1_2.archive.db", "summary-only.archive.db"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err != nil {
			t.Errorf("expected %s: %v", name, err)
		}
	}
	keys, err := memory.ListSessions(s.dir, nil)
	noErr(t, err)
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"s1", "summary-only", "telegram:1/2"}) {
		t.Errorf("ListSessions = %v", keys)
	}
}

func TestCompactionState_RoundTripAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	const key = "agent:alice:main"
	s := openStore(t, dir)
	noErr(t, s.AddMessage(key, "user", "hi"))

	want := memory.CompactionState{
		MeaningfulCount:             7,
		CompressedAtMeaningfulCount: 5,
		Cooling:                     true,
		CoolingSinceCount:           6,
		SummaryGeneratedAt:          time.Now().Add(-time.Hour).UTC(),
		SummaryModel:                "m1",
		ActiveModelIndex:            2,
		ExposeReasoning:             true,
		ShowToolActivity:            true,
	}
	if err := s.SetCompactionState(key, want); err != nil {
		t.Fatalf("SetCompactionState: %v", err)
	}
	noErr(t, s.Close())

	s2 := openStore(t, dir)
	got, err := s2.GetCompactionState(key)
	if err != nil {
		t.Fatalf("GetCompactionState after reopen: %v", err)
	}
	if got != want {
		t.Errorf("compaction state round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	// The counters persist alongside the window and summary.
	if h := s2.GetHistory(key); len(h) != 1 || h[0].Content != "hi" {
		t.Errorf("history after reopen = %+v", h)
	}
}

func TestPersistence_AcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1 := openStore(t, dir)
	seq := mustAdd(t, s1, "persist", spawnllm.Message{Role: "user", Content: "remember me"})
	if err := s1.SetSummary("persist", "a test session"); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	noErr(t, s1.Close())

	s2 := openStore(t, dir)
	history := s2.GetHistoryWithSeqs("persist")
	if len(history) != 1 || history[0].Content != "remember me" || history[0].Seq != seq {
		t.Errorf("history = %+v", history)
	}
	if got := s2.GetSummary("persist"); got != "a test session" {
		t.Errorf("summary = %q", got)
	}
	if next := mustAdd(t, s2, "persist", spawnllm.Message{Role: "assistant", Content: "ok"}); next != seq+1 {
		t.Errorf("seq after reopen = %d, want %d", next, seq+1)
	}
}

// TestPendingTurn_Durable checks that the pending-turn flag survives a new
// store on the same directory, including for a session with no messages.
func TestPendingTurn_Durable(t *testing.T) {
	s := newStore(t)
	noErr(t, s.AddMessage("session-a", "user", "hello"))
	noErr(t, s.SetPendingTurn("session-a"))
	noErr(t, s.AddMessage("session-b", "user", "hi"))
	noErr(t, s.SetPendingTurn("agent:bob:main"))
	noErr(t, s.Close())

	s2 := openStore(t, s.dir)
	keys, err := s2.ListPendingSessions()
	noErr(t, err)
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"agent:bob:main", "session-a"}) {
		t.Errorf("pending after reopen = %v", keys)
	}
}

func TestListPendingSessions_DirectoryNotExist(t *testing.T) {
	s := &SQLiteStore{dir: filepath.Join(t.TempDir(), "does-not-exist"), sessions: map[string]*sessionHandle{}}
	keys, err := s.ListPendingSessions()
	if err != nil || len(keys) != 0 {
		t.Errorf("missing dir: keys=%v err=%v", keys, err)
	}
}

func TestGetArchiveBounds(t *testing.T) {
	s := newStore(t)
	noErr(t, s.AddMessage("s1", "user", "a"))
	// The window is not the archive: nothing archived yet.
	if lo, hi := s.GetArchiveBounds("s1"); lo != 0 || hi != 0 {
		t.Errorf("bounds with empty archive = %d,%d want 0,0", lo, hi)
	}
	if lo, hi := s.GetArchiveBounds("unknown"); lo != 0 || hi != 0 {
		t.Errorf("unknown session bounds = %d,%d want 0,0", lo, hi)
	}

	// The engine archives through its own handle on the same file.
	a, err := memory.Open(memory.ArchivePath(s.dir, "s1"))
	if err != nil {
		t.Fatal(err)
	}
	for seq := int64(3); seq <= 5; seq++ {
		if err := a.Append(seq, spawnllm.Message{Role: "user", Content: "x"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	noErr(t, a.Close())
	if lo, hi := s.GetArchiveBounds("s1"); lo != 3 || hi != 5 {
		t.Errorf("bounds = %d,%d want 3,5", lo, hi)
	}
}

func TestForgetSession(t *testing.T) {
	s := newStore(t)
	const key = "telegram_42"
	noErr(t, s.AddMessage(key, "user", "hello"))

	s.mu.Lock()
	_, present := s.sessions[key]
	s.mu.Unlock()
	if !present {
		t.Fatalf("expected a cached handle for %q after a write", key)
	}

	s.ForgetSession(key)

	s.mu.Lock()
	_, present = s.sessions[key]
	s.mu.Unlock()
	if present {
		t.Fatalf("handle for %q should be gone after ForgetSession", key)
	}

	// Durable data is untouched and the database reopens on access; the
	// noise cache restarts empty, so the duplicate counts as meaningful.
	noErr(t, s.AddMessage(key, "user", "hello"))
	if hist := s.GetHistory(key); len(hist) != 2 {
		t.Fatalf("history altered by ForgetSession: %+v", hist)
	}
	st, err := s.GetCompactionState(key)
	noErr(t, err)
	if st.MeaningfulCount != 2 {
		t.Errorf("MeaningfulCount = %d, want 2 (cache dropped)", st.MeaningfulCount)
	}

	// Forgetting an unknown session is a harmless no-op, and a forgotten
	// session's database can be deleted.
	s.ForgetSession("does-not-exist")
	s.ForgetSession(key)
	if err := memory.DeleteSession(s.dir, key); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if hist := s.GetHistory(key); len(hist) != 0 {
		t.Errorf("history after delete = %+v", hist)
	}
}

func TestConcurrent_AddAndRead(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	const goroutines, perGoroutine = 10, 20
	for range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				noErr(t, s.AddMessage("concurrent", "user", fmt.Sprintf("msg %d", i)))
				_ = s.GetHistory("concurrent")
			}
		})
	}
	// A summariser truncating concurrently, as in the agent loop.
	wg.Go(func() {
		for range 10 {
			noErr(t, s.SetSummary("concurrent", "summary"))
			noErr(t, s.TruncateHistory("concurrent", 50))
			s.ForgetSession("concurrent")
		}
	})
	wg.Wait()

	stored := s.GetHistoryWithSeqs("concurrent")
	for i := 1; i < len(stored); i++ {
		if stored[i].Seq <= stored[i-1].Seq {
			t.Fatalf("seqs not increasing at %d: %+v", i, stored)
		}
	}
	st, err := s.GetCompactionState("concurrent")
	noErr(t, err)
	if st.MeaningfulCount == 0 {
		t.Error("expected meaningful messages")
	}
	if s.GetSummary("concurrent") != "summary" {
		t.Error("summary lost")
	}
}

// gate is a noise key function that stops AddFullMessage for chosen contents
// while it holds the session and before it writes: the call reports on
// arrived and waits for release.
type gate struct {
	arrived map[string]chan struct{}
	release map[string]chan struct{}
}

func newGate(contents ...string) *gate {
	g := &gate{arrived: map[string]chan struct{}{}, release: map[string]chan struct{}{}}
	for _, c := range contents {
		g.arrived[c] = make(chan struct{})
		g.release[c] = make(chan struct{})
	}
	return g
}

// key is the memory.NoiseKeyFunc. IsNoise and Record both call it; only the
// first call (IsNoise, before the write) stops. Content "panic" panics.
func (g *gate) key(content string) (string, bool) {
	if content == "panic" {
		panic("noise key")
	}
	if ch, ok := g.arrived[content]; ok {
		select {
		case <-ch:
		default:
			close(ch)
			<-g.release[content]
		}
	}
	return "", false
}

// reached reports whether the add of content arrived at the gate within d.
func (g *gate) reached(content string, d time.Duration) bool {
	select {
	case <-g.arrived[content]:
		return true
	case <-time.After(d):
		return false
	}
}

// waitForgotten waits up to d for key's handle to leave the cache and
// reports whether it did.
func waitForgotten(s *SQLiteStore, key string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		_, ok := s.sessions[key]
		s.mu.Unlock()
		if !ok {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// TestForgetSession_WriterInFlight forgets a session while an add holds it
// and has not yet written, then adds again. The handle stays cached until the
// add in flight finishes and the second add waits for it, so the session
// never has two handles; before the fix the second add ran on a second handle,
// took the same seq and overwrote the first message.
func TestForgetSession_WriterInFlight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		forget func(t *testing.T, s *SQLiteStore, key string)
	}{
		{"ForgetSession", func(_ *testing.T, s *SQLiteStore, key string) { s.ForgetSession(key) }},
		{"Close", func(t *testing.T, s *SQLiteStore, _ string) {
			t.Helper()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			const key = "alice"
			g := newGate("first", "second")
			s.SetNoiseKey(g.key)

			var wg sync.WaitGroup
			errs := make(chan error, 2)
			wg.Go(func() {
				if err := s.AddMessage(key, "user", "first"); err != nil {
					errs <- err
				}
			})
			if !g.reached("first", 5*time.Second) {
				t.Fatal("the first add never reached the gate")
			}
			forgotten := make(chan struct{})
			go func() {
				tc.forget(t, s, key)
				close(forgotten)
			}()
			if waitForgotten(s, key, 100*time.Millisecond) {
				t.Error("the handle left the cache while a write on it was in flight")
			}
			wg.Go(func() {
				if err := s.AddMessage(key, "user", "second"); err != nil {
					errs <- err
				}
			})
			if g.reached("second", 100*time.Millisecond) {
				t.Error("a second add ran while the first held the session")
			}
			close(g.release["second"])
			close(g.release["first"])
			wg.Wait()
			<-forgotten
			close(errs)
			for err := range errs {
				t.Error(err)
			}

			stored := s.GetHistoryWithSeqs(key)
			got := make([]string, len(stored))
			for i, sm := range stored {
				got[i] = fmt.Sprintf("%d:%s", sm.Seq, sm.Content)
			}
			if want := []string{"1:first", "2:second"}; !slices.Equal(got, want) {
				t.Fatalf("window = %v, want %v", got, want)
			}
		})
	}
}

// TestSessionsIndependent holds one session's lock (an add stopped before its
// write) while that session is being forgotten: adds, reads and a forget on
// another session still complete.
func TestSessionsIndependent(t *testing.T) {
	s := newStore(t)
	g := newGate("held")
	s.SetNoiseKey(g.key)

	var wg sync.WaitGroup
	wg.Go(func() { noErr(t, s.AddMessage("alice", "user", "held")) })
	if !g.reached("held", 5*time.Second) {
		t.Fatal("the add never reached the gate")
	}
	wg.Go(func() { s.ForgetSession("alice") })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5 {
			if err := s.AddMessage("bob", "user", fmt.Sprintf("m%d", i)); err != nil {
				t.Error(err)
			}
		}
		_ = s.GetHistory("bob")
		s.ForgetSession("bob")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("bob's session waited for alice's")
	}
	close(g.release["held"])
	wg.Wait()

	if got := len(s.GetHistory("bob")); got != 5 {
		t.Errorf("bob = %d messages, want 5", got)
	}
	if got := s.GetHistory("alice"); len(got) != 1 || got[0].Content != "held" {
		t.Errorf("alice = %+v", got)
	}
}

// TestTwoHandlesNeverShareSeq writes one session through two stores on the
// same directory, so two database handles are open at once: each add takes
// its seq inside its own write transaction, so none is reused or lost.
func TestTwoHandlesNeverShareSeq(t *testing.T) {
	dir := t.TempDir()
	stores := []*SQLiteStore{openStore(t, dir), openStore(t, dir)}
	// Open both handles one after the other: memory.Open of a new file is
	// not what this tests.
	for _, s := range stores {
		_ = s.GetHistory("alice")
	}
	const perWriter = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*len(stores)*perWriter)
	for _, s := range stores {
		for range 2 {
			wg.Go(func() {
				for i := range perWriter {
					if err := s.AddMessage("alice", "user", fmt.Sprintf("m%d", i)); err != nil {
						errs <- err
					}
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	stored := stores[0].GetHistoryWithSeqs("alice")
	if len(stored) != 2*len(stores)*perWriter {
		t.Fatalf("window = %d messages, want %d", len(stored), 2*len(stores)*perWriter)
	}
	for i, sm := range stored {
		if sm.Seq != int64(i+1) {
			t.Fatalf("stored[%d].Seq = %d", i, sm.Seq)
		}
	}
}

// TestNoiseKeyPanic panics in the host's noise function during an add: the
// session is not left locked and the next add succeeds.
func TestNoiseKeyPanic(t *testing.T) {
	s := newStore(t)
	g := newGate()
	s.SetNoiseKey(g.key)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_ = s.AddMessage("alice", "user", "panic") //nolint:errcheck // it panics
	}()
	done := make(chan error, 1)
	go func() { done <- s.AddMessage("alice", "user", "after") }()
	select {
	case err := <-done:
		noErr(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the session stayed locked after the panic")
	}
	if got := s.GetHistory("alice"); len(got) != 1 || got[0].Content != "after" {
		t.Errorf("window = %+v", got)
	}
}

// openFDs counts the process's open file descriptors, or skips the test where
// /proc/self/fd does not exist.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot count open files: %v", err)
	}
	return len(entries)
}

// TestListPendingSessions_ClosesDatabases lists pending sessions many times:
// every database it opens to read is closed again.
func TestListPendingSessions_ClosesDatabases(t *testing.T) {
	dir := t.TempDir()
	writer := openStore(t, dir)
	for _, key := range []string{"alice", "bob"} {
		noErr(t, writer.SetPendingTurn(key))
	}
	noErr(t, writer.Close())

	s := openStore(t, dir)
	before := openFDs(t)
	for range 20 {
		keys, err := s.ListPendingSessions()
		noErr(t, err)
		if len(keys) != 2 {
			t.Fatalf("pending = %v", keys)
		}
	}
	if after := openFDs(t); after > before {
		t.Errorf("open files grew from %d to %d", before, after)
	}
}
