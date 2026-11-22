package hook

import (
	"time"

	"go.bison9.com/agent/spawnllm/protocoltypes"
	"go.bison9.com/agent/toolkit"
)

// Event is one thing that happened in a run: one of the event types of this
// package. Every event carries an EventHeader.
type Event interface {
	Header() EventHeader
}

// EventHeader is what every event carries: who it is about and when it
// happened (on the agent's clock).
type EventHeader struct {
	Scope Scope
	At    time.Time
}

// Header returns the header; it makes every event type an Event.
func (h EventHeader) Header() EventHeader { return h }

// TurnStarted is the first event of a run that started (left the queue). It
// opens the turn's span: it is emitted before BeforeTurn and before anything
// of the turn is stored, so a compaction while the message is stored falls
// inside the turn. Message and Options are as the run was given them.
type TurnStarted struct {
	EventHeader

	Message Message
	Options RunOptions
}

// LoopStarted is a turn's model loop about to start. It is emitted once,
// for a turn that reached its model loop: after BeforeTurn let it run, after
// the session was opened (and reset when the run asked), after the message
// was stored (unless AlreadyStored) with any compaction that started, after
// the session's context was first assembled for the turn (with any
// compaction that ran) and the pending marker was written (or its failure
// logged), and after every BeforeLoop callback returned without error;
// before the first iteration's tool definitions, assembly and BeforeModel,
// so before every ModelCallStarted. A turn BeforeTurn answered or refused,
// one a BeforeLoop callback ended, or one that ended before its loop (the
// store refused its message), has none. It only reports: set-up the turn
// depends on belongs in BeforeLoop, whose failure ends the turn.
type LoopStarted struct {
	EventHeader
}

// ModelCallStarted is one provider call about to be made: emitted for each
// model the agent's models call for a dispatch (a skipped model gets none),
// just before the call. Exactly one ModelCall follows it. A host that counts
// the model calls in flight counts these against ModelCall.
type ModelCallStarted struct {
	EventHeader

	Iteration int
	Model     ModelRef
}

// ModelCall is one provider call that returned, success or failure, emitted
// as soon as it returns: before the engine retries, falls back, runs
// OnModelError or AfterModel. It reports the provider's own call: what
// logging, usage, spend and request dumps consume. What the turn made of the
// answer is ModelAnswer.
type ModelCall struct {
	EventHeader

	Iteration int
	// Model is the model called.
	Model ModelRef
	// Request is the dispatch's request as BeforeModel left it, with the
	// engine's low-steps note when it has one, before the engine shaped it
	// for this model (its images, reasoning content and options).
	Request *ModelRequest
	// Response is the provider's response as it returned it; nil on a
	// failure. The engine never changes it: AfterModel works on a copy.
	Response *protocoltypes.LLMResponse
	// Err is the call's error; nil on success. When the turn's context had
	// ended by the time the call returned, Err is ctx.Err() (the cause is in
	// TurnResult.Cause), whatever the provider returned.
	Err     error
	Elapsed time.Duration
}

// ModelAnswer is a dispatch's answer as the turn will act on it: emitted
// once per answered dispatch, after AfterModel, before the engine stores the
// assistant message or runs tools. It is what a host showing the turn
// consumes.
type ModelAnswer struct {
	EventHeader

	Iteration int
	// Model is the model that answered (the turn's model for a supplied
	// answer).
	Model ModelRef
	// Response is the response as AfterModel left it.
	Response *protocoltypes.LLMResponse
	// Notes and ReplyLines are what AfterModel added (ModelResponse).
	Notes      []string
	ReplyLines []string
	// Supplied reports an answer BeforeModel or OnModelError gave: no
	// ModelCall reports it.
	Supplied bool
}

// TextDelta is partial reply text as the model streams it. The engine asks
// a provider for partial text only when Loop.TextDeltas is true after
// BeforeLoop (by default, when the agent has an OnTextDelta callback);
// otherwise no TextDelta is emitted. Every TextDelta of a run
// comes before its TurnFinished.
type TextDelta struct {
	EventHeader

	Iteration int
	Text      string
}

// ToolBatchStarted is a model response's tool calls about to run: emitted
// once the batch passed the repeated-batch check, before the tool-call
// message is stored and before the first ToolCallStarted. Content is the text
// the model sent with the calls (its narration), possibly empty.
type ToolBatchStarted struct {
	EventHeader

	Iteration int
	Calls     []ToolCall
	Content   string
}

// ToolCallStarted is a tool call the model made, with the model's
// arguments. The calls of one response are emitted in the response's order,
// all before the first BeforeTool runs.
type ToolCallStarted struct {
	EventHeader

	Call ToolCall
}

// ToolCallFinished is the end of a tool call that was started: emitted as
// each call ends (so not in call order when calls run in parallel), after
// AfterTool, before its result is stored.
type ToolCallFinished struct {
	EventHeader

	// Call is the call as the tool ran it: its arguments as BeforeTool left
	// them.
	Call ToolCall
	// Result is the result as AfterTool left it, or InterruptedResult for an
	// abandoned call (no AfterTool runs for it).
	Result  *toolkit.Result
	Elapsed time.Duration
	// Answered reports a call BeforeTool answered or refused: the tool did
	// not run.
	Answered bool
	// Interrupted reports a call abandoned after an interrupt or cancel;
	// Result is then InterruptedResult.
	Interrupted bool
}

// ToolBatchFinished is every call of a batch ended: emitted once, after the
// batch's last ToolCallFinished and before its first result is stored.
type ToolBatchFinished struct {
	EventHeader

	Iteration int
	// Calls is the number of calls in the batch.
	Calls int
}

// ToolResultStored is one result of a batch stored in the session: emitted
// in call order, each just after its result was stored. A result the session
// refused gets none (the turn ends with a SessionStoreError). A host that
// shows a tool's own output (toolkit.Result's ForUser and Media) consumes
// these, so the output follows the model's order and is never shown for a
// result that was not kept.
type ToolResultStored struct {
	EventHeader

	Call   ToolCall
	Result *toolkit.Result
}

// Attempt is one model a dispatch moved past.
type Attempt struct {
	Model ModelRef
	// Err is the model's error; nil for a skipped model.
	Err error
	// Reason is the error's class (spawnllm's failover reason) or, for a
	// skipped model, why it was parked; "disabled" for a disabled model.
	Reason   string
	Skipped  bool
	Disabled bool
	// Remaining is how long a skipped model stays parked.
	Remaining time.Duration
	Duration  time.Duration
}

// Fallback is a dispatch moving past one or more models (one failure, or a
// run of skipped ones) to try Next. It follows the failed model's ModelCall.
type Fallback struct {
	EventHeader

	Iteration int
	Passed    []Attempt
	Next      ModelRef
}

// CompactionPhase says what a Compaction event reports.
type CompactionPhase string

// The compaction phases.
const (
	// CompactionStarted: a summary pass is about to call the summary model.
	CompactionStarted CompactionPhase = "started"
	// CompactionOverflow: a model refused the dispatch's request as too
	// large and the engine is about to force a compaction and retry. It is
	// reported once per dispatch, on the first such retry; a dispatch that
	// overflows again is retried without another report.
	CompactionOverflow CompactionPhase = "overflow"
	// CompactionEvicted: tool results were collapsed to placeholders before
	// a dispatch, without a model.
	CompactionEvicted CompactionPhase = "evicted"
)

// Eviction is one tool result collapsed to a placeholder.
type Eviction struct {
	Tool     string // the tool that produced it
	Resource string // the path or URL it read
	Bytes    int    // the size freed
	Reason   string // why (the context engine's reason)
}

// Compaction is a change the engine made to the session's context to keep
// it within the model's window. One inside a turn's span (from TurnStarted
// to TurnFinished, storing the message included) is a turn event; one
// outside it (the deferred pass after the reply, one a tool asked for) is
// emitted to OnEvent only, with no turn id.
type Compaction struct {
	EventHeader

	Phase CompactionPhase
	// Reason is the context engine's reason for a pass (threshold, count,
	// age, overflow).
	Reason string
	// Blocking reports a pass the turn waits on; a background pass (after
	// the reply, or asked for by a tool) is not blocking.
	Blocking  bool
	Evictions []Eviction
}

// Image is one image the model generated, emitted after the ModelAnswer of
// the response that carried it.
type Image struct {
	EventHeader

	Iteration int
	Image     protocoltypes.GeneratedImage
}

// TaskStarted is a task registered with the agent.
type TaskStarted struct {
	EventHeader

	Task TaskInfo
}

// TaskFinished is a task that ended: done, failed or cancelled. Result is
// what its owner is to be told (nil for a cancelled task: whoever cancelled
// it already knows). It is emitted on the agent's event stream (OnEvent),
// with the scope of the turn that started the task; on Run.Events only
// while that turn is still running.
type TaskFinished struct {
	EventHeader

	Task   TaskInfo
	Result *toolkit.Result
}

// TurnFinished is the last event of every run. For a run that started it
// closes the turn's span: it is emitted after AfterTurn, after the reply was
// stored and the session saved. A run dropped from the queue has only this
// event.
type TurnFinished struct {
	EventHeader
	TurnResult
}
