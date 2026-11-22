/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package session

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/spawnllm"
)

// MemoryStore implements SessionStore, and the optional interfaces the
// ctxengine manager uses (compaction state, CommitCompaction,
// SetHistoryWithSeqs), in process memory. It behaves as SQLiteStore does —
// the same seqs, ordering, counters and pending-turn flags — but nothing
// persists and nothing is written to disk: everything is gone when the
// process ends. It is for tests and for small or ephemeral hosts.
//
// It has no archive: GetArchiveBounds is always (0, 0), and the summary
// checkpoint of a CommitCompaction is not kept (the current summary is). Its
// size is bounded only by what the caller stores; DeleteSession frees a
// session. Safe for concurrent use.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*memSession
	noiseKey memory.NoiseKeyFunc
}

// memSession is one session's state. Messages are held as their JSON
// encoding, as SQLiteStore holds them, so a caller never shares memory with
// the store and a round trip drops exactly what the database would.
type memSession struct {
	window     []memRow // ordered by seq
	summary    string
	nextSeq    int64
	pending    bool
	compaction memory.CompactionState
	noise      *memory.NoiseCache
}

type memRow struct {
	seq       int64
	payload   []byte
	createdAt time.Time
}

// NewMemory returns an empty in-memory store. It has nothing to log and
// takes no options.
func NewMemory() *MemoryStore {
	return &MemoryStore{sessions: make(map[string]*memSession)}
}

// storedTime normalises t as a round trip through the database does: UTC,
// nanosecond precision, no monotonic reading; the zero time stays zero.
func storedTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return time.Unix(0, t.UnixNano()).UTC()
}

// session returns key's state, creating it. Callers hold s.mu for writing.
func (s *MemoryStore) session(key string) *memSession {
	ms, ok := s.sessions[key]
	if !ok {
		ms = &memSession{noise: memory.NewNoiseCache()}
		s.sessions[key] = ms
	}
	return ms
}

// encodeRows encodes msgs as window rows.
func encodeRows(msgs []memory.StoredMessage) ([]memRow, error) {
	rows := make([]memRow, len(msgs))
	for i, m := range msgs {
		payload, err := json.Marshal(m.Message)
		if err != nil {
			return nil, fmt.Errorf("session: marshal message: %w", err)
		}
		rows[i] = memRow{seq: m.Seq, payload: payload, createdAt: storedTime(m.CreatedAt)}
	}
	return rows, nil
}

// replaceWindow makes rows the window, ordered by seq; of rows sharing a seq
// the last one wins, as a database keyed by seq keeps it.
func (ms *memSession) replaceWindow(rows []memRow) {
	bySeq := make(map[int64]int, len(rows))
	window := make([]memRow, 0, len(rows))
	for _, r := range rows {
		if i, ok := bySeq[r.seq]; ok {
			window[i] = r
			continue
		}
		bySeq[r.seq] = len(window)
		window = append(window, r)
	}
	sort.SliceStable(window, func(i, j int) bool { return window[i].seq < window[j].seq })
	ms.window = window
}

func (s *MemoryStore) AddMessage(sessionKey, role, content string) error {
	_, err := s.AddFullMessage(sessionKey, spawnllm.Message{Role: role, Content: content})
	return err
}

// AddFullMessage appends msg to the window under the next sequence number and
// returns it, or 0 and the error when nothing was written. The message counts
// towards the meaningful count unless the noise classifier finds it a
// duplicate of the previous one.
func (s *MemoryStore) AddFullMessage(sessionKey string, msg spawnllm.Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.session(sessionKey)

	seq := ms.nextSeq + 1
	if seq <= 0 {
		seq = 1
	}
	stored := memory.NewStoredMessage(seq, msg)
	rows, err := encodeRows([]memory.StoredMessage{stored})
	if err != nil {
		return 0, err
	}
	isNoisy := ms.noise.IsNoise(stored, s.noiseKey)
	ms.window = append(ms.window, rows[0])
	ms.nextSeq = seq
	if !isNoisy {
		ms.compaction.MeaningfulCount++
	}
	ms.noise.Record(stored, s.noiseKey)
	return seq, nil
}

func (s *MemoryStore) GetHistory(key string) []spawnllm.Message {
	stored := s.GetHistoryWithSeqs(key)
	msgs := make([]spawnllm.Message, len(stored))
	for i, sm := range stored {
		msgs[i] = sm.Message
	}
	return msgs
}

func (s *MemoryStore) GetHistoryWithSeqs(key string) []memory.StoredMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ms, ok := s.sessions[key]
	if !ok {
		return []memory.StoredMessage{}
	}
	out := make([]memory.StoredMessage, 0, len(ms.window))
	for _, r := range ms.window {
		var msg spawnllm.Message
		if err := json.Unmarshal(r.payload, &msg); err != nil {
			// The payload is json.Marshal's output for this very type.
			panic(fmt.Sprintf("session: stored message does not decode: %v", err))
		}
		out = append(out, memory.StoredMessage{Seq: r.seq, CreatedAt: r.createdAt, Message: msg})
	}
	return out
}

func (s *MemoryStore) GetSummary(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ms, ok := s.sessions[key]; ok {
		return ms.summary
	}
	return ""
}

func (s *MemoryStore) SetSummary(key, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session(key).summary = summary
	return nil
}

// SetHistory replaces the window. The new messages take fresh sequence
// numbers after the current counter and are stamped now, as SQLiteStore does.
func (s *MemoryStore) SetHistory(key string, history []spawnllm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.session(key)
	seq := ms.nextSeq
	stored := make([]memory.StoredMessage, len(history))
	for i, msg := range history {
		seq++
		stored[i] = memory.NewStoredMessage(seq, msg)
	}
	rows, err := encodeRows(stored)
	if err != nil {
		return err
	}
	ms.replaceWindow(rows)
	ms.nextSeq = seq
	return nil
}

// SetHistoryWithSeqs replaces the window while preserving stable seq numbers.
// Messages without a seq are minted one after the highest seen; the counter
// never goes backwards.
func (s *MemoryStore) SetHistoryWithSeqs(key string, history []memory.StoredMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.session(key)
	stored, next := withStableSeqs(history, ms.nextSeq)
	rows, err := encodeRows(stored)
	if err != nil {
		return err
	}
	ms.replaceWindow(rows)
	ms.nextSeq = next
	return nil
}

// CommitCompaction writes one compaction result atomically: the window (seqs
// preserved, as SetHistoryWithSeqs), the current summary when c.Summary is
// set, and the compaction counters after c.Compaction. c.Checkpoint is not
// kept: the checkpoint log belongs to the archive, which this store does not
// have. A manager given WithArchiveDir over this store therefore archives
// messages but no checkpoints. c.Compaction runs under the store's lock and
// must not call the store.
func (s *MemoryStore) CommitCompaction(key string, c CompactionCommit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.session(key)
	stored, next := withStableSeqs(c.History, ms.nextSeq)
	rows, err := encodeRows(stored)
	if err != nil {
		return err
	}
	compaction := ms.compaction
	if c.Compaction != nil {
		c.Compaction(&compaction)
	}
	ms.replaceWindow(rows)
	ms.nextSeq = next
	if c.Summary != nil {
		ms.summary = *c.Summary
	}
	ms.compaction = normaliseCompaction(compaction)
	return nil
}

// TruncateHistory keeps only the last keepLast window messages. keepLast <= 0
// is a full reset: the window is emptied and the compression counters cleared.
func (s *MemoryStore) TruncateHistory(key string, keepLast int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.session(key)
	if keepLast <= 0 {
		ms.window = nil
		ms.compaction.MeaningfulCount = 0
		ms.compaction.CompressedAtMeaningfulCount = 0
		ms.compaction.Cooling = false
		return nil
	}
	if len(ms.window) > keepLast {
		ms.window = slices.Clone(ms.window[len(ms.window)-keepLast:])
	}
	return nil
}

func (s *MemoryStore) SetPendingTurn(sessionKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session(sessionKey).pending = true
	return nil
}

func (s *MemoryStore) ClearPendingTurn(sessionKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session(sessionKey).pending = false
	return nil
}

// ListPendingSessions returns the keys of the sessions whose pending-turn
// flag is set, sorted, or nil when there are none.
func (s *MemoryStore) ListPendingSessions() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for key, ms := range s.sessions {
		if ms.pending {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// GetArchiveBounds is always (0, 0): the store has no archive.
func (s *MemoryStore) GetArchiveBounds(string) (minSeq, maxSeq int64) { return 0, 0 }

// Save is a no-op: every write is complete when it returns.
func (s *MemoryStore) Save(string) error { return nil }

// GetCompactionState returns the session's compaction counters, the zero
// value for an unknown session. Implements ctxengine.CompactionStateStore.
func (s *MemoryStore) GetCompactionState(sessionKey string) (memory.CompactionState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ms, ok := s.sessions[sessionKey]; ok {
		return ms.compaction, nil
	}
	return memory.CompactionState{}, nil
}

// SetCompactionState replaces the session's compaction counters. Implements
// ctxengine.CompactionStateStore.
func (s *MemoryStore) SetCompactionState(sessionKey string, cs memory.CompactionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session(sessionKey).compaction = normaliseCompaction(cs)
	return nil
}

// normaliseCompaction stores cs's timestamp as the database would.
func normaliseCompaction(cs memory.CompactionState) memory.CompactionState {
	cs.SummaryGeneratedAt = storedTime(cs.SummaryGeneratedAt)
	return cs
}

// SetNoiseKey installs the function that recognises repeated fires of one
// source so they count as noise, as SQLiteStore.SetNoiseKey does.
func (s *MemoryStore) SetNoiseKey(fn memory.NoiseKeyFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noiseKey = fn
}

// ForgetSession drops the session's noise cache, as SQLiteStore.ForgetSession
// drops its cached handle. The session's data is kept; DeleteSession removes
// it.
func (s *MemoryStore) ForgetSession(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms, ok := s.sessions[key]; ok {
		ms.noise = memory.NewNoiseCache()
	}
}

// DeleteSession removes the session and everything stored for it, as
// memory.DeleteSession removes a session's database. Deleting an unknown
// session is a no-op.
func (s *MemoryStore) DeleteSession(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
}

// Close drops the noise caches, as SQLiteStore.Close drops its cached
// handles. The sessions stay readable, as a closed SQLiteStore's databases
// stay on disk.
func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ms := range s.sessions {
		ms.noise = memory.NewNoiseCache()
	}
	return nil
}
