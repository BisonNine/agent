package models

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// ErrNoModelAvailable reports a purpose with no model to try: none assigned,
// or every one disabled. The error that wraps it names the purpose.
var ErrNoModelAvailable = errors.New("no model available")

// NoModelError is the error for a purpose with no model to try.
type NoModelError struct {
	Purpose Purpose
	Detail  string // e.g. "2 excluded, 1 in cooldown"; may be empty
}

func (e *NoModelError) Error() string {
	msg := fmt.Sprintf("models: %s for purpose %q", ErrNoModelAvailable, e.Purpose)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *NoModelError) Unwrap() error { return ErrNoModelAvailable }

// Candidate is one model of a chain.
type Candidate struct {
	Name     string // model name
	Provider string // provider name
	Protocol string
	Model    string // wire model id
	Priority int
	Limits   modelspec.Limits
}

func candidateOf(g *generation, a assignment) Candidate {
	e := g.models[a.name]
	return Candidate{
		Name: a.name, Provider: e.prov.Name, Protocol: e.prov.Protocol, Model: e.def.Model,
		Priority: a.priority,
		Limits:   modelspec.ResolveLimits(&e.def, g.cfg.limitDefaults),
	}
}

// Attempt is one model a call passed over: one that failed, or one skipped
// because it was parked (Remaining > 0) or disabled.
type Attempt struct {
	Name      string
	Provider  string
	Model     string
	Err       error
	Reason    spawnllm.FailoverReason
	Duration  time.Duration
	Skipped   bool
	Disabled  bool          // skipped because it is disabled
	Remaining time.Duration // how long a skipped model stays parked
}

// ExhaustedError is the error of a call that tried every model of its chain
// without an answer.
type ExhaustedError struct {
	Purpose  Purpose
	Attempts []Attempt
}

// AllContextLimit reports whether every model that was called failed because
// the request was too large for it, which compaction may fix.
func (e *ExhaustedError) AllContextLimit() bool {
	called := 0
	for _, a := range e.Attempts {
		if a.Skipped {
			continue
		}
		called++
		if a.Reason != spawnllm.FailoverContextLimit {
			return false
		}
	}
	return called > 0
}

func (e *ExhaustedError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "fallback: all %d candidates failed:", len(e.Attempts))
	for i, a := range e.Attempts {
		switch {
		case a.Disabled:
			fmt.Fprintf(&sb, "\n  [%d] %s/%s: skipped (disabled)", i+1, a.Provider, a.Model)
		case a.Skipped:
			fmt.Fprintf(&sb, "\n  [%d] %s/%s: skipped (cooldown)", i+1, a.Provider, a.Model)
		default:
			fmt.Fprintf(&sb, "\n  [%d] %s/%s: %v (reason=%s, %s)",
				i+1, a.Provider, a.Model, a.Err, a.Reason, a.Duration.Round(time.Millisecond))
		}
	}
	return sb.String()
}

// Chain is the models pinned for one turn (Catalog.Begin). Its order and clients
// do not change for its life, so a multi-step turn never changes model
// half-way; only the enable switches are re-read before each call. A Chain
// may be used from several goroutines.
type Chain struct {
	cat       *Catalog
	gen       *generation
	purpose   Purpose
	cands     []Candidate
	isolated  bool
	workspace string
}

// Candidates returns the chain's models in the order they are tried.
func (ch *Chain) Candidates() []Candidate { return slices.Clone(ch.cands) }

// Purpose returns the purpose whose models the chain holds.
func (ch *Chain) Purpose() Purpose { return ch.purpose }

// Limits returns the limits of the chain's first model.
func (ch *Chain) Limits() modelspec.Limits {
	if len(ch.cands) == 0 {
		return modelspec.ResolveLimits(nil, ch.gen.cfg.limitDefaults)
	}
	return ch.cands[0].Limits
}

// Isolated returns a copy of the chain whose CLI models run in workspace,
// with no permission-bypass flag whatever their provider says, and outside
// the declined-tools check: for an agent that has no tools to grant. Each
// call builds its own CLI client; HTTP models use the shared client.
func (ch *Chain) Isolated(workspace string) *Chain {
	n := *ch
	n.isolated, n.workspace = true, workspace
	return &n
}

// Request is one chat request.
type Request struct {
	Messages []spawnllm.Message
	Tools    []spawnllm.ToolDefinition
	Options  map[string]any
}

// Result is a call's answer: the response, the model that gave it, and the
// models passed over before it.
type Result struct {
	Response  *spawnllm.LLMResponse
	Candidate Candidate
	Attempts  []Attempt
}

// CallOption configures one Chat call.
type CallOption func(*callConfig)

type callConfig struct {
	prepare func(Candidate, spawnllm.LLMProvider, Request) Request
	notify  func(passed []Attempt, next Candidate)
	finish  func(Candidate, time.Duration, *spawnllm.LLMResponse, error)
}

// WithPrepare shapes the request for each model before it is called, e.g. its
// own max_tokens or dropping images a model cannot take. fn is given the
// model, its client and the request as passed to Chat, and must not change
// the latter's slices or map in place.
func WithPrepare(fn func(c Candidate, client spawnllm.LLMProvider, req Request) Request) CallOption {
	return func(c *callConfig) { c.prepare = fn }
}

// WithNotify is called each time the call moves past one or more models to
// try next: the attempts just passed (one failure, or a run of skipped
// models) and the model tried next. It is not called for a model that ends
// the call.
func WithNotify(fn func(passed []Attempt, next Candidate)) CallOption {
	return func(c *callConfig) { c.notify = fn }
}

// WithFinish is called after each model call with its outcome.
func WithFinish(fn func(c Candidate, elapsed time.Duration, resp *spawnllm.LLMResponse, err error)) CallOption {
	return func(c *callConfig) { c.finish = fn }
}

// skip returns the attempt of a model the chain must not call now: disabled
// (re-read from the catalogue's current contents) or parked. ok is false when
// the model may be called.
func (ch *Chain) skip(i int) (Attempt, bool) {
	c := ch.cands[i]
	if ch.disabled(i) {
		return Attempt{
			Name: c.Name, Provider: c.Provider, Model: c.Model, Skipped: true, Disabled: true,
			Err: fmt.Errorf("%s/%s disabled", c.Provider, c.Model),
		}, true
	}
	remaining, reason := ch.gen.health.parkedFor(c.Provider, c.Model)
	if remaining <= 0 {
		return Attempt{}, false
	}
	return Attempt{
		Name: c.Name, Provider: c.Provider, Model: c.Model, Skipped: true,
		Reason: reason, Remaining: remaining,
		Err: fmt.Errorf("%s/%s in cooldown (%s remaining)", c.Provider, c.Model, remaining.Round(time.Second)),
	}, true
}

// disabled re-reads the switch of the i-th model, from the catalogue's
// current contents when it still has the model, else from the pinned ones.
func (ch *Chain) disabled(i int) bool {
	name := ch.cands[i].Name
	if ok, known := ch.cat.gen.Load().usable("", name); known {
		return !ok
	}
	ok, _ := ch.gen.usable("", name)
	return !ok
}

func (ch *Chain) clock() clock.Clock { return clock.Or(ch.gen.cfg.clock) }

// call makes one call to candidate c.
func (ch *Chain) call(ctx context.Context, id accounting.Identity, c Candidate, req Request, cfg *callConfig) (*spawnllm.LLMResponse, error) {
	client, release, err := ch.client(c)
	if err != nil {
		return nil, err
	}
	defer release()
	if cfg != nil && cfg.prepare != nil {
		req = cfg.prepare(c, client, req)
	}
	start := ch.clock().Now()
	resp, err := client.Chat(ctx, id, req.Messages, req.Tools, c.Model, req.Options)
	if cfg != nil && cfg.finish != nil {
		cfg.finish(c, ch.clock().Since(start), resp, err)
	}
	return resp, err
}

// client returns the client to call c with, and the function that ends the
// call.
func (ch *Chain) client(c Candidate) (spawnllm.LLMProvider, func(), error) {
	e := ch.gen.models[c.Name]
	if ch.isolated && !e.registered && modelspec.CLIAgentByProtocol(e.prov.Protocol) != nil {
		if strings.TrimSpace(ch.workspace) == "" {
			return nil, nil, errors.New("models: an isolated CLI client needs a workspace")
		}
		spec := isolatedCLISpec(ch.gen.cfg, &e.def, &e.prov, ch.workspace, func(name string) bool {
			_, seen := ch.gen.warned.LoadOrStore(name, struct{}{})
			return !seen
		})
		return launchCLI(ch.gen.cfg, spec), func() {}, nil
	}
	return e.slot.acquire()
}

// Chat runs one chat call for id on the chain: each model in turn, skipping
// disabled and parked ones, until one answers.
//
//   - A cancelled context ends the call at once with context.Canceled.
//   - An error spawnllm cannot classify ends the call; a classified one moves
//     on to the next model, once the failure is recorded against the model's
//     health. A Retry-After of up to 3 seconds is waited out and the same
//     model tried once more first.
//   - An answer clears the model's failures.
//   - When every model failed or was skipped the error is an
//     *ExhaustedError; when every one was disabled, or there are none, a
//     *NoModelError.
func (ch *Chain) Chat(ctx context.Context, id accounting.Identity, req Request, opts ...CallOption) (*Result, error) {
	if _, err := id.Normalize(); err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	cfg := &callConfig{}
	for _, o := range opts {
		o(cfg)
	}
	res, err := ch.chat(ctx, id, req, cfg)
	ch.report(res, err)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (ch *Chain) chat(ctx context.Context, id accounting.Identity, req Request, cfg *callConfig) (*Result, error) {
	if len(ch.cands) == 0 {
		return &Result{}, &NoModelError{Purpose: ch.purpose}
	}
	obs := ch.gen.cfg.observer
	result := &Result{Attempts: make([]Attempt, 0, len(ch.cands))}
	var pending []Attempt // skips not yet announced
	called := false
	notify := func(passed []Attempt, next Candidate) {
		if cfg.notify != nil {
			cfg.notify(passed, next)
		}
		obs.FellBack(FellBack{Purpose: ch.purpose, Passed: slices.Clone(passed), Next: next})
	}
	for i, c := range ch.cands {
		if errors.Is(ctx.Err(), context.Canceled) {
			return result, context.Canceled
		}
		if a, skipped := ch.skip(i); skipped {
			called = called || !a.Disabled
			result.Attempts = append(result.Attempts, a)
			pending = append(pending, a)
			continue
		}
		called = true
		if len(pending) > 0 {
			notify(pending, c)
		}
		pending = nil

		var (
			resp    *spawnllm.LLMResponse
			err     error
			fe      *spawnllm.FailoverError
			elapsed time.Duration
		)
		for n := 1; ; n++ {
			start := ch.clock().Now()
			resp, err = ch.call(ctx, id, c, req, cfg)
			elapsed = ch.clock().Since(start)
			if err == nil {
				if ev, ok := ch.gen.health.success(c.Name, c.Provider, c.Model); ok {
					obs.Recovered(ev)
				}
				result.Response, result.Candidate = resp, c
				return result, nil
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				result.Attempts = append(result.Attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: err, Duration: elapsed})
				return result, context.Canceled
			}
			fe = spawnllm.ClassifyError(err, c.Provider, c.Model)
			if fe == nil {
				result.Attempts = append(result.Attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: err, Duration: elapsed})
				return result, fmt.Errorf("fallback: unclassified error from %s/%s: %w", c.Provider, c.Model, err)
			}
			if !fe.IsRetriable() {
				result.Attempts = append(result.Attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: fe, Reason: fe.Reason, Duration: elapsed})
				return result, fe
			}
			if n == 1 && fe.RetryAfter > 0 && fe.RetryAfter <= maxSameModelRetryWait {
				select {
				case <-ch.clock().After(fe.RetryAfter):
				case <-ctx.Done():
					return result, ctx.Err()
				}
				continue
			}
			break
		}
		if ev, ok := ch.gen.health.failure(c.Name, c.Provider, c.Model, fe.Reason, fe.Status, fe.RetryAfter); ok {
			obs.Parked(ev)
		}
		result.Attempts = append(result.Attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: fe, Reason: fe.Reason, Duration: elapsed})
		if i == len(ch.cands)-1 {
			return result, &ExhaustedError{Purpose: ch.purpose, Attempts: result.Attempts}
		}
		notify(result.Attempts[len(result.Attempts)-1:], ch.cands[i+1])
	}
	if !called {
		return result, &NoModelError{Purpose: ch.purpose, Detail: "every model is disabled"}
	}
	return result, &ExhaustedError{Purpose: ch.purpose, Attempts: result.Attempts}
}

// report tells the observer when a call found no answer.
func (ch *Chain) report(res *Result, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	ch.gen.cfg.observer.Exhausted(Exhausted{Purpose: ch.purpose, Attempts: slices.Clone(res.Attempts), Err: err})
}
