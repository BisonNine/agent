# hook: design and the map from the host's turn loop

This is the brief for the rest of phase 5a and for 5c. In 5a the host's turn
loop is restructured in place: the generic part becomes the turn engine
(`engine_*.go`, host-free), and today's host behaviour becomes the callbacks
and event consumers registered in one place (`turn_hooks.go:newTurnHooks`).
In 5c the engine moves to `agent/loop`.

Host references are to the host's 5a branch, `agent/` directory, as
`file.go:function`. The host now runs on `models`, `toolkit` and
`discovery`; `providers` is a set of aliases over `models`.

## 1. Reading this document

### 1.1 Package paths

The module root is `go.bison9.com/agent`, the root package `agent` (plan
3.1). The plan's `agent/hook` and `agent/loop` are
`go.bison9.com/agent/hook` (`hook/`) and `go.bison9.com/agent/loop`
(`loop/`). The agent id rule is the separate leaf `go.bison9.com/agent/ident`.

### 1.2 The kinds of row

- **ENGINE**: generic; in the engine files now, in `agent/loop` after 5c.
- **CALLBACK** (named): host behaviour that changes a step.
- **CONSUMER** (event named): host behaviour that only watches.
- **HOST**: before or around `Run`; the engine never sees it.

### 1.3 Decisions

- 1.3.1 **`ident` is its own leaf**, importable by a host's configuration,
  forum and classifiers without the rest of the SDK.
- 1.3.2 **`hook` imports `spawnllm/protocoltypes`, not `spawnllm`**: the same
  types without the provider clients. Own thin types only for `ToolCall`
  and `Message`.
- 1.3.3 **No `SessionStore` in `hook`.** The seam is ctxengine's own
  `session.SessionStore`; a copy would bring SQLite into the leaf.
- 1.3.4 **`ErrNoModelAvailable` and the refusal marker stay in `models` and
  `toolkit`**; the root re-exports them.
- 1.3.5 **`BeforeModel` runs once per dispatch; shaping per model is
  ENGINE** (`max_tokens` and its halving, thinking level, images a model
  cannot see, reasoning backfill, the low-steps note with
  `tool_choice: none`), from the facts `models.Candidate` carries:
  `Limits` (max tokens, thinking, vision) and
  `ProviderSpec.RequireReasoningContent`. The engine does not read live
  configuration: the chain is pinned for the turn, and a configuration
  change is a new catalogue (`Adopt`).
- 1.3.6 **A provider call and the turn's answer are two events.**
  `ModelCallStarted` / `ModelCall` report each provider call as it starts
  and returns, with the provider's own response, before any callback:
  this is what the "LLM finish" line, usage, spend, request dumps,
  in-flight counting and the reasoning channel consume, in that order.
  `ModelAnswer` is the dispatch's answer after `AfterModel`: what a host
  showing the turn consumes. The engine gives `AfterModel` a copy, so the
  `ModelCall` value never changes. (The first version had one `ModelCall`
  "as AfterModel left it"; the host's log, usage and dump order needs it
  at the provider call.)
- 1.3.7 **Batch steps are events.** `ToolBatchStarted` (narration),
  `ToolBatchFinished` (all calls ended: progress) and `ToolResultStored`
  (each result in call order, after it is stored: the tool's own output to
  the chat) carry what the interim `Narrate`, `ToolsReturned` and
  `ToolResult` steps did. All three only watch.
- 1.3.8 **One description of a batch's images: `DescribeImages`.** A
  callback, because it adds a message the model reads; per batch, after
  the results are stored, as today. A per-call `AfterTool` would change
  what the model sees.
- 1.3.9 **Panics by kind** (package documentation, "Panics"): an observer
  is skipped; a turn or model step ends the turn with `OutcomeError` and a
  `*PanicError`; `AfterTurn` turns the result into an error and stores
  nothing; a tool callback fails its call only. Recovering a step-changing
  callback and going on could store a half-made step (an assistant message
  without the notes of images that were saved).
- 1.3.10 **A turn's span includes storing the message.** `TurnStarted` is
  emitted before `BeforeTurn`, the session reset and storing the inbound
  message; `TurnFinished` after `AfterTurn`, storing the reply and saving
  the session. Only the deferred compaction after the reply is outside.
- 1.3.11 **Store failures are a typed error.** `*SessionStoreError`
  (`errors.Is(err, hook.ErrSessionStore)`) in `TurnResult.Err` (a write
  during the turn, or storing the reply) or `TurnResult.SaveErr` (the
  save after the reply). The host raises and clears its store-fault alert
  from `TurnFinished`.
- 1.3.12 **Causes are host-wrappable sentinels.** A host's cause wraps a
  `hook` cause; `CauseOutcome` maps any cause to the outcome. The engine
  needs no host classifier of causes.
- 1.3.13 **The engine logs generic fields only**, through its
  `*slog.Logger` (scope, iteration, model, counts, redacted errors; the
  full request, its content shortened by the agent's `hook.Truncator`, when
  content may be logged). A line that carries a field only the host knows
  (its reasoning channel, its chat) is the host's whole line, logged where
  the engine's was: the "LLM response" line from an `AfterModel` callback
  registered first, which changes nothing (7.9). Decorating the turn's
  logger instead would put the host's fields on every engine line.
- 1.3.14 **The engine only ever sees a `models.Chain`.** An agent none of
  whose models resolves is served through `models.Catalog.Direct(name,
  model, opts...)`: a one-candidate chain on the default model's client
  with the agent's model id (also the candidate's name), no health, no
  fallback. The host builds it when it chooses the turn's model, with the
  options that make the candidate report what such a turn always ran with:
  no provider (`WithDirectProvider("")`), the agent's own limits
  (`WithDirectLimits`) and the agent's model's reasoning-content setting
  (`WithDirectRequireReasoningContent`) (7.10).
- 1.3.15 **`AfterTurn` decides; the engine stores; the host replies on
  `TurnFinished`**, so the reply is stored before it is sent.
- 1.3.16 **Tool calls cannot change their context.** Per-turn host values
  go on the context passed to `Run`.

## 2. The turn, in order

- 2.1 `Run(ctx, session, msg, opts...)` returns at once; the run waits in
  the session's queue. A run dropped from it ends with only `TurnFinished`.
- 2.2 The turn's context: the run's context, `WithScope`, the turn budget
  (`ErrDeadline` when it runs out). `TurnStarted`.
- 2.3 `BeforeTurn`. A result skips to 2.8 (`Skipped`); an error to 2.8
  (`OutcomeError`).
- 2.4 The session: its `ctxengine` manager and memory session, reset with
  `ResetSession`; the turn's chain begun and the window set to its model.
- 2.5 The message stored unless `AlreadyStored` (a compaction this starts:
  `OnCompaction`, `Compaction`), observed into memory; the context first
  assembled for the turn; the pending marker set. `LoopStarted`.
- 2.6 The model loop, up to `Limits.MaxIterations`:
  - 2.6.1 tool definitions (toolkit snapshot, `discovery`);
  - 2.6.2 `Assemble` (`Compaction{Evicted}`; a blocking pass
    `OnCompaction` and `Compaction{Started}`);
  - 2.6.3 `BeforeModel` on the working request;
  - 2.6.4 the dispatch through the chain: per model `ModelCallStarted`,
    the request shaped for it, `OnTextDelta`/`TextDelta` while it streams,
    `ModelCall` when it returns; `OnFallback`/`Fallback` as the chain moves
    on; a timeout retried after a backoff, an overflow after a forced
    compaction (`OnCompaction`/`Compaction{Overflow}`, once per dispatch)
    and with lower `max_tokens`; a dispatch failed for good goes to
    `OnModelError`;
  - 2.6.5 `AfterModel` on a copy of the response; `ModelAnswer`; `Image`
    per generated image; prompt-token calibration;
  - 2.6.6 no tool calls: an empty reply with reasoning is poked (twice at
    most), else the loop ends;
  - 2.6.7 tool calls: the repeated-batch check; memory told the tools
    used; `ToolBatchStarted`; the tool-call message stored (with the
    notes); `ToolCallStarted` per call in order; per call on its own
    goroutine `BeforeTool`, the tool (`Limits.ToolTimeout`), `OnToolError`
    for an error result, `AfterTool`, `ToolCallFinished`;
    `ToolBatchFinished`; each result stored in call order, then its
    `ToolResultStored`; tool images by the model's vision mode (on the
    result, or one follow-up user message); for vision off,
    `DescribeImages` and its text stored; the repeat steering.
- 2.7 Interrupt or cancel: the context cancelled with the cause; the call in
  flight ends with its `ModelCall`; tools get `Limits.CancelGrace`, then
  `InterruptedResult` and their `ToolCallFinished`; every result stored.
- 2.8 `TurnResult` built (`CauseOutcome` for a stopped turn); `AfterTurn`
  on a context that is never cancelled.
- 2.9 `Stored` stored and observed, the session saved (`SaveErr`); the
  pending marker cleared unless the cause wraps `ErrShutdown`.
- 2.10 `TurnFinished`; `Done` closes.
- 2.11 The deferred compaction, outside the span (`OnEvent` only).

## 3. Every behaviour of the loop

### 3.1 Session dispatch (`session_dispatch.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.1.1 | One turn per session; queue; idle pruning | `processSessionMessage`, `drainSession`, `pruneIdleSessions` | ENGINE (5b/5c) | the loop's session queues |
| 3.1.2 | Queued messages of one chat merged; commands and reply-required alone | `takeBatch`, `mergeMessages`, `runsAlone` | HOST | 6.1 |
| 3.1.3 | `/cancel` stops the turn, drops the queue, counts both | `sessionState.cancel`, `takeCancelResult` | ENGINE (5b) | `Agent.Interrupt`; `OnInterrupt` |
| 3.1.4 | Cancelled replies for dropped reply-required messages | `turn_reply.go:publishCancelledReplies` | CONSUMER | `TurnFinished{OutcomeCancelled}` |
| 3.1.5 | Turn slot, chat held | `runQueuedTurn`, `turn_slot.go` | HOST | 6.1 |
| 3.1.6 | A person's answer in their human agent's chat | `human.go:handleHumanChat` | HOST | before `Run` |

### 3.2 Around the turn (`turn_reply.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.2.1 | An ask nobody waits for is not run | `runTurn` | HOST | |
| 3.2.2 | Turn budget; a person's turn longer | `turnContext`, `human.go:humanTurnBudget` | ENGINE | `Limits.TurnBudget`, `TurnBudget` option |
| 3.2.3 | An asked turn ends when its asker stops waiting | `turnContext` | HOST | cancel with a cause wrapping `ErrSuperseded` |
| 3.2.4 | One turn id on every line and row | `turnContext`, `turnid.go` | ENGINE | `Scope.TurnID` |
| 3.2.5 | A reply `msg_send` sent is not sent again | `publishInboundReply` | HOST | flag on the run's context |
| 3.2.6 | Shutdown: nothing sent, turn stays pending | `endTurnOnShutdown` | ENGINE + CONSUMER | cause wrapping `ErrShutdown` |
| 3.2.7 | Turn end to reply and outcome; error, failover, billing text | `decideTurnReply`, `failover_render.go:renderTurnErrorFor`, `billing_render.go` | CALLBACK | `AfterTurn` |
| 3.2.8 | Reply to chat, asker or nowhere | `runTurn`, `publishInboundReply`, `ask.go:deliverAskReply` | CONSUMER | `TurnFinished` |

### 3.3 Before the model loop (`loop_turn.go:runAgentLoop`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.3.1 | The agent on provider error logs | `providers.WithAgentID` | ENGINE | `Scope` |
| 3.3.2 | A turn holds its instance across a reload | `beginAgentTurn` | HOST | `Agent.Close` waits |
| 3.3.3 | A human agent's turn answered by the person | `human.go:runHumanTurn` | CALLBACK | `BeforeTurn` |
| 3.3.4 | Single-shot: blank start, discard after | `runAgentLoop`, `eviction.go:discardConversation` | neither | 6.2 |
| 3.3.5 | Session context per agent and session | `context_manager.go:getSessionContext` | ENGINE | 2.4 |
| 3.3.6 | The turn's model chosen once; window set; an unresolvable agent on the default client | `llm_dispatch.go:selectTurnModel`, `defaultModelClient` | ENGINE + HOST | the host begins the chain, or `Catalog.Direct` with its options (1.3.14) |
| 3.3.7 | Age compaction after the reply | `deferred_compaction.go:compactAfterReply` | ENGINE | 2.11 |
| 3.3.8 | Session token scoped to the turn for CLI MCP calls | `scopeSessionToken` | CALLBACK | `BeforeTurn` / `AfterTurn` |
| 3.3.9 | A blocking compaction tells the person | `compaction_notice.go:compactionStarted` | CALLBACK | `OnCompaction` (`Started`) |
| 3.3.10 | `session_clear`: reset, pins, token | `prepareSession` | ENGINE + CALLBACK | `ResetSession`; `BeforeTurn` |
| 3.3.11 | Whispers open the next message | `prepareSession`, `whisper.go:prependWhispers` | CALLBACK | `BeforeTurn` |
| 3.3.12 | User message stored with source and type | `saveUserMessage` | ENGINE | `Message.Source`, `Kind`; inside the span (1.3.10) |
| 3.3.13 | Inbound images described, refs marked and pinned | `attachInboundMedia`, `vision_describe.go:describeInboundMedia` | CALLBACK | `BeforeTurn` |
| 3.3.14 | Stored messages observed into memory | `engine_turn.go:keep`, `saveUserMessage`, `saveAssistantReply` | ENGINE | `MemorySession.Observe` |
| 3.3.15 | Store refusal abandons the turn, with an alert | `sessionStoreWriteFailed`, `alertStoreFault`; interim `storeFailed` | ENGINE + CONSUMER | `*SessionStoreError`; alert from `TurnFinished` |
| 3.3.16 | First request: layers, token layer, tool cost, memory | `turnRequest`, `assembleRequestWithDefs`, `promptLayers` | ENGINE | `WithInstructionLayers`, `Memory` |
| 3.3.17 | Pins follow the assembled context | `turnRequest` | CALLBACK | `BeforeModel`, before 3.3.18 |
| 3.3.18 | Media refs resolved for the request | `loop_media.go:resolveMediaRefs` | CALLBACK | `BeforeModel` |
| 3.3.19 | Pending marker and its source | `markTurnPending`, `recovery.go:recordPendingTurnSource` | ENGINE + CALLBACK | marker 2.5/2.9; source in `BeforeTurn`/`AfterTurn` |
| 3.3.20 | Pending turns replayed after a restart | `recovery.go:recoverPendingTurns` | HOST | `Run` with `AlreadyStored` |

### 3.4 The model loop (`engine_turn.go`, `turn_hooks.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.4.1 | Progress heartbeat with a tool count | `turn_hooks.go:loopPlugins`, `loop_tools.go:startProgressUpdates` | CONSUMER | `LoopStarted`, `ToolBatchFinished`, `TurnFinished` |
| 3.4.2 | One eviction notice per turn | plugin `evictions`, `publishEvictionNotice` | CONSUMER | `Compaction{Evicted}`, `TurnFinished` |
| 3.4.3 | Streaming, flushed before the reply | plugin `stream`, `stream_coalescer.go` | CALLBACK + CONSUMER | `OnTextDelta`; flush on `TurnFinished` |
| 3.4.4 | Dispatch and request logs | `logDispatchTarget`, `logRequest`; `logResponse` | ENGINE; CALLBACK | generic fields, `hook.Truncator` (1.3.13); "LLM response" from `AfterModel` (7.9) |
| 3.4.5 | Iteration cap | `run` | ENGINE | `Limits.MaxIterations` |
| 3.4.6 | Tool definitions; none for `no_tools` | `toolDefinitions` | ENGINE | toolkit, `discovery`, `Candidate.Limits.NoTools` |
| 3.4.7 | Assemble per iteration; evictions | `assemble` | ENGINE | `Compaction{Evicted}` |
| 3.4.8 | Low-steps note | `requestMessages`, `lowStepsNote` | ENGINE | per model |
| 3.4.9 | Request and refusal dumps | plugin `dumps`, `dumpResponse` | CONSUMER | `ModelCall` |
| 3.4.10 | Reasoning to a channel or inline | plugin `reasoning`, `llm_iteration.go:handleReasoning` | CONSUMER | `ModelCall` |
| 3.4.11 | Generated images saved, sent, noted | plugin `images`, `generated_images.go` | CALLBACK | `AfterModel` (`Notes`, `ReplyLines`) |
| 3.4.12 | Reasoning as the reply when configured | `reasoningAsContent` | CALLBACK | `AfterModel` |
| 3.4.13 | Empty-reply poke, degenerate | `directAnswer` | ENGINE | `TurnResult.Degenerate` |
| 3.4.14 | Tool calls normalised | `runToolCalls` | ENGINE | |
| 3.4.15 | Repeated batch: stop at four, steer | `stopRepeatedBatch`, `steerFromRepeatedBatch` | ENGINE | hint: 6.3 |
| 3.4.16 | Memory told the tools used | `runToolCalls` | ENGINE | `MemorySession.ToolsUsed` |
| 3.4.17 | Narration with tool calls | interim `Narrate`, `publishNarration` | CONSUMER | `ToolBatchStarted.Content` |
| 3.4.18 | Tool-call message keeps reasoning, signatures, notes | `assistantToolCallMessage` | ENGINE | |

### 3.5 The dispatch (`engine_dispatch.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.5.1 | Prompt cache key, temperature, streaming option | `newDispatch` | ENGINE | streaming only with an `OnTextDelta` |
| 3.5.2 | Timeout retry with jittered backoff | `requestModel`, `waitToRetry` | ENGINE | `Limits.Retries`, `RetryBackoff` |
| 3.5.3 | Overflow: notice, forced compaction, `max_tokens` halved | `compressForRetry` | ENGINE + CALLBACK | notice in `OnCompaction` (`Overflow`, once per dispatch) |
| 3.5.4 | Usage per call into the turn's | plugin `usage`, `turn_usage.go:addTurnUsage` | CONSUMER | `ModelCall` |
| 3.5.5 | Prompt-token calibration | `requestModel` | ENGINE | |
| 3.5.6 | "LLM finish" line per provider call | plugin `llm-finish`, `dispatch_event.go:emitLLMFinishEvent` | CONSUMER | `ModelCall` |
| 3.5.7 | Daily spend alert | `loop_inbound.go` (`recordSpend`) | CONSUMER | turn usage |
| 3.5.8 | Failed dispatch's error by cause | `dispatchFailed`; interim `stopCause` | ENGINE | `errors.Is` on the cause (1.3.12) |
| 3.5.9 | Fallback, cooldown skips, passed models logged | `callModel`, `logPassed` | ENGINE | `models` chain; `Fallback` |
| 3.5.10 | "Model X failed, trying Y" | plugin `fallback-notice` | CALLBACK | `OnFallback` |
| 3.5.11 | Model calls in flight counted | interim `trackModelCall` | CONSUMER | `ModelCallStarted` / `ModelCall`; after 5c `Agent.Close` |
| 3.5.12 | Unresolvable agent on the default client | `callDefaultModel`; interim `directClients`, `directModel` | ENGINE | `Catalog.Direct` with its options (1.3.14) |
| 3.5.13 | Per model: `max_tokens`, thinking, `tool_choice: none` | `requestOptions` | ENGINE | `Candidate.Limits`; interim `modelFacts` goes |
| 3.5.14 | Images hidden from a non-vision model | `messagesForModel` | ENGINE | `Candidate.Limits.Vision` |
| 3.5.15 | Reasoning backfill | `backfillReasoningContent` | ENGINE | `Candidate.ProviderSpec.RequireReasoningContent` |
| 3.5.16 | A CLI model: one call, no note, no `tool_choice` | `requestMessages`, `requestOptions` | ENGINE | |

### 3.6 Tools (`engine_tools.go`, `tool_exec.go`)

| # | Behaviour | Host reference | Kind | Carried by |
| --- | --- | --- | --- | --- |
| 3.6.1 | Parallel calls, call order kept, per-tool timeout, tool panic as error | `executeToolCalls`, `runToolCall` | ENGINE | |
| 3.6.2 | Breadcrumb per call | plugin `breadcrumbs`, `tool_exec.go:publishBreadcrumb` | CONSUMER | `ToolCallStarted` |
| 3.6.3 | Allow checker and reply chat on the call's context | `llmTurnEnv.executeTool` | ENGINE + HOST | `WithAllow`; reply chat on the run's context |
| 3.6.4 | Tool statistics and audit row | plugin `tool-record`, `recordToolCall`, `audit.go` | CONSUMER | `ToolCallFinished` (arguments as run) |
| 3.6.5 | Async tools' late results | `tool_exec.go:asyncToolCallback` | neither | 6.4 |
| 3.6.6 | A tool's ForUser and media to the chat | interim `ToolResult`, `publishToolForUser`, `publishToolMedia` | CONSUMER | `ToolResultStored`; the setting read on `ToolBatchFinished` |
| 3.6.7 | Result capped; tool-error type; references | `toolResultMessage`, `engine_tool_result_cap.go` | ENGINE | |
| 3.6.8 | Tool images by vision mode | `toolImages.route`, `addToolImages` | ENGINE | `Candidate.Limits.Vision` |
| 3.6.9 | One description of a batch's images | interim `DescribeImages`, `describeToolImages` | CALLBACK | `DescribeImages` (1.3.8) |
| 3.6.10 | Refusals logged as warnings | toolkit | ENGINE | |
| 3.6.11 | Secret redaction | `secret_redact.go` | ENGINE + CALLBACK | `Redactor` on logs; `AfterTurn` renders |
| 3.6.12 | `/cancel` reply; requests to people withdrawn | `commands/cmd_cancel.go`; `human.go:askHuman` | HOST | `Interrupt` return; the turn's context |

## 4. Ordering constraints

- 4.1 The message is complete (whispers, markers, descriptions) before it is
  stored: `BeforeTurn` runs before 2.5.
- 4.2 Pins before media resolution: two `BeforeModel` callbacks in that
  order, pinned by the host's order test.
- 4.3 Images saved before the assistant message is stored: `AfterModel`
  before 2.6.6/2.6.7; a panic there stores nothing (1.3.9).
- 4.4 `ModelCall` consumers in registration order: the "LLM finish" line,
  usage, dumps, reasoning; all before `AfterModel`.
- 4.5 Redaction before logging; `AfterTurn` renders errors redacted.
- 4.6 Audit after the call, from `ToolCallFinished`; nothing is audited
  before a call runs.
- 4.7 Stream before reply: every `TextDelta` precedes `TurnFinished`.
- 4.8 Notices before the wait: `OnCompaction`, `OnFallback` are synchronous.
- 4.9 Stored before sent: 2.9 precedes `TurnFinished`.
- 4.10 Tool output in the model's order, after storage: `ToolResultStored`
  in call order; `ToolCallFinished` is in completion order.
- 4.11 A batch's description after all its results: `DescribeImages` after
  the last `ToolResultStored`.

## 5. Host packages the turn loop imports, and their seams

The engine files import no host package (`engine_guard_test.go`). The rest
of `agent/` imports 37 module packages and three third-party modules.

| Host package | Seam |
| --- | --- |
| `logger` | `*slog.Logger`; the host's handler |
| `providers` | aliases over `models`; `HumanProvider` behind `BeforeTurn` |
| `bus` | consumers and `AfterTurn`; `hook.Outcome` |
| `tools` | `toolkit`; reply chat, ask chain, round-sent flag stay host context values |
| `tools/agents` | `TaskInfo`, `TaskController`; `SpawnDepth` is `Scope.Depth` |
| `config` | read when the agent is built and inside callbacks |
| `routing`, `constants`, `commands`, `agentreg`, `msgtoken`, `voice`, `state` | host, around `Run` |
| `utils`, `fileutil`, `media`, `channels`, `dump`, `internal/audit`, `alerts`, alerter | host callbacks and consumers |
| `global` | `accounting.Identity` via `Scope`; turn usage from `ModelCall` |
| `cogmemhost` | a `hook.Memory` adapter over `cogmem` (7.2) |
| `internal/perms` | `fsmode.Modes` |
| `tools/msg`, `tools/files`, `tools/schedule`, `tools/shell`, `tools/untrusted` | host tools in the toolkit registry |
| `internal/workspace`, `app`, `skills`, `ccv3` | the system prompt, as `WithInstructionLayers` |
| `classifier`, `classifier/classifierfs`, `forum`, `forum/forumfs`, `mcp` | host files that stay whole |

## 6. What fits neither a callback nor an event

- 6.1 **Merging queued messages and the turn slot.** Host rules (a chat, a
  command, a required reply; a slot taken when the turn starts).
  *Resolution:* the host keeps its queue, merging and slot through 5b and
  calls `Run` when the session is idle (8.2).
- 6.2 **Single-shot agents.** *Resolution:* an agent setting in
  `loop.Spec`; the engine resets before and discards after.
- 6.3 **The `file_edit` repeat hint.** The 5a engine takes it as
  `engineSpec.repeatHint`. *Resolution:* an agent option in `loop.Spec`
  (a function of the repeated tool names); not a hook.
- 6.4 **Async tools and late results.** *Resolution:* register the call as a
  task; its completion is `TaskFinished` with the result; the host routes
  it into a new `Run`. Needs the task seam of 5b (8.4).

## 7. Where the 5a engine departs from `hook`, and what changes

Each item is what the host's 5a worker changes; the engine is right where
not listed.

- 7.1 **Turn span.** `TurnStarted`/`TurnFinished` wrap only the model loop
  (`engine_turn.go:run`, with `live` in `engine_hooks.go`), and
  `TurnStarted.Options` is made up. Emit `TurnStarted` with the run's real
  options before `prepareSession` and storing the message, `TurnFinished`
  after `finishTurn` stored the reply and saved the session; drop `live`.
  Wire `BeforeTurn` and `AfterTurn` there.
- 7.2 **`ModelCall`.** Emit `ModelCallStarted` before each provider call
  (in `WithPrepare`) and `ModelCall` with the provider's own response
  pointer (no copy); give `AfterModel` a copy (struct, `ToolCalls` with
  their `Arguments`, `Images`); emit `ModelAnswer` after `AfterModel`,
  then `Image`. The no-model case (`errNoModelConfigured`) is a dispatch
  error, not a provider call: no `ModelCall`.
- 7.3 **Interim steps.** `Narrate` → `ToolBatchStarted`; `ToolsReturned` →
  `ToolBatchFinished`; `ToolResult` → `ToolResultStored`, emitted after
  each result is stored (a result the store refused is no longer shown);
  `DescribeImages` → `hook.DescribeImagesFunc` (the host's
  "could not be described" text becomes its own `ok == true` answer).
- 7.4 **`ToolCallFinished.Call`** carries the arguments as `BeforeTool`
  left them (`runToolCall` builds it from `tc.Arguments`).
- 7.5 **Panics.** `engine_hooks.go:recovered` treats every kind alike. Apply
  the package's "Panics" rules: `AfterModel` (and every turn or model
  step) ends the turn with a `*hook.PanicError`; `BeforeTool` gives the
  internal-error result, not "tool call refused: …"; observers as now.
- 7.6 **Interim `storeFailed`** → wrap in `*hook.SessionStoreError`; the
  host alerts from `TurnFinished` (`errors.Is(..., hook.ErrSessionStore)`),
  and `SaveErr` for the save after the reply.
- 7.7 **Interim `stopCause`** → `errors.Is(context.Cause(ctx),
  hook.ErrShutdown)` and so on; the host's causes (`errCancelledByUser`,
  the shutdown cause, `errAskerStopped`) wrap `ErrInterrupted`,
  `ErrShutdown`, `ErrSuperseded`. Outcomes from `hook.CauseOutcome`.
- 7.8 **Interim `trackModelCall`** → a consumer counting
  `ModelCallStarted` against `ModelCall`.
- 7.9 **Resolved: interim `formatRequest` and `responseLogFields`.**
  - 7.9.1 *`formatRequest`.* Formatting the request for the "Full LLM
    request" line is generic: it is the engine's, and moves with it to
    `loop` (`loop/log.go`). The host's text format is kept; only the
    truncation is the host's, injected as a `hook.Truncator` (the shape of
    `toolkit.WithTruncate`'s function; nil means `hook.Truncate`, the host's
    rule without its switch). It cannot be an event consumer: no event
    carries the request at the line's place (`ModelCallStarted` has no
    request; `ModelCall` has it but comes per provider call, after the
    call, and not at all for a supplied answer), so the line would move or
    multiply. Host checklist:
    - 7.9.1a move `formatMessagesForLog` and `formatToolsForLog` into an
      engine file, taking a `hook.Truncator` in place of `utils.Truncate`
      (same format, same limit of 200);
    - 7.9.1b add `truncate hook.Truncator` to `engineSpec`, nil read as
      `hook.Truncate`; `logRequest` calls the moved formatters with it;
    - 7.9.1c pass `utils.Truncate` (which honours the no-truncate switch)
      from `newEngineTurn`'s caller;
    - 7.9.1d delete `turnHost.formatRequest` and `llmTurnEnv.formatRequest`.
  - 7.9.2 *`responseLogFields`.* The "LLM response" line becomes the
    host's, whole: an `AfterModel` callback registered before every other
    one logs it, unchanged, from `ModelInfo.Iteration` and
    `ModelResponse.Response` (the engine's copy of the answer, before any
    callback changed it). That is where the engine logged it: after the
    dispatch's answer, `ModelCall` consumers, a fallback's success line and
    the token calibration, before `AfterModel` and for a supplied answer
    too, so the line, its fields and its neighbours are as today. No SDK
    surface. A `ModelCall` consumer would move it before the fallback and
    calibration lines; decorating the turn's logger would add the two
    fields to every engine line. Host checklist:
    - 7.9.2a register `hook.Plugin{Name: "response-log", AfterModel: …}`
      first among the `AfterModel` plugins (before `images`), logging
      "LLM response" through `logger.SDK()` (the engine's logger) under
      component `agent` at debug with `agent_id`,
      `iteration`, `content_chars`, `tool_calls`, `reasoning_chars`,
      `target_channel`, `channel`, and `reasoning` when content may be
      logged; it changes nothing. The line's source file and line become
      the callback's, as they would anyway when the engine moves to
      `loop`;
    - 7.9.2b delete the engine's `logResponse` and its call in `iterate`,
      `turnHost.responseLogFields` and `llmTurnEnv.responseLogFields`.
- 7.10 **Resolved: interim `modelFacts`, `directClients`, `spec.direct`,
  `directModel`.** `modelFacts` → `Candidate.Limits` and
  `Candidate.ProviderSpec`. The unresolvable agent → `Catalog.Direct(name,
  model, opts...)`, whose options make its candidate report what the
  interim path reports: `WithDirectProvider("")` (the finish line's empty
  provider, and `d.provider`), `WithDirectLimits` (the agent's own max
  tokens, context window, thinking level, no-tools and vision mode; the
  request timeout is the default model's client's, set when it was built,
  so `RequestTimeout` is left 0 as today) and `WithDirectRequireReasoningContent` (the agent's model's
  provider setting, not the default model's). The chain still records no
  health, sends no observer event and never falls back; its protocol, and
  so temperature for a CLI, is the default model's. Host checklist:
  - 7.10a in `selectTurnModel`, when no candidate resolves and
    `defaultModelClient()` names one: `al.catalog.Direct(direct, model,
    models.WithDirectProvider(""), models.WithDirectLimits(l),
    models.WithDirectRequireReasoningContent(r))`, with `l` from
    `agent.limitsFor(model)` plus the vision mode `modelFacts` reads and
    `r` from `modelFacts`; `candidates` is its `Candidates()`; it is not
    isolated (the interim path never was). An empty model id (an agent
    configured with `"models": [""]`) is sent to the client as `""`, as
    the interim path did: the client decides;
  - 7.10b delete `callDefaultModel`, the direct branches of `callModel`,
    `firstModel` and `turnVision`, `engineSpec.direct`, `directModel`,
    `directClients`, `llmTurnEnv.directClient`, `AgentLoop.directModel`
    and `turnModel.direct` where only these used it;
  - 7.10c expect `ModelRef.Protocol` now set on the direct call's events
    (nothing logged reads it), and a cancelled call's returned error to be
    `ctx.Err()`, as `ModelCall.Err` already was;
  - 7.10d `TestUnresolvedAgent_RunsOnDefaultModelClient` and
    `TestUnresolvedAgent_DefaultModelTemperature` pass unchanged.
- 7.11 **`TextDelta`** only with an `OnTextDelta` callback, as the engine
  does; now documented.
- 7.13 **`ModelCall.Err`** is `ctx.Err()` once the turn's context ended:
  in the `WithFinish` hook, `if ctx.Err() != nil { err = ctx.Err() }`.
- 7.14 **Store failure after `AfterTurn`**: the reply consumer sends the
  reply only when `TurnFinished.Outcome` is still the one it rendered for;
  `finishTurn`'s store failure becomes `Err` and `OutcomeError`.
- 7.15 **Snapshots**: `ModelCall.Request` is the dispatch's request
  (`d.sent`, low-steps note included); the host's `BeforeModel` callbacks
  replace messages instead of editing them in place.
- 7.12 **`BeforeModel`, `OnModelError`** are registered but not yet called:
  wire them as section 2 says.
- 7.16 **Resolved: interim `loopPlugins`.** The host records the pending
  turn's source, reads three per-chat settings (streaming, the fallback
  notice, tool activity) and starts the progress heartbeat when the model
  loop starts: after `BeforeTurn`, the stored message, the first assembly
  (with its compaction) and the pending marker. No existing point matched:
  `TurnStarted` and `BeforeTurn` come before the message is stored (and
  for turns that never reach the loop); the first `BeforeModel` and
  `ModelCallStarted` come after the first iteration's assembly (its
  eviction and compaction notices) and after the dispatch decided whether
  to stream. Added: the event `LoopStarted`, emitted at the end of 2.5, once
  per turn that reaches its loop, on the turn's goroutine and context.
  Plugins cannot be registered from an event, so the callbacks are
  registered when the turn is built and act on what the consumer set.
  Host checklist:
  - 7.16a emit `hook.LoopStarted{EventHeader: e.header()}` at the end of
    `engineTurn.begin`, where `loopPlugins` runs; delete
    `engineSpec.loopPlugins` and its loop;
  - 7.16b in `turnHooksFor`, one `OnEvent` consumer on `LoopStarted` does
    what `loopPlugins` does now: `recordPendingTurnSource` (on the event's
    context) and `st.pendingSource`, the coalescer for a streaming channel,
    the fallback notifier and the breadcrumbs switch into `turnState`, and
    `startProgressUpdates` on the turn's context. A panic there is now an
    observer's (logged, the turn goes on), where in `begin` it reached the
    turn;
  - 7.16c register `fallback-notice` (`OnFallback`) and `breadcrumbs`
    (`OnEvent`) with the other plugins, each doing nothing until the
    consumer set its state;
  - 7.16d register `stream` (`OnTextDelta`) only when the channel can
    stream (`streamToolNarration && al.channelManager != nil &&
    al.channelManager.SupportsStreaming(opts.Channel)`),
    because the engine asks a provider for partial text only when an
    `OnTextDelta` is registered; that capability of the channel is read
    when the turn is built rather than at `LoopStarted`; the coalescer is
    still made at `LoopStarted`, and a delta before it is impossible;
  - 7.16e `turn_reply_send_test.go` and `host_turn_helpers_test.go` start
    the loop's plugins by emitting `LoopStarted` instead of calling
    `loopPlugins`.

## 8. Open questions

- 8.1 **An unusable id and the default agent.** `ident.SameID("!!!",
  "main")` is false where the host says true. *Recommended:* keep `main`
  as the host's fallback in `NormalizeAgentID`, compare with `ident.SameID`.
- 8.2 **Who owns the session queue.** *Recommended:* the host through 5b;
  decide in 5c whether the engine merges by a key given with each run.
- 8.3 **How a tool registers a task.** *Recommended:* design it in 5b with
  the host's task store, as a value on the tool's context; add it here
  then.
- 8.4 **Redacting tool output before it is stored.** *Recommended:* not in
  5a; if adopted, an `AfterTool` callback.
- 8.5 **`ToolResultStored` after storage.** A result the store refused is no
  longer shown to the chat (today it is, just before the turn fails).
  *Recommended:* accept; the turn is abandoned anyway.
