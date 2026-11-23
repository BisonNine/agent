package loop

import (
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"sync"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// agentEnv is the engine's services for one turn of an Agent, made from the
// SDK packages: tool definitions and execution from the agent's toolkit
// registry (with discovery), requests from the session's ctxengine manager,
// the system prompt from the agent's instruction, and recall from its memory.
type agentEnv struct {
	a     *Agent
	s     *sessionState
	scope hook.Scope
	// text is the turn's message as BeforeTurn left it, what memory recalls
	// for.
	text    string
	noTools bool
	// cm and mem are the turn's session context; amend the host's change to
	// each request (Session.Assemble).
	cm    ctxengine.ContextManager
	mem   hook.MemorySession
	amend func(ctx context.Context, req *ctxengine.AssembleRequest)
	// compact is the host's manager for a forced compaction
	// (Session.Compaction); nil means cm.
	compact func() (ctxengine.ContextManager, func())
	// served is the served-tools prompt of a CLI model's turn.
	served string

	mu sync.Mutex
	// snap is the tool snapshot of the latest model call.
	snap *toolkit.Snapshot
}

var _ turnEnv = (*agentEnv)(nil)

// snapshot takes a new snapshot of the agent's tools; nil without tools.
func (e *agentEnv) snapshot() *toolkit.Snapshot {
	if e.a.spec.Tools == nil {
		return nil
	}
	snap := e.a.spec.Tools.Snapshot()
	e.mu.Lock()
	e.snap = snap
	e.mu.Unlock()
	return snap
}

// toolDefs is the agent's tool list for a model call, from a new snapshot:
// every tool offered, sorted by name, the hidden ones deferred (spawnllm
// sends a deferred tool once a tool result references it, natively where the
// provider can), so the list stays the same while the tools do.
func (e *agentEnv) toolDefs() []spawnllm.ToolDefinition {
	snap := e.snapshot()
	if snap == nil {
		return nil
	}
	return providerDefs(discovery.ToolDefs(snap, nil, true).Send)
}

// gatedRegistry is a registry that can run a call through a gate.
type gatedRegistry interface {
	ExecuteGated(ctx context.Context, name string, args map[string]any, cb toolkit.AsyncCallback, gate toolkit.Gate) *toolkit.Result
}

// executeTool runs a call through the agent's gate, against the registry's
// current contents (a tool removed since the model call is not found), on
// the call's context with the call on it (hook.WithToolCall) and the host's
// values (Spec.ToolContext). With Spec.AsyncResult a background tool's late
// result goes there (deliverLate).
func (e *agentEnv) executeTool(ctx context.Context, call hook.ToolCall) *toolkit.Result {
	ctx = hook.WithToolCall(ctx, call)
	if fn := e.a.spec.ToolContext; fn != nil {
		if c := fn(ctx, call); c != nil {
			ctx = c
		}
	}
	tools := e.a.spec.Tools
	if tools == nil {
		return toolkit.ErrorResult("tool not found: " + call.Name).WithError(toolkit.ErrToolNotFound)
	}
	gate := e.a.spec.Gate
	g, resolves := tools.(gatedRegistry)
	// info is the tool the call resolved to, as the gate sees it. A registry
	// that cannot resolve a call for a gate gets the called name for both.
	info := toolkit.Info{Name: call.Name, Published: call.Name}
	var cb toolkit.AsyncCallback
	if sink := e.a.spec.AsyncResult; sink != nil {
		cb = e.deliverLate(sink, context.WithoutCancel(ctx), copyCall(call), &info)
		if resolves {
			// Record the resolved tool; the host's gate, if any, decides.
			hostGate := gate
			gate = func(ctx context.Context, i toolkit.Info) *toolkit.Result {
				info = i
				if hostGate == nil {
					return nil
				}
				return hostGate(ctx, i)
			}
		}
	}
	if gate == nil {
		return tools.Execute(ctx, call.Name, call.Arguments, cb)
	}
	if resolves {
		return g.ExecuteGated(ctx, call.Name, call.Arguments, cb, gate)
	}
	if refused := gate(ctx, info); refused != nil {
		return refused
	}
	return tools.Execute(ctx, call.Name, call.Arguments, cb)
}

// deliverLate is the callback a background tool reports its late result
// to: it hands sink the context the call ran on without its cancellation
// (ctx), the call as it ran and the resolved tool (*info, set by the gate
// before the tool starts, so before any callback). The context the tool
// hands its callback carries none of the host's values and is dropped. It
// takes no lock of the agent's and works after the turn ended and after
// Close: the result is delivered either way. A panic in sink is recovered
// and logged, since it runs on the tool's goroutine.
func (e *agentEnv) deliverLate(sink func(context.Context, hook.ToolCall, toolkit.Info, *toolkit.Result), ctx context.Context, call hook.ToolCall, info *toolkit.Info) toolkit.AsyncCallback {
	a := e.a
	return func(_ context.Context, r *toolkit.Result) {
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("panic in async result sink", "component", "agent", "agent_id", a.id,
					"tool", call.Name, "panic", fmt.Sprintf("%v", p), "stack", string(debug.Stack()))
			}
		}()
		sink(ctx, call, *info, r)
	}
}

// copyCall returns call with its own copy of the arguments, so a late
// result's call is the one that ran whatever later changes the original.
func copyCall(call hook.ToolCall) hook.ToolCall {
	call.Arguments = maps.Clone(call.Arguments)
	return call
}

// turnMessage is the agent's last BeforeTurn: it keeps the recall text as
// the BeforeTurn callbacks left it (hook.Turn.RecallText).
func (e *agentEnv) turnMessage(_ context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
	e.text = turn.RecallText
	return nil, nil //nolint:nilnil // (nil, nil): the turn goes on
}

// assembleRequest is the request for a dispatch that sends defs over
// history.
func (e *agentEnv) assembleRequest(ctx context.Context, defs []spawnllm.ToolDefinition, history []spawnllm.Message) ctxengine.AssembleRequest {
	req := ctxengine.AssembleRequest{
		ToolDefinitionTokens: ctxengine.EstimateToolDefinitionTokens(e.inContext(defs, history)),
		Layers:               e.layers(ctx),
		Injections:           e.recall(ctx),
	}
	if e.amend != nil {
		e.amend(ctx, &req)
	}
	return req
}

// firstRequest is the request for the turn's first assembly: the agent's
// tools now, over the stored history when a deferred tool makes it matter.
func (e *agentEnv) firstRequest(ctx context.Context) ctxengine.AssembleRequest {
	var defs []spawnllm.ToolDefinition
	if !e.noTools {
		defs = e.toolDefs()
	}
	var history []spawnllm.Message
	if slices.ContainsFunc(defs, func(d spawnllm.ToolDefinition) bool { return d.Deferred }) {
		history = e.a.sessions.GetHistory(e.s.key)
	}
	return e.assembleRequest(ctx, defs, history)
}

// inContext is the part of defs a request over history puts in the model's
// context: the listed tools and the hidden ones history's tool results have
// revealed. None when defs is empty.
func (e *agentEnv) inContext(defs []spawnllm.ToolDefinition, history []spawnllm.Message) []spawnllm.ToolDefinition {
	if len(defs) == 0 {
		return nil
	}
	e.mu.Lock()
	snap := e.snap
	e.mu.Unlock()
	if snap == nil {
		return defs
	}
	return providerDefs(discovery.ToolDefs(snap, revealedIn(history), true).InContext)
}

// layers are the system prompt: the instruction, the host's layers, and the
// served-tools prompt of a CLI model's turn after the summary.
func (e *agentEnv) layers(ctx context.Context) []ctxengine.Layer {
	var layers []ctxengine.Layer
	if e.a.spec.Instruction != "" {
		layers = append(layers, ctxengine.Layer{Name: "instruction", Text: e.a.spec.Instruction})
	}
	if fn := e.a.spec.InstructionLayers; fn != nil {
		layers = append(layers, fn(ctx)...)
	}
	if e.served != "" {
		layers = append(layers, ctxengine.Layer{Name: "served_tools", Text: e.served, AfterSummary: true})
	}
	return layers
}

// recall is what the session's memory places in the request for the turn.
func (e *agentEnv) recall(ctx context.Context) []ctxengine.Injection {
	if e.mem == nil {
		return nil
	}
	rec := e.mem.Recall(ctx, e.text)
	out := make([]ctxengine.Injection, 0, len(rec))
	for _, r := range rec {
		p := ctxengine.PlaceSystemStable
		if r.Placement == hook.PlaceCurrentUser {
			p = ctxengine.PlaceCurrentUser
		}
		out = append(out, ctxengine.Injection{Placement: p, Text: r.Text})
	}
	return out
}

// compressionManager is the session's context manager; there is nothing to
// release.
func (e *agentEnv) compressionManager() (ctxengine.ContextManager, func()) {
	if e.compact != nil {
		return e.compact()
	}
	return e.cm, func() {}
}

// sessionSummary is the session's compacted summary.
func (e *agentEnv) sessionSummary() string { return e.a.sessions.GetSummary(e.s.key) }

// providerDefs copies discovery's definitions into the provider type.
func providerDefs(in []discovery.Def) []spawnllm.ToolDefinition {
	out := make([]spawnllm.ToolDefinition, 0, len(in))
	for _, d := range in {
		out = append(out, spawnllm.ToolDefinition{
			Type: "function",
			Function: spawnllm.ToolFunctionDefinition{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
			Deferred:   d.Deferred,
			ToolSearch: d.ToolSearch,
		})
	}
	return out
}

// revealedIn is what history's tool results have revealed. It is rebuilt
// from the messages a request carries, so it follows a restart and a
// compaction that drops messages.
func revealedIn(history []spawnllm.Message) *discovery.Session {
	refs := make([][]string, 0, len(history))
	for _, m := range history {
		if len(m.ToolReferences) > 0 {
			refs = append(refs, m.ToolReferences)
		}
	}
	return discovery.NewSession(refs...)
}

// turnMemoryOf is the engine's view of a session's memory for a turn on ctx;
// nil for none.
func turnMemoryOf(ctx context.Context, ms hook.MemorySession) turnMemory {
	if ms == nil {
		return nil
	}
	return memoryOfTurn{ctx: ctx, ms: ms}
}

// memoryOfTurn adapts a hook.MemorySession to the engine's turnMemory.
type memoryOfTurn struct {
	ctx context.Context //nolint:containedctx // the turn's context, for the calls the engine makes without one
	ms  hook.MemorySession
}

func (m memoryOfTurn) Observe(ctx context.Context, seq int64, role, text string) {
	m.ms.Observe(ctx, seq, role, text)
}

func (m memoryOfTurn) RecordToolUse(names ...string) { m.ms.ToolsUsed(m.ctx, names...) }
