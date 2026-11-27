# Changelog

All notable changes to the Bison9 Agent SDK are recorded here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- **BREAKING** "Project" is now "billing code" throughout, the name for what
  work is billed to (beside the origin agent and the working agent):

  | Old | New |
  | --- | --- |
  | `accounting.Identity.Project` | `accounting.Identity.BillingCode` |
  | `accounting.Record.Project`, JSON key `project` | `accounting.Record.BillingCode`, JSON key `billing_code` |
  | `accounting.DefaultProject` | `accounting.DefaultBillingCode` |
  | `accounting.CheckProject` | `accounting.CheckBillingCode` |
  | `accounting.ErrInvalidProject` | `accounting.ErrInvalidBillingCode` |
  | `hook.Scope.Project`, log attribute `project` | `hook.Scope.BillingCode`, log attribute `billing_code` |
  | `loop.Spec.Project` | `loop.Spec.BillingCode` |
  | `loop.Spec.TurnProject` | `loop.Spec.TurnBillingCode` |
  | `agent.WithProject` | `agent.WithBillingCode` |
  | `agent.WithTurnProject` | `agent.WithTurnBillingCode` |
  | `memory.CompactionState.Project`, JSON key `project` | `memory.CompactionState.BillingCode`, JSON key `billing_code` |

  A host renames its uses; anything that reads the accounting log reads
  `billing_code` instead of `project`.

## [0.2.4]

### Added

- Accounting records name the origin agent: the agent at the top of the
  chain of work that caused a launch (when Alice starts a sub-agent Bob,
  Bob's records name `alice`). `accounting.Identity.Origin` sets it;
  `Normalize` trims it, refuses one longer than 64 characters
  (`ErrNameTooLong`) and sets an empty one to `Agent`. Each record carries
  it as `Record.OriginAgent`, JSON key `origin_agent`, written right after
  `time`.
- `hook.Scope.Origin` carries the origin through a turn; `Scope.Identity()`
  returns it, and the scope's log group has `origin` when it names another
  agent than `agent`. `loop.Spec.TurnOrigin` (root option
  `WithTurnOrigin`) lets the host name it: asked once as the turn is set
  up, after the first `TurnProject` call, and by the compaction the agent
  runs after a turn. Without it, or when it returns "", the origin is
  `Identity.Origin`, by default the agent itself.

### Changed

- **BREAKING** The accounting log's `agent` key is now `agent_id`. A host or
  tool that reads the log must read `agent_id` (and may read
  `origin_agent`); the Go field `Record.Agent` keeps its name.

## [0.2.3]

### Changed

- `cogmem`: routing is bounded in time, so an idle agent stops being shown
  old topics as current. Recency filler loads only domains active within
  `PromptSettings.RecencyHours` (`WithRecencyWindow`, default 72) and its
  header gives the age ("no trigger matched; loaded because it was last
  active 2 days ago") instead of "recently active"; with nothing inside the
  window the routed block is empty. An earlier message or tool use keeps
  routing its domains for `PromptSettings.CarryoverMinutes` (default 60)
  rather than until enough other messages or tools displaced it, which a
  repeated scheduled prompt never did. A domain is marked active only by
  use of it now (a keyword in the current message, a tool called since the
  previous request, `RouteRequest.UsedTools`), no longer by a carried-over
  signal. `cogmem` may now import `clock`: `SessionOptions.Clock` and
  `WithClock` set the clock these are read from, and `store.TouchAt` marks
  a domain active at a given time.

## [0.2.2]

### Changed
- Copyright headers on every source file, an updated README and example
  code, and dependencies brought up to date.

## [0.2.1]

### Fixed

- `toolbox/shell/proc`: a job record no longer pins the workspace's
  location. With the new `Options.Root` (`shell.Config.Jobs.Root`), a
  job's working directory inside the root is recorded relative to it and
  read back against the root the next manager is given, so a host can move
  or rename its data directory with the records in it. A directory outside
  the root stays absolute; a record written by 0.2.0 still loads and is
  written relative when next saved. Without `Root` nothing changes.

## [0.2.0]

### Added

- The agent. The root package `agent` is its front door: the `Agent`
  interface (`ID`, `Name`, `Run`, `Interrupt`, `Cancel`, `Close`), the
  capability interfaces `TaskController`, `ToolHolder` and `ModelHolder`,
  `New(id, opts...)`, the `With…` options (models from a catalogue or a
  set, tools, allow checker, sessions, memory, context options, limits,
  served tools for CLI models,
  project, clock, logger, log content, redactor, plugins and one option per
  callback, each usable more than once, run in the order given) and aliases
  of the `hook` types, so a host imports one package. It holds no loop and
  starts no goroutine (a guard test).
- `loop` package: the agent (`loop.Agent`, `loop.Spec`, `New`) and the turn
  engine, moved from the first host unchanged. It does what the host did
  around the engine with the SDK's packages: tool lists per model call from
  the `toolkit` registry through `discovery`, gated execution, a `ctxengine`
  manager per session (summaries from the `Summarize` models), memory
  through `hook.Memory`, the instruction and the host's layers as the system
  prompt. Runs of one session queue behind the running turn; sessions run
  concurrently. It is the `hook.TaskRegistrar` on every tool call's context
  and its `TaskController`. `New` refuses an invalid id, missing models, a
  chat purpose with no model, or two tools offered under one name. An
  agent without a session store keeps sessions in memory. The agent id is
  used as given: it is not normalised.
- `loop`: hosting seams, each optional with the simple path as its default:
  `Spec.Identity` (who calls are billed to, separate from the id logged),
  `Spec.TurnModels` (the turn's chain and limits; an agent with no chat model
  is then accepted and its turn fails matching `hook.ErrNoModelAvailable`),
  `Spec.Session` (the host's context manager, memory, release and request
  amend per turn; the agent then keeps no session and runs no compaction of
  its own), `Spec.TurnLimits` (read per turn, the tool timeout per batch),
  `Spec.ToolContext` (each call's context), `Spec.AsyncResult` (late results
  of background tools, with the context the tool ran on, detached from
  cancellation, so the values `ToolContext` set at call time, the scope and
  the call are still on it however late the result comes, and the
  `toolkit.Info` of the tool that ran; delivered after the turn and after
  `Close` too, a panic in it recovered and logged), `Agent.ForgetSession`, `EnginePanicError` (an engine
  panic in the run's result), `CompactionStart` for a host's own managers,
  `CapToolResult` and `ToolResultCap` (the cap the engine puts on each tool
  result, for results a host delivers itself).
  `Spec.LogContent` is a function, read live.
- `loop.Tasks`, a task registry a host shares between its agents and keeps
  across their instances (`Spec.Tasks`; `NewTasks(clock, grace)`):
  `For(owner)` (the registrar and `TaskController` of one owner, which
  lists and cancels that owner's tasks only, a cancel cascading across
  owners), `CancelChildren(id, cause)` (for an id that need not be
  registered, remembered so a task started under it later is cancelled),
  `Observe` and `Shutdown`. An agent never shuts a shared registry down;
  with one, every turn's context carries the agent's view of it.
- `hook.TaskHandle.Exited` and `Sealed`: a cancel waits up to the
  registry's grace for the cancelled tasks' workers to exit, then seals
  those still running. An agent's own registry has no grace.
- `loop.ToolDefs(snapshot, revealed) Defs` (`Send`, `InContext`): the tool
  list the engine builds for a model call, exported; the engine calls it
  itself, so a host's listing or golden file is what the model is sent.
- `loop`, for hosts that own their queue and sessions: `Spec.Unqueued`
  (`agent.WithUnqueued`; `agenttest.RunUnqueued`), `Session.Compaction` (the
  manager a forced compaction uses), a nil `Session.Context` for a turn a
  `BeforeTurn` answers (`ErrNoSessionContext` otherwise), and
  `agent.SessionForgetter`. With a host session the agent sets no context
  window. The agent logs no panic or turn error it reports in `TurnFinished`.
- `hook`: `Turn.RecallText` (what memory recalls for; the message as given
  to `Run` by default), `WithoutTurnID`, `WithoutEvents` (a run whose
  handle keeps no events but `TurnFinished`, for a host that only calls
  `Wait`), `NoIterations` (a negative
  `MaxIterations`, kept by `WithDefaults`: no model request).
- `hook`: `Turn.Model` (the turn's chosen model), `WithTurnID` and
  `RunOptions.TurnID`, `NoTurnBudget`, and `WithToolCall`/`ToolCallFrom`
  (the call on a gate's and tool's context, in the engine and `mcphost`).
- `agenttest` package: the conformance suite of `agent.Agent`
  (`agenttest.Run(t, factory)`, the factory given a `Fixture`), on a
  scripted model. The SDK's agent passes it in the gate.
- `examples/chat`: an example host (two models, two tools, stdin and
  stdout, Ctrl-C stops a reply), under 100 lines, tested on a scripted
  model.
- `hook`: the one model-caller interface, `Caller` with `CompleteRequest`,
  `CompleteReply` and `ErrNoModelAvailable`; `ServedTools`, how an agent
  gives a CLI model its tools for one turn.
- `mcphost`: `Served`, a `hook.ServedTools` over a token `Store` and a
  `CLIConfig`.
- `cogmem`: `Memory` and `AsMemorySession`, a cogmem session as a
  `hook.Memory`.

- `mcphost` package: serves a `toolkit` registry over MCP streamable HTTP to
  models that run their own tool loop (CLI models). `Endpoint` (`New`,
  `Config`) is an `http.Handler` the host mounts where it likes: it lists
  every granted, enabled tool (hidden ones included) under its published
  name from a snapshot taken per request, sends `tools/list_changed` on
  catalogue changes (`Watch` for a caller's own set), and runs each call
  through `ExecuteGated` with the caller's `hook.Scope`, the host's `Gate`,
  `CallContext` and `hook.Plugin` tool callbacks and events
  (`ToolCallStarted`, `ToolCallFinished`); `AsyncResult` receives late
  results. Credentials travel as a bearer header (`Bearer`, 401 when
  refused) or as the `session_token` argument (`Argument`, injected into
  every schema and stripped from every call); the host's `Authenticator`
  decides who they are (`Caller`, `CallerOf`). `Store` issues, registers,
  resolves, rescopes and revokes session tokens with an optional expiry; a
  revoked or expired token stops at once, cancelling its calls in flight
  (`ErrRevoked`). `CLIConfig.Access` and `TokenPrompt` give a CLI-model turn
  its prompt text and MCP server entry. Logging is `Config.Logger` /
  `WithLogger`, never with token values. Adds the dependency
  `github.com/mark3labs/mcp-go` (v1.6.0), imported by `mcphost` alone.
- `mcphost`: `AsyncResultFunc` now gets the call's own context (as its
  tool ran on it, with what `CallContext` and `ToolContext` added, detached
  from the request) and the `toolkit.Info` of the tool that ran, so a late
  result is delivered with the values of the call's time. `TargetOf(ctx)`
  gives every call's `Target` (published and internal name, and whether the
  caller's set holds the tool), whether or not the tool runs.
  `Config.RefuseWithInternalName` words the refusal of a tool the caller
  lacks with its internal name; by default it names the called tool on both
  transports. `Endpoint.Refresh` returns a `RefreshResult` (`Changed`, and
  `Added` and `Removed` as `PublishedTool`s: published and internal name),
  `Config.OnChange` is given it for every read of the catalogue that changed
  what is published (at `New` too), and `Config.OnSkip` is told of every
  tool left out (`Skip`, `SkipReason`) on every read of the catalogue. A
  tool whose schema cannot be encoded claims no published name.
- `mcphost`: options to reproduce an existing server's wire behaviour.
  `Config.Catalogue` is now a `Catalogue` (`FromRegistry` for a toolkit
  registry, followed automatically; `Union` for agents that each have their
  own catalogue, changed by `Endpoint.Refresh`, which reports whether it sent
  `tools/list_changed`). `Config.Names` (`PublishedNames`, the default, or
  `ExternalFirst`) chooses published names, and a call now runs exactly the
  tool published under its name. `Config.ToolContext` adjusts a call's
  context for the tool it resolved to (the session key for session-scoped
  tools only). `Config.PanicAsProtocolError` answers a panicking tool, or
  a panic in a host callback during the call (the Authenticator,
  `CallContext`, `ToolContext`, the `Gate`, `BeforeTool`, `OnToolError`,
  `AfterTool`), with the protocol error; without it each is the
  internal-error result. Every such panic but the Authenticator's ends with
  `ToolCallFinished` carrying it. A background tool now runs on the request's context
  unless `Config.DetachBackground`. A tool the caller's set does not hold is
  refused with `NotEnabledMessage`. A bearer caller is listed and described
  from its own set alone.
- `ctxengine/memory`: `ArchiveStore.Update` runs one write transaction
  (`StateTx`) that holds the file's write lock from its start, so state read
  in it cannot change before its writes commit.
- `ctxengine/session`: `MemoryStore` (`NewMemory`), an in-memory session
  store for tests and small or ephemeral hosts. It implements
  `SessionStore` and the optional interfaces the manager uses, behaves as
  `SQLiteStore` does, and writes nothing to disk; `DeleteSession` frees a
  session. `session/sessiontest.Run` is the conformance suite both stores
  run.
- `models`: `Catalog.Direct(name, model, opts...)`, a chain of one
  candidate that calls model `name`'s client with a wire model id the
  catalogue does not define, ignoring health and switches and recording
  nothing, so a caller with no resolvable model still drives a `Chain`.
  The wire id is sent as given, even empty (the client decides).
  `WithDirectProvider`, `WithDirectLimits` and
  `WithDirectRequireReasoningContent` set the provider name, limits and
  reasoning-content setting its candidate reports, for a caller whose own
  settings for the wire model apply.
- `models`: `Chain.Do`, the chain walk for calls that are not chat calls,
  with `Candidate.Spec` and `Candidate.ProviderSpec`; `WithClassify`,
  `Verdict` and `DefaultVerdict` for a caller's own rule on which failures
  count against a model; `Chain.Client`, `Set.Client` and `Catalog.Client`
  for a model's chat client without health; call options on `Caller`
  (`Set.Caller` now takes the scope explicitly); `Set.Replace` and
  `Set.ReplaceAll` to replace whole purpose lists in one step. Parked and
  Recovered events for one model are delivered in order. `SaveSelection` is
  now also called when a list change reverts or moves a selection, and a
  failed `LoadSelection` is retried. A call cancelled during a Retry-After
  wait no longer counts against the model.
- `models`: `Catalog.Close(ctx)` shuts a catalogue down: later calls and
  writes return `ErrClosed`; it waits for calls in flight, then closes the
  clients it built (never registered ones).
- `models`: `Catalog.RemoveSet(id)` drops an agent's set and what is held for
  it in memory, without writing the selection store.
- `models`: `*ClientError` (model, provider, protocol and the bare cause)
  for every failure to build or obtain a model's client, still `ErrNoClient`
  where it was; sentinels `ErrUnknownModel`, `ErrUnknownProvider` and
  `ErrDuplicate`; `Build` reports an empty assignment list as
  `ErrNoModelAvailable`.
- `models` package, the install-wide `Catalog` (`New`): providers and
  models (`AddProvider`, `Add`, `Remove`, `Get`, `Names`), their chat
  clients built on first use and shared (`RegisterClient` for a protocol the
  SDK does not build), per-model health with the cooldown policy
  (`WithCooldown`, `Health`, `ResetHealth`), a run-time switch per model
  (`Enable`, `Disable`, `Enabled`), events for a host's alerts (`Observer`:
  `Parked`, `Recovered`, `FellBack`, `Exhausted`), and `Adopt`, which swaps
  in a newly built catalogue at the turn boundary and returns a `Diff`. A CLI
  client starts from an allowlisted environment (`DefaultBaseEnv`,
  `WithBaseEnv`) and, without its permission bypass, returns a
  `CLIDeclinedError` when the CLI declined to use tools
  (`WithDeclinedMessage`). Options: `WithLogger`, `WithLogContent`,
  `WithClock`, `WithFileModes`, `WithFileModesFor`, `WithLimitDefaults`,
  `WithIgnoredBypassNotice`.
- `models` sets: one `Set` per agent (`Catalog.Set`, the default set
  `Default`) with models by `Purpose` and priority (`Assign`, `WithSettings`,
  `WithEnabled`, `Unassign`, `Reorder`, `List`, `Purposes`), a per-set
  switch, selections with an optional `Scope` (`Select`, `Selected`, `Reset`,
  persisted through a `SelectionStore`; `Forget` drops a scope from memory),
  what is serving now (`Current`),
  purpose fallback rules (`Worker` → `Chat`, `WithFallback`), and `Begin`: a
  `Chain` pinned for one turn whose `Chat` skips parked and disabled models,
  falls back on failure and reports each hop (`WithPrepare`, `WithNotify`,
  `WithFinish`, `ExhaustedError`, `ErrNoModelAvailable`), with the first
  model's `Limits` and an `Isolated` variant. `Caller`, the one model-caller
  interface for summaries and memory consolidation, and `Build`, a catalogue
  from a `modelspec.Document` (`WithSecrets`). `models/modelstest` is a
  scripted client for tests.
- `clock` package: `Clock` (the system clock through `Or(nil)`) and
  `Fake`, a clock moved by hand (`Advance`, `Set`, `Waiters`,
  `BlockUntil`) whose `WithTimeout`/`WithDeadline` contexts end on fake
  time.
- `toolkit` package, the tool core: `Tool` and its optional interfaces
  (`AsyncExecutor`, `SessionScoped`, `ExternalNamer`, `DiscoveryGrouped`),
  `Result` and its constructors, `Refusal` and `IsExpectedRefusal`,
  `NotEnabledMessage`, the toolspec bridge (`FromSpec`, `ResultFromSpec`,
  `ResultToSpec`), `ToolToSchema` and `OpenObjectProperties`, the per-call
  context values (`WithChat`, `WithSessionKey`, `WithAgentID` and their
  getters) and `Tally`, a per-run count of tool calls and the last failure.
- `toolkit` live registry: `New` (or `NewRegistry`) returns a `Catalog`,
  safe to change from any goroutine while turns run. Writes: `Add`
  (toolspec definitions) and `AddEntries`, both refusing a duplicate name
  (`ErrDuplicate`); `Put`, which replaces; `Remove`; `Replace` and
  `ReplaceEntries`, one atomic swap of every tool under a prefix;
  `AddToolset` and `AddProvider`, which mount a named group and return a
  function that removes it. Reads: `Snapshot` (one per model call: the
  listed and hidden tools then offered, with `Execute`), `Execute`,
  `ExecuteUnattended`, `List`, `Listed`, `Hidden`, `Get`, `Lookup`,
  `Names`, `Stats` (calls and errors per tool) and `Version`. `Subscribe`
  sends one `Change{Added, Removed, Changed}` per change and none for a
  change that alters nothing, in commit order, on a delivery goroutine:
  a writer returns before its notice is delivered, and a panicking
  subscriber is logged and skipped. A call to a tool removed since its snapshot
  returns the tool error `tool "x" is no longer available`.
- `toolkit` two levels, like `models`: a process-wide `Catalog` and a
  per-agent `Set`, the tools one agent is granted. `Catalog.Set(id)` (a
  named set, granting nothing until `Grant`), `View(allow)` (an unnamed
  filtered view) and `Default`; `Set.Grant(allow AllowChecker)`. Both
  satisfy `Registry`, what an agent is given. A call to a tool the set is
  not granted returns `NotEnabledMessage` as an expected refusal.
  `Catalog.Adopt(next)` swaps in a catalogue built from a new
  configuration: agents keep their handles, named sets take next's grants,
  a running call finishes on its old tool, and each subscriber gets one
  notice.
- `toolkit` host metadata and gated calls: `Entry.Meta`, the host's own
  data about a tool, carried into `Info.Meta` in the tool's generation;
  `ExecuteGated` and `ExecuteUnattendedGated` (a `Gate` decides, for the
  exact tool a call resolved to, whether it runs), so a per-call policy
  check and the run use the same tool.
- `toolkit` logging: `WithTruncate(fn)` sets how the catalogue shortens
  the error text it logs for a failed or refused call (default: 500 runes
  plus "…"), so a host can log it in full; overwriting a hidden tool warns
  "Hidden tool registration overwrites existing tool".
- `toolkit` enable and disable: `Disable`, `Enable` and `Enabled` on the
  catalogue (off for everyone) and on a set (off for that agent), and
  `DisableGroup` / `EnableGroup` by toolset, provider or name prefix. A
  tool is usable when it is on at both levels. A disabled tool stays
  registered but is offered to no model, discovery index or MCP client,
  and a call to it returns `tool "x" is disabled`.
- `toolkit` quieter, bounded bookkeeping: a gate's result is not logged
  (its caller logs its own decisions; "Tool execution started" is logged on
  entry to every call, before any refusal, as the host's registry logged
  it), a write that changes nothing logs
  nothing, and `Stats` drops a tool's counts when the tool is removed (a
  tool replaced under the same name keeps them).
- `toolkit/untrusted`: `Wrap`, `Unwrap` and `Neutralise` mark tool output
  that came from outside the process as data for the model.
- `discovery` package, progressive tool discovery over a `toolkit`
  registry. `Hidden` and `HideEntries` hide tools (a hidden tool may still
  be called at any time), with `AlwaysShown` and `AlwaysShownFunc` pins;
  `Index` searches an agent's hidden tools with BM25 or a regular
  expression, answering for the snapshot each query names (`Search`,
  `SearchRegex`, `Details`, `Namespaces`, `MaxResults`); `MetaTools`
  provides `search_tools` and `get_tool_details` with the same names,
  schemas and text as the host's, for one registry or resolved per agent;
  `ToolDefs` returns one model call's tool definitions for a provider that
  loads deferred tools natively (`Native`) or not, with a `Session`
  recording the tool references a conversation's results carried.
- `discovery` host parity: `AlwaysShownFunc` asks its predicate once per
  tool, with the internal name only (`AlwaysShown` patterns still match
  the `ExternalName` too); `LogQueries(true)` puts the query on the "Tool
  search completed" record, as the host logged it.
- `modelspec` package: the shared definition of providers and models.
  `Provider` and `Model` with the JSON form of the host configuration they
  came from; `Model.Validate`, `Provider.ValidateProtocol`,
  `Provider.HasCredentials`, `ReservedBodyKeys` and the vision modes; a
  protocol registry (`RegisterProtocol`, `LookupProtocol`, `Protocols`,
  `IsCLIProtocol`, `TaskOnlyProtocol`); the CLI table (`CLIAgents`,
  `CLIAgentByProtocol`, `CLIArgs`, `CLIEnv`); the `Cooldown` policy and its
  defaults; a versioned `Document` (providers, models and optional per-agent
  assignments) with `Parse`, `LoadFile`, `Marshal` and `Validate`; secret
  references (`env:NAME`, `file:/path`) resolved into a copy by
  `ResolveSecrets` with a `Resolver` such as `DefaultResolver`; and
  `ResolveLimits`, a model's per-request limits over caller defaults.
  An `extra_body` key that collides with a managed request field is reported
  as a `*ReservedBodyKeyError` (model name and key) for `errors.As`. A model with no
  model id is reported as `ErrModelRequired`, also by the `models` client
  factory.
- `fsmode` package: `Modes{Dir, File}`, the modes of the directories and
  files a package creates, with the owner-only defaults `DefaultDir` (0700)
  and `DefaultFile` (0600).
- `accounting` package: one `Record` per LLM or API call launched through
  spawnllm, written as a JSON line to `<dir>/accounting.log` (`Start`,
  `Report`, `Close`), or to the logger set with `SetLogger` when no file is
  open. Each record
  includes the number of tool calls the model made. Values a provider did
  not report are written as `"-"`.
- `accounting` pricing table (`LoadPricing`, `SetPricing`, `ClearPricing`):
  per-model prices used to estimate the cost of a call that returned none.
  A row's model is an exact name or a prefix ending in `*` (`*` alone is the
  default); the exact name, then the longest prefix, wins. Cache tokens
  are priced at the input price when a row has no cache price.
  A record's `notes` say which pattern row or fallback an estimate used,
  or why a cost could not be estimated.
  Records gain `cost_source` (`provided`, `estimated` or `-`) and `images`,
  the number of images the call returned (`DispatchStatus.Images`).
- `accounting.Identity` (agent, a stable id, required; name, a friendly
  label, optional; project, default `default`; each at most 64 characters,
  a longer name shortened to end in `…`) and `spawnllm.WithAccounting` to
  set it on a `Worker`. Records carry the name as `agent_name` (`"-"` when
  not set); grouping and pricing use the agent.
- `accounting.CheckProject` and `ErrInvalidProject`: a project a host lets
  someone choose is 1 to 64 ASCII letters, digits, `.`, `_` or `-`,
  case-sensitive. `accounting.Start` takes options; `WithFileModes` sets the
  modes of the directory and file it creates (default 0700 and 0600, as
  before), set with chmod so the umask does not alter them.
- A project per turn: `loop.Spec.TurnProject` (`agent.WithTurnProject`) is
  asked as a turn is set up and before each model call; a non-empty answer
  replaces the scope's project from then on (model calls, compaction, tool
  calls, tasks started from the turn, events), an empty one keeps the
  agent's, and one `CheckProject` refuses ends the turn with an error
  wrapping `ErrInvalidProject`. The agent's summary caller takes its project
  from the scope on its context, and the compaction after a turn asks
  `TurnProject` too.
- `memory.CompactionState.Project`: a session's project, stored for the
  host in the session state (new column `project`, added to an older file
  when it is opened for writing, read as `""` from one opened read-only)
  and kept across a reset, like the host's other per-session fields.
- `DispatchStatus.Reported` and `TaskUsage.Reported`, which say which token
  counts, seconds, characters and tool call counts a response actually
  carried, and `DispatchStatus.ToolCalls`: the tool calls in an API response,
  or those in Codex's event stream.

- `toolbox/shell`: the shell tools as a `toolspec` provider. `shell_exec`
  runs a command (new arguments `timeout_seconds`, `background`, `notify`);
  `shell_ctrl` lists, waits on (`until` a matching line), sends input to, or
  stops background jobs. Long output returns its head and tail and names
  the log file; a foreground command still running at its timeout is kept
  as a job; the directory a command ends in is the default for the next one.
  The host supplies the guard, environment, workspace, owner, end-of-job
  notice, audit, and whether the caller has `shell_ctrl` or is unattended.
  `Config.Route` gives each job a host value, taken before the process
  starts and returned on its `Notice` and `AuditEvent`s, so a notice can be
  routed even for a job that ends at once. `Tools.Update(Settings)` changes
  the timeouts and the job limits at run time.
- `toolbox/kb`: knowledge bases are stored as Open Knowledge Format (OKF)
  v0.3 bundles and are OKF compatible: a bundle written by these tools can
  be read by any OKF tool, and an OKF bundle can be served by these tools
  (the package README states what that covers, where it is stricter than
  OKF, and its one extension, `x-tenebris-include`). A `Store`
  serves every caller; each call names a knowledge base that the host's
  `Resolver` maps to a `Bundle` (root, optional confining folder, writable,
  creatable, dataset rules, modes); results and errors use the name asked. `Get` returns a concept with its
  `x-tenebris-include` concepts one level deep and reports missing ones;
  `Search` ranks Markdown sections with an in-memory SQLite FTS5 index that
  follows edits made outside the store; `Put` replaces a whole record after
  checking the frontmatter schema, OKF's fields, the bundle's JSON Schema
  rules and its includes, and publishes it atomically, keeping the text of
  unchanged keys; `Verify` runs the same checks over a whole bundle. Paths
  go through `os.Root`, and links inside a bundle are not followed; under
  a confining folder the bundle directory may not be a link either. Errors
  name the knowledge base, never a host path. Reading one file costs a
  bounded amount whatever its text, and a put checks sizes on its
  arguments first and creates nothing when it is refused. The search index
  locks per bundle and keeps the most recently used ones. Limits are
  fields of `Limits`.
- `toolbox/kb/tools`: the provider for it, tools `get`, `search` and `put`
  (published as `kb_get`, `kb_search`, `kb_put`) with compact JSON results.
- `github.com/santhosh-tekuri/jsonschema/v6` is now a direct dependency
  (used by `toolbox/kb`).
- `toolbox/shell/proc`: the process manager under it. Each job runs in its
  own process group with its output in a file (head and tail kept past the
  size limit) and a record, so jobs running at a restart are marked lost.
  Limits: jobs per owner, job run time, output size, log retention;
  `SetLimits` changes them at run time. `Close` announces nothing: a job
  with notify on that it stopped is announced once by the next `New`.
- `ident` package: the agent id rule, `ValidID`, `NormalizeID`, `SameID`
  and `RepairID` (1 to 64 ASCII letters, digits, `-` and `_`, starting with
  a letter or digit; identity is the lower-case id). An input of which
  nothing survives the repair normalizes to `""`, and two such inputs are
  never the same agent.
- `hook.ToolPanicError{Tool, Value}`: the error on the result of a tool
  call whose tool panicked, so a host can tell a panic from an ordinary
  failure with `errors.As`. The turn engine (`loop`) and `mcphost` set it.
- `hook.ModelInfo.Rebuilt`: true when a step's request was built from the
  session store for that dispatch (the first request, or one after a
  compaction, eviction or overflow retry), so a `BeforeModel` callback knows
  its earlier edits are gone.
- `hook` package, the contract the agent loop will be built on, without the
  loop: the callback types (`BeforeTurnFunc`, `AfterTurnFunc`,
  `BeforeLoopFunc` with `Loop` (set-up when the model loop starts, and
  whether the turn asks for partial text), `BeforeModelFunc`,
  `AfterModelFunc`, `OnModelErrorFunc`, `OnFallbackFunc`,
  `BeforeToolFunc`, `AfterToolFunc`, `OnToolErrorFunc`, `OnCompactionFunc`,
  `OnTextDeltaFunc`, `OnInterruptFunc`, `OnEventFunc`, bundled as a
  `Plugin`) with what each may change; the events (`TurnStarted`,
  `ModelCall`, `TextDelta`, `ToolCallStarted`, `ToolCallFinished`,
  `Fallback`, `Compaction`, `Image`, `TaskStarted`, `TaskFinished`,
  `TurnFinished`, and for the host's turn loop `LoopStarted`,
  `ModelCallStarted`, `ModelAnswer`, `ToolBatchStarted`,
  `ToolBatchFinished`, `ToolResultStored`) and the guarantees of the
  stream, including a turn's span and what a panic in each kind of
  callback does (`PanicError`);
  `DescribeImagesFunc`; `Outcome`; the `Run` handle; `Message`, `UserText`
  and run options; `Scope` with `WithScope` and `ScopeOf`; the cancel causes,
  `CauseOutcome` and `InterruptedResult`; `ErrSessionStore` and
  `SessionStoreError`; `TaskInfo`, `TaskController`, `ErrUnknownTask`, and
  `TaskCancel`, `TaskInfo.Turn`, and the task seam `TaskRegistrar`,
  `TaskStart` (with `OnCancel`, `OnDetach`), `TaskHandle`,
  `WithTaskRegistrar`, `TaskRegistrarFrom`, `TaskID`, `TaskScope` and
  `Scope.Task`; `Limits`; `Memory`;
  `Redactor`; `Truncator` and its default `Truncate`, which shorten the
  request content the engine logs. `hook/DESIGN.md` maps a
  host's existing turn loop onto it.
- OpenCode CLI provider (`opencode-cli`, binary `opencode`):
  `spawnllm.NewOpenCodeCliProvider` and `NewOpenCodeCliProviderWithTimeout`,
  a `modelspec.CLIAgents` row (`run --format json`, required
  `--standalone` so the run happens in the CLI process rather than
  OpenCode's shared background service, bypass flag `--auto`) and its client
  in `models`. The prompt is piped on stdin; the text, tool calls (those an
  MCP server's Code Mode script made included), denied tool calls, session
  id and the tokens and cost of every step (summed; reasoning counted as
  output) are read from the JSON event stream, and an error event becomes an
  error `ClassifyError` can classify. `OPENCODE_` variables are in the CLIs'
  default environment allowlist, OpenCode's "Model unavailable" classifies
  as an unavailable model, and a conversation it cannot compact to fit
  (`compaction.failed`, `compaction.unavailable`) as a context limit.

### Changed

- Breaking: `hook.Run.Interrupt` takes a cause (nil means
  `hook.ErrInterrupted`): a host's "stop this reply" stops the turn with
  its own cause and leaves the turn's tasks running, where `Run.Cancel`
  also cancels them. `hook.TaskHandle` gains `Exited` and `Sealed`.
- The task registry accepts the id of an ended task again (a relaunch);
  only a running task's id is a duplicate.
- A cancel wins: a model answer that returns after its turn was stopped is
  discarded and the turn ends stopped (it used to end OK with the cause);
  its `ModelCall` still reports the usage.
- Breaking: `ctxengine.WithModelCaller` and `consolidate.NewWorker` take a
  `hook.Caller`; `ctxengine.ModelCaller`, `ModelRequest`, `ModelReply`,
  `ErrNoModel` and `consolidate.ModelCaller`, `ModelRequest`, `ModelReply`
  are gone. A caller with no model left returns an error wrapping
  `hook.ErrNoModelAvailable` (which `models.ErrNoModelAvailable` is), so a
  `models.Set.Caller` serves both packages as it is. `models.Caller`,
  `CompleteRequest` and `CompleteReply` are aliases of hook's.
- Breaking: `LLMProvider.Chat`, `speech` `Transcribe` and `Synthesize`, and
  `imagegen` `Generate` and `Edit` take an `accounting.Identity` after the
  context. A call without an agent name, or with an agent or project
  longer than 64 characters, is refused before anything is launched.
- The `DispatchStatus` of a failed CLI run also carries the usage and cost
  the CLI reported, instead of dropping them.
- The Antigravity CLI's completion token count no longer adds thinking
  tokens a second time; the CLI already includes them in its output count.
- toolspec, spawnllm, ctxengine and cogmem, previously separate modules, are
  consolidated into this repository as packages of `go.bison9.com/agent`.
- Breaking: logging uses `log/slog`. Each package takes a `*slog.Logger` and
  logs nothing without one; records carry the old component as the
  attribute `component`. `spawnllm.WithLogger`, `ctxengine.WithLogger`,
  `consolidate.WithLogger` (Worker) and `consolidate.WithManagerLogger`;
  `WithLogger` on `openai_compat`, `openai_responses`, `azure`,
  `anthropic_messages`, `speech`, `imagegen`, `memory.Open` and
  `session.NewSQLiteStore`; `SetLogger` on the CLI providers and on
  `accounting`; a `Logger` field on `cogmem.SessionOptions` and on
  ctxengine's `tools.Host`, which replaces `Host.Log`. `memory.OpenReadOnly`,
  `memory.ListSessions`, `session.MigrateJSONL`, `spawnllm.NormalizeToolCall`
  and the `common` helpers that log take a logger argument.
- Breaking: whether a CLI's stdout and stderr may be logged is an explicit
  setting, off by default: `spawnllm.WithLogContent` on a `Worker`,
  `SetLogContent` on a CLI provider.
- `anthropic_messages.NewProvider` and `NewProviderWithTimeout`,
  `azure.NewProviderWithTimeout`, and `openai_compat`'s
  `NewProviderWithMaxTokensField` and
  `NewProviderWithMaxTokensFieldAndTimeout` take options.

- Breaking: one option for file and directory modes, taking the new
  `fsmode.Modes{Dir, File}`: `WithFileModes` on `ctxengine`, `memory.Open`,
  `session.NewSQLiteStore`, cogmem's `store.Open` and `consolidate.NewWorker`,
  `openai_compat` and `openai_responses`. A zero field keeps the default and
  only permission bits are used, in every package.

### Removed

- Breaking: `spawnllm.Worker` with `New`, its options (`WithProvider`,
  `WithProviderInstance`, `WithTools`, `WithMaxIterations`,
  `WithLLMOptions`, `WithProgress`, `WithIdentity`, `WithAccounting`,
  `WithLogger`, `WithLogContent`), `ProviderSpec`, `Event` and `Result`. The
  agent loop replaces it; the provider clients stay.
- Breaking: `memory.ArchiveStore.SetState`, `AppendWindow`, `ReplaceWindow`,
  `CommitCompaction` and `TruncateWindow`, which wrote a state read before
  the transaction; use `Update`.
- Breaking: the `logger` package and its process-wide `SetBackend`.
- Breaking: `WithFolderPermissions` and `WithFilePermissions` (ctxengine,
  memory, session, cogmem store and consolidate) and the two-argument
  `WithFileModes(dir, file)` (openai_compat, openai_responses).
- Breaking: `cogmem.ConsolidationSettings.ProposeDomains` and `AutoPromote`,
  `consolidate.WithProposeDomains`, `consolidate.WithAutoPromote` and
  `consolidate.Output.Normalize`, which nothing read or which did nothing.

### Fixed

- `loop`: a panic in the host's `Spec.TurnLimits` as a turn was set up was
  not recovered; it now ends the run with an `*EnginePanicError` (`SetUp`),
  as one in `TurnModels` does.
- `openai_responses`: a user message whose attachments cannot be sent is no
  longer sent with an empty content list.
- `examples/chat`: the example stopped at once with "base_url is required";
  its Anthropic provider now names the API's base URL.
- `ctxengine/memory`: two session keys that map to one file (`a:b`, `a/b`
  and `a_b`, or keys differing only in case on a case-insensitive file
  system) silently shared one history. A file now belongs to the first key
  that writes it: opening it for another key (`memory.OpenSession`,
  `memory.OpenSessionReadOnly`, which the store, the manager's archive, the
  session tools and the JSONL migration use), every read and write through
  such a handle, and `DeleteSession` are refused with `*memory.KeyCollisionError`
  (`memory.ErrKeyCollision`), and its stored key is never overwritten.
  `ListSessions` and `ListPendingSessions` list the file under the key it
  holds. The first write through a keyed handle stores the key even when it
  only appends to the archive, so a file written only by the manager's
  archive is claimed too (and is then listed). File names are unchanged.
- `ctxengine/memory`: a session key containing `?` opened a different file
  (the path ended at the `?`), and a read-only open of one containing `#` or
  `%` could too. The path is now escaped in the SQLite URI, so the file
  opened is the file named.
- `ctxengine/memory`: two handles opening a brand-new session file at the same
  moment could fail with "archive unavailable": switching the new file to
  WAL needs the write lock, and SQLite answers a connection already holding a
  read lock at once instead of waiting. `Open` now retries each idempotent
  initialisation step within the busy timeout, and creates the schema in one
  transaction, so a reader sees all of it or none.

- `ctxengine/session`: `SQLiteStore` could silently lose messages when a
  session was forgotten (`ForgetSession`, as when a host evicts or deletes a
  session's manager) or the store closed while that session was being
  written: a write arriving meanwhile opened a second database handle, took
  the same sequence number as the write in flight, and one message
  overwrote the other. A forget or close now waits for the write in flight,
  the session keeps one handle at a time, and each write takes its sequence
  number inside its own transaction.
- `ctxengine/session`: a panic in the host's noise key function left that
  session locked, so every later operation on it hung.
- `clock`: a `Fake` `WithTimeout`/`WithDeadline` context still reported a
  nil `Err` and `context.Cause`, and an open `Done`, for a moment after its
  parent was cancelled, so code that read the cause right after a cancel
  (as a turn deciding how it ended does) saw none. They now show the
  parent's end as soon as the parent's cancel returns, as a
  `context.WithDeadline` context does.
- `spawnllm/anthropic_messages`: streamed tool call arguments that did not
  decode became `{}`; they are now kept under `"raw"` with a warning, as on
  the OpenAI-compatible path.
- `spawnllm/openai_responses`: a response whose body was not JSON (a proxy's
  HTML error page) was left out of `response_log_file`; it is now logged as
  a string, as `openai_compat` does.
- `ctxengine/session`: `MigrateJSONL` reported a failure to write a
  session's state as a failure to write its window.
- `ctxengine`: a compaction attempt whose request could not be encoded was
  left out of the debug capture file without a word; it is now logged as a
  warning.
- `cogmem/consolidate`: a run whose watermark could not be saved after its
  operations were applied left them applied, so the next run applied the same
  batch again. `Apply` now advances the watermark in its own transaction
  (`ApplyContext.Watermark`), so a batch is applied and marked consolidated
  together or not at all.
- `cogmem/consolidate`: a run that could not read the current domains or
  memories sent the model an empty current state, without a log line, and the
  model could recreate them. The run now fails with "consolidate: current
  state: …", applies nothing, keeps the watermark, and records the error.
- `ctxengine/memory`: `ListSessions` silently skipped an archive it could not read; it now returns the readable keys with an error naming each unreadable archive.
- `spawnllm`: `NormalizeToolCall` filled in the caller's `ToolCall.Function`, so the agent loop changed the response's tool calls; it now returns a copy and leaves its argument unchanged.
- `spawnllm`: a chat stream that broke mid-read was reported with the stop reason `parse_error` by `openai_compat` and `azure` but `error` by the other protocols; all four now report `error`, and `parse_error` is kept for a stream that was read but could not be parsed (`common.ErrStreamRead` tells the two apart).
