# Bison9 Agent SDK

`go.bison9.com/agent` (repository `github.com/BisonNine/agent`): one Go module
holding `toolspec`, `spawnllm`, `ctxengine`, `cogmem` and the shared `logger`.

## Status

- **Unreleased.** No tags exist; do not preserve legacy behaviour unless asked.
- **Version in development: 0.0.1**, on the branch `dev/0.0.1`. The module
  stays in 0.0.x until the maintainer says otherwise. Do not create tags.
- A library: there is no `make install` target.

## Layering

The packages used to be separate modules, and the module boundaries enforced
these rules. Now the `imports_test.go` guard in each package does. Keep them:

- `toolspec` imports only the standard library.
- `spawnllm` imports only `toolspec`, the standard library and provider SDKs.
- `cogmem` imports only `toolspec`, the standard library and SQLite. It never
  imports `spawnllm` or `ctxengine`.
- `ctxengine` imports only `spawnllm`, `toolspec`, the standard library and
  SQLite. It never imports `cogmem`.
- `logger` imports only the standard library; `spawnllm`, `ctxengine` and
  `cogmem` may all import it.
- `internal/` directories stay inside their package (`spawnllm/internal`,
  `ctxengine/internal`); do not hoist them to the module root.
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
