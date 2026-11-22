package models

import (
	"context"
	"fmt"
	"slices"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/spawnllm/openai_compat"
)

// CompleteRequest is one single-shot model call: a system and a user message.
// It is the request ctxengine (summaries) and cogmem (consolidation) make.
type CompleteRequest struct {
	System, User string
	// JSONObject asks for a JSON-object response where the client supports it.
	JSONObject bool
	// Exclude lists wire model ids (as CompleteReply.Model reports them)
	// that must not serve this request.
	Exclude []string
}

// CompleteReply is the answer to a CompleteRequest. Model is the wire model id
// of the model that gave it; on an error, the last model tried, if any.
type CompleteReply struct {
	Content      string
	FinishReason string
	Model        string
}

// Caller is the one model-caller interface: what a summarizer or a memory
// consolidator needs from a model chain. Chain.Caller and Set.Caller return
// one.
type Caller interface {
	Complete(ctx context.Context, req CompleteRequest) (CompleteReply, error)
}

// Caller returns a Caller that makes its calls on this chain, for id.
func (ch *Chain) Caller(id accounting.Identity) Caller {
	return callerFunc(func() (*Chain, accounting.Identity) { return ch, id })
}

// callerFunc is a Caller over the chain the function returns for each call.
type callerFunc func() (*Chain, accounting.Identity)

func (f callerFunc) Complete(ctx context.Context, req CompleteRequest) (CompleteReply, error) {
	ch, id := f()
	if _, err := id.Normalize(); err != nil {
		return CompleteReply{}, fmt.Errorf("models: %w", err)
	}
	return ch.complete(ctx, id, req)
}

// complete walks the chain for a single-shot request: models excluded by the
// request, disabled or parked are skipped; any error moves on to the next
// model (a classified one is recorded against its health); the first answer
// is returned. When every model called failed, the last error is returned.
func (ch *Chain) complete(ctx context.Context, id accounting.Identity, req CompleteRequest) (CompleteReply, error) {
	messages := []spawnllm.Message{
		{Role: "system", Content: req.System},
		{Role: "user", Content: req.User},
	}
	var opts map[string]any
	if req.JSONObject {
		opts = map[string]any{openai_compat.ResponseFormatJSONObjectOption: true}
	}
	log := ch.gen.cfg.logger.With("component", component, "agent", id.Agent, "purpose", string(ch.purpose))
	obs := ch.gen.cfg.observer
	var lastErr error
	lastModel := ""
	excluded, cooling, disabled := 0, 0, 0
	for i, c := range ch.cands {
		if slices.Contains(req.Exclude, c.Model) {
			excluded++
			continue
		}
		if a, skipped := ch.skip(i); skipped {
			if a.Disabled {
				disabled++
				continue
			}
			cooling++
			log.Info("model in cooldown; skipping", "model", c.Name, "remaining", a.Remaining.Round(time.Second).String())
			continue
		}
		resp, err := ch.call(ctx, id, c, Request{Messages: messages, Options: opts}, nil)
		if err != nil {
			lastErr, lastModel = err, c.Model
			if fe := spawnllm.ClassifyError(err, "", c.Model); fe != nil {
				if ev, ok := ch.gen.health.failure(c.Name, c.Provider, c.Model, fe.Reason, fe.Status, fe.RetryAfter); ok {
					obs.Parked(ev)
				}
			}
			log.Warn("model call failed; trying the next", "model", c.Name, "error", err.Error())
			continue
		}
		if ev, ok := ch.gen.health.success(c.Name, c.Provider, c.Model); ok {
			obs.Recovered(ev)
		}
		ch.cat.state.served(selKey{set: ch.set, purpose: ch.purpose, scope: ch.scope}, c.Name, "", nil, ch.clock().Now())
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
