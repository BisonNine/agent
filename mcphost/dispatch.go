package mcphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"runtime/debug"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// call is the mcp-go handler of every published tool: it takes the
// credential out of the arguments, finds the caller and runs the call.
func (e *Endpoint) call(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	e.inflight.Add(1)
	defer e.inflight.Add(-1)

	args := maps.Clone(req.GetArguments())
	if args == nil {
		args = map[string]any{}
	}
	token, isString := args[TokenArgument].(string)
	if !isString {
		token = "" // absent or not a string: no token given
	}
	// Removed on both transports: no tool ever sees a credential.
	delete(args, TokenArgument)

	var caller Caller
	if e.cfg.Transport == Argument {
		c, err := e.authenticate(ctx, token)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		caller = c
	} else {
		c, ok := CallerOf(ctx) // ServeHTTP authenticated the request
		if !ok {
			return mcp.NewToolResultError("invalid bearer token"), nil
		}
		caller = c
	}

	res := e.dispatch(ctx, caller, req.Params.Name, args)
	if res.IsError {
		return mcp.NewToolResultError(res.ForLLM), nil
	}
	return mcp.NewToolResultText(res.ForLLM), nil
}

// dispatch runs one call for caller: the scope, the caller and the host's
// values on its context; ToolCallStarted; BeforeTool; the tool through the
// caller's set and the Gate; OnToolError; AfterTool; ToolCallFinished.
func (e *Endpoint) dispatch(ctx context.Context, caller Caller, name string, args map[string]any) *toolkit.Result {
	ctx = hook.WithScope(ctx, caller.Scope)
	ctx = withCaller(ctx, caller)
	if e.cfg.CallContext != nil {
		ctx = e.cfg.CallContext(ctx, caller)
	}
	ctx, stop := cancelOnDone(ctx, caller.Done)
	defer stop()
	if caller.revoked() {
		return revokedResult()
	}

	call := hook.ToolCall{ID: newCallID(), Name: name, Arguments: args}
	e.logger.InfoContext(ctx, "MCP tool call", "agent", caller.Scope.AgentID, "session", caller.Scope.Session, "tool", name)
	e.emit(ctx, hook.ToolCallStarted{EventHeader: header(caller), Call: copyCall(call)})

	start := time.Now()
	res, answered := e.run(ctx, caller, &call)
	e.emit(ctx, hook.ToolCallFinished{
		EventHeader: header(caller),
		Call:        copyCall(call),
		Result:      copyResult(res),
		Elapsed:     time.Since(start),
		Answered:    answered,
	})

	// The credential stopped working while the call ran: nothing more goes
	// to the client. The callbacks and events above saw the real result.
	if errors.Is(context.Cause(ctx), ErrRevoked) {
		e.logger.WarnContext(ctx, "MCP tool call ended by a revoked credential", "agent", caller.Scope.AgentID, "tool", name)
		return revokedResult()
	}
	return res
}

// run is the call's steps from BeforeTool to AfterTool. answered reports a
// call BeforeTool answered or refused, or one whose BeforeTool panicked:
// the tool did not run. A panic in a callback fails the call with the
// engine's internal-error result and skips the call's later callbacks.
func (e *Endpoint) run(ctx context.Context, caller Caller, call *hook.ToolCall) (res *toolkit.Result, answered bool) {
	for _, p := range e.cfg.Plugins {
		if p.BeforeTool == nil {
			continue
		}
		var r *toolkit.Result
		var err error
		if pe := e.guard(ctx, "BeforeTool", p.Name, call.Name, func() { r, err = p.BeforeTool(ctx, call) }); pe != nil {
			return panicResult(call.Name, pe), true
		}
		if err != nil {
			res, answered = toolkit.ErrorResult(err.Error()).WithError(err), true
			break
		}
		if r != nil {
			res, answered = r, true
			break
		}
	}
	if !answered {
		res = e.execute(ctx, caller, *call)
	}
	// The callbacks work on the call's own copy: never on a result the tool
	// or a callback may still hold.
	res = copyResult(res)

	if res.IsError || res.Err != nil {
		for _, p := range e.cfg.Plugins {
			if p.OnToolError == nil {
				continue
			}
			var r *toolkit.Result
			if pe := e.guard(ctx, "OnToolError", p.Name, call.Name, func() { r = p.OnToolError(ctx, call, res) }); pe != nil {
				return panicResult(call.Name, pe), answered
			}
			if r != nil {
				res = copyResult(r)
			}
		}
	}
	for _, p := range e.cfg.Plugins {
		if p.AfterTool == nil {
			continue
		}
		if pe := e.guard(ctx, "AfterTool", p.Name, call.Name, func() { p.AfterTool(ctx, call, res) }); pe != nil {
			return panicResult(call.Name, pe), answered
		}
	}
	return res, answered
}

// execute runs the tool through the caller's set with the Gate. A panic in
// the tool fails the call as the engine fails it; its OnToolError and
// AfterTool still run.
//
// A background tool (an AsyncExecutor, run when AsyncResult is set) gets
// the call's context without its cancellation: its work outlives the MCP
// request by design, as a turn's background tasks outlive the turn, and
// stopping it is the host's (its task control), not the request's.
func (e *Endpoint) execute(ctx context.Context, caller Caller, call hook.ToolCall) (res *toolkit.Result) {
	var cb toolkit.AsyncCallback
	if e.cfg.AsyncResult != nil {
		ran := copyCall(call)
		cb = func(cbCtx context.Context, r *toolkit.Result) { e.cfg.AsyncResult(cbCtx, caller, ran, r) }
		if info, ok := caller.Set.Lookup(call.Name); ok {
			if _, async := info.Tool.(toolkit.AsyncExecutor); async {
				ctx = context.WithoutCancel(ctx)
			}
		}
	}
	defer func() {
		if v := recover(); v != nil {
			e.logger.ErrorContext(ctx, "MCP tool panicked", "agent", caller.Scope.AgentID, "tool", call.Name,
				"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			res = internalErrorResult(call.Name).WithError(fmt.Errorf("tool %s panicked: %v", call.Name, v))
		}
	}()
	return caller.Set.ExecuteGated(ctx, call.Name, call.Arguments, cb, e.cfg.Gate)
}

// guard runs fn, recovering a panic as a *hook.PanicError, which it logs.
func (e *Endpoint) guard(ctx context.Context, kind, plugin, tool string, fn func()) (pe *hook.PanicError) {
	defer func() {
		if v := recover(); v != nil {
			pe = &hook.PanicError{Callback: kind, Plugin: plugin, Value: v, Stack: debug.Stack()}
			e.logger.ErrorContext(ctx, "MCP tool callback panicked", "tool", tool, "error", pe.Error(), "stack", string(pe.Stack))
		}
	}()
	fn()
	return nil
}

// emit gives ev to every plugin's OnEvent, on the call's goroutine. A panic
// in an observer is logged and changes nothing.
func (e *Endpoint) emit(ctx context.Context, ev hook.Event) {
	for _, p := range e.cfg.Plugins {
		if p.OnEvent == nil {
			continue
		}
		if pe := e.guard(ctx, "OnEvent", p.Name, "", func() { p.OnEvent(ctx, ev) }); pe != nil {
			continue // logged; an observer's panic changes nothing
		}
	}
}

func header(c Caller) hook.EventHeader {
	return hook.EventHeader{Scope: c.Scope, At: time.Now()}
}

// copyCall returns call with its own argument map, for an event: a later
// change of the call's arguments does not reach it.
func copyCall(call hook.ToolCall) hook.ToolCall {
	call.Arguments = maps.Clone(call.Arguments)
	return call
}

// copyResult returns a shallow copy of r, or an error result for nil. Its
// slices (Media, Images, ToolReferences) are shared: a callback replaces
// them rather than editing them in place.
func copyResult(r *toolkit.Result) *toolkit.Result {
	if r == nil {
		return toolkit.ErrorResult("tool returned no result")
	}
	c := *r
	return &c
}

func internalErrorResult(tool string) *toolkit.Result {
	return toolkit.ErrorResult(fmt.Sprintf("Tool %s failed with an internal error.", tool))
}

func panicResult(tool string, pe *hook.PanicError) *toolkit.Result {
	return internalErrorResult(tool).WithError(pe)
}

func revokedResult() *toolkit.Result {
	return toolkit.ErrorResult(ErrRevoked.Error()).WithError(toolkit.Refusal(ErrRevoked))
}

// newCallID returns an id for one call: MCP gives a handler none.
func newCallID() string {
	raw := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "mcp-" + time.Now().Format("150405.000000000")
	}
	return "mcp-" + hex.EncodeToString(raw)
}
