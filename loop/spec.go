package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/ident"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/toolkit"
)

// Spec is an agent's settings: everything New builds an Agent from. The root
// package's options (agent.With…) each set one field; a host may also write
// its own option over a field the root has no option for. The zero value of
// every field but ID and Models is usable.
type Spec struct {
	// ID is the agent's id (ident.ValidID); it is normalised (ident.NormalizeID)
	// and names the agent in its scope, its accounting and its tasks.
	ID string
	// Name is the agent's friendly name, for logs, accounting and notices.
	Name string
	// Description says what the agent is for; the agent does not read it.
	Description string

	// Instruction is the first block of the system prompt.
	Instruction string
	// InstructionLayers, when set, returns the system-prompt blocks that
	// follow Instruction, for each dispatch. Its context is the dispatch's:
	// the turn's Scope (hook.ScopeOf) and the values of the context given to
	// Run.
	InstructionLayers func(ctx context.Context) []ctxengine.Layer

	// Models are the agent's models: its turns use purpose models.Chat, its
	// compaction summaries models.Summarize. Required.
	Models *models.Set
	// Tools are the agent's tools, read once per model call; nil means none.
	Tools toolkit.Registry
	// Allow, when set, is asked again when a tool is executed, so a call of a
	// tool the agent is not granted is refused (defence in depth). Gate, when
	// set, replaces it: it runs on the exact tool executed, and a non-nil
	// result refuses the call.
	Allow toolkit.AllowChecker
	Gate  toolkit.Gate

	// Sessions is the session store; nil means an in-memory store
	// (session.NewMemory), which keeps nothing across a restart.
	Sessions session.SessionStore
	// Memory is the agent's optional long-term memory.
	Memory hook.Memory
	// Context are the options of every session's context manager
	// (ctxengine.New), after the agent's own (its logger, the summary model
	// from Models, the compaction report to OnCompaction), so they win.
	Context []ctxengine.Option
	// Limits are the loop's limits; a zero field takes its default.
	Limits hook.Limits
	// Temperature, when set, is sent with every request to a model that is
	// not a CLI.
	Temperature *float64
	// RepeatHint, when set, adds steering for a repeated batch of the named
	// tools to the engine's own.
	RepeatHint func(toolNames []string) string
	// Served gives a CLI model the agent's tools for each of its turns (see
	// hook.ServedTools); nil means a CLI model runs without them.
	Served hook.ServedTools

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
	// LogContent lets the full request be logged (at debug). Off by default.
	LogContent bool

	// Plugins are the agent's callbacks and event consumers, each kind in
	// the order given.
	Plugins []hook.Plugin
}

// The errors New refuses a spec with.
var (
	// ErrInvalidSpec is wrapped by every error New returns.
	ErrInvalidSpec = errors.New("invalid agent")
	// ErrClosed ends a run given to an agent that is closed.
	ErrClosed = errors.New("agent closed")
)

// validate checks spec as New requires: a valid id, a model set whose chat
// purpose has a model (its own or by a fallback rule), and no two tools
// offered under one name.
func (s *Spec) validate() error {
	if !ident.ValidID(s.ID) {
		return fmt.Errorf("%w: id %q is not a valid agent id", ErrInvalidSpec, s.ID)
	}
	if s.Models == nil {
		return fmt.Errorf("%w %q: no models", ErrInvalidSpec, s.ID)
	}
	if len(s.Models.Begin(models.Chat).Candidates()) == 0 {
		return fmt.Errorf("%w %q: purpose %q has no models and no fallback rule: %w",
			ErrInvalidSpec, s.ID, models.Chat, hook.ErrNoModelAvailable)
	}
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
