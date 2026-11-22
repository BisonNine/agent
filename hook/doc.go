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
// After… and On…Error change a step; OnFallback, OnCompaction, OnTextDelta
// and OnInterrupt only observe, and exist for a host that must act at the
// moment the step happens, before the turn continues.
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
//   - A panic in a callback is recovered: in a Before… or On…Error callback
//     it is that callback's error, in an After… or observing callback it is
//     logged and the turn goes on.
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
//   - Every ToolCallStarted is followed by exactly one ToolCallFinished for
//     the same call ID, before TurnFinished. A call that was refused,
//     answered by BeforeTool, timed out, panicked or was abandoned by an
//     interrupt gets one too; an abandoned call's result is the synthetic
//     result InterruptedResult, which is also what the session stores, so
//     the transcript never holds a tool call without a result.
//   - An event is emitted after the callbacks of its step have run, and
//     carries what they decided (ModelCall carries the response as AfterModel
//     left it, ToolCallFinished the result as AfterTool left it, TurnFinished
//     the result as AfterTurn left it).
//   - Within a run, events are delivered in the order they were emitted. The
//     engine never blocks on a consumer: events not yet read are kept until
//     the run is released, and a consumer that stops reading early stops
//     nothing.
//   - On an interrupt or cancel, the model call in flight ends with a
//     ModelCall whose Err is the context's error, the tools in flight end as
//     above, and TurnFinished carries OutcomeCancelled and the cause.
package hook
