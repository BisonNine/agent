package hook

import (
	"context"

	"go.bison9.com/agent/spawnllm/protocoltypes"
	"go.bison9.com/agent/toolkit"
)

// ModelRef names one model of the agent's models.
type ModelRef struct {
	Name     string // the model's name in the catalogue
	Provider string // its provider's name
	Protocol string // its provider's protocol (modelspec.IsCLIProtocol tells a CLI)
	Model    string // the wire model id sent to the provider
}

// ModelRequest is one model request: the messages, the tool definitions and
// the request options (spawnllm's option keys).
type ModelRequest struct {
	Messages []protocoltypes.Message
	Tools    []protocoltypes.ToolDefinition
	Options  map[string]any
}

// Turn is the turn a BeforeTurn or AfterTurn callback is called for.
type Turn struct {
	// Message is what the turn answers. BeforeTurn may change it (prepend
	// held notes, describe attachments); the engine stores it as changed.
	Message Message
	// Options are the run's options. Read only.
	Options RunOptions
}

// ModelInfo is one dispatch of a turn: the request the engine is about to
// send through the agent's models (BeforeModel), or has sent (AfterModel,
// OnModelError).
type ModelInfo struct {
	// Iteration is the turn's model request, from 1.
	Iteration int
	// Model is the model the turn chose (it is kept for every iteration);
	// the one that answered may be another, after a fallback.
	Model ModelRef
	// Request is the turn's working request. BeforeModel may edit it, and
	// the edits stay for the turn's later iterations (they are never stored
	// in the session), so a callback must be idempotent. Read only in
	// AfterModel and OnModelError.
	Request ModelRequest
}

// ModelResponse is a model's answer to one dispatch, as AfterModel sees it.
type ModelResponse struct {
	// Model is the model that answered.
	Model ModelRef
	// Response is the answer. AfterModel may change it (content, tool
	// calls); the engine acts on it as changed.
	Response *protocoltypes.LLMResponse
	// Notes are lines for the model: the engine appends them, after a blank
	// line, to the assistant message it stores for this response (the
	// tool-call message, or the final reply).
	Notes []string
	// ReplyLines are lines for the person: the engine appends them, after a
	// blank line, to the turn's reply (TurnResult.Reply).
	ReplyLines []string
}

// ToolCall is one tool call the model made.
type ToolCall struct {
	// ID is the call's id, which its result answers.
	ID string
	// Name is the tool's published name.
	Name string
	// Arguments are the call's arguments. BeforeTool may change them; the
	// tool is called with them as changed, and the stored call keeps the
	// model's own.
	Arguments map[string]any
	// Iteration is the turn's model request that made the call, from 1.
	Iteration int
}

// BeforeTurnFunc runs once at the start of a turn, before anything of the
// turn is stored, on the turn's goroutine and context.
//
// It may change turn.Message. A non-nil result answers the turn instead of
// the model: no model is called (TurnResult.Skipped is set). A non-nil
// error refuses the turn: it ends with OutcomeError and that error. Either
// way nothing of the turn is stored, whatever Stored says after AfterTurn,
// which runs in both cases.
type BeforeTurnFunc func(ctx context.Context, turn *Turn) (*TurnResult, error)

// AfterTurnFunc runs once at the end of every turn that ran BeforeTurn,
// however it ended, before the engine stores result.Stored and emits
// TurnFinished. Its context carries the turn's values but is never
// cancelled and has no deadline, so a host can persist and prepare its
// reply after an interrupt.
//
// It may change result.Reply, result.Stored and result.Outcome (render an
// error, replace an empty reply, decide that a reply is not stored). Stored
// is ignored for a turn BeforeTurn answered or refused. It must not change
// the other fields.
type AfterTurnFunc func(ctx context.Context, turn *Turn, result *TurnResult)

// BeforeModelFunc runs before each dispatch of a turn (once per iteration,
// and again for a retry after a timeout or a context overflow), on the
// turn's goroutine and context, before the agent's models are called.
//
// It may edit call.Request (see ModelInfo.Request). A non-nil response
// answers instead of the models: no model is called and the response goes
// to AfterModel as if a model had given it. A non-nil error ends the
// dispatch as a model error (OnModelError, then the turn fails); it is not
// held against any model.
//
// Shaping the request for one model (its max_tokens, its thinking level,
// images it cannot take, fields its provider requires) is the engine's, per
// model, after this callback.
type BeforeModelFunc func(ctx context.Context, call *ModelInfo) (*protocoltypes.LLMResponse, error)

// AfterModelFunc runs once per answered dispatch, on the turn's goroutine
// and context, before the engine acts on the response: before it stores the
// assistant message, runs tools or ends the loop.
//
// It may change resp.Response and add to resp.Notes and resp.ReplyLines. It
// must not change call.
type AfterModelFunc func(ctx context.Context, call *ModelInfo, resp *ModelResponse)

// OnModelErrorFunc runs when a dispatch failed for good, after the engine's
// own retries and every fallback model, on the turn's goroutine and context.
// It is not called when the turn's context ended.
//
// A non-nil response is used as the dispatch's answer and the turn goes on
// (AfterModel runs on it). Otherwise a non-nil error replaces err, and the
// turn ends with the error that is left.
type OnModelErrorFunc func(ctx context.Context, call *ModelInfo, err error) (*protocoltypes.LLMResponse, error)

// OnFallbackFunc observes the agent's models moving past one or more models
// within a dispatch, on the turn's goroutine and context, before the next
// model is called. The Fallback event carries the same.
type OnFallbackFunc func(ctx context.Context, ev Fallback)

// BeforeToolFunc runs before each tool call, on the call's own goroutine:
// the calls of one model response run in parallel, so it must be safe for
// concurrent use. Its context is the call's (the turn's, with
// Limits.ToolTimeout).
//
// It may change call.Arguments. A non-nil result answers the call instead
// of the tool. A non-nil error refuses the call: its result is an error
// result with the error's text (marked a refusal when toolkit's
// IsExpectedRefusal says so), and OnToolError runs on it. ToolCallStarted
// has been emitted for every call of the response before the first
// BeforeTool runs.
type BeforeToolFunc func(ctx context.Context, call *ToolCall) (*toolkit.Result, error)

// AfterToolFunc runs after each tool call, on the call's goroutine and
// context, once OnToolError has run for an error result, and before the
// result is stored or emitted.
//
// It may change *result (redact it, cap it, replace it). It must not keep
// result after returning.
type AfterToolFunc func(ctx context.Context, call *ToolCall, result *toolkit.Result)

// OnToolErrorFunc runs after a tool call whose result is an error (IsError
// or Err), including a refusal, a timeout and a panic, on the call's
// goroutine and context. It is not called for an abandoned call
// (InterruptedResult). A non-nil result replaces result; nil keeps it.
type OnToolErrorFunc func(ctx context.Context, call *ToolCall, result *toolkit.Result) *toolkit.Result

// OnCompactionFunc observes the session's context being compacted or
// evicted, on the goroutine doing it, at the moment it happens: for a
// CompactionStarted, before the summary model is called. A blocking pass
// runs on the turn's goroutine and context; a background pass (after the
// reply, or asked for by a tool) gets a context that is never cancelled,
// with the session's scope and no turn id. It runs while the session's
// context is held, so it must not call back into the session. The
// Compaction event carries the same.
type OnCompactionFunc func(ctx context.Context, ev Compaction)

// OnTextDeltaFunc observes partial reply text as a provider streams it, on
// the goroutine the provider delivers it on, in order for one model call.
// It must return quickly. The TextDelta event carries the same.
type OnTextDeltaFunc func(ctx context.Context, ev TextDelta)

// OnInterruptFunc observes an interrupt (Agent.Interrupt, Run.Interrupt) or
// a cancel (Run.Cancel, Agent.Cancel), on the goroutine that called it,
// after the turn's context was cancelled and what it drops was dropped. Its
// context is never cancelled.
type OnInterruptFunc func(ctx context.Context, ev Interrupted)

// OnEventFunc observes every event of the agent, including those of runs the
// host holds no handle for (background results, restart replays) and those
// of no live turn (a background compaction, a task that ends after its
// turn), on the goroutine that emits it and synchronously, before Run.Events
// delivers it. Its context is the turn's while one is live, otherwise one
// that is never cancelled, carrying the event's scope. It must be safe for
// concurrent use and must not block.
type OnEventFunc func(ctx context.Context, ev Event)

// Interrupted is what an interrupt or a cancel stopped. It is passed to
// OnInterrupt only; it is not an event.
//
//   - Agent.Interrupt(session): the session's running turn and its queue;
//     every task is left running.
//   - Run.Interrupt: that run only (its turn, or its place in the queue);
//     other queued runs and every task are left.
//   - Run.Cancel: that run and the tasks its turn started (with their own
//     tasks); other queued runs and tasks are left.
//   - Agent.Cancel: every turn, queue and task of the agent.
type Interrupted struct {
	// Scope names the agent and the session (empty for Agent.Cancel);
	// TurnID is the stopped turn's, empty when no turn was running.
	Scope Scope
	// Cause is the cancel cause.
	Cause error
	// Running reports whether a turn was running and was stopped.
	Running bool
	// Dropped counts the queued runs that were dropped.
	Dropped int
	// StillRunning lists the session's tasks that were left running.
	StillRunning []TaskInfo
	// Cancelled lists the tasks that were cancelled.
	Cancelled []TaskInfo
}

// Plugin is a named bundle of callbacks, registered as one. A nil field adds
// nothing. Each of its callbacks runs, among the callbacks of its kind, at
// the position of the option that registered the plugin. The name is for
// logs (a recovered panic names the plugin).
type Plugin struct {
	Name string

	BeforeTurn   BeforeTurnFunc
	AfterTurn    AfterTurnFunc
	BeforeModel  BeforeModelFunc
	AfterModel   AfterModelFunc
	OnModelError OnModelErrorFunc
	OnFallback   OnFallbackFunc
	BeforeTool   BeforeToolFunc
	AfterTool    AfterToolFunc
	OnToolError  OnToolErrorFunc
	OnCompaction OnCompactionFunc
	OnTextDelta  OnTextDeltaFunc
	OnInterrupt  OnInterruptFunc
	OnEvent      OnEventFunc
}
