package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// llmMaxRetries is how many times one dispatch is retried after a timeout or
// a context-window overflow (hook.DefaultRetries).
const llmMaxRetries = hook.DefaultRetries

// llmDispatch is one iteration's model request.
type llmDispatch struct {
	defs []spawnllm.ToolDefinition
	// llmOpts are the request options every call shares; the per-model ones
	// (max_tokens, thinking_level) are added for each call by requestOptions.
	llmOpts map[string]any
	// maxTokensHalvings is how many times a context-window retry halved
	// max_tokens; each call halves its model's max_tokens that many times.
	maxTokensHalvings int
	// provider is the config provider name serving the dispatch, updated when
	// the fallback chain lands on another candidate.
	provider string
	// promptEstimate is the engine's prompt-token estimate for the request.
	promptEstimate int
	// sent is what the latest provider call was given before per-model
	// adjustments: the messages plus any per-request note.
	sent []spawnllm.Message
	// answered names the model that answered.
	answered hook.ModelRef
	// supplied: a BeforeModel or OnModelError callback answered, not a model.
	supplied bool
	// rebuilt: the request of the current attempt was built from the session
	// store (hook.ModelInfo.Rebuilt).
	rebuilt bool
	// calling is the model of the provider call in flight, set from its
	// ModelCallStarted to its ModelCall.
	calling *hook.ModelRef
}

// callStarted reports a provider call about to be made.
func (e *engineTurn) callStarted(ctx context.Context, d *llmDispatch, model hook.ModelRef) {
	d.calling = &model
	e.hooks.emit(ctx, hook.ModelCallStarted{EventHeader: e.header(), Iteration: e.iteration, Model: model})
}

// callAbandoned ends, with its ModelCall, a provider call that panicked, so
// every ModelCallStarted has its ModelCall.
func (e *engineTurn) callAbandoned(ctx context.Context, d *llmDispatch) {
	if d.calling == nil {
		return
	}
	if r := recover(); r != nil {
		e.modelCallDone(ctx, d, *d.calling, 0, nil, fmt.Errorf("model call panicked: %v", r))
		panic(r)
	}
}

// newDispatch builds the request for this iteration. The model that will
// serve it first decides which options apply: a CLI takes no HTTP request
// parameters.
func (e *engineTurn) newDispatch(ctx context.Context, defs []spawnllm.ToolDefinition, promptEstimate int) *llmDispatch {
	llmOpts := map[string]any{
		"prompt_cache_key": e.spec.agentID,
	}
	provider, isCLI := e.firstModel()
	// CLI providers run a subprocess and take no HTTP request parameters.
	// With no temperature configured none is sent: the model's default applies.
	if !isCLI && e.spec.temperature != nil {
		llmOpts["temperature"] = *e.spec.temperature
	}
	// Partial text is asked for only when the turn streams (BeforeLoop).
	// Providers that cannot stream text deltas (CLI, Anthropic) ignore this.
	if e.textDeltas {
		iteration := e.iteration
		llmOpts[spawnllm.TextDeltaOption] = func(delta string) {
			ev := hook.TextDelta{EventHeader: e.header(), Iteration: iteration, Text: delta}
			e.hooks.callOnTextDelta(ctx, ev)
			e.hooks.emit(ctx, ev)
		}
	}

	return &llmDispatch{
		defs:           defs,
		llmOpts:        llmOpts,
		provider:       provider,
		promptEstimate: promptEstimate,
	}
}

// requestModel sends the dispatch, retrying a timeout after a backoff and a
// context-window overflow after compressing, up to the spec's retries. Before
// each attempt BeforeModel may change the working request or answer it; a
// dispatch that failed for good goes to OnModelError. An error it returns
// ends the turn; a *hook.PanicError from a callback ends it too.
//
// A cancel wins: an answer (a model's, or one a callback supplied) that
// comes back once the turn's context has ended is discarded, and the
// dispatch fails with ctx.Err() as a call cut short in flight does, so the
// turn ends alike either way. The model's ModelCall has already reported
// the call, with its response and usage.
func (e *engineTurn) requestModel(ctx context.Context, d *llmDispatch) (*spawnllm.LLMResponse, error) {
	var err error
	for retry := 0; retry <= e.spec.retries; retry++ {
		d.rebuilt, e.rebuilt = e.rebuilt, false
		info := e.modelInfo(d)
		answer, perr, berr := e.hooks.callBeforeModel(ctx, info)
		if perr != nil {
			return nil, perr
		}
		e.adoptRequest(d, info)
		if retry == 0 {
			e.logRequest(d)
		}
		if berr != nil {
			err = berr
			break
		}
		if answer != nil {
			if err = ctx.Err(); err == nil {
				d.supplied = true
				return answer, nil
			}
		} else {
			e.logAt(slog.LevelInfo, "LLM dispatch", map[string]any{
				"agent_id":     e.spec.agentID,
				"iteration":    e.iteration,
				"provider":     d.provider,
				"model":        e.spec.model,
				"num_messages": len(e.messages),
				"num_tools":    len(d.defs),
				"max_tokens":   d.maxTokens(e.spec.modelMaxTokens),
			})
			var response *spawnllm.LLMResponse
			response, err = e.callModel(ctx, d)
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				if d.promptEstimate > 0 && response != nil && response.Usage != nil && response.Usage.PromptTokens > 0 {
					e.spec.cm.ObserveUsage(d.promptEstimate, response.Usage.PromptTokens)
				}
				return response, nil
			}
		}

		isTimeoutError, isContextError := classifyLLMError(err, d.provider, e.spec.model)
		if isTimeoutError && retry < e.spec.retries {
			if waitErr := e.waitToRetry(ctx, err, retry); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if isContextError && retry < e.spec.retries {
			e.compressForRetry(ctx, d, err, retry)
			continue
		}
		break
	}
	if ctx.Err() == nil {
		answer, perr, left := e.hooks.callOnModelError(ctx, e.modelInfo(d), err)
		if perr != nil {
			return nil, perr
		}
		switch {
		case answer == nil:
			err = left
		case ctx.Err() == nil:
			d.supplied = true
			return answer, nil
		default:
			err = ctx.Err()
		}
	}
	return nil, e.dispatchFailed(ctx, err)
}

// stopped is the error a turn whose context ended before it took a
// dispatch's answer ends with: the one requestModel fails with when its
// context cuts a dispatch's first call short (a budget that ran out is
// ctx.Err() itself, as waitToRetry returns it), so a turn stopped there ends
// as one stopped in flight.
func (e *engineTurn) stopped(ctx context.Context) error {
	err := ctx.Err()
	if errors.Is(err, context.DeadlineExceeded) && e.spec.retries > 0 {
		return err
	}
	return e.dispatchFailed(ctx, err)
}

// modelInfo is the dispatch as the model callbacks see it: the turn's
// working request, and whether the current attempt's was built from the
// session store.
func (e *engineTurn) modelInfo(d *llmDispatch) *hook.ModelInfo {
	return &hook.ModelInfo{
		Iteration: e.iteration,
		Model:     e.turnModelRef(),
		Request:   hook.ModelRequest{Messages: e.messages, Tools: d.defs, Options: d.llmOpts},
		Rebuilt:   d.rebuilt,
	}
}

// adoptRequest takes the working request as BeforeModel left it.
func (e *engineTurn) adoptRequest(d *llmDispatch, info *hook.ModelInfo) {
	e.messages = info.Request.Messages
	d.defs = info.Request.Tools
	if info.Request.Options != nil {
		d.llmOpts = info.Request.Options
	}
}

// waitToRetry waits out the backoff, on the spec's clock, before retrying a
// timed-out dispatch. It returns the context's error when the turn's budget
// ran out first; such a turn is not retried, so no retry is logged.
func (e *engineTurn) waitToRetry(ctx context.Context, err error, retry int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	backoff := withJitter(time.Duration(retry+1) * e.spec.retryBackoff)
	e.logAt(slog.LevelWarn, "Timeout error, retrying after backoff", map[string]any{
		"error":   e.redacted(err),
		"retry":   retry,
		"backoff": backoff.String(),
	})
	timer := e.spec.clock.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

// compressForRetry answers a context-window error: the turn is told (an
// OnCompaction and a Compaction event, phase overflow, for the dispatch's
// first attempt), the session is force-compressed and the request
// reassembled. When that does not shrink the history, max_tokens is halved
// instead, since the provider's per-request limit may be below the model's
// stated maximum (e.g. Groq's free tier).
func (e *engineTurn) compressForRetry(ctx context.Context, d *llmDispatch, err error, retry int) {
	e.logAt(slog.LevelWarn, "Context window error detected, attempting compression",
		map[string]any{
			"error": e.redacted(err),
			"retry": retry,
		})

	if retry == 0 {
		ev := hook.Compaction{EventHeader: e.header(), Phase: hook.CompactionOverflow, Reason: "overflow", Blocking: true}
		e.hooks.callOnCompaction(ctx, ev)
		e.hooks.emit(ctx, ev)
	}

	prevMsgCount := len(e.messages)
	comprMgr, releaseComprMgr := e.spec.env.compressionManager()
	e.releases = append(e.releases, releaseComprMgr)
	if ferr := comprMgr.ForceCompress(ctx); ferr != nil {
		e.logAt(slog.LevelWarn, "force compression failed",
			map[string]any{"error": ferr.Error(), "session": e.spec.scope.Session})
	}
	if asm, berr := comprMgr.Assemble(ctx, e.spec.env.assembleRequest(ctx, d.defs, e.messages)); berr == nil {
		e.messages = asm.Messages
		e.rebuilt = true
		d.promptEstimate = asm.PromptTokenEstimate
	}

	if len(e.messages) < prevMsgCount {
		return
	}
	// Counted on every such retry: each model's max_tokens is halved by
	// d.maxTokens down to its own floor, so a selected model already at the
	// floor still lets a larger fallback be lowered.
	current := d.maxTokens(e.spec.modelMaxTokens)
	d.maxTokensHalvings++
	e.logAt(slog.LevelWarn, "History already minimal; reducing max_tokens for retry",
		map[string]any{
			"agent_id":       e.spec.agentID,
			"model":          e.spec.model,
			"old_max_tokens": current,
			"new_max_tokens": d.maxTokens(e.spec.modelMaxTokens),
		})
}

// dispatchFailed is the error a dispatch that failed for good ends the turn
// with: an interruption by shutdown (a cause wrapping hook.ErrShutdown), a
// deliberate stop (hook.ErrInterrupted, hook.ErrSuperseded), or a failure.
func (e *engineTurn) dispatchFailed(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, hook.ErrShutdown) {
		return fmt.Errorf("LLM call interrupted: %w", context.Cause(ctx))
	}
	if errors.Is(cause, hook.ErrInterrupted) || errors.Is(cause, hook.ErrSuperseded) {
		e.logAt(slog.LevelInfo, "LLM call stopped: the turn was cancelled",
			map[string]any{
				"agent_id":  e.spec.agentID,
				"iteration": e.iteration,
				"model":     e.spec.model,
				"cause":     context.Cause(ctx).Error(),
			})
		return fmt.Errorf("LLM call stopped: %w: %w", context.Cause(ctx), err)
	}
	e.logAt(slog.LevelError, "LLM call failed",
		map[string]any{
			"agent_id":  e.spec.agentID,
			"iteration": e.iteration,
			"model":     e.spec.model,
			"error":     e.redacted(err),
		})
	return fmt.Errorf("LLM call failed after retries: %w", err)
}

// modelCallDone reports one provider call: a ModelCall event with the
// provider's own response and a snapshot of the request the call was given.
// Once the turn's context has ended the call's error is the context's.
func (e *engineTurn) modelCallDone(ctx context.Context, d *llmDispatch, model hook.ModelRef, elapsed time.Duration, resp *spawnllm.LLMResponse, err error) {
	d.calling = nil
	if err != nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	e.hooks.emit(ctx, hook.ModelCall{
		EventHeader: e.header(),
		Iteration:   e.iteration,
		Model:       model,
		Request: &hook.ModelRequest{
			Messages: slices.Clone(d.sent),
			Tools:    slices.Clone(d.defs),
			Options:  maps.Clone(d.llmOpts),
		},
		Response: resp,
		Err:      err,
		Elapsed:  elapsed,
	})
}

// errNoModelConfigured ends a turn whose agent has no model to call. Its
// text is the one a configuration without models has always answered with.
// It matches hook.ErrNoModelAvailable (errors.Is), so a host can tell it and
// render its own text.
var errNoModelConfigured error = noModelError{}

// noModelError is errNoModelConfigured's type.
type noModelError struct{}

func (noModelError) Error() string {
	return "no model configured — enable a model and add your API key in the configuration"
}

func (noModelError) Unwrap() error { return hook.ErrNoModelAvailable }

// callModel makes one call for the dispatch through the turn's pinned chain:
// each model in turn, skipping parked ones, until one answers. Every provider
// call it makes is reported as a ModelCallStarted and a ModelCall, and each
// candidate passed over is logged as the chain moves on. A turn with no
// model to call fails without a provider call.
func (e *engineTurn) callModel(ctx context.Context, d *llmDispatch) (*spawnllm.LLMResponse, error) {
	hasTools := len(d.defs) > 0
	if e.spec.chain == nil || len(e.spec.candidates) == 0 {
		d.sent = e.messages
		return nil, errNoModelConfigured
	}
	defer e.callAbandoned(ctx, d)
	fbResult, err := e.spec.chain.Chat(ctx, e.spec.scope.Identity(), models.Request{Tools: d.defs},
		// Image parts are dropped for a candidate whose model cannot take
		// them: a screenshot in history would otherwise fail every turn on
		// a non-vision candidate.
		models.WithPrepare(func(c models.Candidate, client spawnllm.LLMProvider, _ models.Request) models.Request {
			e.callStarted(ctx, d, candidateRef(c))
			d.sent = e.requestMessages(hasTools, isCLIClient(client))
			return models.Request{
				Messages: messagesForModel(d.sent, c.Limits.Vision, c.ProviderSpec.RequireReasoningContent, hasTools),
				Tools:    d.defs,
				Options:  e.requestOptions(d, hasTools, client, c),
			}
		}),
		models.WithNotify(func(passed []models.Attempt, next models.Candidate) {
			e.logPassed(passed)
			e.fellBack(ctx, passed, next)
		}),
		models.WithFinish(func(c models.Candidate, elapsed time.Duration, resp *spawnllm.LLMResponse, err error) {
			e.modelCallDone(ctx, d, candidateRef(c), elapsed, resp, err)
		}),
	)
	if err != nil {
		// Cooldown skips at the tail of the chain reach no notifier.
		if exhausted, ok := errors.AsType[*models.ExhaustedError](err); ok {
			e.logPassed(trailingSkips(exhausted.Attempts))
		}
		return nil, err
	}
	d.provider = fbResult.Candidate.Provider
	d.answered = candidateRef(fbResult.Candidate)
	if len(fbResult.Attempts) > 0 {
		e.logAt(slog.LevelInfo,
			fmt.Sprintf("Fallback: succeeded with %s/%s after %d attempts",
				fbResult.Candidate.Provider, fbResult.Candidate.Model, len(fbResult.Attempts)+1),
			map[string]any{"agent_id": e.spec.agentID, "iteration": e.iteration},
		)
	}
	return fbResult.Response, nil
}

// fellBack reports the chain moving past passed to next: OnFallback, then a
// Fallback event.
func (e *engineTurn) fellBack(ctx context.Context, passed []models.Attempt, next models.Candidate) {
	ev := hook.Fallback{EventHeader: e.header(), Iteration: e.iteration, Next: candidateRef(next)}
	for _, a := range passed {
		reason := string(a.Reason)
		if a.Disabled && reason == "" {
			reason = "disabled"
		}
		ev.Passed = append(ev.Passed, hook.Attempt{
			Model:  hook.ModelRef{Name: a.Name, Provider: a.Provider, Model: a.Model},
			Err:    a.Err,
			Reason: reason, Skipped: a.Skipped, Disabled: a.Disabled,
			Remaining: a.Remaining, Duration: a.Duration,
		})
	}
	e.hooks.callOnFallback(ctx, ev)
	e.hooks.emit(ctx, ev)
}

// requestOptions are the options for one provider call to candidate c,
// served by provider: the shared options plus that model's max_tokens
// (lowered by any context-window retry) and thinking level. On the turn's
// last step, where the low-steps notice tells the model not to call tools,
// the call also forbids tool calls (tool_choice none). The tools stay
// declared, so earlier tool calls in the history and the prompt cache stay
// valid. d.llmOpts is shared by every candidate the fallback chain tries, so
// it is copied rather than changed.
func (e *engineTurn) requestOptions(d *llmDispatch, hasTools bool, provider spawnllm.LLMProvider, c models.Candidate) map[string]any {
	opts := make(map[string]any, len(d.llmOpts)+3)
	maps.Copy(opts, d.llmOpts)
	opts["max_tokens"] = d.maxTokens(c.Limits.MaxTokens)
	// modelspec.ParseThinkingLevel guarantees ThinkingOff for empty/unknown values.
	if level := string(c.Limits.ThinkingLevel); level != string(modelspec.ThinkingOff) {
		if tc, ok := provider.(spawnllm.ThinkingCapable); ok && tc.SupportsThinking() {
			opts["thinking_level"] = level
		} else {
			e.logAt(slog.LevelWarn, "thinking_level is set but current provider does not support it, ignoring",
				map[string]any{"agent_id": e.spec.agentID, "model": c.Name, "thinking_level": level})
		}
	}
	if !hasTools || e.iteration != e.spec.maxIterations {
		return opts
	}
	if isCLIClient(provider) {
		return opts
	}
	opts[spawnllm.ToolChoiceOption] = spawnllm.ToolChoiceNone
	return opts
}

// isCLIClient reports whether the client runs a CLI subprocess.
func isCLIClient(p spawnllm.LLMProvider) bool {
	c, ok := p.(spawnllm.CLIProvider)
	return ok && c.IsCLI()
}

// minHalvedMaxTokens is the max_tokens at or below which a context-window
// retry stops halving it.
const minHalvedMaxTokens = 512

// maxTokens is the max_tokens a call to a model whose own is base sends:
// base halved once per context-window retry that lowered it, never from a
// value at or below minHalvedMaxTokens.
func (d *llmDispatch) maxTokens(base int) int {
	for range d.maxTokensHalvings {
		if base <= minHalvedMaxTokens {
			break
		}
		base /= 2
	}
	return base
}

// trailingSkips returns the cooldown skips after the last candidate called,
// which no notifier saw.
func trailingSkips(attempts []models.Attempt) []models.Attempt {
	i := len(attempts)
	for i > 0 && attempts[i-1].Skipped {
		i--
	}
	return attempts[i:]
}

// logPassed logs each attempt the chain passed over: a failed candidate or a
// cooldown skip.
func (e *engineTurn) logPassed(attempts []models.Attempt) {
	for _, attempt := range attempts {
		if !attempt.Skipped {
			e.logAt(slog.LevelWarn, "Fallback: candidate failed",
				map[string]any{
					"agent_id": e.spec.agentID,
					"provider": attempt.Provider,
					"model":    attempt.Model,
					"reason":   attempt.Reason,
					"duration": attempt.Duration.Round(time.Millisecond),
					"error":    e.redacted(attempt.Err),
				})
			continue
		}
		e.logAt(slog.LevelWarn, "Fallback: skipped candidate (cooldown)",
			map[string]any{
				"agent_id":  e.spec.agentID,
				"provider":  attempt.Provider,
				"model":     attempt.Model,
				"reason":    attempt.Reason,
				"remaining": attempt.Remaining.Round(time.Second),
				"error":     e.redacted(attempt.Err),
			})
	}
}

// reasoningPlaceholder is what backfillReasoningContent writes into an empty
// reasoning_content. A single space, deliberately: DeepSeek V4 Pro rejects the
// empty string as "not passed back", so the value must be non-empty, and
// anything longer would be fabricated reasoning the model never produced.
const reasoningPlaceholder = " "

// backfillReasoningContent gives every assistant message a non-empty
// reasoning_content when the target provider demands one.
//
// DeepSeek V4 thinking mode requires the field on every assistant message in
// history whenever the request carries tools — including turns that made no tool
// call — and answers 400 without it. Messages written by a model that records no
// reasoning (any CLI provider, or the same model with thinking off) therefore
// wedge a session the moment it is pointed at DeepSeek: the same history replays
// every turn and is rejected every turn, and the fallback chain cannot rescue it
// because a 400 is not retriable.
//
// Three conditions, all necessary:
//   - the provider opts in via require_reasoning_content (required);
//   - the request carries tools, since without them DeepSeek ignores the field
//     entirely and there is nothing to satisfy;
//   - the message is an assistant turn whose reasoning_content is empty. Real
//     reasoning is never touched, so a thinking model's own output round-trips
//     unchanged.
//
// The input slice is not modified: callers share `messages` across fallback
// candidates, and one candidate's wire quirk must not follow the history to the
// next.
func backfillReasoningContent(messages []spawnllm.Message, required, hasTools bool) []spawnllm.Message {
	if !hasTools || !required {
		return messages
	}

	// Copy lazily: most turns need no backfill at all once a thinking model has
	// been running, and copying the whole slice per dispatch is wasted work.
	out := messages
	copied := false
	for i := range messages {
		if messages[i].Role != "assistant" || messages[i].ReasoningContent != "" {
			continue
		}
		if !copied {
			out = make([]spawnllm.Message, len(messages))
			copy(out, messages)
			copied = true
		}
		out[i].ReasoningContent = reasoningPlaceholder
	}
	return out
}

// messagesForModel returns messages suitable for a model whose vision mode
// is vision and whose provider may require reasoning content.
// Vision-capable models get the slice unchanged; models with vision off or
// unset get a shallow copy with image Media removed. Some providers (e.g.
// deepseek via OpenRouter) reject the ENTIRE request with a 404 when an image
// part is present, so a screenshot left in conversation history must not be
// replayed to a non-vision model. The persisted originals are never mutated,
// so a later turn on a vision model still surfaces the image. An unknown
// model is treated as no-vision, matching the tool-image injection default.
func messagesForModel(messages []spawnllm.Message, vision string, requireReasoning, hasTools bool) []spawnllm.Message {
	// Reasoning backfill runs first and independently of the media rules below:
	// it is a wire-contract requirement, not a capability downgrade.
	messages = backfillReasoningContent(messages, requireReasoning, hasTools)

	if vision == modelspec.VisionUserMessage || vision == modelspec.VisionToolResponse {
		return messages
	}
	hasMedia := false
	for i := range messages {
		if len(messages[i].Media) > 0 {
			hasMedia = true
			break
		}
	}
	if !hasMedia {
		return messages
	}
	out := make([]spawnllm.Message, len(messages))
	copy(out, messages)
	for i := range out {
		if len(out[i].Media) == 0 {
			continue
		}
		n := len(out[i].Media)
		out[i].Media = nil
		// Say so instead of stripping silently: the model should know the message
		// carried attachments it cannot see (any media:// refs remain in the text
		// via the attachment marker and can be delegated via agent_spawn).
		out[i].Content = strings.TrimRight(out[i].Content, "\n") +
			fmt.Sprintf("\n[%d attachment(s) on this message are hidden — the current model cannot view images]", n)
	}
	return out
}

// backoffJitterFraction spreads a retry backoff by ±20% so concurrent turns
// retrying the same provider do not land on it in lockstep.
const backoffJitterFraction = 0.2

// withJitter returns d scaled by a random factor in [1-jitter, 1+jitter].
func withJitter(d time.Duration) time.Duration {
	f := 1 + backoffJitterFraction*(2*rand.Float64()-1) //nolint:gosec // retry spread, not security-relevant
	return time.Duration(float64(d) * f)
}

// localContextLimitPatterns are context-window/token-limit markers that
// spawnllm's ClassifyError does not recognise. Candidates for upstreaming;
// remove each one here once spawnllm classifies it.
var localContextLimitPatterns = []string{
	"context window",
	"token limit",
	"max_tokens",
	"invalidparameter",
}

// classifyLLMError sorts a dispatch error into the two retry buckets the turn
// acts on: a transient timeout (retry after backoff) or a context-window
// overflow (compress and retry). spawnllm's classifier is the source of
// truth; the local patterns cover only what it does not classify.
func classifyLLMError(err error, provider, model string) (isTimeout, isContext bool) {
	cls := spawnllm.ClassifyError(err, provider, model)
	if errors.Is(err, context.DeadlineExceeded) || (cls != nil && cls.Reason == spawnllm.FailoverTimeout) {
		return true, false
	}
	if cls != nil && cls.Reason == spawnllm.FailoverContextLimit {
		return false, true
	}
	msg := strings.ToLower(err.Error())
	for _, p := range localContextLimitPatterns {
		if strings.Contains(msg, p) {
			return false, true
		}
	}
	var exhausted *models.ExhaustedError
	if errors.As(err, &exhausted) && exhausted.AllContextLimit() {
		return false, true
	}
	return false, false
}
