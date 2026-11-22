# Bison9 Agent SDK

`go.bison9.com/agent` is one Go module holding the building blocks of an LLM
agent. A host application supplies the policy, the configuration and the tools;
the SDK never imports a host.

## Packages

| Package | What it is |
| --- | --- |
| [`toolspec`](toolspec) | The transport-neutral tool contract between tool packages and the hosts that run them. |
| [`spawnllm`](spawnllm) | LLM dispatch: calls a CLI or API provider and drives the tool-call loop to completion. |
| [`ctxengine`](ctxengine) | The context engine: keeps the transcript, assembles each request, evicts and summarises as the window fills. |
| [`cogmem`](cogmem) | Cognitive memory: durable, typed memories organised into domains and refined by background consolidation. |
| [`accounting`](accounting) | One record per LLM or API call launched through spawnllm, written to a file or to the logger. |
| [`modelspec`](modelspec) | The shared definition of providers and models: their JSON form, validation, the CLI table, cooldown policy, a model document, secret references and per-model limits. |
| [`models`](models) | Models at run time: the install-wide catalogue (clients, health, cooldown), per-agent sets by purpose and priority, pinned fallback chains, and `Adopt` for configuration changes. |
| [`clock`](clock) | The time source for code whose timing is tested: the system clock and a fake one moved by hand, with deadline contexts. |
| [`ident`](ident) | The agent id rule: valid ids, an id's identity, and when two ids name the same agent. |
| [`hook`](hook) | The contract the agent loop is built on: callbacks, events, the run handle, the scope on a turn's context, cancel causes, tasks and limits. |
| [`mcphost`](mcphost) | Serves an agent's tools over MCP to CLI models: the HTTP handler, the authentication seam, session tokens, and calls through the tool callbacks. |
| [`fsmode`](fsmode) | The modes of the directories and files the packages create, taken by each as `WithFileModes`. |

Each package has its own README.

## Layering

Each package imports only the ones below it, and a guard test in each package
enforces that:

- `toolspec` imports only the standard library.
- `fsmode` imports only the standard library.
- `accounting` imports only the standard library.
- `modelspec` imports only the standard library.
- `clock` imports only the standard library.
- `ident` imports only the standard library.
- `hook` imports `toolkit`, `spawnllm/protocoltypes` and `accounting`.
- `mcphost` imports `toolkit`, `toolspec`, `hook` and mcp-go; it is the only
  package that imports mcp-go, and nothing imports it.
- `spawnllm` imports `toolspec`, `accounting` and `fsmode`.
- `models` imports `modelspec`, `spawnllm`, `accounting`, `clock` and `fsmode`.
- `ctxengine` imports `spawnllm`, `toolspec` and `fsmode`, never `cogmem`.
- `cogmem` imports `toolspec` and `fsmode`, never `spawnllm` or `ctxengine`.

## Logging

Every package logs through a `*slog.Logger` the host passes in, usually with
`WithLogger`, and logs nothing without one. Records carry the attribute
`component` naming the package that wrote them. Message content is logged
only when the host allows it (`spawnllm.WithLogContent`).

## Development

`make test` is the one gate: format check, `go vet`, vulnerability scan, lint
and the race-enabled test suite. It needs `golangci-lint` and `govulncheck` on
the PATH.

## Copyright and license

Copyright (c) 2026 Bison9 Software, a division of Tenebris Technologies Inc.

This software is licensed under the MIT License. Please see LICENSE for details.

## Trademarks

Any trademarks referenced are the property of their respective owners, used for identification only, and do not imply sponsorship, endorsement, or affiliation.

## No Warranty

**(zilch, none, void, nil, null, "", {}, 0x00, 0b00000000, EOF)**

THIS SOFTWARE IS PROVIDED “AS IS,” WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND NON-INFRINGEMENT. IN NO EVENT SHALL THE COPYRIGHT HOLDERS OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

Made in Canada with internationally sourced components.
