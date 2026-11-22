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
| `ID`, `Name`, `Description` | who the agent is; the id is normalised | — |
| `Instruction`, `InstructionLayers` | the system prompt: the instruction, then the host's layers per dispatch | none |
| `Models` | the agent's `*models.Set`: `Chat` for turns, `Summarize` for compaction | required |
| `Tools`, `Allow`, `Gate` | a `toolkit.Registry`, read per model call; a grant check or gate run again at execution | no tools |
| `Sessions` | ctxengine's `session.SessionStore` | `session.NewMemory()` |
| `Memory` | `hook.Memory` (`cogmem.Memory` adapts cogmem) | none |
| `Context` | `ctxengine` options for every session's manager, after the agent's own | none |
| `Limits` | `hook.Limits`: iterations, tool timeout, turn budget, cancel grace, retries | `hook.Default…` |
| `Temperature`, `RepeatHint` | sent to non-CLI models; extra steering for a repeated tool batch | none |
| `Served` | `hook.ServedTools`: a CLI model's tools for its turn (`mcphost.Served`) | none |
| `Project`, `Clock`, `Logger`, `Redactor`, `Truncator`, `LogContent` | accounting project, time, logging | default project, real clock, discard |
| `Plugins` | the callbacks and consumers, each kind in order | none |

## What a turn does

`Run(ctx, session, msg, opts...)` returns the run's handle at once. Runs of
one session are served one at a time, in order; a run on a busy session
waits in its queue. Different sessions run concurrently. A host that keeps
its own queue and calls `Run` only on an idle session gets the same turn.

Each turn, on the run's context with the turn's `hook.Scope`, the turn
budget, the agent's task registrar and its callbacks:

- the chain is begun from `Models` for purpose `Chat` in the session's
  scope (so a per-session selection applies), and the session's context
  window set to its first model's;
- the session's ctxengine manager (made on its first turn, with the agent's
  logger, a summary caller from `Models` for `Summarize`, and compaction
  reports to `OnCompaction`) and memory session are used for every store,
  assembly and recall;
- the tool list of each model call is a new snapshot of `Tools` through
  `discovery.ToolDefs` (hidden tools deferred, so the list is stable);
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
  agent started has ended when it returns.

`OnInterrupt` is told once per stop, with what was stopped and dropped.

## Tasks

The agent is the `hook.TaskRegistrar` on every tool call's and task's
context (`hook.TaskRegistrarFrom`), and its `Tasks`, `CancelTask` and
`CancelAll` are the `hook.TaskController` over the same registry. A task's
context is not a child of the turn's; cancelling cascades to children
first; `OnCancel` runs before the context is cancelled; `Detach` turns a
`TaskWait` task into a `TaskCallback` one, or cancels it once the agent is
shutting down. `TaskStarted` and `TaskFinished` go to the run while its turn
runs, and to `OnEvent` always. The registry remembers the last 256 ended
tasks for `AlreadyEnded`.

## Not here

The host's queue rules (merging, commands, slots), its task records and
supervisor, single-shot agents, idle session pruning and async tools' late
results (they are dropped; start a `TaskCallback` task instead) are not part
of this package.
