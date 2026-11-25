/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/spawnllm/openai_compat"
)

// CompleteRequest, CompleteReply and Caller are hook's: the one model-caller
// interface, declared in the leaf so ctxengine and cogmem take it without
// importing models.
type (
	CompleteRequest = hook.CompleteRequest
	CompleteReply   = hook.CompleteReply
	Caller          = hook.Caller
)

// Caller returns a Caller that makes its calls on this chain, for id, with
// the call options given (WithPrepare, WithNotify, WithFinish, WithClassify).
func (ch *Chain) Caller(id accounting.Identity, opts ...CallOption) Caller {
	return &caller{chain: func() *Chain { return ch }, id: id, opts: opts}
}

// caller is a Caller over the chain chain returns for each call.
type caller struct {
	chain func() *Chain
	id    accounting.Identity
	opts  []CallOption
}

func (c *caller) Complete(ctx context.Context, req CompleteRequest) (CompleteReply, error) {
	if _, err := c.id.Normalize(); err != nil {
		return CompleteReply{}, fmt.Errorf("models: %w", err)
	}
	return c.chain().complete(ctx, c.id, req, newCallConfig(c.opts))
}

// completeVerdict is the default verdict of Complete: any failure moves on to
// the next model; one spawnllm classifies is recorded against the model.
func completeVerdict(err error, c Candidate) Verdict {
	fe := spawnllm.ClassifyError(err, "", c.Model)
	if fe == nil {
		return Verdict{Next: true}
	}
	return Verdict{Reason: fe.Reason, Status: fe.Status, RetryAfter: fe.RetryAfter, Record: true, Next: true}
}

// complete walks the chain for a single-shot request: models excluded by the
// request, disabled or parked are skipped; a failure is judged by its
// Verdict (by default: recorded when spawnllm classifies it, and the next
// model tried; Wait is not used); the first answer is returned. When every
// model called failed, the last error is returned; when none could be
// called, an error wrapping ErrNoModelAvailable. When ctx ends the walk stops
// with ctx.Err(), and the call cut short is not held against its model.
// Without a WithNotify hook the models passed are logged; with one they are
// reported to it instead.
func (ch *Chain) complete(ctx context.Context, id accounting.Identity, req CompleteRequest, cfg *callConfig) (CompleteReply, error) {
	if err := ch.cat.life.enter(); err != nil {
		return CompleteReply{}, err
	}
	defer ch.cat.life.leave()
	messages := []spawnllm.Message{
		{Role: "system", Content: req.System},
		{Role: "user", Content: req.User},
	}
	var opts map[string]any
	if req.JSONObject {
		opts = map[string]any{openai_compat.ResponseFormatJSONObjectOption: true}
	}
	if ch.direct != "" {
		return ch.completeDirect(ctx, id, Request{Messages: messages, Options: opts}, cfg)
	}
	if cfg.classify == nil {
		cfg.classify = completeVerdict
	}
	log := ch.gen.cfg.logger.With("component", component, "agent", id.Agent, "purpose", string(ch.purpose))
	next := func(i int) Candidate {
		if i+1 < len(ch.cands) {
			return ch.cands[i+1].copied()
		}
		return Candidate{}
	}
	passed := func(i int, a Attempt) {
		if cfg.notify != nil {
			cfg.notify([]Attempt{a}, next(i))
			return
		}
		switch {
		case a.Disabled:
		case a.Skipped:
			log.Info("model in cooldown; skipping", "model", a.Name, "remaining", a.Remaining.Round(time.Second).String())
		default:
			log.Warn("model call failed; trying the next", "model", a.Name, "error", a.Err.Error())
		}
	}
	var lastErr error
	lastModel := ""
	excluded, cooling, disabled := 0, 0, 0
	for i, c := range ch.cands {
		if err := ctx.Err(); err != nil {
			return CompleteReply{Model: lastModel}, err
		}
		if slices.Contains(req.Exclude, c.Model) {
			excluded++
			continue
		}
		if a, skipped := ch.skip(i); skipped {
			if a.Disabled {
				disabled++
			} else {
				cooling++
			}
			passed(i, a)
			continue
		}
		start := ch.clock().Now()
		resp, err := ch.chatOnce(ctx, id, c, Request{Messages: messages, Options: opts}, cfg)
		elapsed := ch.clock().Since(start)
		if err != nil {
			lastErr, lastModel = err, c.Model
			if ctxErr := ctx.Err(); ctxErr != nil {
				return CompleteReply{Model: lastModel}, ctxErr
			}
			v := cfg.verdict(err, c)
			if v.Record {
				ch.gen.health.failure(c.Name, c.Provider, c.Model, v.Reason, v.Status, v.RetryAfter)
			}
			if !v.Next {
				return CompleteReply{Model: lastModel}, err
			}
			passed(i, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: err, Reason: v.Reason, Duration: elapsed})
			continue
		}
		ch.gen.health.success(c.Name, c.Provider, c.Model)
		ch.cat.state.served(selKey{set: ch.set, purpose: ch.purpose, scope: ch.scope}, ch.from, c.Name, "", nil, ch.clock().Now())
		return CompleteReply{Content: resp.Content, FinishReason: resp.FinishReason, Model: c.Model}, nil
	}
	if lastErr != nil {
		return CompleteReply{Model: lastModel}, lastErr
	}
	detail := fmt.Sprintf("%d excluded, %d in cooldown", excluded, cooling)
	if disabled > 0 {
		detail += fmt.Sprintf(", %d disabled", disabled)
	}
	return CompleteReply{}, &NoModelError{Purpose: ch.purpose, Detail: detail}
}

// completeDirect is complete on a chain made by Direct: one call, with no
// skip, health or serving record, the client's error returned unless ctx
// ended.
func (ch *Chain) completeDirect(ctx context.Context, id accounting.Identity, req Request, cfg *callConfig) (CompleteReply, error) {
	if err := ctx.Err(); err != nil {
		return CompleteReply{}, err
	}
	c := ch.cands[0]
	resp, err := ch.chatOnce(ctx, id, c, req, cfg)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CompleteReply{Model: c.Model}, ctxErr
		}
		return CompleteReply{Model: c.Model}, err
	}
	return CompleteReply{Content: resp.Content, FinishReason: resp.FinishReason, Model: c.Model}, nil
}
