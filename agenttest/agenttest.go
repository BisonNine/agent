// Package agenttest is the conformance suite of the agent.Agent interface:
// the behaviour any implementation must have, driven by a scripted model
// (models/modelstest) and an in-memory session store. An implementation
// passes it before a host is given it:
//
//	func TestConformance(t *testing.T) {
//		agenttest.Run(t, func(f agenttest.Fixture) agent.Agent {
//			a, err := agent.New("alice", f.Options()...)
//			if err != nil {
//				t.Fatal(err)
//			}
//			return a
//		})
//	}
package agenttest

import (
	"context"
	"testing"
	"time"

	"go.bison9.com/agent"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

// The fixture's models: Primary is tried first, Backup when it fails. Their
// wire ids are what Provider is scripted by.
const (
	Primary     = "primary"
	PrimaryWire = "primary-wire"
	Backup      = "backup"
	BackupWire  = "backup-wire"
)

// Fixture is what the suite gives an implementation to build its agent
// from, for one test. The agent must use every field: its chat models are
// Models' default set (Primary, then Backup); its tools Tools, read for each
// model call; its sessions Sessions; its limits Limits; and Plugins
// registered in order (they carry the test's callbacks and observers).
type Fixture struct {
	// Provider answers both models, by wire id, as the test scripts it.
	Provider *modelstest.Provider
	Models   *models.Catalog
	Tools    *toolkit.Catalog
	Sessions session.SessionStore
	Limits   hook.Limits
	Plugins  []hook.Plugin
	// Unqueued: the agent must be built not to queue runs of one session
	// (agent.WithUnqueued); set by RunUnqueued.
	Unqueued bool
}

// Options are the fixture as the root package's options.
func (f Fixture) Options() []agent.Option {
	opts := make([]agent.Option, 0, 4+len(f.Plugins))
	opts = append(opts,
		agent.WithModels(f.Models),
		agent.WithTools(f.Tools),
		agent.WithSessions(f.Sessions),
		agent.WithLimits(f.Limits),
	)
	for _, p := range f.Plugins {
		opts = append(opts, agent.WithPlugin(p))
	}
	if f.Unqueued {
		opts = append(opts, agent.WithUnqueued())
	}
	return opts
}

// Factory builds the agent under test from a fixture.
type Factory func(f Fixture) agent.Agent

// newFixture is a fixture with both models and no tools.
func newFixture(tb testing.TB, plugins ...hook.Plugin) Fixture {
	tb.Helper()
	p := modelstest.New()
	cat := models.New()
	if err := cat.AddProvider(modelspec.Provider{Name: "api", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
		tb.Fatal(err)
	}
	for i, m := range []modelspec.Model{
		{ModelName: Primary, Model: PrimaryWire, Provider: "api", MaxTokens: 4096},
		{ModelName: Backup, Model: BackupWire, Provider: "api", MaxTokens: 4096},
	} {
		if err := cat.Add(m, models.For(models.Chat, i+1)); err != nil {
			tb.Fatal(err)
		}
		if err := cat.RegisterClient(m.ModelName, p); err != nil {
			tb.Fatal(err)
		}
	}
	return Fixture{
		Provider: p,
		Models:   cat,
		Tools:    toolkit.NewRegistry(),
		Sessions: session.NewMemory(),
		Limits:   hook.Limits{CancelGrace: 50 * time.Millisecond, Retries: -1},
		Plugins:  plugins,
	}
}

// build makes the agent and closes it when the test ends.
func build(tb testing.TB, factory Factory, f Fixture) agent.Agent {
	tb.Helper()
	a := factory(f)
	if a == nil {
		tb.Fatal("the factory returned no agent")
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})
	return a
}

// addTool registers a tool that runs fn.
func addTool(tb testing.TB, f Fixture, name string, fn func(ctx context.Context, args map[string]any) (*toolspec.Result, error)) {
	tb.Helper()
	err := f.Tools.Add(toolspec.ToolDefinition{
		Name:        name,
		Description: "Test tool " + name + ".",
		Handler:     func(call *toolspec.ToolCall) (*toolspec.Result, error) { return fn(call.Ctx, call.Args) },
	})
	if err != nil {
		tb.Fatal(err)
	}
}

// Run runs the suite against the agents factory builds, one per test.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	run(t, factory, false)
}

// RunUnqueued runs the suite against an agent built not to queue runs of one
// session (Fixture.Unqueued): the cases about the queue are left out, and one
// checks that two runs of a session overlap.
func RunUnqueued(t *testing.T, factory Factory) {
	t.Helper()
	run(t, func(f Fixture) agent.Agent {
		f.Unqueued = true
		return factory(f)
	}, true)
}

func run(t *testing.T, factory Factory, unqueued bool) {
	t.Helper()
	queued := map[string]bool{"InterruptDropsQueue": true}
	tests := []struct {
		name string
		fn   func(t testing.TB, factory Factory)
	}{
		{"OutcomeOK", testOutcomeOK},
		{"OutcomeEmpty", testOutcomeEmpty},
		{"OutcomeError", testOutcomeError},
		{"Fallback", testFallback},
		{"ToolCalls", testToolCalls},
		{"ToolError", testToolError},
		{"ToolPanic", testToolPanic},
		{"ToolRefused", testToolRefused},
		{"ToolRemovedMidTurn", testToolRemovedMidTurn},
		{"InterruptRunningTurn", testInterruptRunningTurn},
		{"InterruptDropsQueue", testInterruptDropsQueue},
		{"InterruptLeavesTasks", testInterruptLeavesTasks},
		{"RunInterruptLeavesTasks", testRunInterruptLeavesTasks},
		{"RunCancelStopsTasks", testRunCancelStopsTasks},
		{"AgentCancel", testAgentCancel},
		{"CancelWins", testCancelWins},
		{"CancelWinsRunInterrupt", testCancelWinsRunInterrupt},
		{"CancelWinsRunCancel", testCancelWinsRunCancel},
		{"AbandonedTool", testAbandonedTool},
		{"CallbackPanic", testCallbackPanic},
		{"ObserverPanic", testObserverPanic},
		{"SessionsRunConcurrently", testSessionsConcurrent},
		{"TurnIDAndModel", testTurnIDAndModel},
		{"Close", testClose},
		{"WithoutTurnID", testWithoutTurnID},
		{"Unqueued", testUnqueued},
	}
	for _, tt := range tests {
		if (unqueued && queued[tt.name]) || (!unqueued && tt.name == "Unqueued") {
			continue
		}
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, factory) })
	}
}
