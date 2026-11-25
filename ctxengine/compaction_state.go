/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"go.bison9.com/agent/ctxengine/memory"
	"go.bison9.com/agent/ctxengine/session"
)

// CompactionCommitter is an optional interface a session store implements
// when it can write a whole compaction result — retained window, new summary,
// its checkpoint and the compaction counters — in one transaction.
// session.SQLiteStore does; the Manager uses it when present and otherwise
// writes the parts separately, which leaves a crash window between them.
type CompactionCommitter interface {
	CommitCompaction(sessionKey string, c session.CompactionCommit) error
}

// CompactionStateStore is an optional interface that session store backends
// may implement to persist compression state across process restarts.
// Manager checks for this interface via type assertion; backends that do not
// implement it (a test fake) use zero-state initialization.
//
// CompactionState is defined in memory so that implementing backends in
// session can satisfy this interface without importing the engine package
// (which would create a circular import). Go's structural typing means
// session.SQLiteStore satisfies CompactionStateStore without an explicit
// declaration.
//
// A host reads and writes its own per-session values kept in the state
// (ActiveModelIndex, ExposeReasoning, ShowToolActivity, Project) through it:
// GetCompactionState, change the field, SetCompactionState. The Manager
// changes only its counters, and a reset (TruncateHistory with keepLast <= 0)
// keeps the host's values.
type CompactionStateStore interface {
	GetCompactionState(sessionKey string) (memory.CompactionState, error)
	SetCompactionState(sessionKey string, state memory.CompactionState) error
}
