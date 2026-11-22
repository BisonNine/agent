# hook: design and the map from the host's turn loop

This is the brief for phase 5a: restructuring the host's turn loop (`llmTurn`
and the code around it) in place so that it calls the callbacks and emits the
events of this package, with today's host behaviour as the callback
implementations and event consumers. Phase 5c then moves what the table marks
ENGINE into `agent/loop`.

Host references are to the host checkout's `agent/` directory unless another
directory is named, as `file.go:function`.

## 1. Reading this document

### 1.1 Package paths

The module root is `go.bison9.com/agent`, and it is the root package `agent`
(plan 3.1: "the agent itself is the module's root package"). The plan's
`agent/hook` and `agent/loop` are therefore the packages
`go.bison9.com/agent/hook` (directory `hook/`) and
`go.bison9.com/agent/loop` (directory `loop/`), beside `toolkit/`,
`models/` and the rest, as the plan's 3.2 table lists them. There is no
`agent/` directory. The agent id rule is the separate leaf
`go.bison9.com/agent/ident` (1.3).

### 1.2 The three kinds of row

- **ENGINE**: generic behaviour the loop keeps for every host. In 5a it
  stays where it is; in 5c it moves to `agent/loop`.
- **CALLBACK** (named): host behaviour that changes a step. In 5a it moves
  into a function of the named callback type, registered in one place.
- **CONSUMER** (event named): host behaviour that only watches. In 5a it
  moves into a consumer of the named event (`OnEvent`, or an observing
  callback where it must act at the moment).

A few rows are HOST: they happen before `Run` is called or around it, and
the engine never sees them.

### 1.3 Decisions this package makes

These shape the table; each departs from, or settles, something the plan
left open.

- 1.3.1 **`ident` is its own leaf.** The id rule is needed by the host's
  configuration, its forum and classifiers, and later `accounting`. All of
  them can import a standard-library-only package; none should import
  `hook` (which brings `toolkit` and the provider types) to compare two
  strings.
- 1.3.2 **`hook` imports `spawnllm/protocoltypes`, not `spawnllm`.** The
  types are the same (`spawnllm.Message` is an alias of
  `protocoltypes.Message`), and the leaf stays free of the provider
  clients. Thinner own types are used where the provider type carries
  wire detail a callback has no business with: `ToolCall` (id, name,
  arguments; not the thought signatures) and `Message` (what a person
  sent, not a provider message). Requests and responses use the provider
  types, since a callback that edits them must see every field.
- 1.3.3 **No `SessionStore` in `hook`.** What the loop needs from sessions
  is a `ctxengine.ContextManager` per session plus the pending-turn marker
  and `Save`, all of which `ctxengine/session.SessionStore` already
  declares as an interface. A copy in `hook` would need ctxengine's
  stored-message type (which brings SQLite into the leaf), and a narrower
  copy would force the loop to type-assert back to the real one. The
  agent option takes `session.SessionStore`. D8's second implementation is
  an in-memory store in `ctxengine/session` (7.6).
- 1.3.4 **No `ErrNoModelAvailable` or refusal marker in `hook`.** `models`
  owns `ErrNoModelAvailable` and `toolkit` owns `Refusal` and
  `IsExpectedRefusal` (which already accepts any error with a
  `Refusal() bool` method, so the forum and classifier need no import).
  `hook` cannot import `models`, and `models` cannot import `hook` (it
  imports `toolkit`). The root package re-exports both. `hook` holds only
  the cancel causes.
- 1.3.5 **`BeforeModel` runs once per dispatch, not once per model.**
  Shaping a request for one model (its `max_tokens`, halved after an
  overflow; its thinking level; images it cannot see; reasoning content
  its provider requires; `tool_choice: none` on the last step; the
  low-steps note) depends only on facts `modelspec` already holds
  (`Limits.Vision`, `Provider.RequireReasoningContent`, the protocol), so
  it is ENGINE, done per model through `models.WithPrepare`. The plan put
  the low-steps note, strict alternation and the reasoning backfill in a
  host `BeforeModel`; the low-steps note and `tool_choice: none` are one
  mechanism (a forced no-tools step with no word to the model would be a
  defect in a host without the callback), and strict alternation is a
  client option `models` builds.
- 1.3.6 **Watching rows are consumers, not callbacks.** The plan's table
  put `emitFinish`, spend, usage and the reasoning channel under
  `AfterModel`, and the audit row, breadcrumb, ForUser and media
  publishing and tool stats under `BeforeTool`/`AfterTool`. None of them
  changes the step, so each is a consumer of `ModelCall`,
  `ToolCallStarted` or `ToolCallFinished` (plan 4.10).
- 1.3.7 **`AfterTurn` decides; the engine stores; the host replies on
  `TurnFinished`.** `AfterTurn` may change the reply, the outcome and
  what is stored (`TurnResult.Stored`); the engine then stores it and
  saves the session, and emits `TurnFinished`. The host publishes the
  reply from its `TurnFinished` consumer, so the reply is stored before
  it is sent, as today. A store failure therefore still fails the turn
  before anything is sent.
- 1.3.8 **After… callbacks edit in place and return nothing.** One
  convention for all: Before… may replace (a non-nil value skips the
  step), After… edits through the pointer, On…Error may supply a
  replacement. A callback that wants a tool call to fail sets an error
  result.
- 1.3.9 **Tool calls cannot change their context.** Every per-call value
  the host sets today (allow checker, session key, reply chat, depth) is
  constant for the turn. The engine sets its own (scope, allow checker);
  the host puts its own on the context it passes to `Run`, which reaches
  every callback and every tool.
- 1.3.10 **`Scope` bridges to toolkit and never folds into it.** Tools read
  `toolkit.AgentID` and `toolkit.SessionKey`; `WithScope` sets both. Tools
  never import `hook`; the loop, logging, accounting and an MCP host read
  `ScopeOf`. `toolkit.WithChat` stays a host value.
- 1.3.11 **The observing callbacks are taps on events.** `OnFallback`,
  `OnCompaction`, `OnTextDelta` take the event value they mirror;
  `OnInterrupt` takes `Interrupted`. They exist for code that must act
  before the step continues (a notice before the next model is called,
  before the summary model is called; streaming latency).

## 2. The turn, in order

What the engine does for one `Run`, and where each callback and event
falls. The table in section 3 refers to these steps.

- 2.1 `Run(ctx, session, msg, opts...)` returns the handle at once. The
  run waits in the session's queue; one turn per session runs at a time.
  A run dropped from the queue ends with only `TurnFinished`
  (`OutcomeCancelled`).
- 2.2 The run leaves the queue: the turn's context is the run's context
  with `WithScope` (new turn id, `AtDepth`), a cancel cause, and the turn
  budget (`TurnBudget` option or `Limits.TurnBudget`; when it runs out the
  cause is `ErrDeadline`). `TurnStarted`.
- 2.3 `BeforeTurn` callbacks, in order. A result skips to 2.8 with
  `Skipped`; an error skips to 2.8 with `OutcomeError`.
- 2.4 The session's context: the `ctxengine` manager and the memory
  session, opened once per session and kept until idle. `ResetSession`
  resets it. The turn's model chain is begun (`models` `Begin`, sticky for
  the turn) and the manager's context window set to its model's.
- 2.5 The message is stored (unless `AlreadyStored`) and observed into
  memory. The pending-turn marker is set.
- 2.6 The model loop, up to `Limits.MaxIterations`:
  - 2.6.1 tool definitions (toolkit snapshot, discovery);
  - 2.6.2 `Assemble` (evictions: `Compaction{Evicted}`; a blocking pass:
    `OnCompaction` and `Compaction{Started}`), with memory recalled for the
    message;
  - 2.6.3 `BeforeModel` callbacks on the working request;
  - 2.6.4 the dispatch through the chain: per model, the request shaped
    for it, `OnTextDelta` and `TextDelta` while it streams, a `ModelCall`
    for each failed call, `OnFallback` and `Fallback` as the chain moves
    on; a timeout is retried after a backoff, an overflow (`OnCompaction`
    and `Compaction{Overflow}`) after a forced compaction and with lower
    `max_tokens`, up to `Limits.Retries`; a dispatch that failed for good
    goes to `OnModelError`, then ends the turn;
  - 2.6.5 `AfterModel` callbacks; `Image` per generated image; the
    answering `ModelCall`; prompt-token calibration;
  - 2.6.6 no tool calls: an empty reply with reasoning is poked (twice at
    most), else the loop ends;
  - 2.6.7 tool calls: the repeated-batch check; memory told the tools
    used; the tool-call message stored (with `ModelResponse.Notes`);
    `ToolCallStarted` for every call in order; each call on its own
    goroutine: `BeforeTool`, the tool (with `Limits.ToolTimeout`, a panic
    recovered), `OnToolError` for an error result, `AfterTool`,
    `ToolCallFinished`; results stored in call order, images routed by
    the model's vision mode; the repeat steering.
- 2.7 Interrupt or cancel at any point: the turn's context is cancelled
  with the cause; the model call in flight ends (`ModelCall` with the
  context's error); tools in flight get `Limits.CancelGrace`, then those
  still running are abandoned with `InterruptedResult` (and their
  `ToolCallFinished`), and every result is stored.
- 2.8 `TurnResult` is built (reply with `ReplyLines`, stored text with the
  last response's `Notes`, outcome from the error and cause); `AfterTurn`
  callbacks, in order, on a context that is never cancelled.
- 2.9 `Stored` is stored and observed, the session saved; the pending
  marker cleared unless the cause is `ErrShutdown`.
- 2.10 `TurnFinished`; `Done` closes; the session's next run may start.
- 2.11 A compaction the engine deferred (the age trigger) runs in the
  background.

## 3. Every behaviour of today's loop

Walked from the session dispatcher down through `llmTurn` and back up.

### 3.1 Session dispatch (`session_dispatch.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.1.1 | One turn per session at a time; later messages queue; idle session state pruned | `processSessionMessage`, `drainSession`, `pruneIdleSessions` | ENGINE | the loop's session queues (2.1) |
| 3.1.2 | Queued messages of one chat merged into one turn; commands and reply-required messages run alone | `takeBatch`, `mergeMessages`, `runsAlone` | neither | 6.1 |
| 3.1.3 | `/cancel` stops the running turn with its cause, drops the queue, counts both | `sessionState.cancel`, `takeCancelResult` | ENGINE | `Agent.Interrupt(session)` returns `(running, dropped)`; `OnInterrupt` |
| 3.1.4 | A dropped message that requires a reply gets a cancelled reply | `publishCancelledReplies` | CONSUMER | `TurnFinished{OutcomeCancelled}` of each dropped run; one reply per cancel (plan 5.4) |
| 3.1.5 | Concurrent-turn slot (taken, lent during a wait), chat held (typing, placeholder) | `runQueuedTurn`, `acquireTurnSlot`, `turn_slot.go` | HOST | around `Run` (6.1) |
| 3.1.6 | A person writing in their human agent's chat answers the waiting request | `handleHumanChat` | HOST | before `Run` |

### 3.2 The turn around the loop (`turn_reply.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.2.1 | An ask nobody waits for is not run | `runTurn` | HOST | `Run` is not called |
| 3.2.2 | Turn budget; a person's turn gets a longer one | `turnContext`, `human.go:humanTurnBudget` | ENGINE | `Limits.TurnBudget`, `TurnBudget` run option |
| 3.2.3 | An asked turn ends when its asker stops waiting | `turnContext` (`stopTurn`) | HOST | `Run.Cancel` with a cause wrapping `ErrSuperseded` |
| 3.2.4 | One turn id on the context, on every log line and audit row | `turnContext`, `turnid.go` | ENGINE | `Scope.TurnID` |
| 3.2.5 | A reply `msg_send` already sent is not sent again | `tools.WithRoundSentFlag`, `publishInboundReply` | HOST | flag on the run's context, read by the `TurnFinished` consumer |
| 3.2.6 | Shutdown: nothing sent, the turn stays pending; a request to a person ends cancelled | `endTurnOnShutdown` | ENGINE + CONSUMER | cause `ErrShutdown` keeps the marker (2.9); the consumer decides the reply |
| 3.2.7 | How a turn's end becomes a reply and an outcome; error text with the assistant's name, failover and billing text, redacted | `decideTurnReply`, `failover_render.go:renderTurnErrorFor`, `billing_render.go` | CALLBACK | `AfterTurn` sets `Reply` and `Outcome` (not `OnModelError`: it renders every turn error, and needs the budget and name) |
| 3.2.8 | The reply goes to the chat, the asker, or nowhere (system channel) | `runTurn`, `publishInboundReply`, `ask.go:deliverAskReply` | CONSUMER | `TurnFinished` |

### 3.3 Before the model loop (`loop_turn.go:runAgentLoop`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.3.1 | The agent's id on provider error logs | `providers.WithAgentID` | ENGINE | `Scope` on the context |
| 3.3.2 | A turn holds its agent instance; a reloaded instance closes after its turns | `beginAgentTurn` | HOST | the host's registry; `Agent.Close` waits for running turns |
| 3.3.3 | A human agent's turn is answered by the person | `human.go:runHumanTurn` | CALLBACK | `BeforeTurn` returns the `TurnResult`; its errors render in `AfterTurn` |
| 3.3.4 | A single-shot agent starts blank and its conversation is discarded after | `runAgentLoop`, `prepareSession`, `eviction.go:discardConversation` | neither | 6.2 |
| 3.3.5 | Context manager and memory session per agent and session, cached, shared, idle-evicted | `context_manager.go:getSessionContext`, `eviction.go` | ENGINE | 2.4 |
| 3.3.6 | The turn's model chosen once and kept; the window set to it | `llm_dispatch.go:selectTurnModel`, `SetContextWindow` | ENGINE | `models` `Begin` |
| 3.3.7 | An age compaction deferred until the reply is out | `deferred_compaction.go:claimAfterReply`, `compactAfterReply` | ENGINE | 2.11, after `TurnFinished` |
| 3.3.8 | The session token scoped to the turn for a CLI model's MCP calls (depth, ask chain, reply chat) | `scopeSessionToken` | CALLBACK | `BeforeTurn` begins it, `AfterTurn` ends it (until `mcphost`, phase 6) |
| 3.3.9 | A blocking compaction tells the person once | `compaction_notice.go:withCompactionNotice`, `compactionStarted` | CALLBACK | `OnCompaction` (`Started`, `Blocking`) |
| 3.3.10 | `session_clear` handoff: reset the session; release its media pins; reissue its token | `prepareSession` | ENGINE + CALLBACK | `ResetSession` run option; pins and token in `BeforeTurn` (`Turn.Options.ResetSession`) |
| 3.3.11 | Whispers open the next message, once, not on a retry | `prepareSession`, `whisper.go:prependWhispers` | CALLBACK | `BeforeTurn` edits `Turn.Message.Text` unless `AlreadyStored` |
| 3.3.12 | The user message stored with its source and type (`callback`) | `saveUserMessage` | ENGINE | `Message.Source`, `Message.Kind` |
| 3.3.13 | Inbound images described for a text-only model; refs marked in the text and pinned | `attachInboundMedia`, `vision_describe.go:describeInboundMedia`, `media_pins.go` | CALLBACK | `BeforeTurn` edits `Turn.Message` |
| 3.3.14 | Every stored message observed into memory | `saveUserMessage`, `llmTurn.keep`, `saveAssistantReply` | ENGINE | `MemorySession.Observe` |
| 3.3.15 | A store write the session refuses abandons the turn, with an alert | `sessionStoreWriteFailed`, `alertStoreFault` | ENGINE + CONSUMER | turn error; the alert from `TurnFinished` (6.3) |
| 3.3.16 | First request: prompt layers, the session token layer, tool-schema cost, recalled memory | `turnRequest`, `assembleRequestWithDefs`, `promptLayers`, `memory_wiring.go:recallInjections` | ENGINE | `WithInstructionLayers`, `Memory` |
| 3.3.17 | Media pins follow what the assembled context still references | `turnRequest` (`reconcileSessionPins`) | CALLBACK | `BeforeModel`, iteration 1, before 3.3.18 |
| 3.3.18 | Media refs resolved to data URLs and paths for the request | `loop_media.go:resolveMediaRefs` | CALLBACK | `BeforeModel` (idempotent; edits persist for the turn) |
| 3.3.19 | Pending-turn marker, with the message's source for a restart | `markTurnPending`, `clearTurnPending`, `recovery.go:recordPendingTurnSource` | ENGINE + CALLBACK | marker in the engine (2.5, 2.9); the source in `BeforeTurn`, cleared in `AfterTurn` unless the cause is `ErrShutdown` |
| 3.3.20 | Pending turns replayed after a restart | `recovery.go:recoverPendingTurns` | HOST | lists the store's pending sessions, calls `Run` with `AlreadyStored` |

### 3.4 The model loop (`llm_iteration.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.4.1 | Progress heartbeat with a running tool count | `loop_tools.go:startProgressUpdates` | CONSUMER | counts `ToolCallFinished`, own ticker, stops on `TurnFinished` |
| 3.4.2 | One eviction notice per turn when the policy says to notify | `publishEvictionNotice`, `summarizeEvictions` | CONSUMER | collects `Compaction{Evicted}`, posts on `TurnFinished` |
| 3.4.3 | Partial text coalesced and streamed; flushed before the reply | `newLLMTurn`, `stream_coalescer.go` | CALLBACK | `OnTextDelta`; flushed by the `TurnFinished` consumer before it publishes |
| 3.4.4 | Managers taken for a forced compaction released at the end | `releaseCompressionManagers` | ENGINE | |
| 3.4.5 | Dispatch target and per-iteration logs; the full request only when content may be logged | `logDispatchTarget`, `logRequest` | ENGINE | `WithLogContent` |
| 3.4.6 | At most `max_tool_iterations` requests; `max_iterations` finish reason | `runLLMIteration` | ENGINE | `Limits.MaxIterations` |
| 3.4.7 | Tool definitions; none for a `no_tools` model | `toolDefinitions` | ENGINE | toolkit snapshot, `discovery`, `modelspec.Limits.NoTools` |
| 3.4.8 | Assemble per iteration; keep the working slice unless history was rewritten; collect evictions | `assemble` | ENGINE | 2.6.2 |
| 3.4.9 | Low-steps note in the request, never stored | `requestMessages`, `lowStepsNote` | ENGINE | per model, not for a CLI model (1.3.5) |
| 3.4.10 | Request and refusal dumps | `recordResponse`, `llm_logging.go:dumpAll`, `dumpRefusal` | CONSUMER | `ModelCall` (`Request`, `Response`) |
| 3.4.11 | Reasoning delivered to a reasoning channel or inline (`/reasoning on`) | `recordResponse`, `handleReasoning` | CONSUMER | `ModelCall.Response.Reasoning` |
| 3.4.12 | Generated images saved, sent to the chat, noted for the model and the reply | `generated_images.go:handleImages` | CALLBACK | `AfterModel` (`Notes`, `ReplyLines`); `Image` events for watchers |
| 3.4.13 | Reasoning shown as the reply when configured | `directAnswer` (`ShowReasoningAsContent`) | CALLBACK | `AfterModel` sets `Response.Content` |
| 3.4.14 | An empty reply with reasoning is poked twice, then marked degenerate | `directAnswer` | ENGINE | `TurnResult.Degenerate` |
| 3.4.15 | Tool calls normalised | `runToolCalls` | ENGINE | |
| 3.4.16 | The same batch four times ends the turn; each repeat is steered | `stopRepeatedBatch`, `steerFromRepeatedBatch` | ENGINE | `loop_detected`; the `file_edit` hint: 6.4 |
| 3.4.17 | Memory told which tools were used | `runToolCalls` (`RecordToolUse`) | ENGINE | `MemorySession.ToolsUsed` |
| 3.4.18 | Text sent with tool calls posted when tool activity streams | `publishNarration` | CONSUMER | `ModelCall` with tool calls and content |
| 3.4.19 | The tool-call message keeps reasoning, thinking blocks and signatures, and the image notes | `assistantToolCallMessage` | ENGINE | `ModelResponse.Notes` |

### 3.5 The dispatch (`llm_dispatch.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.5.1 | Prompt cache key; temperature (not for a CLI); streaming option | `newDispatch` | ENGINE | temperature is an agent setting, not a hook |
| 3.5.2 | Timeout retried after a jittered backoff | `requestModel`, `waitToRetry`, `withJitter` | ENGINE | `Limits.Retries`, `Limits.RetryBackoff`, clock |
| 3.5.3 | Context overflow: notice, forced compaction, reassembly, `max_tokens` halved when history did not shrink | `compressForRetry`, `classifyLLMError` | ENGINE + CALLBACK | the notice in `OnCompaction` (`Overflow`) |
| 3.5.4 | Each call's usage added to the turn's | `turn_usage.go:addTurnUsage` | CONSUMER | answering `ModelCall` |
| 3.5.5 | Prompt-token calibration | `requestModel` (`ObserveUsage`) | ENGINE | |
| 3.5.6 | One "LLM finish" line per provider call | `emitFinish`, `dispatch_event.go:emitLLMFinishEvent` | CONSUMER | every `ModelCall` |
| 3.5.7 | Daily spend and its threshold alert | `loop_inbound.go` (`recordSpend` from turn usage) | CONSUMER | `ModelCall` |
| 3.5.8 | A failed dispatch's error says shutdown, stopped on purpose, or failed | `dispatchFailed` | ENGINE | the cause |
| 3.5.9 | Fallback per call with cooldown skips; each passed model logged | `callModel`, `loggingNotifier`, `logPassed`, `trailingSkips` | ENGINE | `models` chain; `Fallback` |
| 3.5.10 | "Model X failed, trying Y" posted to the chat | `failover_render.go:fallbackNotifier` | CALLBACK | `OnFallback` |
| 3.5.11 | Calls in flight counted so a replaced client closes after them | `callModel` (`activeRequests`, `modelCalls`) | ENGINE | `models` `Adopt`; `Agent.Close` |
| 3.5.12 | The client per model; an isolated one for a fresh temporary agent | `chatCandidate`, `dispatchProvider` | ENGINE | `models` (`Isolated`) |
| 3.5.13 | Per model: `max_tokens` (halved after overflows), thinking level when supported, `tool_choice: none` on the last step | `requestOptions`, `llmDispatch.maxTokens` | ENGINE | `models.WithPrepare` |
| 3.5.14 | Images removed, with a note, for a model that cannot see them | `messagesForModel` | ENGINE | per model, `modelspec.Limits.Vision` (1.3.5) |
| 3.5.15 | Empty reasoning content backfilled for a provider that requires it | `backfillReasoningContent` | ENGINE | per model, `modelspec.Provider.RequireReasoningContent` (7.5) |
| 3.5.16 | Strict alternation | `providers/factory_provider.go` | ENGINE | a client option `models` builds; not in the loop |
| 3.5.17 | A CLI model: the turn is one call, no note, no `tool_choice`; its MCP tool calls are not loop tool calls | `requestMessages`, `requestOptions` | ENGINE | no `ToolCallStarted` for them |

### 3.6 Tools (`tool_exec.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.6.1 | Calls run in parallel, results kept in call order; per-tool timeout; a panic becomes an error result | `executeToolCalls`, `runToolCall` | ENGINE | `Limits.ToolTimeout` |
| 3.6.2 | A breadcrumb per call before it runs (`/tools on`) | `publishBreadcrumb`, `toolactivity.go` | CONSUMER | `ToolCallStarted` (all of a batch, in order, before any runs) |
| 3.6.3 | "Tool call dispatched" log without arguments | `runToolCall` | ENGINE | |
| 3.6.4 | The allow checker on the call's context (checked again at execution) | `runToolCall` (`WithToolAllowChecker`) | ENGINE | `WithAllow` / the toolkit set's grant |
| 3.6.5 | Session key and reply chat on the call's context | `runToolCall` | ENGINE + HOST | `WithScope` sets the session key; the reply chat is on the run's context (1.3.9) |
| 3.6.6 | Tool statistics per session | `runToolCall` (`tools.RecordToolResult`) | CONSUMER | `ToolCallFinished` |
| 3.6.7 | Audit row per call, after it, with duration; a clone's under its source | `audit.go:recordToolCallAudit` | CONSUMER | `ToolCallFinished` (4.5) |
| 3.6.8 | Async tools: user output to the chat, the late result into a new turn of the right session at the same depth | `asyncToolCallback`, `deliverAsyncForUser` | neither | 6.5 |
| 3.6.9 | A tool's user output posted when tool activity streams | `publishToolForUser` | CONSUMER | `ToolCallFinished` |
| 3.6.10 | A tool's media posted | `publishToolMedia` | CONSUMER | `ToolCallFinished` |
| 3.6.11 | Result capped to the context window; a failed write typed as a tool error; references kept | `toolResultMessage`, `tool_result_cap.go` | ENGINE | |
| 3.6.12 | Tool images: on the tool message or one follow-up user message, by the model's vision mode | `toolImages.route`, `addToolImages` | ENGINE | `modelspec.Limits.Vision` |
| 3.6.13 | Tool images described for a text-only model by a vision model | `describeUnseenImages` | neither | 6.6 |
| 3.6.14 | Refusals logged as warnings | `tools/registry.go` (`IsExpectedRefusal`) | ENGINE | `toolkit` already does it; `OnToolError` is not needed for it |

### 3.7 Behaviours the plan names that are not where it put them

| # | Behaviour | Where it is today | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.7.1 | Secret redaction | `secret_redact.go:redactSecrets`, applied to provider error text in logs and in the rendered turn error; not to tool output | ENGINE + CALLBACK | the `Redactor` on logged error text; `AfterTurn` renders the error redacted (7.4) |
| 3.7.2 | Approval of a tool call | nothing today | CALLBACK | `BeforeTool` is where one goes |
| 3.7.3 | Depth and turn-chat scoping | `tools/agents.SpawnDepth`, `scopeSessionToken`, `WithReplyChat` | ENGINE + HOST | `Scope.Depth` (`AtDepth`); host values on the run's context |
| 3.7.4 | Requests to people withdrawn on cancel | `human.go:askHuman` ends when its context does | HOST | the turn's context; no hook |
| 3.7.5 | The `/cancel` reply | `commands/cmd_cancel.go`, `decideTurnReply` | HOST | the command uses `Interrupt`'s return and `TaskController.Tasks`; the interrupted turn's `AfterTurn` sends no text of its own (plan 5.4) |
| 3.7.6 | Session reset, whispers, pending-turn marker, human turns | 3.3.10, 3.3.11, 3.3.19, 3.3.3 | | as listed |

## 4. Ordering constraints

What today's code relies on, and what keeps it.

- 4.1 **The message is complete before it is stored.** Whispers, the
  attachment marker and inbound image descriptions change the text before
  the user message is stored: `BeforeTurn` runs before 2.5.
- 4.2 **Pins before media resolution.** The pin reconciliation must see
  the `media://` refs before the resolver replaces them with data URLs:
  two `BeforeModel` callbacks, registered in that order. The host
  registers every callback in one place, with a test that pins the order
  (plan risk "Callback ordering").
- 4.3 **Images saved before the assistant message is stored.** `AfterModel`
  runs before the engine stores the tool-call message or builds the final
  reply (2.6.5 before 2.6.6 and 2.6.7), so the notes are in it.
- 4.4 **Redaction before logging and rendering.** The engine applies the
  `Redactor` before it logs an error; `AfterTurn` renders the turn error
  redacted. A redaction of tool output, if adopted (7.4), goes in
  `AfterTool`, which runs before the result is stored or emitted.
- 4.5 **Audit.** Today the audit row is written after the call, with its
  outcome and duration; nothing is audited before execution, so the plan's
  "audit before execution" holds for nothing that exists. An approval
  added later records its decision in `BeforeTool`, before the tool runs.
- 4.6 **Stream before reply.** Every `TextDelta` precedes `TurnFinished`;
  the consumer flushes its coalescer before publishing the reply.
- 4.7 **Notices before the wait.** The summarising notice is posted before
  the summary model is called and the fallback notice before the next
  model is called: both are synchronous taps (`OnCompaction`,
  `OnFallback`), not stream consumers.
- 4.8 **Stored before sent.** The reply is stored and the session saved
  (2.9) before `TurnFinished` (2.10), whose consumer sends it.
- 4.9 **Breadcrumbs in call order.** `ToolCallStarted` for a batch is
  emitted in the model's order before any call runs; `ToolCallFinished`
  comes in completion order, and results are stored in call order.
- 4.10 **The deferred compaction after the reply.** It starts after
  `TurnFinished` has been delivered to `OnEvent`; a host that publishes
  synchronously there publishes before it starts.

## 5. The host packages the loop imports, and their seams

Counted over the non-test files of `agent/`: 37 module packages (the plan
says 38) and three third-party modules. Files that stay in the host whole
are named; everything else is cut at the seam given.

| Host package | Seam |
| --- | --- |
| `logger` | `*slog.Logger` (`WithLogger`); the host's handler |
| `providers` | `models` (chains, fallback, cooldown, clients) and the `spawnllm` types; `HumanProvider` stays host, behind `BeforeTurn` |
| `bus` | event consumers and `AfterTurn`; outcomes are `hook.Outcome` |
| `tools` | `toolkit` (registry, result, agent id and session key); reply chat, ask chain and the round-sent flag stay host context values |
| `tools/agents` | `TaskInfo`, `TaskController`; `SpawnDepth` becomes `Scope.Depth`; the task store stays host |
| `config` | read by the host when it builds the agent (limits, models, tools) and inside its callbacks |
| `routing` | host, before `Run`; id comparisons through `ident` |
| `constants` | host: channel names, read in callbacks and consumers |
| `utils` | standard library or the package's own helpers |
| `global` | `accounting.Identity` (`Scope.Identity`); turn usage summed by a consumer |
| `agentreg` | host: its registry holds `agent.Agent` values |
| `fileutil` | host: image saving (`AfterModel`), dumps (consumer) |
| `media` | host: `BeforeTurn` and `BeforeModel` callbacks, consumers |
| `commands` | host: the cancel command, queue rules (6.1) |
| `cogmemhost` | `hook.Memory`, an adapter over `cogmem` (7.2) |
| `channels` | host consumers: streaming, placeholders, typing |
| `voice` | host: transcription before `Run` (`loop_transcribe.go`) |
| `tools/msg`, `tools/files`, `tools/schedule`, `tools/untrusted` | host tools mounted in the toolkit registry; `toolkit/untrusted` |
| `internal/audit` | host consumer (`ToolCallFinished`) |
| `dump` | host consumer (`ModelCall`) |
| `state` | host: the pending turn's source (`BeforeTurn`, `AfterTurn`) and recovery |
| `msgtoken` | host: message tokens, checked before `Run` |
| `internal/perms` | `fsmode.Modes` options |
| `internal/clock` | `clock.Clock` |
| `internal/workspace`, `app`, `skills`, `ccv3` | host: the system prompt (`context.go`, `character_card.go`), given as `WithInstructionLayers` |
| `classifier`, `classifier/classifierfs`, `forum`, `forum/forumfs` | host: `classifier_host.go`, `forum_host.go` stay in the host whole |
| `mcp` | host: `loop_mcp.go` stays (upstream MCP into the toolkit registry) |
| `alerts`, `github.com/tenebris-tech/alerter` | host consumers (store faults, spend) |
| `github.com/google/uuid`, `github.com/h2non/filetype` | used by host-side files only |

## 6. What fits neither a callback nor an event

- 6.1 **Merging queued messages and the turn slot.** The queue merges one
  chat's queued messages into one turn and runs commands and messages that
  need their own reply alone, and a concurrent-turn slot is taken when the
  turn starts, not while it waits. Both depend on host notions (a chat, a
  command, a required reply), and a slot taken in `BeforeTurn` would count
  its wait against the turn budget. *Resolution:* in 5a and 5b the host
  keeps its own queue, merging and slot, and calls `Run` only when the
  session is idle; the engine's queue then never holds more than one run
  for it, and `/cancel` drops the host's queue. See 8.2.
- 6.2 **Single-shot agents.** A single-shot agent's session is reset before
  each turn and discarded after it, once the engine has let go of the
  session's context; no callback runs late enough. *Resolution:* an agent
  setting in `loop.Spec` (an option such as `WithSingleShot`), with the
  engine resetting before and discarding after. Not in `hook`, since it is
  an agent property.
- 6.3 **The store-fault alert.** The host alerts when the session store
  refuses a write and clears the alert when a save succeeds; it must tell
  a store failure from any other turn error. *Resolution:* `agent/loop`
  wraps such errors in a sentinel of its own (`ErrSessionStore`), which
  the `TurnFinished` consumer tests with `errors.Is`; a successful turn
  clears the alert.
- 6.4 **The `file_edit` hint in the repeat steering.** The steering text is
  generic except one sentence naming the host's file tools. *Resolution:*
  an optional toolkit interface a tool implements to give its own hint
  when it is repeated; the engine appends it. Until then the engine's text
  is generic and the sentence is lost (a host-visible change to decide in
  5c).
- 6.5 **Async tools and late results.** An async tool's late result enters
  a new turn of a session the host chooses (a clone's goes to its source)
  at the same depth. *Resolution:* the engine registers an async call as a
  task (`TaskCallback`) and its completion is `TaskFinished`, whose
  `Result` carries `ForUser` and `ForLLM`; the host's `OnEvent` consumer
  routes it into a new `Run`, as it routes it today. This needs the task
  registration seam of 5b (8.4).
- 6.6 **Describing tool images for a text-only model.** The engine routes
  tool images by vision mode (ENGINE), but for a model that cannot see
  them the host asks a vision model for one description of the whole
  batch, added as one user message. That is neither per call (`AfterTool`)
  nor per dispatch. *Resolution:* see 8.3.

## 7. What other packages need for this

- 7.1 `loop.Spec` and the root options carry what the plan lists, plus the
  agent settings the table names (temperature, single-shot, log content,
  instruction layers).
- 7.2 A `hook.Memory` adapter over `cogmem` (in `agent/loop` or the root,
  since `cogmem` cannot import `hook`), so a host does not write one.
- 7.3 The root re-exports `models.ErrNoModelAvailable` and toolkit's
  refusal helpers next to the `hook` causes (1.3.4).
- 7.4 Redaction of stored tool output, which plan 4.9 proposes and the
  host does not do today: not done in 5a (8.5).
- 7.5 `models.Candidate` (or what `WithPrepare` is given) must say whether
  the model's provider requires reasoning content, so the engine can
  backfill it per model (3.5.15).
- 7.6 `ctxengine/session`: an in-memory `SessionStore` for tests and small
  hosts (D8), passing the same tests as the SQLite store.

## 8. Open questions

- 8.1 **An unusable id and the default agent.** `ident.NormalizeID`
  returns `""` where the host's `config.NormalizeAgentID` returns `main`,
  so `SameID("!!!", "main")` is false where the host's `SameAgentID` says
  true today. *Recommended:* the host keeps `main` as its fallback in
  `NormalizeAgentID` and calls `ident.SameID` for comparisons, accepting
  that input with nothing usable never names the default agent.
- 8.2 **Who owns the session queue.** Plan 3.1 puts the queues in the
  loop; merging and the turn slot are host rules (6.1). *Recommended:*
  the host keeps queueing and merging through 5a and 5b; decide in 5c
  whether the engine merges by a key the host gives with each run, once a
  second host shows whether merging is general.
- 8.3 **A vision describer.** *Recommended:* an agent option
  `WithImageDescriber(func(ctx, images []string, focus string) (string,
  bool))`, called by the engine once per batch for a model with vision
  off, with its type in `hook`. It is a new option, so it needs approval.
- 8.4 **How a tool registers a task.** `TaskInfo` and `TaskController` are
  here; the call a tool (`agent_spawn`, an async tool, a task model) makes
  to run work on its own context under the agent is not. *Recommended:*
  design it in 5b with the host's task store, as a value the engine puts
  on the tool's context; add it to `hook` then.
- 8.5 **Redacting tool output before it is stored.** *Recommended:* not in
  5a (it changes what the model sees); decide separately, and if adopted,
  do it as an `AfterTool` callback the host registers.
