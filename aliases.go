package agent

import (
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/toolkit"
)

// The run and what a turn answers.
type (
	Run         = hook.Run
	Message     = hook.Message
	RunOption   = hook.RunOption
	RunOptions  = hook.RunOptions
	TurnResult  = hook.TurnResult
	Outcome     = hook.Outcome
	Limits      = hook.Limits
	Scope       = hook.Scope
	Memory      = hook.Memory
	Redactor    = hook.Redactor
	ServedTools = hook.ServedTools
)

// The outcomes of a turn.
const (
	OutcomeOK        = hook.OutcomeOK
	OutcomeError     = hook.OutcomeError
	OutcomeCancelled = hook.OutcomeCancelled
	OutcomeEmpty     = hook.OutcomeEmpty
)

// UserText is a message of text alone.
func UserText(text string) Message { return hook.UserText(text) }

// AlreadyStored marks a message that is already in the session.
func AlreadyStored() RunOption { return hook.AlreadyStored() }

// ResetSession resets the session's context before the turn.
func ResetSession() RunOption { return hook.ResetSession() }

// TurnBudget gives the turn its own budget.
func TurnBudget(d time.Duration) RunOption { return hook.TurnBudget(d) }

// WithTurnID gives the turn the id the host made for it.
func WithTurnID(id string) RunOption { return hook.WithTurnID(id) }

// WithoutTurnID runs the turn with no turn id.
func WithoutTurnID() RunOption { return hook.WithoutTurnID() }

// WithoutEvents runs the turn with a handle that keeps no events but
// TurnFinished: for a host that only calls Wait.
func WithoutEvents() RunOption { return hook.WithoutEvents() }

// AtDepth runs the turn at sub-agent depth n.
func AtDepth(n int) RunOption { return hook.AtDepth(n) }

// The events (package hook documents when each is emitted).
type (
	Event             = hook.Event
	EventHeader       = hook.EventHeader
	TurnStarted       = hook.TurnStarted
	LoopStarted       = hook.LoopStarted
	ModelCallStarted  = hook.ModelCallStarted
	ModelCall         = hook.ModelCall
	ModelAnswer       = hook.ModelAnswer
	TextDelta         = hook.TextDelta
	ToolBatchStarted  = hook.ToolBatchStarted
	ToolCallStarted   = hook.ToolCallStarted
	ToolCallFinished  = hook.ToolCallFinished
	ToolBatchFinished = hook.ToolBatchFinished
	ToolResultStored  = hook.ToolResultStored
	Fallback          = hook.Fallback
	Attempt           = hook.Attempt
	Compaction        = hook.Compaction
	Image             = hook.Image
	TaskStarted       = hook.TaskStarted
	TaskFinished      = hook.TaskFinished
	TurnFinished      = hook.TurnFinished
	Interrupted       = hook.Interrupted
)

// The callbacks and what they are given.
type (
	Plugin           = hook.Plugin
	BeforeTurnFunc   = hook.BeforeTurnFunc
	AfterTurnFunc    = hook.AfterTurnFunc
	BeforeModelFunc  = hook.BeforeModelFunc
	AfterModelFunc   = hook.AfterModelFunc
	OnModelErrorFunc = hook.OnModelErrorFunc
	BeforeToolFunc   = hook.BeforeToolFunc
	AfterToolFunc    = hook.AfterToolFunc
	OnToolErrorFunc  = hook.OnToolErrorFunc
	OnFallbackFunc   = hook.OnFallbackFunc
	OnCompactionFunc = hook.OnCompactionFunc
	OnTextDeltaFunc  = hook.OnTextDeltaFunc
	OnInterruptFunc  = hook.OnInterruptFunc
	OnEventFunc      = hook.OnEventFunc
	Turn             = hook.Turn
	ModelInfo        = hook.ModelInfo
	ModelRequest     = hook.ModelRequest
	ModelResponse    = hook.ModelResponse
	ModelRef         = hook.ModelRef
	ToolCall         = hook.ToolCall
	ToolResult       = toolkit.Result
)

// Tasks.
type (
	TaskInfo   = hook.TaskInfo
	TaskCancel = hook.TaskCancel
)

// The error vocabulary: the cancel causes, the failures a turn reports, and
// the agent's own.
var (
	ErrInterrupted      = hook.ErrInterrupted
	ErrShutdown         = hook.ErrShutdown
	ErrDeadline         = hook.ErrDeadline
	ErrSuperseded       = hook.ErrSuperseded
	ErrSessionStore     = hook.ErrSessionStore
	ErrUnknownTask      = hook.ErrUnknownTask
	ErrNoModelAvailable = hook.ErrNoModelAvailable
	ErrInvalidSpec      = loop.ErrInvalidSpec
	ErrClosed           = loop.ErrClosed
	ErrNoSessionContext = loop.ErrNoSessionContext
)

// SessionStoreError, PanicError and EnginePanicError are the typed errors a turn ends with.
type (
	SessionStoreError = hook.SessionStoreError
	PanicError        = hook.PanicError
	EnginePanicError  = loop.EnginePanicError
)

// Refusal marks err as an expected refusal (a tool declining a call), which
// is logged as a warning, not an error.
func Refusal(err error) error { return toolkit.Refusal(err) }

// IsRefusal reports whether err was marked with Refusal.
func IsRefusal(err error) bool { return toolkit.IsExpectedRefusal(err) }
