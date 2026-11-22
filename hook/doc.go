// Package hook is the contract between an agent's turn engine and the host
// that runs it: the callback signatures, the events, the run handle, the
// small interfaces the engine depends on, the scope a turn's context carries,
// the cancel causes and the loop's limits. It holds types, interfaces,
// constants and pure helpers only: no loop, no goroutine, no provider code.
// The engine (agent/loop) implements it; the root agent package re-exports
// it, so a host normally imports only the root.
//
// # Watching and changing
//
// Watching is done with events; a callback exists only to change a step. An
// observer reads Run.Events (one run) or registers OnEvent (every run of the
// agent); it cannot alter or reorder a turn. The callbacks named Before…,
// After… and On…Error, and DescribeImages, change a step; OnFallback,
// OnCompaction, OnTextDelta and OnInterrupt only observe, and exist for a
// host that must act at the moment the step happens, before the turn
// continues.
//
// # Callback rules
//
// These hold for every callback unless its type says otherwise.
//
//   - A Before… callback may edit the step's input through the pointer it is
//     given. A non-nil replacement it returns skips the step: the replacement
//     is used as the step's result, and the later callbacks of the same kind
//     are not called. A non-nil error ends the step as its type documents.
//   - An After… callback edits the step's result in place through the
//     pointer it is given. It cannot fail the step; to fail a tool call it
//     sets an error result.
//   - Several callbacks of one kind run in the order they were registered,
//     one after the other, each seeing the edits of the ones before it. A
//     Plugin's callbacks take the position of the option that added it.
//   - A callback runs synchronously: the step waits for it. It must not wait
//     for another turn of the same session, which cannot start until this
//     one ends.
//   - Every callback's context carries the turn's Scope (ScopeOf) and the
//     values of the context the host passed to Run. It is the turn's context,
//     cancelled when the turn is interrupted or cancelled, except where a
//     type says otherwise (AfterTurn, OnInterrupt).
//   - A panic in a callback is recovered and logged; see Panics.
//
// # Panics
//
// The engine recovers a panic in a callback and records it as a *PanicError.
// What follows depends on the kind of callback, because one that changes a
// step may have changed half of it, and going on would store a half-made
// step:
//
//   - An observing callback (OnFallback, OnCompaction, OnTextDelta,
//     OnInterrupt, OnEvent): nothing more; the turn goes on as if it had
//     returned. Only an observer is safe to skip.
//   - A turn or model step (BeforeTurn, BeforeModel, AfterModel,
//     OnModelError, DescribeImages): the turn ends with OutcomeError, its
//     Err the *PanicError. The later callbacks of that kind do not run and
//     nothing of the step is used or stored (after an AfterModel panic no
//     assistant message is stored for the response). AfterTurn still runs.
//   - AfterTurn: the result becomes OutcomeError with the *PanicError as Err
//     and an empty Stored, so nothing is stored; the later AfterTurn
//     callbacks still run, so the host can render the failure.
//   - A tool step (BeforeTool, OnToolError, AfterTool): only that call
//     fails. Its result is replaced by an error result ("Tool <name> failed
//     with an internal error.", Err the *PanicError), whatever the callbacks
//     had done to it, and the call's later callbacks do not run; the turn
//     goes on and the model sees a failed call, as for a panic in the tool
//     itself.
//
// # Event rules
//
// The event stream is part of the contract; any implementation of the agent
// must keep these.
//
//   - Every Run ends with exactly one TurnFinished, its last event, whether
//     the turn answered, failed, was interrupted, cancelled, or dropped from
//     the queue before it started. A run that started has TurnStarted as its
//     first event.
//   - A turn's span, TurnStarted to TurnFinished, holds everything of the
//     turn: BeforeTurn, opening and resetting the session, storing the
//     message (and a compaction that storing it starts), the model loop,
//     AfterTurn, storing the reply and saving the session. Only the
//     compaction deferred until after the reply falls outside it.
//   - A turn that reaches its model loop has exactly one LoopStarted, after
//     its message is stored and the turn marked pending, before its first
//     ModelCallStarted (and before anything else of the loop). A turn
//     BeforeTurn answered or refused, or that ended before its loop, has
//     none.
//   - Every ModelCallStarted is followed by exactly one ModelCall. A
//     dispatch's events come in this order: ModelCallStarted and ModelCall
//     for each model called, a Fallback after each model passed over, then
//     for the answer ModelAnswer and an Image per generated image.
//   - A batch of tool calls is reported as ToolBatchStarted; a
//     ToolCallStarted per call in the model's order; a ToolCallFinished per
//     call as each ends; ToolBatchFinished once all have ended; then a
//     ToolResultStored per result in the model's order as each is stored.
//   - Every ToolCallStarted is followed by exactly one ToolCallFinished for
//     the same call ID, before TurnFinished. A call that was refused,
//     answered by BeforeTool, timed out, panicked or was abandoned by an
//     interrupt gets one too; an abandoned call's result is the synthetic
//     result InterruptedResult, which is also what the session stores, so
//     the transcript never holds a tool call without a result. Once a call
//     is abandoned no callback is started for it and nothing more is
//     emitted for it: its tool's late return is discarded. A callback that
//     was already running for it may finish later, after TurnFinished, on
//     the call's cancelled context; what it does to the result is
//     discarded.
//   - ModelCall reports the provider's call as it returned, before any
//     callback acts on it. The other events that report a result are
//     emitted after the callbacks of their step and carry what they decided:
//     ModelAnswer the response as AfterModel left it, ToolCallFinished the
//     result as AfterTool left it (and the arguments as BeforeTool left
//     them), TurnFinished the result as AfterTurn left it, except that a
//     failure to store the reply afterwards sets Err and OutcomeError (the
//     Reply AfterTurn rendered is left as it is; a host checks Outcome
//     before sending it) and a failed save sets SaveErr. TurnStarted and
//     ToolCallStarted come before their callbacks (BeforeTurn, BeforeTool)
//     and carry the input as it was received.
//   - An event is a snapshot: the engine never changes what it points to
//     after the event is emitted (it copies what it will go on changing),
//     so a consumer on another goroutine may read it at any time. A
//     consumer must not change it either. For this a BeforeModel callback
//     replaces a message of the working request rather than writing into
//     one (into its tool calls, media or content blocks) in place.
//   - Within a run, events are delivered in the order they were emitted. The
//     engine never blocks on a consumer: events not yet read are held by the
//     run's handle, and a consumer that stops reading early stops nothing.
//     Ranging Events again yields only the events not yet yielded; once the
//     turn has finished, unread events go with the handle when the host
//     drops it.
//   - On an interrupt or cancel, the model call in flight ends with a
//     ModelCall whose Err is ctx.Err(), the tools in flight end as
//     above, and TurnFinished carries the outcome the cause decides
//     (CauseOutcome) and the cause.
package hook
