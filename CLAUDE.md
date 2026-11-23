# Bison9 Agent SDK

`go.bison9.com/agent` (repository `github.com/BisonNine/agent`): one Go module
holding `toolspec`, `toolkit`, `discovery`, `spawnllm`, `ctxengine`,
`cogmem`, `accounting`, `fsmode`, `modelspec`, `clock`, `models`, `ident`,
`hook`, `loop`, the root package `agent`, `agenttest`, `mcphost`, the
`examples`, and the tool packages under `toolbox/` (`toolbox/shell`,
`toolbox/kb`).

## Status

- **Unreleased.** No tags exist; do not preserve legacy behaviour unless asked.
- **Version in development: 0.2.0**, on the branch `dev/0.0.1` (named before
  the version was chosen). It will be merged and tagged `v0.2.0` by the
  maintainer. Do not create tags.
- A library: there is no `make install` target.

## Layering

The packages used to be separate modules, and the module boundaries enforced
these rules. Now the `imports_test.go` guard in each package does. Keep them:

- `toolspec` imports only the standard library.
- `toolkit` imports only `toolspec` and the standard library (its
  subpackage `toolkit/untrusted` only the standard library). It never
  imports `spawnllm`: tool definitions for a provider are built by the
  caller from what the registry lists.
- `discovery` imports only `toolkit`, `toolspec` and the standard library.
  It never imports `spawnllm`: its `Def` is copied into a provider's type
  by the caller, and `Native` asks a provider through its own interface.
- `fsmode` imports only the standard library; `spawnllm`, `ctxengine` and
  `cogmem` take their file and directory modes as an `fsmode.Modes`
  (`WithFileModes`).
- `spawnllm` imports only `toolspec`, `accounting`, `fsmode`, the standard
  library, `google/uuid` and provider SDKs.
- `cogmem` imports only `toolspec`, `fsmode`, `hook` (the one model-caller
  interface, and its `hook.Memory` adapter), the standard library, SQLite,
  `google/uuid` and `yaml.v3`. It never imports `spawnllm`, `models` or
  `ctxengine`.
- `ctxengine` imports only `spawnllm`, `toolspec`, `fsmode`, `hook` (the one
  model-caller interface), the standard library and SQLite. It never imports
  `cogmem` or `models`.
- `modelspec` imports only the standard library. It never imports
  `spawnllm`, and does not log.
- `clock` imports only the standard library; code whose timing is tested
  takes a `clock.Clock` instead of calling the `time` package.
- `models` imports only `modelspec`, `spawnllm` (with its provider
  packages), `accounting`, `clock`, `fsmode`, `hook` (whose model-caller
  types it aliases) and the standard library. It
  knows no host protocol, name or alert; a host supplies those through
  `RegisterClient`, `WithObserver` and `WithDeclinedMessage`.
- `accounting` imports only the standard library. `spawnllm` may
  import it; `ctxengine` and `cogmem` do not.
- `ident` (the agent id rule) imports only the standard library, so a
  host's configuration can use it without the rest of the SDK.
- `hook` (the contract the agent loop is built on) imports only `toolkit`,
  `spawnllm/protocoltypes` (types, never the `spawnllm` package with its
  clients), `accounting` and the standard library. It holds no logic: no
  loop, no goroutine, no provider code. It never imports `models`,
  `ctxengine`, `cogmem` or `discovery`; the loop does.
- The root package `agent` is a front door: only `agent.go` (the `Agent`
  interface, the capability interfaces, `New`), `options.go` and
  `aliases.go`, with their tests. It imports only `loop`, `hook`, `models`,
  `toolkit`, `clock`, `ctxengine` (with `session`) and `context`, `log/slog`,
  `time`; it holds no loop and starts no goroutine (`imports_test.go`).
  Nothing in the SDK imports it except `agenttest` and `examples`.
- `loop` (the agent and its turn engine: `loop.Spec`, `loop.Agent`) imports
  only `hook`, `models`, `modelspec`, `toolkit`, `discovery`, `ctxengine`
  (with `ctxengine/session`), `accounting`, `clock`, `spawnllm`
  (message types and its pure helpers; never a client of its own) and the
  standard library; its tests also `models/modelstest` and `toolspec`. It
  never imports `cogmem`, `mcphost` or the root package: memory arrives as a
  `hook.Memory`, served tools for CLI models as a `hook.ServedTools`.
- `mcphost` (serving tools over MCP to CLI models) imports only `toolkit`,
  `toolspec`, `hook`, the standard library and `mark3labs/mcp-go`. It is the
  only package that imports mcp-go, and nothing else in the SDK imports
  `mcphost`, so a host with no CLI model never links mcp-go. It knows no
  host name, path, token format or tool prefix: the host passes them in.
- `toolbox/shell/proc` imports only `fsmode`, the standard library and its
  own `proctest`. It takes its modes as `Options.Modes` (`fsmode.Modes`).
- `toolbox/shell` imports only `toolspec`, `fsmode`, its own `proc`, and the
  standard library. Nothing else in the SDK imports `toolbox`.
- `toolbox/kb` (knowledge bases as OKF bundles) imports only `fsmode`, the
  standard library, `yaml.v3`, `santhosh-tekuri/jsonschema/v6` and SQLite;
  never `toolspec`. Its provider `toolbox/kb/tools` imports only `kb`,
  `toolspec` and the standard library, so the tools use nothing a host's
  own code cannot. The library knows no folder or naming scheme: bundles
  come from the host's `kb.Resolver`.
- `internal/` directories stay inside their package (`spawnllm/internal`,
  `ctxengine/internal`); do not hoist them to the module root.
- Logging is `log/slog`, injected per instance (`WithLogger` or the
  package's equivalent; `accounting`, one sink per process, has
  `SetLogger`). Nothing logs without one and nothing uses `slog.Default()`.
  Each package tags its records with the attribute `component`. There is no
  logger package or logging interface of our own.
- Each guard lists the third-party modules its package may import. Adding a
  dependency means adding it to that list on purpose.
- The SDK never imports a host (ClawEh, Maestro, MCPFusion). Tools reach it by
  injection as `toolspec` definitions.

## Testing

- `make test` is the one gate for the whole module (`./test.sh`): format
  check, `go vet`, vulnerability scan, `golangci-lint` and
  `go test -race -count=1 ./...`.
- One `.golangci.yaml` covers every package. Where the packages' old
  configurations differed, the stricter setting is the default and the looser
  package keeps its exemption by path.
- Tests behind the `integration` build tag need real CLIs and are not part of
  the gate.
