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

// TurnStarted is the first event of a run that started (left the queue),
// emitted before BeforeTurn.
type TurnStarted struct {
	EventHeader

	Message Message
	Options RunOptions
}

// ModelCall is one call to one model: one per model the agent's models
// called for a dispatch (a model skipped because it is parked or disabled
// is reported in Fallback instead), and one for a response BeforeModel or
// OnModelError supplied (Model is then the turn's model and Supplied is
// set). For the model that answered it is emitted after AfterModel.
type ModelCall struct {
	EventHeader

	Iteration int
	Model     ModelRef
	// Request is the dispatch's request as BeforeModel left it, before the
	// engine shaped it for this model. Read only.
	Request *ModelRequest
	// Response is the answer (as AfterModel left it); nil on a failure.
	// Read only.
	Response *protocoltypes.LLMResponse
	// Err is the call's error; nil on success.
	Err      error
	Elapsed  time.Duration
	Supplied bool
}

// TextDelta is partial reply text as the model streams it. Every TextDelta
// of a run comes before its TurnFinished.
type TextDelta struct {
	EventHeader

	Iteration int
	Text      string
}

// ToolCallStarted is a tool call the model made. The calls of one response
// are emitted in the response's order, all before the first of them runs.
type ToolCallStarted struct {
	EventHeader

	Call ToolCall
}

// ToolCallFinished is the end of a tool call that was started: emitted as
// each call ends (so not necessarily in call order), after AfterTool.
type ToolCallFinished struct {
	EventHeader

	Call ToolCall
	// Result is the result as stored. Read only.
	Result  *toolkit.Result
	Elapsed time.Duration
	// Answered reports a call BeforeTool answered or refused: the tool did
	// not run.
	Answered bool
	// Interrupted reports a call abandoned after an interrupt or cancel;
	// Result is then InterruptedResult.
	Interrupted bool
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
	// CompactionOverflow: a model refused the request as too large; the
	// engine is about to force a compaction and retry.
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
// it within the model's window.
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

// Image is one image the model generated, emitted after AfterModel for the
// response that carried it.
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
// it already knows). It is emitted on the agent's event stream (OnEvent);
// on Run.Events only while the run that started the task is still open.
type TaskFinished struct {
	EventHeader

	Task   TaskInfo
	Result *toolkit.Result
}

// TurnFinished is the last event of every run.
type TurnFinished struct {
	EventHeader

	TurnResult
}
