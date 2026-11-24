package hook

import "iter"

// Outcome is how a turn ended. The values are the strings a host already
// carries on a turn's final reply.
type Outcome string

// The outcomes of a turn.
const (
	// OutcomeOK: the agent replied (text, or images alone).
	OutcomeOK Outcome = "ok"
	// OutcomeError: the turn failed (a model, the session store, a panic in
	// a callback that changes a step), or used up its budget (ErrDeadline).
	OutcomeError Outcome = "error"
	// OutcomeCancelled: the turn was interrupted or cancelled with any cause
	// but ErrDeadline (CauseOutcome), or dropped from the session's queue
	// before it started.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeEmpty: the agent produced no reply.
	OutcomeEmpty Outcome = "empty"
)

// TurnResult is what a turn produced. The engine fills it when the model
// loop ends; AfterTurn may change Reply, Stored and Outcome; the engine then
// stores Stored, and TurnFinished carries the result.
type TurnResult struct {
	// Outcome is how the turn ended.
	Outcome Outcome
	// Reply is the text the turn answers with: the model's final content
	// followed, after a blank line, by the reply lines AfterModel added
	// (ModelResponse.ReplyLines) over the whole turn. Empty when there is
	// nothing to say. A host renders an error or cancellation into it in
	// AfterTurn. When storing the reply fails after AfterTurn the engine
	// leaves Reply as it was and sets Err and OutcomeError: a host sends
	// Reply only for the outcome it rendered it for.
	Reply string
	// Stored is the assistant message the engine stores in the session once
	// AfterTurn has run: the model's final content followed by the notes
	// AfterModel added to the last response (ModelResponse.Notes). Nothing
	// is stored when it is empty. A failed or cancelled turn starts with it
	// empty; for a turn BeforeTurn answered or refused it is never stored.
	Stored string
	// Err is the error that ended the turn, nil when it answered. A store
	// the session refused matches ErrSessionStore (*SessionStoreError); a
	// recovered panic is a *PanicError.
	Err error
	// SaveErr is the error of storing Stored or saving the session after
	// AfterTurn (a *SessionStoreError), nil when both succeeded or there was
	// nothing to store. Storing the reply failing also sets Err and makes the
	// outcome OutcomeError; a failed save only sets SaveErr.
	SaveErr error
	// Cause is why the turn's context ended (a cancel cause, or
	// context.DeadlineExceeded wrapped in ErrDeadline); nil while it was live.
	Cause error
	// FinishReason is the last model response's finish reason, or
	// "max_iterations" or "loop_detected" when the engine ended the loop.
	FinishReason string
	// Normal reports whether the last model response ended normally.
	Normal bool
	// Degenerate reports a reply that stayed empty after every prompt to
	// answer (the model produced only reasoning).
	Degenerate bool
	// Iterations is the number of model requests the turn made.
	Iterations int
	// Images counts the images the model generated over the turn.
	Images int
	// Skipped reports a turn BeforeTurn answered: no model was called and
	// nothing was stored.
	Skipped bool
}

// Run is the handle of one turn, returned by Agent.Run at once; the turn
// runs (or waits in the session's queue) in the background. Every method is
// safe from any goroutine, at any time, also after the turn ended.
type Run interface {
	// Events is the turn's event stream, ending with TurnFinished. The
	// second value is nil for every event except TurnFinished, where it is
	// TurnResult.Err, so `for ev, err := range run.Events()` ends on the
	// turn's error. Breaking out of the range stops nothing; a later range
	// continues with the events not yet yielded. The handle holds every
	// event until it is yielded, so a host that never ranges Events runs
	// the turn WithoutEvents; such a run yields TurnFinished alone.
	Events() iter.Seq2[Event, error]
	// Interrupt stops this run's turn with cause (nil means ErrInterrupted),
	// or drops this run from the queue. Other queued runs and the tasks it
	// started are left running: a host's "stop this reply" (/cancel) is an
	// Interrupt.
	Interrupt(cause error)
	// Cancel stops the turn as Interrupt does and, unlike it, also cancels
	// every task the turn started (TaskInfo.Turn), with theirs, with cause
	// (nil means ErrInterrupted); like every task cancel it waits up to the
	// registry's grace for their workers (TaskHandle.Exited).
	//
	// For the turn itself the two are the same: the turn ends with cause and
	// its outcome (CauseOutcome), and a cancel wins (package documentation,
	// "Cancel wins") from the instant either is called.
	Cancel(cause error)
	// Done is closed once TurnFinished has been emitted.
	Done() <-chan struct{}
	// Wait blocks until the turn finished and returns its result.
	Wait() TurnResult
}
