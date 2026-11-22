package loop

// This file is part of the turn engine (the engine_*.go files): it imports no
// host package and uses no host identifier, so it can live in a package of
// its own. engine_guard_test.go enforces that.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// maxIdenticalToolBatches caps how many consecutive iterations may request the
// exact same tool-call batch before the loop aborts (degenerate-loop guard).
// The model is steered (told it is repeating, and to re-read/adjust) on each
// repeat before this cap, so the cap allows a couple of correction chances.
const maxIdenticalToolBatches = 4

// maxEmptyRetries caps how many times the loop pokes the model to actually reply
// after it finished a turn with no visible content but non-empty reasoning (a
// common degenerate-model failure mode). After the cap the caller sends a
// graceful fallback rather than leaving the user with no answer.
const maxEmptyRetries = 2

// lowStepsWarnAt is the number of iterations remaining in the turn (the
// current one included) at or below which each request tells the model how
// many tool steps it has left.
const lowStepsWarnAt = 4

// lowStepsNote is the note added to the request of iteration iteration
// (1-based) of a turn capped at maxIterations, or "" when enough steps remain.
// It reports one step fewer than actually remain, so the model always has a
// spare step to give its reply in.
func lowStepsNote(iteration, maxIterations int) string {
	remaining := maxIterations - iteration + 1
	if remaining > lowStepsWarnAt || remaining < 1 {
		return ""
	}
	switch told := remaining - 1; told {
	case 0:
		return "[System notice] This is your last step in this turn. Do not call tools; reply now with your findings and next steps."
	case 1:
		return "[System notice] You have 1 tool step left in this turn. Save any work in progress and give your reply with your findings and next steps before it runs out."
	default:
		return fmt.Sprintf("[System notice] You have %d tool steps left in this turn. Save any work in progress and give your reply with your findings and next steps before they run out.", told)
	}
}

// turnMemory is what the engine uses of a session's long-term memory. A nil
// *cogmem.Session satisfies it and does nothing.
type turnMemory interface {
	Observe(ctx context.Context, seq int64, role, text string)
	RecordToolUse(names ...string)
}

// turnEnv is what the engine reaches the host's services through, besides
// the callbacks: tools and the session's context.
type turnEnv interface {
	turnTools
	turnSession
}

// turnTools are the agent's tools.
type turnTools interface {
	// toolDefs is the agent's tool list for a dispatch.
	toolDefs() []spawnllm.ToolDefinition
	// executeTool runs one tool call on ctx (the call's context).
	executeTool(ctx context.Context, name string, args map[string]any) *toolkit.Result
}

// turnSession is the session's context and store.
type turnSession interface {
	// assembleRequest is the context engine's request for a dispatch that
	// sends defs over history.
	assembleRequest(ctx context.Context, defs []spawnllm.ToolDefinition, history []spawnllm.Message) ctxengine.AssembleRequest
	// compressionManager is the session's context manager for a forced
	// compaction, with its release.
	compressionManager() (ctxengine.ContextManager, func())
	// firstRequest is the context engine's request for the turn's first
	// assembly, before the turn has a request of its own.
	firstRequest(ctx context.Context) ctxengine.AssembleRequest
	// sessionSummary is the session's compacted summary, for the logs.
	sessionSummary() string
}

// turnStore is what the engine uses of the session store: the pending-turn
// marker and the save after the reply (session.SessionStore has them).
type turnStore interface {
	SetPendingTurn(sessionKey string) error
	ClearPendingTurn(sessionKey string) error
	Save(key string) error
}

// engineSpec is everything one turn of the engine runs with.
type engineSpec struct {
	// agentID names the agent in logs and is the prompt cache key.
	agentID string
	// scope is the turn's scope; its Identity is the accounting identity of
	// every model call.
	scope hook.Scope
	// message is what the turn answers, and options the run's options, as
	// TurnStarted reports them.
	message hook.Message
	options hook.RunOptions

	maxIterations int
	// onHandle, when set, is given the turn's handle as the turn starts,
	// before TurnStarted: the means to stop it.
	onHandle func(*turnHandle)
	// cancelGrace is how long a stopped turn waits for its tools before
	// abandoning them; 0 means hook.DefaultCancelGrace.
	cancelGrace time.Duration
	// toolTimeout is each tool call's budget, read for each batch; 0 means
	// none.
	toolTimeout  func() time.Duration
	retries      int
	retryBackoff time.Duration
	temperature  *float64
	// repeatHint is extra steering for a repeated batch of the named tools
	// ("" for none).
	repeatHint func(toolNames []string) string

	clock clock.Clock
	// log is the logger; the engine's own lines carry component "agent".
	log        *slog.Logger
	logContent func() bool
	redact     hook.Redactor
	// truncate shortens the content the "Full LLM request" line logs; nil
	// means hook.Truncate.
	truncate hook.Truncator

	// The turn's model choice: the pinned chain, its candidates, the first
	// one's name and the limits of that model.
	chain          *models.Chain
	candidates     []models.Candidate
	model          string
	modelMaxTokens int
	contextWindow  int
	noTools        bool

	cm    ctxengine.ContextManager
	store turnStore
	mem   turnMemory
	env   turnEnv
	hooks *turnHooks
}

// engineResult is how the model loop of a turn ended.
type engineResult struct {
	content      string
	normal       bool
	degenerate   bool
	finishReason string
	iterations   int
	// images counts the images the model generated in the turn, lastImages
	// those of the last response.
	images     int
	lastImages int
	// replyLines are the lines AfterModel added for the reply over the turn;
	// notes those for the stored reply not yet written into a stored
	// assistant message.
	replyLines []string
	notes      []string
}

// engineTurn is one turn's model loop: what the turn was started with and
// the state carried from one iteration to the next.
type engineTurn struct {
	spec   engineSpec
	hooks  *turnHooks
	handle *turnHandle

	// messages is the request as sent: stored history plus this turn's
	// unsaved steering messages, with media already resolved.
	messages []spawnllm.Message
	// rebuilt: messages were built from the session store since the last
	// dispatch attempt took them (hook.ModelInfo.Rebuilt).
	rebuilt bool
	// releases are the context managers taken for compression, held until
	// the turn ends.
	releases []func()

	// pending: the pending-turn marker was set.
	pending bool
	// textDeltas: the turn's model calls ask for partial text, as the
	// BeforeLoop callbacks left hook.Loop.TextDeltas.
	textDeltas bool

	iteration    int
	finalContent string
	lastNormal   bool
	finishReason string
	// degenerate marks a reply that stayed empty after every poke.
	degenerate   bool
	emptyRetries int
	// lastToolSig and identicalToolBatches detect a model repeating the
	// exact same tool-call batch.
	lastToolSig          string
	identicalToolBatches int

	imageCount int
	lastImages int
	replyLines []string
	// pendingNotes are the AfterModel notes not yet written into a stored
	// assistant message.
	pendingNotes []string
}

// newEngineTurn prepares the model loop of one turn over messages.
func newEngineTurn(spec engineSpec, messages []spawnllm.Message) *engineTurn {
	if spec.hooks == nil {
		spec.hooks = &turnHooks{}
	}
	if spec.clock == nil {
		spec.clock = clock.Real
	}
	if spec.log == nil {
		spec.log = slog.New(slog.DiscardHandler)
	}
	if spec.logContent == nil {
		spec.logContent = func() bool { return false }
	}
	if spec.redact == nil {
		spec.redact = func(s string) string { return s }
	}
	if spec.truncate == nil {
		spec.truncate = hook.Truncate
	}
	if spec.mem == nil {
		spec.mem = noTurnMemory{}
	}
	return &engineTurn{
		spec:         spec,
		hooks:        spec.hooks,
		messages:     messages,
		finishReason: "max_iterations",
	}
}

// turnHandle is a running turn's handle: the means to stop it with a cause,
// and its end. The run hands it to spec.onHandle as the turn starts.
type turnHandle struct {
	// ctx is the turn's context; OnInterrupt observers get it without its
	// cancellation.
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	result hook.TurnResult
	// hooks and scope are the turn's, for OnInterrupt.
	hooks *turnHooks
	scope hook.Scope
}

// Interrupt stops the turn with hook.ErrInterrupted.
func (h *turnHandle) Interrupt() { h.Cancel(hook.ErrInterrupted) }

// Cancel stops the turn with cause (nil means hook.ErrInterrupted), then
// tells the OnInterrupt observers, on the turn's context without its
// cancellation.
func (h *turnHandle) Cancel(cause error) { h.cancelFrom(h.ctx, cause) }

// cancelFrom is Cancel for a caller with a context of its own: the
// OnInterrupt observers get ctx, without its cancellation.
func (h *turnHandle) cancelFrom(ctx context.Context, cause error) {
	h.cancelWith(ctx, cause, h.running())
}

// running reports whether the turn has not finished yet.
func (h *turnHandle) running() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// cancelWith stops the turn with cause (nil means hook.ErrInterrupted) and
// tells the OnInterrupt observers, on ctx without its cancellation, that a
// turn was stopped when running says one was (read before the caller
// started stopping it).
func (h *turnHandle) cancelWith(ctx context.Context, cause error, running bool) {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	h.cancel(cause)
	h.hooks.callOnInterrupt(hook.WithScope(context.WithoutCancel(ctx), h.scope),
		hook.Interrupted{Scope: h.scope, Cause: cause, Running: running})
}

// Done is closed once TurnFinished has been emitted.
func (h *turnHandle) Done() <-chan struct{} { return h.done }

// Wait blocks until the turn finished and returns its result.
func (h *turnHandle) Wait() hook.TurnResult {
	<-h.done
	return h.result
}

// header is an event header for the turn, now.
func (e *engineTurn) header() hook.EventHeader {
	return hook.EventHeader{Scope: e.spec.scope, At: e.spec.clock.Now()}
}

// noTurnMemory is a session without long-term memory.
type noTurnMemory struct{}

func (noTurnMemory) Observe(context.Context, int64, string, string) {}
func (noTurnMemory) RecordToolUse(...string)                        {}

// errEngineIncomplete ends a turn whose spec lacks the session's context
// manager or store, or the host's services.
var errEngineIncomplete = errors.New("turn engine: no context manager, session store or host services")

// run runs one turn, its whole span: TurnStarted; BeforeTurn; the session
// reset and the message stored (unless the run says it already is); the
// pending-turn marker; BeforeLoop and LoopStarted; the model loop;
// AfterTurn; the reply stored and the session saved; the marker cleared
// (kept for a turn shutdown stopped); exactly one TurnFinished, last. Its
// error is the TurnResult's Err; the TurnResult is the handle's. A panic in
// the loop itself (not in a callback) still ends the turn with OutcomeError
// in its TurnFinished, then goes on to the caller. A spec without host services is refused before the
// turn starts (errEngineIncomplete): no event is emitted.
func (e *engineTurn) run(ctx context.Context) (res engineResult, err error) {
	if e.spec.env == nil {
		return engineResult{}, errEngineIncomplete
	}
	ctx, cancel := context.WithCancelCause(ctx)
	e.handle = &turnHandle{ctx: ctx, cancel: cancel, done: make(chan struct{}), hooks: e.hooks, scope: e.spec.scope}
	defer cancel(nil)
	if e.spec.onHandle != nil {
		e.spec.onHandle(e.handle)
	}

	e.hooks.emit(ctx, hook.TurnStarted{EventHeader: e.header(), Message: e.spec.message, Options: e.spec.options})
	turn := &hook.Turn{Message: e.spec.message, Options: e.spec.options}
	var tr hook.TurnResult
	stage := stageBeforeTurn
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		e.releaseCompressionManagers()
		if stage < stageFinished {
			// The turn still ends as any turn does: AfterTurn sees the
			// failure, nothing is stored, the marker is cleared.
			tr = e.turnResult(ctx, fmt.Errorf("turn engine: panic: %v", r))
			tr.Outcome = hook.OutcomeError
			if stage >= stageStored && stage < stageAfterTurn {
				e.hooks.callAfterTurn(context.WithoutCancel(ctx), turn, &tr)
			}
			tr.Reply, tr.Stored = "", ""
			if stage >= stageStored {
				e.clearPending(tr)
			}
			e.finish(ctx, tr)
		}
		panic(r)
	}()

	answer, err := e.hooks.callBeforeTurn(ctx, turn)
	switch {
	case err != nil:
		tr = hook.TurnResult{Outcome: hook.OutcomeError, Err: err}
	case answer != nil:
		tr = *answer
		tr.Skipped = true
	default:
		stage = stageStored
		err = e.begin(ctx, turn)
		if err == nil {
			// A BeforeLoop failure ends the turn as a failed loop: the
			// marker is cleared.
			stage = stageLoop
			err = e.startLoop(ctx, turn)
		}
		if err == nil {
			res, err = e.loop(ctx)
		}
		e.releaseCompressionManagers()
		tr = e.turnResult(ctx, err)
	}
	afterLoop := stage >= stageLoop
	stage = stageAfterTurn
	e.hooks.callAfterTurn(context.WithoutCancel(ctx), turn, &tr)
	if afterLoop {
		e.storeReply(ctx, &tr)
		e.clearPending(tr)
	}
	stage = stageFinished
	e.finish(ctx, tr)
	return res, tr.Err
}

// The stages of a turn, for what a panic in the loop still owes.
const (
	stageBeforeTurn = iota
	stageStored
	stageLoop
	stageAfterTurn
	stageFinished
)

// finish emits TurnFinished and closes the handle.
func (e *engineTurn) finish(ctx context.Context, tr hook.TurnResult) {
	if t, ok := turnOf(ctx); ok && t.hooks == e.hooks {
		t.finished.Store(true)
	}
	e.hooks.emit(ctx, hook.TurnFinished{EventHeader: e.header(), TurnResult: tr})
	e.handle.result = tr
	close(e.handle.done)
}

// begin readies the session for the model loop: reset when the run asks,
// the message stored unless it already is, the first request assembled, the
// pending-turn marker set.
func (e *engineTurn) begin(ctx context.Context, turn *hook.Turn) error {
	if e.spec.cm == nil || e.spec.store == nil {
		return errEngineIncomplete
	}
	if turn.Options.ResetSession {
		if err := e.spec.cm.Reset(ctx); err != nil {
			e.logAt(slog.LevelWarn, "session reset failed", map[string]any{
				"session_key": e.spec.scope.Session,
				"error":       err.Error(),
			})
		}
	}
	if !turn.Options.AlreadyStored {
		msg := spawnllm.Message{Role: "user", Content: turn.Message.Text, Media: turn.Message.Media, Source: turn.Message.Source, Type: turn.Message.Kind}
		seq, err := e.spec.cm.AddUserMessage(ctx, msg)
		if err != nil {
			return &hook.SessionStoreError{What: "user message", Err: err}
		}
		e.spec.mem.Observe(ctx, seq, msg.Role, msg.Content)
	}
	asm, err := e.spec.cm.Assemble(ctx, e.spec.env.firstRequest(ctx))
	if err != nil {
		return fmt.Errorf("context manager assemble: %w", err)
	}
	e.messages = asm.Messages
	e.rebuilt = true
	if err := e.spec.store.SetPendingTurn(e.spec.scope.Session); err != nil {
		e.logAt(slog.LevelWarn, "Failed to set pending turn flag",
			map[string]any{"error": err.Error(), "session": e.spec.scope.Session})
	}
	e.pending = true
	return nil
}

// startLoop runs the BeforeLoop callbacks, which decide whether the turn's
// model calls ask for partial text (by default when an OnTextDelta is
// registered), then emits LoopStarted. A callback's error or panic (a
// *hook.PanicError) is returned: it ends the turn before its first model
// call, with no LoopStarted.
func (e *engineTurn) startLoop(ctx context.Context, turn *hook.Turn) error {
	loop := &hook.Loop{Turn: *turn, TextDeltas: len(e.hooks.onTextDelta) > 0}
	if err := e.hooks.callBeforeLoop(ctx, loop); err != nil {
		return err
	}
	e.textDeltas = loop.TextDeltas
	e.hooks.emit(ctx, hook.LoopStarted{EventHeader: e.header()})
	return nil
}

// loop runs the model loop to its end.
func (e *engineTurn) loop(ctx context.Context) (engineResult, error) {
	e.logDispatchTarget()
	for e.iteration < e.spec.maxIterations {
		e.iteration++
		done, ierr := e.iterate(ctx)
		if ierr != nil {
			return engineResult{iterations: e.iteration}, ierr
		}
		if done {
			break
		}
	}
	return engineResult{
		content:      e.finalContent,
		normal:       e.lastNormal,
		degenerate:   e.degenerate,
		finishReason: e.finishReason,
		iterations:   e.iteration,
		images:       e.imageCount,
		lastImages:   e.lastImages,
		replyLines:   e.replyLines,
		notes:        e.pendingNotes,
	}, nil
}

// storeReply stores tr.Stored and saves the session, when there is a reply
// to store: a refused store fails the turn, a failed save is SaveErr.
func (e *engineTurn) storeReply(ctx context.Context, tr *hook.TurnResult) {
	if tr.Stored == "" {
		return
	}
	msg := spawnllm.Message{Role: "assistant", Content: tr.Stored}
	seq, err := e.spec.cm.AddAssistantMessage(ctx, msg)
	if err != nil {
		tr.Err = &hook.SessionStoreError{What: "assistant reply", Err: err}
		tr.SaveErr = tr.Err
		tr.Outcome = hook.OutcomeError
		return
	}
	e.spec.mem.Observe(ctx, seq, msg.Role, msg.Content)
	if err := e.spec.store.Save(e.spec.scope.Session); err != nil {
		tr.SaveErr = &hook.SessionStoreError{What: "session save", Err: err}
	}
}

// clearPending clears the pending-turn marker, unless shutdown stopped the
// turn: a restart replays it.
func (e *engineTurn) clearPending(tr hook.TurnResult) {
	if !e.pending || (tr.Err != nil && errors.Is(tr.Cause, hook.ErrShutdown)) {
		return
	}
	if err := e.spec.store.ClearPendingTurn(e.spec.scope.Session); err != nil {
		e.logAt(slog.LevelWarn, "Failed to clear pending turn flag",
			map[string]any{"error": err.Error(), "session": e.spec.scope.Session})
	}
}

// turnResult is the TurnResult the model loop's end reports, before
// AfterTurn: its outcome from the error and the cancel cause
// (hook.CauseOutcome), the reply and what to store.
func (e *engineTurn) turnResult(ctx context.Context, err error) hook.TurnResult {
	tr := hook.TurnResult{
		Err:          err,
		FinishReason: e.finishReason,
		Normal:       e.lastNormal,
		Degenerate:   e.degenerate,
		Iterations:   e.iteration,
		Images:       e.imageCount,
	}
	if ctx.Err() != nil {
		tr.Cause = context.Cause(ctx)
		if errors.Is(tr.Cause, context.DeadlineExceeded) && !errors.Is(tr.Cause, hook.ErrDeadline) {
			tr.Cause = fmt.Errorf("%w: %w", hook.ErrDeadline, tr.Cause)
		}
	}
	switch {
	case err != nil && tr.Cause != nil:
		tr.Outcome = hook.CauseOutcome(tr.Cause)
	case err != nil:
		tr.Outcome = hook.OutcomeError
	case e.finalContent == "" && e.lastImages == 0:
		tr.Outcome = hook.OutcomeEmpty
	default:
		tr.Outcome = hook.OutcomeOK
	}
	if err == nil {
		tr.Reply = withImageNotes(e.finalContent, e.replyLines)
		tr.Stored = withImageNotes(e.finalContent, e.pendingNotes)
	}
	return tr
}

// releaseCompressionManagers releases the context managers taken to compress
// after a context-window error, last taken first.
func (e *engineTurn) releaseCompressionManagers() {
	for _, release := range slices.Backward(e.releases) {
		release()
	}
	e.releases = nil
}

// logAt writes one engine line under component "agent", recording the call
// site of the logAt call as the line's source.
//
//nolint:contextcheck,nolintlint // a log line carries its fields, not the turn's context (contextcheck reports this only on some runs)
func (e *engineTurn) logAt(level slog.Level, msg string, fields map[string]any) {
	ctx := context.Background()
	if !e.spec.log.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:]) // skip runtime.Callers and logAt
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.AddAttrs(slog.String("component", "agent"))
	for k, v := range fields {
		r.AddAttrs(slog.Any(k, v))
	}
	if err := e.spec.log.Handler().Handle(ctx, r); err != nil {
		return // a line the handler cannot write is lost, as with slog.Logger
	}
}

// redacted is err's text with secrets redacted, for a log line; nil for nil.
func (e *engineTurn) redacted(err error) any {
	if err == nil {
		return nil
	}
	return e.spec.redact(err.Error())
}

// turnFields adds the turn id to fields when the turn has one.
func (e *engineTurn) turnFields(fields map[string]any) map[string]any {
	if e.spec.scope.TurnID != "" {
		fields["turn_id"] = e.spec.scope.TurnID
	}
	return fields
}

// isCLIProtocol reports whether a provider protocol runs a CLI.
func isCLIProtocol(protocol string) bool {
	return modelspec.CLIAgentByProtocol(protocol) != nil
}

// firstModel reports the provider and whether the model serving the turn
// first runs a CLI.
func (e *engineTurn) firstModel() (provider string, isCLI bool) {
	if len(e.spec.candidates) > 0 {
		return e.spec.candidates[0].Provider, isCLIProtocol(e.spec.candidates[0].Protocol)
	}
	return "", false
}

// turnVision is the vision mode of the turn's model: VisionOff without one.
func (e *engineTurn) turnVision() string {
	if len(e.spec.candidates) > 0 {
		return e.spec.candidates[0].Limits.Vision
	}
	return modelspec.VisionOff
}

// logDispatchTarget logs which model and provider the turn is routed to.
func (e *engineTurn) logDispatchTarget() {
	model := e.spec.model
	provider := ""
	if len(e.spec.candidates) > 0 {
		provider = e.spec.candidates[0].Provider
		model = e.spec.candidates[0].Model
	}
	fields := map[string]any{"agent_id": e.spec.agentID, "model": model}
	if provider != "" {
		fields["provider"] = provider
	}
	e.logAt(slog.LevelInfo, "Dispatching to model", fields)
}

// iterate runs one model request and, when the model asked for tools, runs
// them. It reports done once the turn has its final answer.
func (e *engineTurn) iterate(ctx context.Context) (bool, error) {
	e.logAt(slog.LevelDebug, "LLM iteration",
		map[string]any{
			"agent_id":  e.spec.agentID,
			"iteration": e.iteration,
			"max":       e.spec.maxIterations,
		})

	defs := e.toolDefinitions()
	promptEstimate := e.assemble(ctx, defs)
	d := e.newDispatch(ctx, defs, promptEstimate)

	response, err := e.requestModel(ctx, d)
	if err != nil {
		return false, err
	}

	// AfterModel works on a copy, so the ModelCall event keeps the
	// provider's response; it may change it and adds the notes and reply
	// lines for what it did with it (generated images).
	answered := d.answered
	if answered.Name == "" {
		answered = e.turnModelRef()
	}
	mr := &hook.ModelResponse{Model: answered, Response: copyResponse(response)}
	if perr := e.hooks.callAfterModel(ctx, e.modelInfo(d), mr); perr != nil {
		return false, perr
	}
	response = mr.Response
	e.lastImages = len(response.Images)
	e.imageCount += len(response.Images)
	e.pendingNotes = append(e.pendingNotes, mr.Notes...)
	e.replyLines = append(e.replyLines, mr.ReplyLines...)
	e.hooks.emit(ctx, hook.ModelAnswer{
		EventHeader: e.header(),
		Iteration:   e.iteration,
		Model:       answered,
		Response:    copyResponse(response),
		Notes:       slices.Clone(mr.Notes),
		ReplyLines:  slices.Clone(mr.ReplyLines),
		Supplied:    d.supplied,
	})
	for _, img := range response.Images {
		e.hooks.emit(ctx, hook.Image{EventHeader: e.header(), Iteration: e.iteration, Image: img})
	}

	if len(response.ToolCalls) == 0 {
		return e.directAnswer(response), nil
	}
	return e.runToolCalls(ctx, response)
}

// copyResponse is a copy of r that shares nothing a callback may change:
// its tool calls (with their arguments and function) and its images.
func copyResponse(r *spawnllm.LLMResponse) *spawnllm.LLMResponse {
	c := *r
	if r.ToolCalls != nil {
		c.ToolCalls = make([]spawnllm.ToolCall, len(r.ToolCalls))
		for i, tc := range r.ToolCalls {
			tc.Arguments = maps.Clone(tc.Arguments)
			if tc.Function != nil {
				f := *tc.Function
				tc.Function = &f
			}
			c.ToolCalls[i] = tc
		}
	}
	c.Images = slices.Clone(r.Images)
	return &c
}

// turnModelRef names the model the turn chose.
func (e *engineTurn) turnModelRef() hook.ModelRef {
	if len(e.spec.candidates) > 0 {
		return candidateRef(e.spec.candidates[0])
	}
	return hook.ModelRef{Name: e.spec.model, Model: e.spec.model}
}

// candidateRef names a candidate.
func candidateRef(c models.Candidate) hook.ModelRef {
	return hook.ModelRef{Name: c.Name, Provider: c.Provider, Protocol: c.Protocol, Model: c.Model}
}

// requestMessages is the slice sent to provider for this iteration: the
// turn's messages plus, when few steps remain, the low-steps note as a
// trailing user message. The note is never added to e.messages nor stored, so
// each request carries only its own count and history never sees it. A
// dispatch without tools has no steps to count, and a CLI provider finishes
// the turn in one call, so neither gets the note.
func (e *engineTurn) requestMessages(hasTools, isCLI bool) []spawnllm.Message {
	if !hasTools || isCLI {
		return e.messages
	}
	note := lowStepsNote(e.iteration, e.spec.maxIterations)
	if note == "" {
		return e.messages
	}
	// Clip so the append copies instead of writing into e.messages' spare
	// capacity.
	return append(slices.Clip(e.messages), spawnllm.Message{Role: "user", Content: note})
}

// toolDefinitions are the tools offered to the model in this dispatch. A
// sub-agent is an instance of its parent and is offered the parent's full
// toolset: recursion is bounded by the spawner's depth limit, not by
// withholding tools.
func (e *engineTurn) toolDefinitions() []spawnllm.ToolDefinition {
	if e.spec.noTools {
		return nil
	}
	return e.spec.env.toolDefs()
}

// assemble prepares the context for this dispatch: the LLM-free eviction
// sweep first (cheap relief, so summarization fires far less often), then the
// safety-net compaction checks, then memory placement. The slice in hand is
// kept unless stored history was rewritten, because it carries this turn's
// unsaved steering messages and already-resolved media. The tool schemas'
// cost rides on the request so every trigger measures the real request.
//
// It returns the engine's estimate of the request's prompt tokens, which the
// provider's reported count calibrates after the call; 0 when assembly failed.
func (e *engineTurn) assemble(ctx context.Context, defs []spawnllm.ToolDefinition) int {
	asm, err := e.spec.cm.Assemble(ctx, e.spec.env.assembleRequest(ctx, defs, e.messages))
	if err != nil {
		e.logAt(slog.LevelWarn, "assemble failed (continuing with current slice)", map[string]any{
			"agent_id": e.spec.agentID,
			"error":    err.Error(),
		})
		return 0
	}
	if len(asm.Evictions) > 0 {
		ev := hook.Compaction{EventHeader: e.header(), Phase: hook.CompactionEvicted, Blocking: true}
		for _, x := range asm.Evictions {
			ev.Evictions = append(ev.Evictions, hook.Eviction{Tool: x.Tool, Resource: x.Resource, Bytes: x.Bytes, Reason: x.Reason})
		}
		e.hooks.emit(ctx, ev)
	}
	if asm.Changed() {
		e.messages = asm.Messages
		e.rebuilt = true
	}
	return asm.PromptTokenEstimate
}

// logRequest logs the session status and the request about to be sent to
// the dispatch's first provider; the full messages only when content may be
// logged.
func (e *engineTurn) logRequest(d *llmDispatch) {
	defs := d.defs
	_, isCLI := e.firstModel()
	sent := e.requestMessages(len(defs) > 0, isCLI)
	sessionSummary := e.spec.env.sessionSummary()
	e.logAt(slog.LevelInfo, "Session status",
		map[string]any{
			"agent_id":    e.spec.agentID,
			"messages":    len(e.messages),
			"compacted":   sessionSummary != "",
			"summary_len": len(sessionSummary),
			"tools":       len(defs),
			"model":       e.spec.model,
		})

	systemPromptLen := 0
	if len(sent) > 0 {
		systemPromptLen = len(sent[0].Content)
	}
	var temperature any = "model default"
	if e.spec.temperature != nil {
		temperature = *e.spec.temperature
	}
	e.logAt(slog.LevelDebug, "LLM request",
		map[string]any{
			"agent_id":          e.spec.agentID,
			"iteration":         e.iteration,
			"model":             e.spec.model,
			"messages_count":    len(sent),
			"tools_count":       len(defs),
			"max_tokens":        e.spec.modelMaxTokens,
			"temperature":       temperature,
			"system_prompt_len": systemPromptLen,
		})

	if e.spec.logContent() {
		e.logAt(slog.LevelDebug, "Full LLM request",
			map[string]any{
				"iteration":     e.iteration,
				"messages_json": formatMessagesForLog(sent, e.spec.truncate),
				"tools_json":    formatToolsForLog(defs, e.spec.truncate),
			})
	}
}

// directAnswer takes a response without tool calls as the turn's answer. A
// response with reasoning but no visible content gets the model poked to
// reply, up to maxEmptyRetries times; it reports done=false while poking.
func (e *engineTurn) directAnswer(response *spawnllm.LLMResponse) bool {
	e.finalContent = response.Content
	// "Thinking" may arrive as reasoning_content (OpenAI-style) or reasoning
	// (OpenRouter-style); either is the model's reasoning.
	reasoningText := response.ReasoningContent
	if reasoningText == "" {
		reasoningText = response.Reasoning
	}
	e.lastNormal = response.Normal
	e.finishReason = response.FinishReason

	// A reply of images alone is complete: it is not poked.
	if e.finalContent == "" && reasoningText != "" && len(response.Images) == 0 {
		if e.emptyRetries < maxEmptyRetries {
			e.emptyRetries++
			e.messages = append(e.messages, spawnllm.Message{
				Role:    "user",
				Content: "Your previous turn produced no reply. Make sure you have completed the user's request, then respond to the user now.",
			})
			e.logAt(slog.LevelInfo, "empty response with reasoning; poking model to reply",
				map[string]any{
					"agent_id": e.spec.agentID, "iteration": e.iteration, "retry": e.emptyRetries,
				})
			return false
		}
		// Retries exhausted: the caller sends a fallback instead of going
		// silent.
		e.degenerate = true
	}

	e.logAt(slog.LevelInfo, "LLM response without tool calls (direct answer)",
		map[string]any{
			"agent_id":      e.spec.agentID,
			"iteration":     e.iteration,
			"content_chars": len(e.finalContent),
		})
	return true
}

// hookCall is tc as a hook.ToolCall, with its own copy of the arguments.
func (e *engineTurn) hookCall(tc spawnllm.ToolCall) hook.ToolCall {
	return hook.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: maps.Clone(tc.Arguments), Iteration: e.iteration}
}

// runToolCalls records the model's tool calls, runs them, and adds their
// results to the conversation. It reports done when the model kept repeating
// the same batch and the turn was stopped.
func (e *engineTurn) runToolCalls(ctx context.Context, response *spawnllm.LLMResponse) (bool, error) {
	calls := make([]spawnllm.ToolCall, 0, len(response.ToolCalls))
	for _, tc := range response.ToolCalls {
		calls = append(calls, spawnllm.NormalizeToolCall(tc, e.spec.log))
	}
	toolNames := make([]string, 0, len(calls))
	for _, tc := range calls {
		toolNames = append(toolNames, tc.Name)
	}
	if e.stopRepeatedBatch(calls, toolNames) {
		return true, nil
	}

	// Long-term memory auto-loads what is tied to a tool the agent just
	// used; the next assembly in this turn picks it up.
	e.spec.mem.RecordToolUse(toolNames...)
	e.logAt(slog.LevelInfo, "LLM requested tool calls",
		map[string]any{
			"agent_id":  e.spec.agentID,
			"tools":     toolNames,
			"count":     len(calls),
			"iteration": e.iteration,
		})
	batch := make([]hook.ToolCall, 0, len(calls))
	for _, tc := range calls {
		batch = append(batch, e.hookCall(tc))
	}
	e.hooks.emit(ctx, hook.ToolBatchStarted{EventHeader: e.header(), Iteration: e.iteration, Calls: batch, Content: response.Content})

	// The model is told what became of what AfterModel handled alongside
	// these calls (generated images), in the message that carried them.
	callMsg := e.assistantToolCallMessage(response, calls)
	callMsg.Content = withImageNotes(callMsg.Content, e.pendingNotes)
	e.pendingNotes = nil
	if err := e.keep(ctx, e.spec.cm.AddToolCallMessage, callMsg, "tool call message"); err != nil {
		return false, err
	}
	results := e.executeToolCalls(ctx, calls)
	if err := e.addToolResults(ctx, results); err != nil {
		return false, err
	}
	e.steerFromRepeatedBatch(toolNames)
	return false, nil
}

// stopRepeatedBatch counts consecutive identical tool-call batches and, once
// maxIdenticalToolBatches is reached, ends the turn with a reply saying so
// rather than letting the model spin (e.g. re-writing the same memory).
func (e *engineTurn) stopRepeatedBatch(calls []spawnllm.ToolCall, toolNames []string) bool {
	sig := toolCallSignature(calls)
	if sig == "" {
		return false
	}
	if sig == e.lastToolSig {
		e.identicalToolBatches++
	} else {
		e.identicalToolBatches = 0
		e.lastToolSig = sig
	}
	if e.identicalToolBatches+1 < maxIdenticalToolBatches {
		return false
	}
	e.logAt(slog.LevelWarn, "aborting: identical tool call repeated", map[string]any{
		"agent_id":  e.spec.agentID,
		"iteration": e.iteration,
		"tools":     toolNames,
		"repeats":   e.identicalToolBatches + 1,
	})
	e.finalContent = fmt.Sprintf(
		"Stopped: I kept making the same %s call (%d times) without success, so I'm aborting to avoid a loop. Please check the request or rephrase it.",
		strings.Join(toolNames, ", "), e.identicalToolBatches+1)
	e.finishReason = "loop_detected"
	return true
}

// steerFromRepeatedBatch tells a model that just repeated its previous
// tool-call batch to change approach, before the next iteration. The message
// applies to any tool, with the spec's hint for the tools repeated.
func (e *engineTurn) steerFromRepeatedBatch(toolNames []string) {
	if e.identicalToolBatches == 0 {
		return
	}
	n := e.identicalToolBatches + 1
	guidance := fmt.Sprintf(
		"⚠️ You have made the exact same tool call (%s) %d times in a row and it is not working — stop repeating the identical call. Change your approach: adjust the arguments, try a different tool, or explain the problem to the user instead of retrying the same thing.",
		strings.Join(toolNames, ", "), n)
	if e.spec.repeatHint != nil {
		if hint := e.spec.repeatHint(toolNames); hint != "" {
			guidance += " " + hint
		}
	}
	e.messages = append(e.messages, spawnllm.Message{Role: "user", Content: guidance})
	e.logAt(slog.LevelInfo, "steering model away from repeated identical tool call",
		map[string]any{"agent_id": e.spec.agentID, "iteration": e.iteration, "repeats": n, "tools": toolNames})
}

// keep adds msg to the request and records it through add (one of the
// context manager's Add methods), observing it into memory. A message the
// store refuses ends the turn with a *hook.SessionStoreError; what names it.
func (e *engineTurn) keep(
	ctx context.Context,
	add func(context.Context, spawnllm.Message) (int64, error),
	msg spawnllm.Message,
	what string,
) error {
	e.messages = append(e.messages, msg)
	seq, err := add(ctx, msg)
	if err != nil {
		return &hook.SessionStoreError{What: what, Err: err}
	}
	e.spec.mem.Observe(ctx, seq, msg.Role, msg.Content)
	return nil
}

// assistantToolCallMessage is the assistant message carrying a response's
// tool calls, in the wire form stored in history.
func (e *engineTurn) assistantToolCallMessage(response *spawnllm.LLMResponse, calls []spawnllm.ToolCall) spawnllm.Message {
	msg := spawnllm.Message{
		Role:             "assistant",
		Content:          response.Content,
		ReasoningContent: response.ReasoningContent,
		// Responses reasoning items are replayed before this turn's
		// function_call on the next request (reasoning models with tools).
		ResponsesReasoning: response.ResponsesReasoning,
		// Anthropic thinking blocks must go back with the tool_use turn
		// they preceded, or Anthropic refuses the next request.
		AnthropicThinking: response.AnthropicThinking,
	}
	for _, tc := range calls {
		argumentsJSON, err := json.Marshal(tc.Arguments)
		if err != nil {
			e.logAt(slog.LevelError, "Failed to marshal tool call arguments",
				map[string]any{
					"agent_id": e.spec.agentID,
					"tool":     tc.Name,
					"error":    err.Error(),
				})
			argumentsJSON = []byte("{}")
		}
		// ExtraContent carries Gemini 3's thought_signature, which must be
		// persisted.
		thoughtSignature := ""
		if tc.Function != nil {
			thoughtSignature = tc.Function.ThoughtSignature
		}
		msg.ToolCalls = append(msg.ToolCalls, spawnllm.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Name: tc.Name,
			Function: &spawnllm.FunctionCall{
				Name:             tc.Name,
				Arguments:        string(argumentsJSON),
				ThoughtSignature: thoughtSignature,
			},
			ExtraContent:     tc.ExtraContent,
			ThoughtSignature: thoughtSignature,
		})
	}
	return msg
}

// toolCallSignature returns a stable signature for a tool-call batch (sorted
// name+arguments), used to detect a model repeating the identical call. Returns
// "" for an empty batch.
func toolCallSignature(calls []spawnllm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		args := ""
		if len(c.Arguments) > 0 {
			if b, err := json.Marshal(c.Arguments); err == nil { // json sorts map keys
				args = string(b)
			}
		} else if c.Function != nil {
			args = c.Function.Arguments
		}
		name := c.Name
		if name == "" && c.Function != nil {
			name = c.Function.Name
		}
		parts = append(parts, name+":"+args)
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// withImageNotes is content followed by lines, one per line, after a blank
// line.
func withImageNotes(content string, lines []string) string {
	if len(lines) == 0 {
		return content
	}
	joined := strings.Join(lines, "\n")
	if content == "" {
		return joined
	}
	return content + "\n\n" + joined
}
