/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.bison9.com/agent/ctxengine/internal/iox"
	"go.bison9.com/agent/spawnllm"
)

// SessionState is the per-session record that used to live in <key>.meta.json.
// It is the single session_state row of the session's archive database.
type SessionState struct {
	Key         string
	Summary     string
	NextSeq     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	PendingTurn bool
	Compaction  CompactionState
}

// CompactionState holds the durable compression counters for a session.
// Defined here so the session store can return it without importing the
// engine package (which would create a circular import).
type CompactionState struct {
	MeaningfulCount             int       `json:"meaningful_count"`
	CompressedAtMeaningfulCount int       `json:"compressed_at_meaningful_count"`
	Cooling                     bool      `json:"cooling"`
	CoolingSinceCount           int       `json:"cooling_since_count"`
	SummaryGeneratedAt          time.Time `json:"summary_generated_at,omitempty"`
	SummaryModel                string    `json:"summary_model,omitempty"`
	ActiveModelIndex            int       `json:"active_model_index,omitempty"`
	// ExposeReasoning, when true, delivers the model's reasoning to the user for
	// this session (toggled via /reasoning). Default false.
	ExposeReasoning bool `json:"expose_reasoning,omitempty"`
	// ShowToolActivity, when true, posts a one-line breadcrumb for each tool call
	// to the user for this session (toggled via /tools). Default false.
	ShowToolActivity bool `json:"show_tool_activity,omitempty"`
}

// sanitizeKey converts a session key to a safe filename component.
// Replaces ':' with '_' (session key separator) and '/' and '\' with '_'
// so composite IDs (e.g. Telegram forum "chatID/threadID", Slack "channel/thread_ts")
// do not create subdirectories or break on Windows.
func sanitizeKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// SanitizeSessionKey is the single filename rule for every per-session file:
// ':' '/' '\' become '_'. Every package that names a session file must use it
// rather than carry a copy.
func SanitizeSessionKey(key string) string { return sanitizeKey(key) }

// archiveSuffix is the file extension of a per-session database.
const archiveSuffix = ".archive.db"

// ArchivePath returns the SQLite archive path for a session under dir
// (normally <workspace>/sessions): <dir>/<sanitized-key>.archive.db.
func ArchivePath(dir, sessionKey string) string {
	return filepath.Join(dir, sanitizeKey(sessionKey)+archiveSuffix)
}

// ListSessions returns the session keys of every *.archive.db in dir that has
// a session_state row, sorted. Keys are read from the databases, not
// reconstructed from file names. A database that cannot be opened or has no
// state row (an archive that predates the window fold, or an empty one) is
// skipped. Returns nil, nil if dir does not exist.
//
// A file is listed under the key it holds. When several keys map to one file
// (see KeyCollisionError) only the one that wrote it first is a session; the
// others were refused and are not listed. This never makes the listing fail.
//
// Unreadable archives are logged to logger, tagged with the component
// "memory"; nil discards.
func ListSessions(dir string, logger *slog.Logger) ([]string, error) {
	log := componentLog(logger)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: list sessions: %w", err)
	}
	var keys []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), archiveSuffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		a, err := OpenReadOnly(path, logger)
		if err != nil {
			continue
		}
		st, err := a.State()
		if err != nil {
			log.Debug("list sessions: skipping unreadable archive",
				"path", entry.Name(), "error", err.Error())
			continue
		}
		if st.Key != "" {
			keys = append(keys, st.Key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// DeleteSession removes <key>.archive.db and its -wal/-shm sidecars. A
// missing file is not an error. Any open handle on the database must be
// closed first (session.SQLiteStore.ForgetSession). A file that holds another
// session is not removed: DeleteSession returns a *KeyCollisionError. A file
// whose key cannot be read (a damaged one) is removed; one that stays locked
// past the busy timeout is not, and the error is returned. The check and the
// removal are not one step: the caller must keep other handles off the key's
// file meanwhile, as it must for any open handle.
func DeleteSession(dir, key string) error {
	path := ArchivePath(dir, key)
	if err := refuseOtherSession(path, key); err != nil {
		return err
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("memory: delete session: %w", err)
		}
	}
	return nil
}

// refuseOtherSession returns a *KeyCollisionError when the file at path
// exists and holds a session other than key, the busy error when the file
// stayed locked past the busy timeout, and nil otherwise, including when the
// key cannot be read (a damaged file).
func refuseOtherSession(path, key string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := openReadOnly(path)
	if err != nil {
		return nil
	}
	defer iox.CloseQuietly(slog.New(slog.DiscardHandler), db)
	err = retryBusy(time.Now(), func() error { return checkKey(db, path, key) })
	if errors.As(err, new(*KeyCollisionError)) || isBusy(err) {
		return err
	}
	return nil
}

// unixNano encodes t for the window and session_state tables; the zero time
// is stored as 0.
func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromUnixNano decodes a window/session_state timestamp; 0 is the zero time.
func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// reader returns the connection to read window and state rows from: the write
// connection when the store owns one (so a read that follows a write on the
// same handle never sees a WAL visibility gap), otherwise a short-lived
// read-only connection that done closes. On a store opened for a session key
// the read runs in a transaction that has checked the key (readFrom).
func (a *ArchiveStore) reader() (q querier, done func(), err error) {
	if a.unavailable {
		return nil, nil, ErrArchiveUnavailable
	}
	if a.db != nil {
		a.mu.Lock()
		q, end, err := a.readFrom(a.db)
		if err != nil {
			a.mu.Unlock()
			return nil, nil, err
		}
		return q, func() {
			end()
			a.mu.Unlock()
		}, nil
	}
	return a.openRead()
}

// Window returns the live history window ordered by seq. Returns an empty
// slice (not nil) when the window is empty.
func (a *ArchiveStore) Window() ([]StoredMessage, error) {
	db, done, err := a.reader()
	if err != nil {
		return nil, err
	}
	defer done()

	rows, err := db.QueryContext(context.Background(),
		`SELECT seq, payload, created_at FROM window ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			a.logger().Debug("rows close failed", "error", closeErr.Error())
		}
	}()

	msgs := []StoredMessage{}
	for rows.Next() {
		var (
			seq       int64
			payload   string
			createdAt int64
		)
		if err := rows.Scan(&seq, &payload, &createdAt); err != nil {
			return nil, err
		}
		var msg spawnllm.Message
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil, err
		}
		// A literal rather than NewStoredMessageAt: a legacy message migrated
		// without a timestamp stays zero instead of being stamped on every read.
		msgs = append(msgs, StoredMessage{Seq: seq, CreatedAt: fromUnixNano(createdAt), Message: msg})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return msgs, nil
}

// State returns the session_state row, or the zero value and nil if there is
// none yet.
func (a *ArchiveStore) State() (SessionState, error) {
	db, done, err := a.reader()
	if err != nil {
		return SessionState{}, err
	}
	defer done()
	return readState(db)
}

// queryRower is the subset of *sql.DB and *sql.Conn readState uses.
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readState reads the session_state row, or the zero value and nil if there
// is none yet.
func readState(q queryRower) (SessionState, error) {
	var (
		st                                                      SessionState
		createdAt, updatedAt, summaryGeneratedAt                int64
		cooling, exposeReasoning, showToolActivity, pendingTurn bool
	)
	err := q.QueryRowContext(context.Background(),
		`SELECT key, summary, next_seq, created_at, updated_at,
		        meaningful_count, compressed_at_meaningful_count, cooling, cooling_since_count,
		        summary_generated_at, summary_model, active_model_index,
		        expose_reasoning, show_tool_activity, pending_turn
		 FROM session_state WHERE id = 1`).Scan(
		&st.Key, &st.Summary, &st.NextSeq, &createdAt, &updatedAt,
		&st.Compaction.MeaningfulCount, &st.Compaction.CompressedAtMeaningfulCount, &cooling, &st.Compaction.CoolingSinceCount,
		&summaryGeneratedAt, &st.Compaction.SummaryModel, &st.Compaction.ActiveModelIndex,
		&exposeReasoning, &showToolActivity, &pendingTurn,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionState{}, nil
	}
	if err != nil {
		return SessionState{}, err
	}
	st.CreatedAt = fromUnixNano(createdAt)
	st.UpdatedAt = fromUnixNano(updatedAt)
	st.PendingTurn = pendingTurn
	st.Compaction.Cooling = cooling
	st.Compaction.SummaryGeneratedAt = fromUnixNano(summaryGeneratedAt)
	st.Compaction.ExposeReasoning = exposeReasoning
	st.Compaction.ShowToolActivity = showToolActivity
	return st, nil
}

// execer is the subset of *sql.DB and txConn the state and window writers use.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// txConn is the connection a write transaction runs on.
type txConn struct{ *sql.Conn }

// Exec runs query on the transaction's connection.
func (c txConn) Exec(query string, args ...any) (sql.Result, error) {
	return c.ExecContext(context.Background(), query, args...)
}

// QueryRow runs query on the transaction's connection.
func (c txConn) QueryRow(query string, args ...any) *sql.Row {
	return c.QueryRowContext(context.Background(), query, args...)
}

// writeState upserts the single session_state row.
func writeState(ex execer, st SessionState) error {
	_, err := ex.Exec(
		`INSERT OR REPLACE INTO session_state
		    (id, key, summary, next_seq, created_at, updated_at,
		     meaningful_count, compressed_at_meaningful_count, cooling, cooling_since_count,
		     summary_generated_at, summary_model, active_model_index,
		     expose_reasoning, show_tool_activity, pending_turn)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.Key, st.Summary, st.NextSeq, unixNano(st.CreatedAt), unixNano(st.UpdatedAt),
		st.Compaction.MeaningfulCount, st.Compaction.CompressedAtMeaningfulCount, st.Compaction.Cooling, st.Compaction.CoolingSinceCount,
		unixNano(st.Compaction.SummaryGeneratedAt), st.Compaction.SummaryModel, st.Compaction.ActiveModelIndex,
		st.Compaction.ExposeReasoning, st.Compaction.ShowToolActivity, st.PendingTurn,
	)
	return err
}

// insertWindow inserts one window row.
func insertWindow(ex execer, msg StoredMessage) error {
	payload, err := json.Marshal(msg.Message)
	if err != nil {
		return fmt.Errorf("memory: marshal message: %w", err)
	}
	_, err = ex.Exec(
		`INSERT OR REPLACE INTO window (seq, role, payload, created_at) VALUES (?, ?, ?, ?)`,
		msg.Seq, msg.Role, string(payload), unixNano(msg.CreatedAt),
	)
	return err
}

// StateTx is one write transaction begun by Update. It takes the database's
// write lock when it begins (BEGIN IMMEDIATE), not at its first write, so the
// session_state row State returns is the committed one and stays so until the
// transaction ends: a value computed from it (the next seq) cannot be taken
// by another writer, even one on a second connection to the same file.
type StateTx struct {
	tx txConn
}

// rollbackStmt ends a failed transaction; a test replaces it to make the
// rollback fail.
var rollbackStmt = "ROLLBACK"

// Update runs fn in one write transaction and commits what it wrote, or rolls
// all of it back when fn returns an error or panics (the panic then
// continues). fn runs while the file's write lock is held, so it must be
// quick: a writer on another connection to the file waits up to the busy
// timeout (2 s) for it, then fails with SQLITE_BUSY.
//
// On a store opened for a session key (OpenSession) the transaction first
// checks, under the lock, that the file holds that key or none, and returns a
// *KeyCollisionError without running fn otherwise: a key is written to the
// file only while it holds none, never over another session's.
func (a *ArchiveStore) Update(fn func(tx *StateTx) error) error {
	if a.unavailable || a.db == nil {
		return ErrArchiveUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inTx(func(tx txConn) error { return fn(&StateTx{tx: tx}) })
}

// write runs fn on the write connection under the write mutex. On a store
// opened for a session key it runs in a write transaction that first checks
// the key, as Update does; on one opened by path each statement commits on
// its own.
func (a *ArchiveStore) write(fn func(ex execer) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.key == "" {
		return fn(a.db)
	}
	return a.inTx(func(tx txConn) error { return fn(tx) })
}

// inTx runs fn in one write transaction begun with BEGIN IMMEDIATE, after
// checking the key on a store opened for one, and commits, or rolls back
// when fn fails or panics. The caller holds a.mu.
func (a *ArchiveStore) inTx(fn func(tx txConn) error) error {
	conn, err := a.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer iox.CloseQuietly(a.logger(), conn)
	tx := txConn{conn}
	if _, err := tx.Exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			a.rollback(tx)
		}
	}()
	if a.key != "" {
		if err := claimKey(tx, a.path, a.key); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// claimKey checks, inside a write transaction, that the file holds key or
// none, and stores key when it holds none, so the first keyed write claims
// the file whether it writes the session state or only the archive. A new
// row takes the column defaults for everything but the key.
func claimKey(tx txConn, path, key string) error {
	if err := checkKey(tx, path, key); err != nil {
		return err
	}
	_, err := tx.Exec(
		`INSERT INTO session_state (id, key) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET key = excluded.key WHERE key = ''`, key)
	return err
}

// rollback ends tx's failed transaction. When SQLite has already rolled it
// back itself (after an I/O error, say) there is nothing to end. When the
// rollback fails with the transaction still open, the connection is
// discarded rather than returned to the pool holding the file's write lock.
func (a *ArchiveStore) rollback(tx txConn) {
	_, err := tx.Exec(rollbackStmt)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "no transaction is active") {
		a.logger().Debug("archive rollback: transaction already ended",
			"path", a.path)
		return
	}
	a.logger().Warn("archive rollback failed, discarding the connection",
		"path", a.path, "error", err.Error())
	if rawErr := tx.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(rawErr, driver.ErrBadConn) {
		a.logger().Warn("archive connection discard failed",
			"path", a.path, "error", fmt.Sprint(rawErr))
	}
}

// State returns the session_state row, or the zero value and nil if there is
// none yet.
func (t *StateTx) State() (SessionState, error) { return readState(t.tx) }

// SetState upserts the session_state row.
func (t *StateTx) SetState(st SessionState) error { return writeState(t.tx, st) }

// AppendWindow inserts one window row.
func (t *StateTx) AppendWindow(msg StoredMessage) error { return insertWindow(t.tx, msg) }

// ReplaceWindow replaces every window row with msgs.
func (t *StateTx) ReplaceWindow(msgs []StoredMessage) error { return replaceWindow(t.tx, msgs) }

// TruncateWindow deletes all but the newest keepLast window rows (every row
// when keepLast <= 0).
func (t *StateTx) TruncateWindow(keepLast int) error { return truncateWindow(t.tx, keepLast) }

// AppendSummary appends rec to the summaries table.
func (t *StateTx) AppendSummary(rec SummaryRecord) error {
	_, err := insertSummary(t.tx, rec)
	return err
}

// replaceWindow deletes every window row and inserts msgs.
func replaceWindow(ex execer, msgs []StoredMessage) error {
	if _, err := ex.Exec(`DELETE FROM window`); err != nil {
		return err
	}
	for _, msg := range msgs {
		if err := insertWindow(ex, msg); err != nil {
			return err
		}
	}
	return nil
}

// truncateWindow deletes all but the newest keepLast window rows (every row
// when keepLast <= 0).
func truncateWindow(ex execer, keepLast int) error {
	if keepLast <= 0 {
		_, err := ex.Exec(`DELETE FROM window`)
		return err
	}
	_, err := ex.Exec(
		`DELETE FROM window WHERE seq NOT IN (SELECT seq FROM window ORDER BY seq DESC LIMIT ?)`,
		keepLast)
	return err
}
