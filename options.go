/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package agent

import (
	"context"
	"log/slog"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/toolkit"
)

// Option sets one of the agent's settings (loop.Spec). A host may write its
// own over a field no option here covers.
type Option func(*loop.Spec)

// WithName sets the agent's friendly name: logs, accounting, notices.
func WithName(name string) Option { return func(s *loop.Spec) { s.Name = name } }

// WithDescription says what the agent is for.
func WithDescription(d string) Option { return func(s *loop.Spec) { s.Description = d } }

// WithInstruction sets the first block of the system prompt.
func WithInstruction(text string) Option { return func(s *loop.Spec) { s.Instruction = text } }

// WithInstructionLayers sets the system-prompt blocks that follow the
// instruction, asked for each dispatch with the dispatch's context.
func WithInstructionLayers(fn func(ctx context.Context) []ctxengine.Layer) Option {
	return func(s *loop.Spec) { s.InstructionLayers = fn }
}

// WithModels sets the agent's models: a catalogue's default set, or a set.
func WithModels[M *models.Catalog | *models.Set](m M) Option {
	return func(s *loop.Spec) {
		switch v := any(m).(type) {
		case *models.Catalog:
			if v != nil {
				s.Models = v.Default()
			}
		case *models.Set:
			if v != nil {
				s.Models = v
			}
		}
	}
}

// WithTools sets the agent's tools, read once per model call.
func WithTools(r toolkit.Registry) Option { return func(s *loop.Spec) { s.Tools = r } }

// WithAllow sets the checker a tool call is refused by when the agent is not
// granted the tool.
func WithAllow(a toolkit.AllowChecker) Option { return func(s *loop.Spec) { s.Allow = a } }

// WithSessions sets the session store (in memory by default).
func WithSessions(st session.SessionStore) Option { return func(s *loop.Spec) { s.Sessions = st } }

// WithMemory sets the agent's long-term memory.
func WithMemory(m Memory) Option { return func(s *loop.Spec) { s.Memory = m } }

// WithContext adds options of every session's context manager.
func WithContext(opts ...ctxengine.Option) Option {
	return func(s *loop.Spec) { s.Context = append(s.Context, opts...) }
}

// WithServedTools gives a CLI model the agent's tools for each of its turns
// (for instance mcphost.Served).
func WithServedTools(st ServedTools) Option { return func(s *loop.Spec) { s.Served = st } }

// WithUnqueued tells the agent the host serialises each session's runs: Run
// never queues (see loop.Spec.Unqueued).
func WithUnqueued() Option { return func(s *loop.Spec) { s.Unqueued = true } }

// WithLimits sets the loop's limits.
func WithLimits(l Limits) Option { return func(s *loop.Spec) { s.Limits = l } }

// WithProject sets the project the agent's accounting records go to.
func WithProject(p string) Option { return func(s *loop.Spec) { s.Project = p } }

// WithTurnProject sets how each turn chooses its project, asked as the turn
// is set up and before each model call: a non-empty result replaces the
// agent's project from then on, "" keeps it, and one
// accounting.CheckProject refuses ends the turn with an error (see
// loop.Spec.TurnProject).
func WithTurnProject(fn func(ctx context.Context, s Scope) string) Option {
	return func(s *loop.Spec) { s.TurnProject = fn }
}

// WithClock sets the agent's clock.
func WithClock(c clock.Clock) Option { return func(s *loop.Spec) { s.Clock = c } }

// WithLogger sets where the agent logs; nothing is logged without one.
func WithLogger(l *slog.Logger) Option { return func(s *loop.Spec) { s.Logger = l } }

// WithLogContent lets the agent log the full request (at debug). Off by
// default.
func WithLogContent(on bool) Option {
	return func(s *loop.Spec) { s.LogContent = func() bool { return on } }
}

// WithRedactor sets what the agent applies to the error text and content it
// logs.
func WithRedactor(r Redactor) Option { return func(s *loop.Spec) { s.Redactor = r } }

// WithPlugin registers a named bundle of callbacks. Each callback option, and
// each plugin, may be given more than once; callbacks of one kind run in the
// order their options were given.
func WithPlugin(p Plugin) Option { return func(s *loop.Spec) { s.Plugins = append(s.Plugins, p) } }

// WithBeforeTurn registers a BeforeTurn callback.
func WithBeforeTurn(fn BeforeTurnFunc) Option { return WithPlugin(Plugin{BeforeTurn: fn}) }

// WithAfterTurn registers an AfterTurn callback.
func WithAfterTurn(fn AfterTurnFunc) Option { return WithPlugin(Plugin{AfterTurn: fn}) }

// WithBeforeModel registers a BeforeModel callback.
func WithBeforeModel(fn BeforeModelFunc) Option { return WithPlugin(Plugin{BeforeModel: fn}) }

// WithAfterModel registers an AfterModel callback.
func WithAfterModel(fn AfterModelFunc) Option { return WithPlugin(Plugin{AfterModel: fn}) }

// WithOnModelError registers an OnModelError callback.
func WithOnModelError(fn OnModelErrorFunc) Option { return WithPlugin(Plugin{OnModelError: fn}) }

// WithBeforeTool registers a BeforeTool callback.
func WithBeforeTool(fn BeforeToolFunc) Option { return WithPlugin(Plugin{BeforeTool: fn}) }

// WithAfterTool registers an AfterTool callback.
func WithAfterTool(fn AfterToolFunc) Option { return WithPlugin(Plugin{AfterTool: fn}) }

// WithOnToolError registers an OnToolError callback.
func WithOnToolError(fn OnToolErrorFunc) Option { return WithPlugin(Plugin{OnToolError: fn}) }

// WithOnFallback registers an OnFallback observer.
func WithOnFallback(fn OnFallbackFunc) Option { return WithPlugin(Plugin{OnFallback: fn}) }

// WithOnCompaction registers an OnCompaction observer.
func WithOnCompaction(fn OnCompactionFunc) Option { return WithPlugin(Plugin{OnCompaction: fn}) }

// WithOnTextDelta registers an OnTextDelta observer; with one, the agent asks
// its models for partial text.
func WithOnTextDelta(fn OnTextDeltaFunc) Option { return WithPlugin(Plugin{OnTextDelta: fn}) }

// WithOnInterrupt registers an OnInterrupt observer.
func WithOnInterrupt(fn OnInterruptFunc) Option { return WithPlugin(Plugin{OnInterrupt: fn}) }

// WithOnEvent registers a consumer of every event of the agent.
func WithOnEvent(fn OnEventFunc) Option { return WithPlugin(Plugin{OnEvent: fn}) }
