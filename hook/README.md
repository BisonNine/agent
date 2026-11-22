# hook

`go.bison9.com/agent/hook` is the contract between the agent's turn engine
and the host that runs it. It holds types, interfaces, constants and pure
helpers only: the engine is `agent/loop`, and the root `agent` package
re-exports what a host needs, so a host normally imports only the root. It
imports `toolkit`, `spawnllm/protocoltypes` (types only, not the provider
clients), `accounting` and the standard library.

`DESIGN.md` maps a host's existing turn loop onto this package: which of its
behaviours stay in the engine, which become callbacks and which become event
consumers.

## Watching and changing

Watching is done with events; a callback exists only to change a step.

- **Events.** `Run.Events()` is one turn's stream (a Go iterator,
  `for ev, err := range run.Events()`); `OnEvent` sees every run of the
  agent, including those the host holds no handle for. The events are
  `TurnStarted`; per provider call `ModelCallStarted` and `ModelCall` (the
  provider's own response, before any callback: logging, usage, dumps);
  `ModelAnswer` (the answer after `AfterModel`: what a UI shows);
  `TextDelta`; `Fallback`; per batch of tool calls `ToolBatchStarted`,
  `ToolCallStarted`, `ToolCallFinished`, `ToolBatchFinished` and
  `ToolResultStored`; `Compaction`; `Image`; `TaskStarted`, `TaskFinished`;
  and `TurnFinished`. Each carries an `EventHeader` (the turn's `Scope` and
  the time).
- **Callbacks that change a step.** `BeforeTurn`, `AfterTurn`,
  `BeforeModel`, `AfterModel`, `OnModelError`, `BeforeTool`, `AfterTool`,
  `OnToolError`, and `DescribeImages` (one text for the images a batch's
  tools returned, for a model that cannot see them). A `Before…` callback
  may edit the step's input; a non-nil replacement it returns skips the
  step. An `After…` callback edits the step's result in place. Several of
  one kind run in registration order.
- **Panics.** A panic in an observer is logged and the turn goes on; in a
  callback that changes a turn or model step it ends the turn with
  `OutcomeError` and a `*PanicError`; in a tool callback it fails that call.
- **Callbacks that only observe,** at the moment a step happens:
  `OnFallback`, `OnCompaction`, `OnTextDelta`, `OnInterrupt`.
- A `Plugin` is a named bundle of callbacks.

The package documentation and each type's comment state what a callback may
change, the goroutine and context it runs on, and the guarantees of the
event stream (exactly one `TurnFinished` per run, a `ToolCallFinished` for
every `ToolCallStarted`, and what is emitted on a cancel).

## The run

`Run` is the handle `Agent.Run` returns: `Events`, `Interrupt` (the turn
only; its tasks keep running), `Cancel(cause)` (the turn and its tasks),
`Done` and `Wait`, which returns the `TurnResult`. `Outcome` is `ok`,
`error`, `cancelled` or `empty`. A run is given a `Message` (`UserText` for
text alone) and `RunOption`s: `AlreadyStored`, `ResetSession`,
`TurnBudget`, `AtDepth`.

## Scope

`Scope` (agent id and name, session, turn id, depth, project) is put on a
turn's context once with `WithScope` and read anywhere with `ScopeOf`.
`Identity()` is its `accounting.Identity`; it logs as a group
(`slog.LogValuer`). `WithScope` also sets toolkit's agent id and session
key, so tools keep reading those through `toolkit`. Values only a host
knows stay in the host's own context keys.

## Cancellation and tasks

The cancel causes are `ErrInterrupted`, `ErrShutdown`, `ErrDeadline` and
`ErrSuperseded`; a host's own cause wraps one, and `CauseOutcome` maps a
cause to the turn's outcome. A write the session store refused is a
`*SessionStoreError` (`errors.Is(err, ErrSessionStore)`), in
`TurnResult.Err` or, for the save after the reply, `TurnResult.SaveErr`. A tool call abandoned after
an interrupt gets `InterruptedResult()`. Tasks (sub-agents and other
background jobs) run on their own contexts: `TaskInfo`, `TaskState`
(`running`, `done`, `error`, `cancelled`), `TaskMode` (`wait`, `callback`)
and the `TaskController` capability (`Tasks`, `CancelTask`).

## What the loop is given

- `Limits`: iterations, per-tool timeout, turn budget, cancel grace,
  retries and their backoff. `WithDefaults` fills zero fields. A request's
  own limits are the model's (`modelspec.Limits`).
- `Memory` / `MemorySession`: optional long-term memory, opened per session
  (`Recall`, `Observe`, `ToolsUsed`, `Close`).
- `Redactor`: what the engine applies to the error text and content it logs.

The session store is `ctxengine`'s own `session.SessionStore`, the clock
`clock.Clock`, and the model-caller `models.Caller`; this package does not
declare copies of them. The agent id rule is the `ident` package.
