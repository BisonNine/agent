// Package agent is the front door of the Bison9 agent SDK: the Agent
// interface a host holds, New, the options everything is supplied through,
// and aliases of the types a host needs to talk to an agent, so ordinary use
// imports this one package.
//
//	alice, err := agent.New("alice",
//		agent.WithInstruction("You are Alice, a support agent."),
//		agent.WithModels(catalog),
//		agent.WithTools(registry),
//	)
//	run := alice.Run(ctx, "session-42", agent.UserText("Where is my order?"))
//	for ev, err := range run.Events() { /* stream to the user */ }
//
// It holds no loop, no provider code and no session logic, and starts no
// goroutine: the agent is go.bison9.com/agent/loop, the contract (callbacks,
// events, the run handle) go.bison9.com/agent/hook.
package agent

import (
	"context"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/loop"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/toolkit"
)

// Agent is everything a host needs to hold a conversation and stop it. A
// host stores and calls this interface, never the concrete agent, so another
// implementation can be put behind New. The event stream and the callback
// order (package hook) are part of the contract; go.bison9.com/agent/agenttest
// is the suite an implementation must pass.
type Agent interface {
	// ID is the agent's id, normalised.
	ID() string
	// Name is the agent's friendly name.
	Name() string
	// Run starts a turn of session answering msg and returns its handle at
	// once; on a busy session the run waits behind the running turn.
	Run(ctx context.Context, session string, msg Message, opts ...RunOption) Run
	// Interrupt stops session's running turn and drops its queued runs; the
	// agent's tasks keep running.
	Interrupt(session string) (running bool, dropped int)
	// Cancel stops every turn, queue and task of the agent with cause.
	Cancel(cause error)
	// Close shuts the agent down: running turns may finish until ctx ends,
	// then whatever is left is stopped.
	Close(ctx context.Context) error
}

// Optional capabilities, found by type assertion. The agent New builds has
// all three.
type (
	// TaskController lists and cancels the agent's tasks.
	TaskController = hook.TaskController
	// ToolHolder gives the agent's tools.
	ToolHolder interface{ Tools() toolkit.Registry }
	// ModelHolder gives the agent's models.
	ModelHolder interface{ Models() *models.Set }
)

// New builds the agent id names with opts applied in order. It fails
// (wrapping ErrInvalidSpec) on an invalid id, missing models, a chat purpose
// with no model, or two tools offered under one name.
func New(id string, opts ...Option) (Agent, error) {
	spec := loop.Spec{ID: id}
	for _, opt := range opts {
		if opt != nil {
			opt(&spec)
		}
	}
	a, err := loop.New(spec)
	if err != nil {
		return nil, err
	}
	return a, nil
}

var (
	_ Agent          = (*loop.Agent)(nil)
	_ TaskController = (*loop.Agent)(nil)
	_ ToolHolder     = (*loop.Agent)(nil)
	_ ModelHolder    = (*loop.Agent)(nil)
)
