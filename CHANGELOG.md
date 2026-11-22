# Changelog

All notable changes to the Bison9 Agent SDK are recorded here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the
project uses [Semantic Versioning](https://semver.org/).

## [0.2.0]

### Added

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
  as a `*ReservedBodyKeyError` (model name and key) for `errors.As`.
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
- `toolbox/shell/proc`: the process manager under it. Each job runs in its
  own process group with its output in a file (head and tail kept past the
  size limit) and a record, so jobs running at a restart are marked lost.
  Limits: jobs per owner, job run time, output size, log retention.

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
