/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package hook

import (
	"context"
	"errors"
	"fmt"

	"go.bison9.com/agent/toolkit"
)

// The cancel causes: why a turn or a task was stopped. The engine cancels
// with one of them (context.WithCancelCause), records it in TurnResult.Cause
// and TaskInfo.Cause, and decides the outcome from it (CauseOutcome).
//
// A host passes its own cause to Run.Cancel or Agent.Cancel, or cancels the
// context it gave Run with context.WithCancelCause, wrapping one of these so
// the engine treats it the same way: fmt.Errorf("…: %w", ErrShutdown), or a
// type of its own whose Unwrap returns one. context.Cause returns the host's
// error unchanged and errors.Is finds the cause through it.
//
//   - ErrDeadline, or context.DeadlineExceeded (the turn budget ran out):
//     OutcomeError.
//   - Any other cause, including one that wraps none of these:
//     OutcomeCancelled.
//   - Only a cause wrapping ErrShutdown keeps the turn pending in the
//     session store for a restart.
var (
	// ErrInterrupted: the person stopped the reply (Run.Interrupt,
	// Agent.Interrupt). The turn ends cancelled and is not replayed; the
	// tasks it started keep running.
	ErrInterrupted = errors.New("interrupted by user")
	// ErrShutdown: the host is shutting down. The turn ends cancelled and
	// stays pending in the session store, so a restart can replay it.
	ErrShutdown = errors.New("shutdown")
	// ErrDeadline: the turn used up its budget (Limits.TurnBudget). The turn
	// ends with OutcomeError, as a turn that timed out always has. The engine
	// records a budget that ran out as ErrDeadline wrapping
	// context.DeadlineExceeded.
	ErrDeadline = errors.New("deadline")
	// ErrSuperseded: whoever was waiting for the turn no longer needs it (an
	// asking agent stopped waiting). The turn ends cancelled and is not
	// replayed.
	ErrSuperseded = errors.New("superseded")
)

// CauseOutcome is the outcome a turn stopped with cause ends with: "" for a
// nil cause (the turn was not stopped), OutcomeError for a cause wrapping
// ErrDeadline or context.DeadlineExceeded, OutcomeCancelled for any other.
func CauseOutcome(cause error) Outcome {
	switch {
	case cause == nil:
		return ""
	case errors.Is(cause, ErrDeadline), errors.Is(cause, context.DeadlineExceeded):
		return OutcomeError
	default:
		return OutcomeCancelled
	}
}

// ErrSessionStore marks an error from the session store: a message the
// session refused to record, or a save that failed. A host tells it from
// other turn errors with errors.Is, for instance to raise its store-fault
// alert from TurnFinished.
var ErrSessionStore = errors.New("session store")

// SessionStoreError is the error the engine returns when the session store
// refuses a write. A write refused during the turn ends it (TurnResult.Err);
// a failed save after the reply is TurnResult.SaveErr. It matches
// ErrSessionStore and the store's own error with errors.Is.
type SessionStoreError struct {
	// What names what was being written: "user message", "tool call
	// message", "tool result", "assistant reply", "session save", …
	What string
	Err  error
}

func (e *SessionStoreError) Error() string {
	return fmt.Sprintf("session store rejected the %s: %v", e.What, e.Err)
}

// Unwrap returns ErrSessionStore and the store's error.
func (e *SessionStoreError) Unwrap() []error { return []error{ErrSessionStore, e.Err} }

// ToolPanicError is the error on the result of a tool call whose tool
// panicked: the call fails with an internal-error result and this as its
// error, so a host can tell a panic from an ordinary tool failure with
// errors.As. Tool is the name the tool was called by.
type ToolPanicError struct {
	Tool  string
	Value any
}

func (e *ToolPanicError) Error() string {
	return fmt.Sprintf("tool %s panicked: %v", e.Tool, e.Value)
}

// PanicError is a panic the engine recovered from a callback. Callback names
// the callback's kind ("AfterModel", "BeforeTool", …) and Plugin the plugin
// that registered it ("" for one registered on its own). See the package
// documentation for what a panic does to the turn, per kind of callback.
type PanicError struct {
	Callback string
	Plugin   string
	Value    any
	// Stack is the panicking goroutine's stack, for the log.
	Stack []byte
}

func (e *PanicError) Error() string {
	if e.Plugin != "" {
		return fmt.Sprintf("%s callback of %s panicked: %v", e.Callback, e.Plugin, e.Value)
	}
	return fmt.Sprintf("%s callback panicked: %v", e.Callback, e.Value)
}

// InterruptedText is the text of the result an abandoned tool call gets.
const InterruptedText = "The call was interrupted before it finished."

// InterruptedResult is the synthetic result the engine records for a tool
// call it abandons after an interrupt or cancel (Limits.CancelGrace), so the
// session never holds a tool call without a result. Each call returns a new
// value.
func InterruptedResult() *toolkit.Result {
	return toolkit.ErrorResult(InterruptedText)
}
