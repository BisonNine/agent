/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"context"

	"go.bison9.com/agent/toolspec"
)

// The toolspec bridge. A tool package publishes toolspec definitions with
// BARE names; FromSpec mounts one under a namespace as a Tool, so the
// published name is "<namespace>_<bare>". The wrapper satisfies
// AsyncExecutor and SessionScoped exactly when the definition says so.

// ResultFromSpec converts a toolspec handler's return into a Result. A nil
// result becomes an error result (err's text, or "tool returned no
// result"); a non-nil err marks the result as failed, keeps err as Err when
// the result carries none, and supplies the model text when it is empty.
func ResultFromSpec(r *toolspec.Result, err error) *Result {
	if r == nil {
		if err != nil {
			return ErrorResult(err.Error()).WithError(err)
		}
		return ErrorResult("tool returned no result")
	}
	out := &Result{
		ForLLM:  r.ForLLM,
		ForUser: r.ForUser,
		Silent:  r.Silent,
		IsError: r.IsError,
		Async:   r.Async,
		Media:   r.Media,
		Images:  r.Images,
		Err:     r.Err,
	}
	if err != nil {
		out.IsError = true
		if out.Err == nil {
			out.Err = err
		}
		if out.ForLLM == "" {
			out.ForLLM = err.Error()
		}
	}
	return out
}

// ResultToSpec converts a Result into a toolspec result, for a toolspec
// handler that calls into a Tool. A nil result becomes an error result.
// ToolReferences has no toolspec field and is dropped.
func ResultToSpec(r *Result) *toolspec.Result {
	if r == nil {
		return &toolspec.Result{IsError: true, ForLLM: "tool returned no result"}
	}
	return &toolspec.Result{
		ForLLM:  r.ForLLM,
		ForUser: r.ForUser,
		Silent:  r.Silent,
		IsError: r.IsError,
		Async:   r.Async,
		Media:   r.Media,
		Images:  r.Images,
		Err:     r.Err,
	}
}

// callFromCtx assembles a toolspec call from the context values the caller
// set (WithAgentID, WithSessionKey, WithChat), so a toolspec handler reads
// them off the call.
func callFromCtx(ctx context.Context, args map[string]any, notify func(*toolspec.Result)) *toolspec.ToolCall {
	return &toolspec.ToolCall{
		Ctx:     ctx,
		Args:    args,
		AgentID: AgentID(ctx),
		Session: SessionKey(ctx),
		Channel: Channel(ctx),
		ChatID:  ChatID(ctx),
		Notify:  notify,
	}
}

// Distinct concrete types, so a type assertion for AsyncExecutor or
// SessionScoped reflects the definition's flags rather than always
// succeeding.

type specBase struct {
	ns  string
	def toolspec.ToolDefinition
}

func (t specBase) Name() string {
	// An empty namespace publishes the tool under its own name, with no
	// "<ns>_" prefix.
	if t.ns == "" {
		return t.def.Name
	}
	return t.ns + "_" + t.def.Name
}

func (t specBase) Description() string { return t.def.Description }

func (t specBase) Parameters() map[string]any { return t.def.Schema() }

func (t specBase) Execute(ctx context.Context, args map[string]any) *Result {
	return ResultFromSpec(t.def.Handler(callFromCtx(ctx, args, nil)))
}

// DiscoveryGroup reports the definition's Group and RevealTogether.
func (t specBase) DiscoveryGroup() (group string, revealTogether bool) {
	return t.def.Group, t.def.RevealTogether
}

type specAsync struct{ specBase }

func (t specAsync) ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *Result {
	notify := func(r *toolspec.Result) {
		if cb != nil {
			cb(ctx, ResultFromSpec(r, nil))
		}
	}
	return ResultFromSpec(t.def.Handler(callFromCtx(ctx, args, notify)))
}

type specSession struct{ specBase }

func (specSession) IsSessionScoped() bool { return true }

type specAsyncSession struct{ specAsync }

func (specAsyncSession) IsSessionScoped() bool { return true }

// FromSpec returns def as a Tool published as "<ns>_<def.Name>", or as
// def.Name when ns is empty. It implements AsyncExecutor when def.Async is
// set, SessionScoped when def.SessionScoped is set, and DiscoveryGrouped
// always.
func FromSpec(ns string, def toolspec.ToolDefinition) Tool {
	b := specBase{ns: ns, def: def}
	switch {
	case def.Async && def.SessionScoped:
		return specAsyncSession{specAsync{b}}
	case def.Async:
		return specAsync{b}
	case def.SessionScoped:
		return specSession{b}
	default:
		return b
	}
}
