package loop

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// toolCallResult is one tool call of a batch and what it returned.
type toolCallResult struct {
	tc     spawnllm.ToolCall
	result *toolkit.Result
}

// toolPanicError is the error of a tool call whose tool panicked. Its text is
// what it always was; a consumer tells a panic from other failures with
// errors.As.
type toolPanicError struct {
	tool  string
	value any
}

func (e *toolPanicError) Error() string { return fmt.Sprintf("tool %s panicked: %v", e.tool, e.value) }

// executeToolCalls runs a batch of tool calls in parallel and returns their
// results in call order. ToolCallStarted is emitted for every call, in call
// order, before the first call runs; each call's ToolCallFinished as it
// ends; ToolBatchFinished once all have ended. When the turn's context ends
// (an interrupt or a cancel) the calls still running get cancelled contexts
// and spec.cancelGrace to return; after that each is abandoned: it ends with
// hook.InterruptedResult (ToolCallFinished.Interrupted), so the stored
// transcript never holds a call without a result, and what it returns later
// is dropped.
func (e *engineTurn) executeToolCalls(ctx context.Context, calls []spawnllm.ToolCall) []toolCallResult {
	b := &toolBatch{results: make([]toolCallResult, len(calls)), ended: make([]bool, len(calls)), calls: make([]hook.ToolCall, len(calls))}
	for i, tc := range calls {
		b.results[i].tc = tc
		b.calls[i] = e.hookCall(tc)
		e.hooks.emit(ctx, hook.ToolCallStarted{EventHeader: e.header(), Call: e.hookCall(tc)})
	}
	// Per-tool budget: a tool's context is cancelled at the deadline so a
	// well-behaved tool returns a timeout error and the model can continue the
	// turn. The turn's context is the backstop for a tool that ignores
	// cancellation.
	var toolTimeout time.Duration
	if e.spec.toolTimeout != nil {
		toolTimeout = e.spec.toolTimeout()
	}
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() { e.runToolCall(ctx, b, i, toolTimeout) })
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		grace := e.spec.clock.NewTimer(e.cancelGrace())
		select {
		case <-done:
			grace.Stop()
		case <-grace.C():
			e.abandon(ctx, b)
			b.emitting.Wait()
		}
	}
	e.hooks.emit(ctx, hook.ToolBatchFinished{EventHeader: e.header(), Iteration: e.iteration, Calls: len(calls)})
	return b.take()
}

// cancelGrace is how long a stopped turn waits for its tools.
func (e *engineTurn) cancelGrace() time.Duration {
	if e.spec.cancelGrace > 0 {
		return e.spec.cancelGrace
	}
	return hook.DefaultCancelGrace
}

// toolBatch is a batch's results while its calls run: a call's result is
// written once, by the call or by abandon, whichever comes first.
type toolBatch struct {
	mu      sync.Mutex
	results []toolCallResult
	ended   []bool
	// calls are the calls as BeforeTool left them, for an abandoned one.
	calls []hook.ToolCall
	// emitting counts the calls that ended and are emitting their
	// ToolCallFinished, so ToolBatchFinished never comes before one.
	emitting sync.WaitGroup
}

// take returns the batch's results, each set.
func (b *toolBatch) take() []toolCallResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.results)
}

// abandon ends every call of b still running with hook.InterruptedResult.
func (e *engineTurn) abandon(ctx context.Context, b *toolBatch) {
	var events []hook.ToolCallFinished
	b.mu.Lock()
	for i := range b.results {
		if b.ended[i] {
			continue
		}
		b.ended[i] = true
		r := &b.results[i]
		r.result = hook.InterruptedResult()
		snapshot := *r.result
		call := b.calls[i]
		call.Arguments = maps.Clone(call.Arguments)
		events = append(events, hook.ToolCallFinished{EventHeader: e.header(), Call: call, Result: &snapshot, Interrupted: true})
	}
	b.mu.Unlock()
	for _, ev := range events {
		e.logAt(slog.LevelWarn, "Tool call abandoned: it did not return after the turn was stopped",
			e.turnFields(map[string]any{"agent_id": e.spec.agentID, "tool": ev.Call.Name, "grace": e.cancelGrace().String()}))
		e.hooks.emit(ctx, ev)
	}
}

// runToolCall runs call i of b through the tool callbacks and records its
// result. A panicking tool, or a panicking tool callback, becomes an error
// result for that call only, so it neither crashes the process nor wedges
// the batch. Every call ends with one ToolCallFinished, carrying the
// arguments as BeforeTool left them; a call abandoned meanwhile gets no
// AfterTool and its result is dropped.
func (e *engineTurn) runToolCall(ctx context.Context, b *toolBatch, i int, toolTimeout time.Duration) {
	tc := b.results[i].tc
	call := e.hookCall(tc)
	answered := false
	start := time.Now()
	execCtx, cancelExec := ctx, context.CancelFunc(func() {})
	if toolTimeout > 0 {
		execCtx, cancelExec = context.WithTimeout(ctx, toolTimeout)
	}
	slot := &toolCallResult{tc: tc}
	defer func() {
		// The call's context is live until its ToolCallFinished is emitted.
		defer cancelExec()
		b.mu.Lock()
		if b.ended[i] {
			b.mu.Unlock()
			return
		}
		b.ended[i] = true
		b.results[i].result = slot.result
		b.emitting.Add(1)
		b.mu.Unlock()
		defer b.emitting.Done()
		ev := hook.ToolCallFinished{
			EventHeader: e.header(),
			Call:        hook.ToolCall{ID: call.ID, Name: call.Name, Arguments: maps.Clone(call.Arguments), Iteration: call.Iteration},
			Elapsed:     time.Since(start),
			Answered:    answered,
		}
		if slot.result != nil {
			snapshot := *slot.result
			ev.Result = &snapshot
		}
		e.hooks.emit(execCtx, ev)
	}()

	// Arguments are not logged: they routinely carry memory content, file
	// contents and other user data.
	e.logAt(slog.LevelInfo, "Tool call dispatched",
		e.turnFields(map[string]any{
			"agent_id":  e.spec.agentID,
			"tool":      tc.Name,
			"iteration": e.iteration,
		}))

	result, perr := e.beforeTool(execCtx, &call)
	b.mu.Lock()
	b.calls[i] = hook.ToolCall{ID: call.ID, Name: call.Name, Arguments: maps.Clone(call.Arguments), Iteration: call.Iteration}
	b.mu.Unlock()
	if perr != nil {
		slot.result = e.toolCallbackFailed(tc.Name, perr)
		return
	}
	answered = result != nil
	if !answered {
		result = e.execute(execCtx, tc.Name, call.Arguments)
	}
	slot.result = result
	if b.abandoned(i) {
		return
	}
	if perr := e.afterTool(execCtx, &call, slot); perr != nil {
		slot.result = e.toolCallbackFailed(tc.Name, perr)
	}
}

// abandoned reports whether call i was abandoned.
func (b *toolBatch) abandoned(i int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended[i]
}

// toolCallbackFailed logs a tool callback's panic and returns the result
// its call ends with.
func (e *engineTurn) toolCallbackFailed(name string, perr *hook.PanicError) *toolkit.Result {
	e.logAt(slog.LevelError, "panic in tool callback",
		map[string]any{
			"tool":  name,
			"panic": fmt.Sprintf("%v", perr.Value),
			"stack": string(perr.Stack),
		})
	return toolCallbackFailure(name, perr)
}

// execute runs the tool. A panicking tool is logged and becomes an error
// result.
func (e *engineTurn) execute(ctx context.Context, name string, args map[string]any) (result *toolkit.Result) {
	defer func() {
		if r := recover(); r != nil {
			e.logAt(slog.LevelError, "panic in tool call",
				map[string]any{
					"tool":  name,
					"panic": fmt.Sprintf("%v", r),
					"stack": string(debug.Stack()),
				})
			result = &toolkit.Result{
				Err:    &toolPanicError{tool: name, value: r},
				ForLLM: fmt.Sprintf("Tool %s failed with an internal error.", name),
			}
		}
	}()
	return e.spec.env.executeTool(ctx, name, args)
}

// beforeTool runs the BeforeTool callbacks; a non-nil result answers the
// call instead of the tool, an error refuses it. A panic is returned.
func (e *engineTurn) beforeTool(ctx context.Context, call *hook.ToolCall) (*toolkit.Result, *hook.PanicError) {
	for _, c := range e.hooks.beforeTool {
		var result *toolkit.Result
		perr := toolStep("BeforeTool", c.plugin, func() {
			var refusal error
			if result, refusal = c.fn(ctx, call); refusal != nil {
				result = toolkit.ErrorResult(refusal.Error()).WithError(refusal)
			}
		})
		if perr != nil {
			return nil, perr
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, nil
}

// afterTool runs OnToolError for an error result, then AfterTool, on
// slot.result. A panic stops them and is returned.
func (e *engineTurn) afterTool(ctx context.Context, call *hook.ToolCall, slot *toolCallResult) *hook.PanicError {
	if r := slot.result; r != nil && (r.IsError || r.Err != nil) {
		for _, c := range e.hooks.onToolError {
			var replaced *toolkit.Result
			if perr := toolStep("OnToolError", c.plugin, func() { replaced = c.fn(ctx, call, slot.result) }); perr != nil {
				return perr
			}
			if replaced != nil {
				slot.result = replaced
			}
		}
	}
	for _, c := range e.hooks.afterTool {
		if perr := toolStep("AfterTool", c.plugin, func() { c.fn(ctx, call, slot.result) }); perr != nil {
			return perr
		}
	}
	return nil
}

// toolImages routes the images a batch of tools returned according to the
// turn model's vision mode.
type toolImages struct {
	mode string
	// followUp is for VisionUserMessage: Chat Completions tool messages are
	// text-only, so the images go in one user message after the results.
	followUp []string
	// unseen and focus are for VisionOff: the images go to a describer
	// instead of being dropped, steered by the image tools' ForLLM text
	// (which carries any file_view_image focus line).
	unseen []string
	focus  []string
}

// route attaches result's images to its tool message or sets them aside.
func (ti *toolImages) route(result *toolkit.Result, msg *spawnllm.Message) {
	switch ti.mode {
	case modelspec.VisionToolResponse:
		// The OpenAI Responses API and xAI take images on the tool result itself.
		msg.Media = result.Images
	case modelspec.VisionUserMessage:
		ti.followUp = append(ti.followUp, result.Images...)
	default:
		if len(result.Images) > 0 {
			ti.unseen = append(ti.unseen, result.Images...)
			if f := strings.TrimSpace(result.ForLLM); f != "" {
				ti.focus = append(ti.focus, f)
			}
		}
	}
}

// addToolResults stores a batch's results in call order, one tool message per
// call, each followed by its ToolResultStored, then any images the model is
// to see.
func (e *engineTurn) addToolResults(ctx context.Context, results []toolCallResult) error {
	images := toolImages{mode: e.turnVision()}
	for _, r := range results {
		msg := e.toolResultMessage(r)
		images.route(r.result, &msg)
		if err := e.keep(ctx, e.spec.cm.AddToolResult, msg, "tool result"); err != nil {
			return err
		}
		snapshot := *r.result
		e.hooks.emit(ctx, hook.ToolResultStored{EventHeader: e.header(), Call: e.hookCall(r.tc), Result: &snapshot})
	}
	if err := e.addToolImages(ctx, images.followUp); err != nil {
		return err
	}
	return e.describeUnseenImages(ctx, images)
}

// toolResultMessage is the tool message for one result, its content capped
// to fit the context window.
func (e *engineTurn) toolResultMessage(r toolCallResult) spawnllm.Message {
	contentForLLM := r.result.ForLLM
	if contentForLLM == "" && r.result.Err != nil {
		contentForLLM = r.result.Err.Error()
	}
	if capped := capToolResult(contentForLLM, r.result.IsError, e.spec.contextWindow); len(capped) != len(contentForLLM) {
		e.logAt(slog.LevelWarn, "Tool result truncated for context", map[string]any{
			"agent_id":     e.spec.agentID,
			"tool":         r.tc.Name,
			"original_len": len(contentForLLM),
			"kept_len":     len(capped),
		})
		contentForLLM = capped
	}
	msg := spawnllm.Message{
		Role:       "tool",
		Content:    contentForLLM,
		ToolCallID: r.tc.ID,
		// The tools a discovery result found: spawnllm loads their deferred
		// definitions for every later request of this conversation.
		ToolReferences: r.result.ToolReferences,
	}
	// A failed write leaves the file unchanged, so the eviction sweep must
	// never treat it as superseding the read needed to correct it.
	if r.result.IsError {
		msg.Type = ctxengine.MessageTypeToolError
	}
	return msg
}

// addToolImages hands the batch's images to a VisionUserMessage model as one
// follow-up user message.
func (e *engineTurn) addToolImages(ctx context.Context, images []string) error {
	if len(images) == 0 {
		return nil
	}
	imgMsg := spawnllm.Message{
		Role:    "user",
		Content: "Image(s) returned by the tool call(s) above:",
		Media:   images,
	}
	if err := e.keep(ctx, e.spec.cm.AddUserMessage, imgMsg, "tool image message"); err != nil {
		return err
	}
	e.logAt(slog.LevelInfo, "passed tool image(s) to vision model (user message)",
		map[string]any{"agent_id": e.spec.agentID, "model": e.spec.model, "images": len(images)})
	return nil
}

// describeUnseenImages gives a text-only model a text in place of the images
// its tools returned (DescribeImages), as a follow-up user message. Without
// an answer nothing is added: the model gets only the hidden-attachment note
// from messagesForModel. A panic in a describer ends the turn.
func (e *engineTurn) describeUnseenImages(ctx context.Context, images toolImages) error {
	if images.mode != modelspec.VisionOff || len(images.unseen) == 0 {
		return nil
	}
	text, ok, perr := e.hooks.callDescribeImages(ctx, hook.UnseenImages{Iteration: e.iteration, Images: images.unseen, Focus: images.focus})
	if perr != nil {
		return perr
	}
	if !ok {
		return nil
	}
	return e.keep(ctx, e.spec.cm.AddUserMessage, spawnllm.Message{Role: "user", Content: text}, "vision description message")
}
