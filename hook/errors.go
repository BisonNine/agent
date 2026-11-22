package hook

import (
	"errors"

	"go.bison9.com/agent/toolkit"
)

// The cancel causes: why a turn or a task was stopped. The engine cancels
// with one of them (context.WithCancelCause), records it in TurnResult.Cause
// and TaskInfo.Cause, and decides the outcome from it. A host may pass its
// own cause to Run.Cancel or Agent.Cancel; it should wrap one of these
// (fmt.Errorf("…: %w", ErrShutdown)) so the engine treats it the same way.
var (
	// ErrInterrupted: the person stopped the reply (Run.Interrupt,
	// Agent.Interrupt). The turn ends cancelled and is not replayed; the
	// tasks it started keep running.
	ErrInterrupted = errors.New("interrupted by user")
	// ErrShutdown: the host is shutting down. The turn ends cancelled and
	// stays pending in the session store, so a restart can replay it.
	ErrShutdown = errors.New("shutdown")
	// ErrDeadline: the turn used up its budget (Limits.TurnBudget). The turn
	// ends with OutcomeError, as a turn that timed out always has.
	ErrDeadline = errors.New("deadline")
	// ErrSuperseded: whoever was waiting for the turn no longer needs it (an
	// asking agent stopped waiting). The turn ends cancelled and is not
	// replayed.
	ErrSuperseded = errors.New("superseded")
)

// InterruptedText is the text of the result an abandoned tool call gets.
const InterruptedText = "The call was interrupted before it finished."

// InterruptedResult is the synthetic result the engine records for a tool
// call it abandons after an interrupt or cancel (Limits.CancelGrace), so the
// session never holds a tool call without a result. Each call returns a new
// value.
func InterruptedResult() *toolkit.Result {
	return toolkit.ErrorResult(InterruptedText)
}
