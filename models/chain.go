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

// Candidate is one model of a chain. Spec and ProviderSpec are copies of the
// model's definition and its provider's (secrets resolved), for a caller
// that builds its own client for the model (Chain.Do).
type Candidate struct {
	Name         string // model name
	Provider     string // provider name
	Protocol     string
	Model        string // wire model id
	Priority     int
	Settings     any // the assignment's (WithSettings)
	Limits       modelspec.Limits
	Spec         modelspec.Model
	ProviderSpec modelspec.Provider
}

func candidateOf(g *generation, a assignment) Candidate {
	e := g.models[a.name]
	return Candidate{
		Name: a.name, Provider: e.prov.Name, Protocol: e.prov.Protocol, Model: e.def.Model,
		Priority: a.priority, Settings: a.settings,
		Limits: modelspec.ResolveLimits(&e.def, g.cfg.limitDefaults),
		Spec:   cloneModel(e.def), ProviderSpec: e.prov,
	}
}

// copied returns c with its own copy of the model definition.
func (c Candidate) copied() Candidate {
	c.Spec = cloneModel(c.Spec)
	return c
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

// Chain is the models pinned for one turn (Set.Begin). Its order and clients
// do not change for its life, so a multi-step turn never changes model
// half-way; only the enable switches are re-read before each call. A Chain
// may be used from several goroutines.
type Chain struct {
	cat       *Catalog
	from      *Set // the set it was begun from
	gen       *generation
	set       string
	purpose   Purpose // as asked
	used      Purpose // whose list it is (a fallback rule may have applied)
	scope     Scope
	cands     []Candidate
	off       []bool // assignment switched off
	isolated  bool
	workspace string
}

// Candidates returns the chain's models in the order they are tried.
func (ch *Chain) Candidates() []Candidate {
	out := make([]Candidate, len(ch.cands))
	for i, c := range ch.cands {
		out[i] = c.copied()
	}
	return out
}

// Purpose returns the purpose whose models the chain holds: the one asked
// for, or the one a fallback rule led to.
func (ch *Chain) Purpose() Purpose { return ch.used }

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

// CallOption configures one call of a chain (Chat, Do, or a Caller's
// Complete).
type CallOption func(*callConfig)

type callConfig struct {
	prepare  func(Candidate, spawnllm.LLMProvider, Request) Request
	notify   func(passed []Attempt, next Candidate)
	finish   func(Candidate, time.Duration, *spawnllm.LLMResponse, error)
	classify func(err error, c Candidate) Verdict
}

func newCallConfig(opts []CallOption) *callConfig {
	cfg := &callConfig{}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// WithPrepare shapes the request for each model before it is called, e.g. its
// own max_tokens or dropping images a model cannot take. fn is given the
// model, its client and the request as passed in, and must not change the
// latter's slices or map in place. Chat and Complete; Do has no request.
func WithPrepare(fn func(c Candidate, client spawnllm.LLMProvider, req Request) Request) CallOption {
	return func(c *callConfig) { c.prepare = fn }
}

// WithNotify is called as the call moves past models. For Chat and Do: each
// time it moves past one or more models to try next, with the attempts just
// passed (one failure, or a run of skipped models) and the model tried next;
// not for a model that ends the call. For Complete: for each model passed
// (skipped or failed) as it is passed, with that one attempt and the model
// considered next, or the zero Candidate when none is left; with a notify
// hook, Complete logs none of these itself.
func WithNotify(fn func(passed []Attempt, next Candidate)) CallOption {
	return func(c *callConfig) { c.notify = fn }
}

// WithFinish is called after each model call with its outcome. For Do, resp
// is nil.
func WithFinish(fn func(c Candidate, elapsed time.Duration, resp *spawnllm.LLMResponse, err error)) CallOption {
	return func(c *callConfig) { c.finish = fn }
}

// Verdict is how a call treats a model's failure.
type Verdict struct {
	Reason     spawnllm.FailoverReason // the failure's reason, for attempts and health ("" when unknown)
	Status     int                     // its HTTP status, which decides the cooldown
	RetryAfter time.Duration           // a server's Retry-After, which replaces the computed cooldown
	Record     bool                    // count it against the model's health
	Next       bool                    // try the next model; false ends the call with this failure
	Wait       time.Duration           // > 0: wait this long and call the same model once more first (its first failure only)
}

// DefaultVerdict is spawnllm's classification: a failure spawnllm cannot
// classify ends the call and is not recorded; a classified one is recorded
// and the next model is tried, after one more call to the same model when a
// Retry-After of at most 3 seconds came with it.
func DefaultVerdict(err error, c Candidate) Verdict {
	fe := spawnllm.ClassifyError(err, c.Provider, c.Model)
	if fe == nil {
		return Verdict{}
	}
	v := Verdict{Reason: fe.Reason, Status: fe.Status, RetryAfter: fe.RetryAfter, Record: true, Next: fe.IsRetriable()}
	if fe.RetryAfter > 0 && fe.RetryAfter <= maxSameModelRetryWait {
		v.Wait = fe.RetryAfter
	}
	return v
}

// WithClassify replaces DefaultVerdict for one call: fn decides, for each
// failure (never for one caused by the caller's context ending), whether it
// counts against the model's health and whether the next model is tried.
func WithClassify(fn func(err error, c Candidate) Verdict) CallOption {
	return func(c *callConfig) { c.classify = fn }
}

func (cfg *callConfig) verdict(err error, c Candidate) Verdict {
	if cfg.classify != nil {
		return cfg.classify(err, c)
	}
	return DefaultVerdict(err, c)
}

// failed is the error an attempt records for a failure with verdict v.
func failed(err error, c Candidate, v Verdict) error {
	if v.Reason == "" {
		return err
	}
	if fe, ok := errors.AsType[*spawnllm.FailoverError](err); ok && fe.Reason == v.Reason && fe.Status == v.Status {
		return fe
	}
	return &spawnllm.FailoverError{Reason: v.Reason, Provider: c.Provider, Model: c.Model, Status: v.Status, RetryAfter: v.RetryAfter, Wrapped: err}
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

// disabled re-reads the switches of the i-th model, from the catalogue's
// current contents when it still has the model, else from the pinned ones.
func (ch *Chain) disabled(i int) bool {
	if ch.off[i] {
		return true
	}
	name := ch.cands[i].Name
	if ok, known := ch.cat.gen.Load().usable(ch.set, name); known {
		return !ok
	}
	ok, _ := ch.gen.usable(ch.set, name)
	return !ok
}

func (ch *Chain) clock() clock.Clock { return clock.Or(ch.gen.cfg.clock) }

// chatOnce makes one chat call to candidate c.
func (ch *Chain) chatOnce(ctx context.Context, id accounting.Identity, c Candidate, req Request, cfg *callConfig) (*spawnllm.LLMResponse, error) {
	client, release, err := ch.clientOf(c.Name)
	if err != nil {
		return nil, err
	}
	defer release()
	if cfg.prepare != nil {
		req = cfg.prepare(c.copied(), client, req)
	}
	start := ch.clock().Now()
	resp, err := client.Chat(ctx, id, req.Messages, req.Tools, c.Model, req.Options)
	if cfg.finish != nil {
		cfg.finish(c.copied(), ch.clock().Since(start), resp, err)
	}
	return resp, err
}

// Client returns the chat client of model name as this chain has it: from
// the contents it pinned, isolated if the chain is. It reads and records no
// health and ignores the enable switches. The client stays usable until
// release is called, which must be done exactly once when the caller is
// done with it; a client Adopt replaced is closed only after that.
func (ch *Chain) Client(name string) (client spawnllm.LLMProvider, release func(), err error) {
	if err = ch.cat.life.enter(); err != nil {
		return nil, nil, err
	}
	client, release, err = ch.clientOf(name)
	return client, ch.cat.counted(release, err), err
}

func (ch *Chain) clientOf(name string) (spawnllm.LLMProvider, func(), error) {
	e, ok := ch.gen.models[name]
	if !ok {
		return nil, nil, fmt.Errorf("models: unknown model %q", name)
	}
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

// Chat runs one chat call for id on the chain: Do, calling each model's chat
// client with req.
func (ch *Chain) Chat(ctx context.Context, id accounting.Identity, req Request, opts ...CallOption) (*Result, error) {
	if _, err := id.Normalize(); err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	cfg := newCallConfig(opts)
	var resp *spawnllm.LLMResponse
	c, attempts, err := ch.run(ctx, cfg, func(ctx context.Context, c Candidate) error {
		r, err := ch.chatOnce(ctx, id, c, req, cfg)
		resp = r
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Result{Response: resp, Candidate: c, Attempts: attempts}, nil
}

// Do runs fn for each model of the chain in turn until one succeeds, with
// the rules of every call of a chain; it is how a caller makes a call that
// is not a chat call (speech, images) with its own client for the model
// (Candidate.Spec and ProviderSpec), sharing health, switches, fallback and
// events with chat. It returns the model that succeeded and the attempts
// passed over before it.
//
//   - Disabled and parked models are skipped.
//   - When ctx ends (cancelled or past its deadline) the call stops with
//     ctx.Err(): no further model is called, nothing is recorded against a
//     model, and no FellBack or Exhausted event is sent. A model's own
//     timeout, with ctx still live, is an ordinary failure.
//   - Each other failure is judged by its Verdict (DefaultVerdict, or
//     WithClassify): recorded against the model's health or not, waited out
//     and retried once on the same model, then the next model tried, or the
//     call ended with it.
//   - A success clears the model's failures.
//   - When every model failed or was skipped the error is an
//     *ExhaustedError; when every one was disabled, or there are none, a
//     *NoModelError.
//
// fn is given a copy of the candidate and ctx; it must not keep either.
func (ch *Chain) Do(ctx context.Context, fn func(ctx context.Context, c Candidate) error, opts ...CallOption) (Candidate, []Attempt, error) {
	cfg := newCallConfig(opts)
	return ch.run(ctx, cfg, func(ctx context.Context, c Candidate) error {
		start := ch.clock().Now()
		err := fn(ctx, c.copied())
		if cfg.finish != nil {
			cfg.finish(c.copied(), ch.clock().Since(start), nil, err)
		}
		return err
	})
}

// run is Do's walk, reported as what is serving now and to the observer.
func (ch *Chain) run(ctx context.Context, cfg *callConfig, fn func(context.Context, Candidate) error) (Candidate, []Attempt, error) {
	if err := ch.cat.life.enter(); err != nil {
		return Candidate{}, nil, err
	}
	defer ch.cat.life.leave()
	c, attempts, err := ch.walk(ctx, cfg, fn)
	ch.report(ctx, c, attempts, err)
	if err != nil {
		return Candidate{}, attempts, err
	}
	return c.copied(), attempts, nil
}

func (ch *Chain) walk(ctx context.Context, cfg *callConfig, fn func(context.Context, Candidate) error) (Candidate, []Attempt, error) {
	if len(ch.cands) == 0 {
		return Candidate{}, nil, &NoModelError{Purpose: ch.purpose}
	}
	obs := ch.gen.cfg.observer
	attempts := make([]Attempt, 0, len(ch.cands))
	var pending []Attempt // skips not yet announced
	called := false
	notify := func(passed []Attempt, next Candidate) {
		if cfg.notify != nil {
			cfg.notify(passed, next.copied())
		}
		obs.FellBack(FellBack{Set: ch.set, Purpose: ch.purpose, Scope: ch.scope, Passed: slices.Clone(passed), Next: next.copied()})
	}
	for i, c := range ch.cands {
		if err := ctx.Err(); err != nil {
			return Candidate{}, attempts, err
		}
		if a, skipped := ch.skip(i); skipped {
			called = called || !a.Disabled
			attempts = append(attempts, a)
			pending = append(pending, a)
			continue
		}
		called = true
		if len(pending) > 0 {
			notify(pending, c)
		}
		pending = nil

		var (
			err     error
			v       Verdict
			elapsed time.Duration
		)
		for n := 1; ; n++ {
			start := ch.clock().Now()
			err = fn(ctx, c)
			elapsed = ch.clock().Since(start)
			if err == nil {
				ch.gen.health.success(c.Name, c.Provider, c.Model)
				return c, attempts, nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				// The caller gave up: this is not the model's failure.
				attempts = append(attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: err, Duration: elapsed})
				return Candidate{}, attempts, ctxErr
			}
			v = cfg.verdict(err, c)
			if !v.Next {
				attempts = append(attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: failed(err, c, v), Reason: v.Reason, Duration: elapsed})
				if v.Record {
					ch.gen.health.failure(c.Name, c.Provider, c.Model, v.Reason, v.Status, v.RetryAfter)
				}
				if v.Reason == "" {
					return Candidate{}, attempts, fmt.Errorf("fallback: unclassified error from %s/%s: %w", c.Provider, c.Model, err)
				}
				return Candidate{}, attempts, failed(err, c, v)
			}
			if n == 1 && v.Wait > 0 {
				select {
				case <-ch.clock().After(v.Wait):
				case <-ctx.Done():
					// The caller gave up while waiting: nothing is recorded.
					attempts = append(attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: failed(err, c, v), Reason: v.Reason, Duration: elapsed})
					return Candidate{}, attempts, ctx.Err()
				}
				continue
			}
			break
		}
		if v.Record {
			ch.gen.health.failure(c.Name, c.Provider, c.Model, v.Reason, v.Status, v.RetryAfter)
		}
		attempts = append(attempts, Attempt{Name: c.Name, Provider: c.Provider, Model: c.Model, Err: failed(err, c, v), Reason: v.Reason, Duration: elapsed})
		if i == len(ch.cands)-1 {
			return Candidate{}, attempts, &ExhaustedError{Purpose: ch.purpose, Attempts: slices.Clone(attempts)}
		}
		notify(attempts[len(attempts)-1:], ch.cands[i+1])
	}
	if !called {
		return Candidate{}, attempts, &NoModelError{Purpose: ch.purpose, Detail: "every model is disabled"}
	}
	return Candidate{}, attempts, &ExhaustedError{Purpose: ch.purpose, Attempts: slices.Clone(attempts)}
}

// report records the outcome of a call as what is serving now, and tells the
// observer when the call found no answer.
func (ch *Chain) report(ctx context.Context, answered Candidate, attempts []Attempt, err error) {
	k := selKey{set: ch.set, purpose: ch.purpose, scope: ch.scope}
	now := ch.clock().Now()
	if err == nil {
		ch.cat.state.served(k, ch.from, answered.Name, firstReason(ch, attempts, answered.Name), attempts, now)
		return
	}
	if ctx.Err() != nil {
		return // the caller gave up; nothing about the models to report
	}
	ch.cat.state.served(k, ch.from, "", firstReason(ch, attempts, ""), attempts, now)
	ch.gen.cfg.observer.Exhausted(Exhausted{Set: ch.set, Purpose: ch.purpose, Scope: ch.scope, Attempts: slices.Clone(attempts), Err: err})
}

// firstReason is why the chain's first model did not answer, when another
// did (or none did): "disabled" or its failover reason.
func firstReason(ch *Chain, attempts []Attempt, answered string) string {
	if len(ch.cands) == 0 || ch.cands[0].Name == answered || len(attempts) == 0 {
		return ""
	}
	a := attempts[0]
	if a.Disabled {
		return "disabled"
	}
	return string(a.Reason)
}
