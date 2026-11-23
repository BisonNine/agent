package loop

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// turnHooks are a turn's callbacks and event consumers, each kind in the
// order it was registered, each with the name of the plugin that registered
// it. The agent builds one per turn and the engine calls it; the engine never
// reaches a host's behaviour any other way than through these.
//
// Panics follow the hook package's rules ("Panics"): an observer (OnFallback,
// OnCompaction, OnTextDelta, OnInterrupt, an event consumer) is logged and
// skipped; a turn or model step (BeforeTurn, BeforeLoop, BeforeModel,
// AfterModel, OnModelError, DescribeImages) ends the turn with a
// *hook.PanicError; an AfterTurn panic makes the result an error that
// stores nothing; a tool callback fails its call only.
type turnHooks struct {
	beforeTurn     []named[hook.BeforeTurnFunc]
	beforeLoop     []named[hook.BeforeLoopFunc]
	afterTurn      []named[hook.AfterTurnFunc]
	beforeModel    []named[hook.BeforeModelFunc]
	afterModel     []named[hook.AfterModelFunc]
	onModelError   []named[hook.OnModelErrorFunc]
	onFallback     []named[hook.OnFallbackFunc]
	beforeTool     []named[hook.BeforeToolFunc]
	afterTool      []named[hook.AfterToolFunc]
	onToolError    []named[hook.OnToolErrorFunc]
	onCompaction   []named[hook.OnCompactionFunc]
	onTextDelta    []named[hook.OnTextDeltaFunc]
	onInterrupt    []named[hook.OnInterruptFunc]
	describeImages []named[hook.DescribeImagesFunc]
	onEvent        []named[hook.OnEventFunc]

	// log receives the panics recovered from observers.
	log *slog.Logger
}

// named is a callback with the name of the plugin that registered it.
type named[F any] struct {
	plugin string
	fn     F
}

// addTo appends fn to list, named for plugin, when it is set.
func addTo[F any](list []named[F], plugin string, fn F, set bool) []named[F] {
	if !set {
		return list
	}
	return append(list, named[F]{plugin: plugin, fn: fn})
}

// add registers p's callbacks, each at the end of its kind.
func (h *turnHooks) add(p hook.Plugin) {
	h.beforeTurn = addTo(h.beforeTurn, p.Name, p.BeforeTurn, p.BeforeTurn != nil)
	h.beforeLoop = addTo(h.beforeLoop, p.Name, p.BeforeLoop, p.BeforeLoop != nil)
	h.afterTurn = addTo(h.afterTurn, p.Name, p.AfterTurn, p.AfterTurn != nil)
	h.beforeModel = addTo(h.beforeModel, p.Name, p.BeforeModel, p.BeforeModel != nil)
	h.afterModel = addTo(h.afterModel, p.Name, p.AfterModel, p.AfterModel != nil)
	h.onModelError = addTo(h.onModelError, p.Name, p.OnModelError, p.OnModelError != nil)
	h.onFallback = addTo(h.onFallback, p.Name, p.OnFallback, p.OnFallback != nil)
	h.beforeTool = addTo(h.beforeTool, p.Name, p.BeforeTool, p.BeforeTool != nil)
	h.afterTool = addTo(h.afterTool, p.Name, p.AfterTool, p.AfterTool != nil)
	h.onToolError = addTo(h.onToolError, p.Name, p.OnToolError, p.OnToolError != nil)
	h.onCompaction = addTo(h.onCompaction, p.Name, p.OnCompaction, p.OnCompaction != nil)
	h.onTextDelta = addTo(h.onTextDelta, p.Name, p.OnTextDelta, p.OnTextDelta != nil)
	h.onInterrupt = addTo(h.onInterrupt, p.Name, p.OnInterrupt, p.OnInterrupt != nil)
	h.describeImages = addTo(h.describeImages, p.Name, p.DescribeImages, p.DescribeImages != nil)
	h.onEvent = addTo(h.onEvent, p.Name, p.OnEvent, p.OnEvent != nil)
}

// protect runs fn and returns a panic in it as a *hook.PanicError of the
// callback kind, registered by plugin; nil when fn returned.
func protect(kind, plugin string, fn func()) (perr *hook.PanicError) {
	defer func() {
		if r := recover(); r != nil {
			perr = &hook.PanicError{Callback: kind, Plugin: plugin, Value: r, Stack: debug.Stack()}
		}
	}()
	fn()
	return nil
}

// observe runs an observer: a panic in it is logged and skipped.
func (h *turnHooks) observe(kind, plugin string, detail string, fn func()) {
	perr := protect(kind, plugin, fn)
	if perr == nil || h.log == nil {
		return
	}
	h.log.Error("panic in turn event consumer", "component", "agent", "consumer", plugin,
		"event", detail, "panic", fmt.Sprintf("%v", perr.Value), "stack", string(perr.Stack))
}

// emit delivers ev to every OnEvent consumer, in order, synchronously. A
// consumer that panics is logged and skipped.
func (h *turnHooks) emit(ctx context.Context, ev hook.Event) {
	if h == nil {
		return
	}
	for _, c := range h.onEvent {
		h.observe("OnEvent", c.plugin, fmt.Sprintf("%T", ev), func() { c.fn(ctx, ev) })
	}
}

// callBeforeTurn runs the BeforeTurn callbacks until one answers or refuses
// the turn. A panic ends the turn: it is returned as the error.
func (h *turnHooks) callBeforeTurn(ctx context.Context, turn *hook.Turn) (*hook.TurnResult, error) {
	for _, c := range h.beforeTurn {
		var res *hook.TurnResult
		var err error
		if perr := protect("BeforeTurn", c.plugin, func() { res, err = c.fn(ctx, turn) }); perr != nil {
			return nil, perr
		}
		if err != nil || res != nil {
			return res, err
		}
	}
	return nil, nil //nolint:nilnil // (nil, nil): go on with the step
}

// callBeforeLoop runs the BeforeLoop callbacks on loop until one fails. A
// panic is returned as the error: it ends the turn.
func (h *turnHooks) callBeforeLoop(ctx context.Context, loop *hook.Loop) error {
	for _, c := range h.beforeLoop {
		var err error
		if perr := protect("BeforeLoop", c.plugin, func() { err = c.fn(ctx, loop) }); perr != nil {
			return perr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// callAfterTurn runs every AfterTurn callback. A panic makes the result an
// error that stores nothing; the later callbacks still run.
func (h *turnHooks) callAfterTurn(ctx context.Context, turn *hook.Turn, result *hook.TurnResult) {
	for _, c := range h.afterTurn {
		if perr := protect("AfterTurn", c.plugin, func() { c.fn(ctx, turn, result) }); perr != nil {
			result.Outcome = hook.OutcomeError
			result.Err = perr
			result.Stored = ""
		}
	}
}

// callBeforeModel runs the BeforeModel callbacks until one answers or fails
// the dispatch. A panic is returned as panicked: it ends the turn.
func (h *turnHooks) callBeforeModel(ctx context.Context, info *hook.ModelInfo) (resp *spawnllm.LLMResponse, panicked *hook.PanicError, err error) {
	for _, c := range h.beforeModel {
		if perr := protect("BeforeModel", c.plugin, func() { resp, err = c.fn(ctx, info) }); perr != nil {
			return nil, perr, nil
		}
		if err != nil || resp != nil {
			return resp, nil, err
		}
	}
	return nil, nil, nil
}

// callAfterModel runs the AfterModel callbacks on resp. A callback that sets
// resp.Response to nil changes nothing: the response it was given stays. A
// panic stops the callbacks and is returned: it ends the turn.
func (h *turnHooks) callAfterModel(ctx context.Context, call *hook.ModelInfo, resp *hook.ModelResponse) *hook.PanicError {
	for _, c := range h.afterModel {
		given := resp.Response
		if perr := protect("AfterModel", c.plugin, func() { c.fn(ctx, call, resp) }); perr != nil {
			return perr
		}
		if resp.Response == nil {
			resp.Response = given
		}
	}
	return nil
}

// callOnModelError runs the OnModelError callbacks on a dispatch that failed
// for good: a response answers it, an error replaces err. A panic is
// returned: it ends the turn.
func (h *turnHooks) callOnModelError(ctx context.Context, info *hook.ModelInfo, err error) (*spawnllm.LLMResponse, *hook.PanicError, error) {
	for _, c := range h.onModelError {
		var resp *spawnllm.LLMResponse
		var replaced error
		if perr := protect("OnModelError", c.plugin, func() { resp, replaced = c.fn(ctx, info, err) }); perr != nil {
			return nil, perr, err
		}
		if resp != nil {
			return resp, nil, nil
		}
		if replaced != nil {
			err = replaced
		}
	}
	return nil, nil, err
}

// callDescribeImages runs the DescribeImages callbacks until one returns ok.
// A panic is returned: it ends the turn.
func (h *turnHooks) callDescribeImages(ctx context.Context, images hook.UnseenImages) (text string, ok bool, panicked *hook.PanicError) {
	for _, c := range h.describeImages {
		if perr := protect("DescribeImages", c.plugin, func() { text, ok = c.fn(ctx, images) }); perr != nil {
			return "", false, perr
		}
		if ok {
			return text, true, nil
		}
	}
	return "", false, nil
}

// callOnFallback runs the OnFallback observers.
func (h *turnHooks) callOnFallback(ctx context.Context, ev hook.Fallback) {
	for _, c := range h.onFallback {
		h.observe("OnFallback", c.plugin, fmt.Sprintf("%T", ev), func() { c.fn(ctx, ev) })
	}
}

// callOnCompaction runs the OnCompaction observers.
func (h *turnHooks) callOnCompaction(ctx context.Context, ev hook.Compaction) {
	for _, c := range h.onCompaction {
		h.observe("OnCompaction", c.plugin, fmt.Sprintf("%T", ev), func() { c.fn(ctx, ev) })
	}
}

// callOnTextDelta runs the OnTextDelta observers.
func (h *turnHooks) callOnTextDelta(ctx context.Context, ev hook.TextDelta) {
	for _, c := range h.onTextDelta {
		h.observe("OnTextDelta", c.plugin, fmt.Sprintf("%T", ev), func() { c.fn(ctx, ev) })
	}
}

// callOnInterrupt runs the OnInterrupt observers.
func (h *turnHooks) callOnInterrupt(ctx context.Context, ev hook.Interrupted) {
	for _, c := range h.onInterrupt {
		h.observe("OnInterrupt", c.plugin, fmt.Sprintf("%T", ev), func() { c.fn(ctx, ev) })
	}
}

// toolStep runs one tool callback of kind; a panic is returned.
func toolStep(kind, plugin string, fn func()) *hook.PanicError { return protect(kind, plugin, fn) }

// toolCallbackFailure is the result a call whose tool callback panicked
// ends with.
func toolCallbackFailure(name string, perr *hook.PanicError) *toolkit.Result {
	return toolkit.ErrorResult(fmt.Sprintf("Tool %s failed with an internal error.", name)).WithError(perr)
}

// turnOnContext is what a turn puts on its context: its hooks and clock, so
// a step reached through another package (the context engine's compaction
// start) can call back into the turn it runs for.
type turnOnContext struct {
	hooks *turnHooks
	clock clock.Clock
	// finished is set once the turn emitted TurnFinished: a step reported
	// after it still reaches the callbacks but emits no event, so
	// TurnFinished stays the last.
	finished atomic.Bool
}

type turnOnContextKey struct{}

// beginTurnContext returns ctx carrying the turn's scope (hook.WithScope,
// which also sets toolkit's agent id and session key) and hooks. The agent
// calls it once per turn, for the context it runs the turn on.
func beginTurnContext(ctx context.Context, scope hook.Scope, hooks *turnHooks, now clock.Clock) context.Context {
	ctx = hook.WithScope(ctx, scope)
	return context.WithValue(ctx, turnOnContextKey{}, &turnOnContext{hooks: hooks, clock: now})
}

// turnOf returns the turn ctx belongs to; false outside a turn.
func turnOf(ctx context.Context) (*turnOnContext, bool) {
	t, ok := ctx.Value(turnOnContextKey{}).(*turnOnContext)
	if !ok || t == nil || t.hooks == nil {
		return nil, false
	}
	return t, true
}

// CompactionStart is the context engine's compaction-start hook. A host that
// builds its own session context managers (Spec.Session) gives them this
// one, so their blocking passes reach the turn as OnCompaction and a
// Compaction event, as the agent's own managers do
// (ctxengine.WithCompactionStartFunc): it reports the pass to the turn the
// context belongs to, as OnCompaction and a Compaction event (phase started),
// before the summary model is called. A pass with no turn on its context (a
// background pass) reaches no turn.
func CompactionStart(ctx context.Context, info ctxengine.CompactionStart) {
	t, ok := turnOf(ctx)
	if !ok {
		return
	}
	scope, _ := hook.ScopeOf(ctx)
	scope.Session = info.SessionKey
	ev := hook.Compaction{Phase: hook.CompactionStarted, Reason: string(info.Reason), Blocking: info.Blocking}
	ev.Scope, ev.At = scope, t.clock.Now()
	t.hooks.callOnCompaction(ctx, ev)
	if !t.finished.Load() {
		t.hooks.emit(ctx, ev)
	}
}
