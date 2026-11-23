# loop

`go.bison9.com/agent/loop` is the agent: `loop.Agent`, built by `New` from
its settings, `loop.Spec`, and the turn engine that runs each turn. A host
normally builds it through the root package (`agent.New` and its options,
which are `func(*loop.Spec)`) and holds it as `agent.Agent`.

It implements the contract in [`hook`](../hook): every callback, event and
ordering rule documented there, and the conformance suite
[`agenttest`](../agenttest).

## Settings

`Spec` holds everything an agent is made of. `New` refuses (wrapping
`ErrInvalidSpec`) an invalid id (`ident.ValidID`), a missing model set, a
chat purpose with no model of its own and no fallback rule, and two tools
offered under one name.

| Field | What it is | Default |
| --- | --- | --- |
| `ID`, `Name`, `Description` | who the agent is; the id is used as given | — |
| `Identity` | who calls are billed to, the scope's agent, the tasks' owner | `{ID, Name, Project}` |
| `Instruction`, `InstructionLayers` | the system prompt: the instruction, then the host's layers per dispatch | none |
| `Models` | the agent's `*models.Set`: `Chat` for turns, `Summarize` for compaction | required without `TurnModels` |
| `TurnModels` | the turn's chain (and limits), chosen by the host per turn | `Models.Begin(Chat, session scope)` |
| `Session` | the turn's session context from the host (manager, memory, release, request amend, a manager for forced compaction); `Context` nil for a turn a `BeforeTurn` answers | the agent's own per session |
| `Unqueued` | the host serialises each session's runs; `Run` never queues | queued |
| `Tools`, `Allow`, `Gate` | a `toolkit.Registry`, read per model call; a grant check or gate run again at execution | no tools |
| `Sessions` | ctxengine's `session.SessionStore` | `session.NewMemory()` |
| `Memory` | `hook.Memory` (`cogmem.Memory` adapts cogmem) | none |
| `Context` | `ctxengine` options for every session's manager, after the agent's own | none |
| `Limits`, `TurnLimits` | `hook.Limits`: iterations, tool timeout, turn budget (`NoTurnBudget` for none), cancel grace, retries; or the host's per turn | `hook.Default…` |
| `ToolContext`, `AsyncResult` | each call's context from the host; background tools' late results, on the context the tool ran on (detached from cancellation) with the call and the resolved `toolkit.Info`, delivered even after the turn and after `Close` | none; synchronous |
| `Temperature`, `RepeatHint` | sent to non-CLI models; extra steering for a repeated tool batch | none |
| `Served` | `hook.ServedTools`: a CLI model's tools for its turn (`mcphost.Served`) | none |
| `Project`, `Clock`, `Logger`, `Redactor`, `Truncator`, `LogContent` (a func, read live) | accounting project, time, logging | default project, real clock, discard |
| `Plugins` | the callbacks and consumers, each kind in order | none |

## What a turn does

`Run(ctx, session, msg, opts...)` returns the run's handle at once. Runs of
one session are served one at a time, in order; a run on a busy session
waits in its queue. Different sessions run concurrently. A host that keeps
its own queue sets `Spec.Unqueued`: every run then starts at once, keeping
runs of one session apart is the host's job, and `Interrupt(session)` stops
every running turn of the session (dropped is 0 but for runs waiting on a
`ForgetSession`). It is meant with `Spec.Session`: with the agent's own
session context a turn's deferred compaction may overlap the next run.

Each turn, on the run's context with the turn's `hook.Scope`, the turn
budget, the agent's task registrar and its callbacks:

- the chain is the host's (`TurnModels`) or begun from `Models` for purpose
  `Chat` in the session's scope (so a per-session selection applies), and
  the session's context window set to its first model's;
- the session's ctxengine manager (made on its first turn, with the agent's
  logger, a summary caller from `Models` for `Summarize`, and compaction
  reports to `OnCompaction`) and memory session are used for every store,
  assembly and recall;
- the tool list of each model call is a new snapshot of `Tools` through
  `ToolDefs` (hidden tools deferred, so the list is stable);
  execution runs against the registry's current contents through `Gate` (or
  one made from `Allow`), so a tool removed mid-turn is gone from the next
  call and refused if called;
- for a CLI model with `Served` set, a credential is issued for the turn,
  its prompt added after the summary, and revoked when the turn ends or is
  stopped;
- after the turn, the compaction the session's adds deferred runs outside
  the turn (`OnCompaction`, `OnEvent` only).

The engine's own behaviour is unchanged from the host it came from (sticky
model per turn, fallback per call with cooldown skips, timeout and overflow
retries, parallel tools with per-tool timeout and cancel grace, the
repeated-batch stop, the empty-reply poke, `tool_choice: none` on the last
step, prompt-token calibration).

## Stopping

- `Run.Interrupt`: that run only (its turn, or its place in the queue).
- `Run.Cancel(cause)`: that run and the tasks its turn started.
- `Agent.Interrupt(session) (running, dropped)`: the session's turn and its
  queue; tasks keep running.
- `Agent.Cancel(cause)`: every turn, queue and task. A cause wrapping
  `hook.ErrShutdown` tells the agent it is shutting down: tasks' contexts
  end without `OnCancel` or `TaskFinished`, so a host's records relaunch
  them, and no task starts.
- `Agent.Close(ctx)`: refuses new runs (`ErrClosed`), lets running and
  queued turns finish until `ctx` ends, then stops the rest as a shutdown,
  waits, and closes every session's manager and memory. Every goroutine the
  agent started has ended when it returns, except that of a tool call that
  ignored its cancellation past the cancel grace (the turn abandoned it).

`OnInterrupt` is told once per stop, with what was stopped and dropped.

A cancel wins (package `hook`, "Cancel wins"): once a turn's context has
ended, a model answer it has not taken is discarded and the turn ends as one
stopped with its call in flight. Its `ModelCall` still carries the response
and usage (`Err` nil). The last points are: just before `ToolBatchStarted`
for tool calls (then the batch runs and a stop interrupts its tools), and
the loop's end, before `AfterTurn`, for a reply. A stop after that keeps the
turn's outcome and stored reply.

## Tasks

The agent is the `hook.TaskRegistrar` on every tool call's and task's
context (`hook.TaskRegistrarFrom`), and its `Tasks`, `CancelTask` and
`CancelAll` are the `hook.TaskController` over the same registry. A task's
context is not a child of the turn's; cancelling cascades to children
first; `OnCancel` runs before the context is cancelled; `Detach` turns a
`TaskWait` task into a `TaskCallback` one, or cancels it once the agent is
shutting down. `TaskStarted` and `TaskFinished` go to the run while its turn
runs, and to `OnEvent` always. A turn run on a task's context (a
sub-agent's) keeps the registrar that task was started from, so the tasks
it starts are that task's children there and a cancel of it reaches them.
The registry remembers the last 256 ended
tasks for `AlreadyEnded`.

## Hosting an existing application

A host with its own session cache, model selection, identity and timing
supplies them through the optional `Spec` fields above; `hook/DESIGN.md`
section 10 is the checklist. A panic in the engine (or in the host's
`TurnModels`/`Session`) ends the run with an `*EnginePanicError` in its one
`TurnFinished`: a host that wants its own recovery re-panics with its
`Value` on its own goroutine. The agent logs neither a panic nor a turn
error it delivers in `TurnFinished`. `ForgetSession(key)` (the root
package's `agent.SessionForgetter`) drops an idle session the agent holds;
with `Spec.Session` it holds none. Memory recalls for `hook.Turn.RecallText`:
the message as given to `Run`, unless a `BeforeTurn` sets it (a host that
prepends held notes and then appends attachment references sets it in
between). `hook.WithoutTurnID()` runs a turn with no id at all;
`hook.NoIterations` a turn with no model request. A run's handle holds its
events until `Events` reads them; a host that only calls `Wait` runs its
turns with `hook.WithoutEvents()`, so the handle keeps only `TurnFinished`.
`CapToolResult` caps a tool result a host delivers itself as the engine
caps those it stores (`ToolResultCap` is the cap for a context window).
`ToolDefs(snapshot, revealed)` is the tool list the engine builds for a
model call (`Defs.Send`, what the provider is sent; `Defs.InContext`, what
the schemas cost): the engine calls it itself, so a host that shows or
tests the tool list uses it rather than a copy.

## Not here

The host's queue rules (merging, commands, slots), its task records and
supervisor, and single-shot agents (the host's session context does the
reset and discard) are not part of this package.
