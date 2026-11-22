# Changelog

All notable changes to the Bison9 Agent SDK are recorded here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/).

## [0.2.0]

### Added

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
- `hook.ModelInfo.Rebuilt`: true when a step's request was built from the
  session store for that dispatch (the first request, or one after a
  compaction, eviction or overflow retry), so a `BeforeModel` callback knows
  its earlier edits are gone.
- `hook` package, the contract the agent loop will be built on, without the
  loop: the callback types (`BeforeTurnFunc`, `AfterTurnFunc`,
  `BeforeModelFunc`, `AfterModelFunc`, `OnModelErrorFunc`, `OnFallbackFunc`,
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
  `SessionStoreError`; `TaskInfo` and `TaskController`; `Limits`; `Memory`;
  `Redactor`; `Truncator` and its default `Truncate`, which shorten the
  request content the engine logs. `hook/DESIGN.md` maps a
  host's existing turn loop onto it.

### Changed

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

- Breaking: the `logger` package and its process-wide `SetBackend`.
- Breaking: `WithFolderPermissions` and `WithFilePermissions` (ctxengine,
  memory, session, cogmem store and consolidate) and the two-argument
  `WithFileModes(dir, file)` (openai_compat, openai_responses).
