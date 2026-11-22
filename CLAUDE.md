# Bison9 Agent SDK

`go.bison9.com/agent` (repository `github.com/BisonNine/agent`): one Go module
holding `toolspec`, `toolkit`, `discovery`, `spawnllm`, `ctxengine`,
`cogmem`, `accounting`, `fsmode`, `modelspec`, `clock`, `models`, and the
tool packages under `toolbox/` (`toolbox/shell`).

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
- `cogmem` imports only `toolspec`, `fsmode`, the standard library, SQLite,
  `google/uuid` and `yaml.v3`. It never imports `spawnllm` or `ctxengine`.
- `ctxengine` imports only `spawnllm`, `toolspec`, `fsmode`, the standard
  library and SQLite. It never imports `cogmem`.
- `modelspec` imports only the standard library. It never imports
  `spawnllm`, and does not log.
- `clock` imports only the standard library; code whose timing is tested
  takes a `clock.Clock` instead of calling the `time` package.
- `models` imports only `modelspec`, `spawnllm` (with its provider
  packages), `accounting`, `clock`, `fsmode` and the standard library. It
  knows no host protocol, name or alert; a host supplies those through
  `RegisterClient`, `WithObserver` and `WithDeclinedMessage`.
- `accounting` imports only the standard library. `spawnllm` may
  import it; `ctxengine` and `cogmem` do not.
- `toolbox/shell/proc` imports only `fsmode`, the standard library and its
  own `proctest`. It takes its modes as `Options.Modes` (`fsmode.Modes`).
- `toolbox/shell` imports only `toolspec`, `fsmode`, its own `proc`, and the
  standard library. Nothing else in the SDK imports `toolbox`.
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
