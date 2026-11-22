package hook

import (
	"context"
	"log/slog"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/toolkit"
)

// Scope is who a turn, and every call made inside it, is for. The engine
// puts it on the turn's context once with WithScope; logging, accounting,
// tools and an MCP host read it from there with ScopeOf. Values only a host
// knows (the chat a reply goes to, who is waiting on the turn) stay in the
// host's own context keys.
type Scope struct {
	// AgentID is the agent's id as normalised by ident.NormalizeID.
	AgentID string
	// AgentName is the agent's friendly name, for logs and records; it may
	// be empty.
	AgentName string
	// Session is the session the turn belongs to.
	Session string
	// TurnID names one turn, so every log line and record of it can be
	// pulled together. Empty when no turn is involved; a background task's
	// calls carry the id of the turn that started the task.
	TurnID string
	// Depth is the sub-agent depth: 0 for an agent talked to directly, one
	// more for each agent that started another.
	Depth int
	// Project groups accounting records; empty is the default project.
	Project string
	// Task is the id of the task the context runs in (TaskScope); empty in
	// a turn no task runs. A turn started on a task's context (a sub-agent's
	// turn) keeps it: the engine copies Task into the turn's scope, so the
	// tasks that turn starts are that task's children.
	Task string
}

// Identity is the accounting identity of calls made in the scope.
func (s Scope) Identity() accounting.Identity {
	return accounting.Identity{Agent: s.AgentID, Name: s.AgentName, Project: s.Project}
}

// LogValue renders the scope as a group of the fields that are set: agent,
// agent_name, session, turn, depth (when not 0) and project. The engine
// derives each turn's logger with it once, so call sites do not repeat them.
func (s Scope) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 7)
	add := func(key, v string) {
		if v != "" {
			attrs = append(attrs, slog.String(key, v))
		}
	}
	add("agent", s.AgentID)
	add("agent_name", s.AgentName)
	add("session", s.Session)
	add("turn", s.TurnID)
	if s.Depth != 0 {
		attrs = append(attrs, slog.Int("depth", s.Depth))
	}
	add("project", s.Project)
	add("task", s.Task)
	return slog.GroupValue(attrs...)
}

type scopeKey struct{}

// WithScope returns a child of ctx carrying s. It also sets toolkit's agent
// id (toolkit.WithAgentID) and session key (toolkit.WithSessionKey) from s,
// so a tool reads them through toolkit as before; tools never import this
// package. The engine is the one writer: a host that sets those toolkit
// values itself after WithScope makes them disagree with the scope.
func WithScope(ctx context.Context, s Scope) context.Context {
	ctx = toolkit.WithAgentID(ctx, s.AgentID)
	ctx = toolkit.WithSessionKey(ctx, s.Session)
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeOf returns the scope on ctx and whether there is one.
func ScopeOf(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(scopeKey{}).(Scope)
	return s, ok
}
