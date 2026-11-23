package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"
)

// Spec is an agent's settings: everything New builds an Agent from. The root
// package's options (agent.With…) each set one field; a host may also write
// its own option over a field the root has no option for. The zero value of
// every field but ID and Models is usable.
type Spec struct {
	// ID is the agent's id, used as given (never rewritten): it names the
	// agent in its log lines and is its prompt cache key. Required.
	ID string
	// Name is the agent's friendly name, for logs, accounting and notices.
	Name string
	// Description says what the agent is for; the agent does not read it.
	Description string
	// Identity is who the agent's model calls are accounted to, and the
	// agent its Scope names (AgentID, AgentName, Project) and its tasks'
	// owner: a temporary clone of an agent bills its source. Zero means
	// {ID, Name, Project}.
	Identity accounting.Identity

	// Instruction is the first block of the system prompt.
	Instruction string
	// InstructionLayers, when set, returns the system-prompt blocks that
	// follow Instruction, for each dispatch. Its context is the dispatch's:
	// the turn's Scope (hook.ScopeOf) and the values of the context given to
	// Run.
	InstructionLayers func(ctx context.Context) []ctxengine.Layer

	// Models are the agent's models: its turns use purpose models.Chat, its
	// compaction summaries models.Summarize. Required unless TurnModels is
	// set; without it the agent's own context managers have no summary model
	// (compaction then summarises nothing; a host that supplies Session
	// builds its managers with its own).
	Models *models.Set
	// TurnModels, when set, chooses each turn's models: called once per
	// turn, before BeforeTurn, with the turn's scope. Nil means the chat
	// models of Models in the session's selection scope. A turn whose chain
	// is nil or empty fails with an error matching hook.ErrNoModelAvailable.
	TurnModels func(ctx context.Context, s hook.Scope) TurnModels
	// Tools are the agent's tools, read once per model call; nil means none.
	Tools toolkit.Registry
	// Allow, when set, is asked again when a tool is executed, so a call of a
	// tool the agent is not granted is refused (defence in depth). Gate, when
	// set, replaces it: it runs on the exact tool executed, and a non-nil
	// result refuses the call.
	Allow toolkit.AllowChecker
	Gate  toolkit.Gate

	// Sessions is the session store; nil means an in-memory store
	// (session.NewMemory), which keeps nothing across a restart. The engine
	// writes the pending-turn marker and saves through it.
	Sessions session.SessionStore
	// Session, when set, gives each turn its session context: called once
	// per turn, after TurnModels; the agent then makes no context manager or
	// memory of its own, never closes what it is given, calls Release once
	// when the turn has ended, and runs no compaction after the turn (the
	// host does). Nil means the agent's own, per session, kept until
	// ForgetSession or Close. An error ends the run with OutcomeError.
	Session func(ctx context.Context, s hook.Scope) (Session, error)
	// Memory is the agent's optional long-term memory.
	Memory hook.Memory
	// Context are the options of every session's context manager
	// (ctxengine.New), after the agent's own (its logger, the summary model
	// from Models, the compaction report to OnCompaction), so they win.
	Context []ctxengine.Option
	// Limits are the loop's limits; a zero field takes its default.
	Limits hook.Limits
	// TurnLimits, when set, replaces Limits: called at the start of each turn,
	// and again before each tool batch for ToolTimeout, so a host's settings
	// are read live.
	TurnLimits func(ctx context.Context, s hook.Scope) hook.Limits
	// Temperature, when set, is sent with every request to a model that is
	// not a CLI.
	Temperature *float64
	// RepeatHint, when set, adds steering for a repeated batch of the named
	// tools to the engine's own.
	RepeatHint func(toolNames []string) string
	// Served gives a CLI model the agent's tools for each of its turns (see
	// hook.ServedTools); nil means a CLI model runs without them.
	Served hook.ServedTools
	// ToolContext, when set, gives each tool call the host's own values: it
	// receives the call's context (the turn's, with the tool timeout and the
	// call on it, hook.ToolCallFrom) and the call as BeforeTool left it, and
	// returns the context the gate and the tool run on (nil keeps the one it
	// was given). The callbacks see the turn's context.
	ToolContext func(ctx context.Context, call hook.ToolCall) context.Context
	// AsyncResult, when set, receives the late result of a tool that
	// finishes in the background (toolkit.AsyncExecutor), with the call's
	// context and the call, on the tool's goroutine, possibly after the turn
	// ended; the host delivers it (as a new Run, say). Nil runs such tools
	// synchronously. A task (hook.TaskRegistrar) is the alternative that
	// also lists and cancels the work.
	AsyncResult func(ctx context.Context, call hook.ToolCall, result *toolkit.Result)

	// Project groups the agent's accounting records; empty is the default.
	Project string
	// Clock is the agent's clock; nil means the real one.
	Clock clock.Clock
	// Logger receives the agent's log lines; nil discards them.
	Logger *slog.Logger
	// Redactor is applied to the error text and content the agent logs.
	Redactor hook.Redactor
	// Truncator shortens the request content logged when LogContent is set;
	// nil means hook.Truncate.
	Truncator hook.Truncator
	// LogContent, when set and true, lets the full request be logged (at
	// debug); it is asked at each line, so a host may change it live. Nil is
	// off.
	LogContent func() bool

	// Plugins are the agent's callbacks and event consumers, each kind in
	// the order given.
	Plugins []hook.Plugin
}

// TurnModels are the models of one turn, as Spec.TurnModels chooses them.
type TurnModels struct {
	// Chain is the turn's pinned chain; nil for none (the turn fails).
	Chain *models.Chain
	// Limits, when set, are the turn's own limits (max tokens, context
	// window, no tools) in place of Chain.Limits(), its first model's. Each
	// call still uses its own model's limits for max tokens, thinking and
	// vision.
	Limits *modelspec.Limits
}

// Session is a turn's session context as the host supplies it
// (Spec.Session).
type Session struct {
	// Context is the session's context manager. Required. A manager the host
	// builds reports its compaction passes to the turn when it is given
	// ctxengine.WithCompactionStartFunc(CompactionStart).
	Context ctxengine.ContextManager
	// Memory is the session's memory; nil for none.
	Memory hook.MemorySession
	// Release, when set, is called once when the turn has ended: after its
	// TurnFinished, before the session's next turn starts.
	Release func()
	// Assemble, when set, amends each request the engine builds before it
	// is assembled (the turn's first and every dispatch's): to name the
	// conversation (Channel, ChatID) or change the layers.
	Assemble func(ctx context.Context, req *ctxengine.AssembleRequest)
}

// The errors New refuses a spec with.
var (
	// ErrInvalidSpec is wrapped by every error New returns.
	ErrInvalidSpec = errors.New("invalid agent")
	// ErrClosed ends a run given to an agent that is closed.
	ErrClosed = errors.New("agent closed")
)

// validate checks spec as New requires: an id; unless the host chooses the
// turns' models, a model set whose chat purpose has a model (its own or by a
// fallback rule); and no two tools offered under one name.
func (s *Spec) validate() error {
	if s.ID == "" {
		return fmt.Errorf("%w: no id", ErrInvalidSpec)
	}
	if s.TurnModels != nil {
		return s.validateTools()
	}
	if s.Models == nil {
		return fmt.Errorf("%w %q: no models", ErrInvalidSpec, s.ID)
	}
	if len(s.Models.Begin(models.Chat).Candidates()) == 0 {
		return fmt.Errorf("%w %q: purpose %q has no models and no fallback rule: %w",
			ErrInvalidSpec, s.ID, models.Chat, hook.ErrNoModelAvailable)
	}
	return s.validateTools()
}

// validateTools refuses two tools offered under one name.
func (s *Spec) validateTools() error {
	if s.Tools != nil {
		snap := s.Tools.Snapshot()
		seen := map[string]bool{}
		for _, info := range append(snap.Listed(), snap.Hidden()...) {
			if seen[info.Published] {
				return fmt.Errorf("%w %q: two tools are offered as %q", ErrInvalidSpec, s.ID, info.Published)
			}
			seen[info.Published] = true
		}
	}
	return nil
}
