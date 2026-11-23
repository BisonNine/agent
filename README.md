# Bison9 Agent SDK

`go.bison9.com/agent` is one Go module holding an LLM agent and the building
blocks it is made of. A host application supplies the configuration, the
tools, a logger and the moving of messages between people and the agent;
model fallback and cooldown, the tool loop, discovery, context assembly and
compaction, optional memory, cancellation and accounting come from the SDK.
The SDK never imports a host.

## An agent in a few lines

```go
m := models.New()
m.AddProvider(modelspec.Provider{Name: "anthropic", Protocol: "anthropic", APIKey: key})
m.Add(modelspec.Model{ModelName: "sonnet", Model: "claude-sonnet-5-5", Provider: "anthropic"},
    models.For(models.Chat, 1))
m.Add(modelspec.Model{ModelName: "haiku", Model: "claude-haiku-5-5", Provider: "anthropic"},
    models.For(models.Chat, 2), models.For(models.Summarize, 1))

reg := toolkit.NewRegistry()
reg.Add(lookupOrder, refund)                          // toolspec definitions
reg.AddToolset(ctx, discovery.Hidden(crmTools))       // listed only once searched for

store, _ := session.NewSQLiteStore(dir)
alice, _ := agent.New("alice",
    agent.WithName("Alice"),                          // friendly name: logs, accounting, notices
    agent.WithInstruction("You are Alice, a support agent for ..."),
    agent.WithModels(m),
    agent.WithTools(reg),
    agent.WithSessions(store),
    agent.WithBeforeTool(approveRefunds),
    agent.WithLogger(logger),
)

run := alice.Run(ctx, "session-42", agent.UserText("Where is my order?"))
for ev, err := range run.Events() { /* stream to the user */ }

// from anywhere, at any time:
run.Interrupt(nil)                  // stop this reply; sub-agents keep working
reg.Remove("refund")                // takes effect at the next model call
m.Select(models.Chat, "haiku")      // takes effect at the next turn
```

[`examples/chat`](examples/chat) is this as a program: two models, two
tools, stdin and stdout, Ctrl-C to stop a reply.

A host holds `agent.Agent`, the interface, never the concrete agent.
`TaskController`, `ToolHolder` and `ModelHolder` are optional capabilities
found by type assertion; [`agenttest`](agenttest) is the conformance suite
any implementation passes.

## Packages

| Package | What it is |
| --- | --- |
| [`agent`](.) (root) | The front door: the `Agent` interface, `New`, the `With…` options and aliases of the types a host uses. |
| [`loop`](loop) | The agent itself and its turn engine: settings (`Spec`), session queues, tools, retries and fallback, interrupt, cancel and tasks. |
| [`hook`](hook) | The contract the agent loop is built on: callbacks, events, the run handle, the scope on a turn's context, cancel causes, tasks, limits and the one model-caller interface. |
| [`agenttest`](agenttest) | The conformance suite of `agent.Agent`, on a scripted model. |
| [`toolspec`](toolspec) | The transport-neutral tool contract between tool packages and the hosts that run them. |
| [`toolkit`](toolkit) | Tools at run time: the live registry, sets per agent, gates and results. |
| [`discovery`](discovery) | Progressive tool discovery: hidden tools, search and deferred loading. |
| [`spawnllm`](spawnllm) | The provider clients (CLI and HTTP) and the message types they share. |
| [`ctxengine`](ctxengine) | The context engine: keeps the transcript, assembles each request, evicts and summarises as the window fills. |
| [`cogmem`](cogmem) | Cognitive memory: durable, typed memories organised into domains and refined by background consolidation. |
| [`accounting`](accounting) | One record per LLM or API call launched through spawnllm, written to a file or to the logger. |
| [`modelspec`](modelspec) | The shared definition of providers and models: their JSON form, validation, the CLI table, cooldown policy, a model document, secret references and per-model limits. |
| [`models`](models) | Models at run time: the install-wide catalogue (clients, health, cooldown), per-agent sets by purpose and priority, pinned fallback chains, and `Adopt` for configuration changes. |
| [`clock`](clock) | The time source for code whose timing is tested: the system clock and a fake one moved by hand, with deadline contexts. |
| [`ident`](ident) | The agent id rule: valid ids, an id's identity, and when two ids name the same agent. |
| [`mcphost`](mcphost) | Serves an agent's tools over MCP to CLI models: the HTTP handler, the authentication seam, session tokens, and calls through the tool callbacks. |
| [`fsmode`](fsmode) | The modes of the directories and files the packages create, taken by each as `WithFileModes`. |

Each package has its own README.

## Layering

Each package imports only the ones below it, and a guard test in each package
enforces that:

- `toolspec`, `fsmode`, `accounting`, `modelspec`, `clock` and `ident`
  import only the standard library.
- `hook` imports `toolkit`, `spawnllm/protocoltypes` and `accounting`.
- `spawnllm` imports `toolspec`, `accounting` and `fsmode`.
- `models` imports `modelspec`, `spawnllm`, `accounting`, `clock`, `fsmode`
  and `hook`.
- `ctxengine` imports `spawnllm`, `toolspec`, `fsmode` and `hook`, never
  `cogmem` or `models`.
- `cogmem` imports `toolspec`, `fsmode` and `hook`, never `spawnllm`,
  `models` or `ctxengine`.
- `loop` imports `hook`, `models`, `modelspec`, `toolkit`, `discovery`,
  `ctxengine`, `accounting`, `clock` and `spawnllm`; never `cogmem`
  or `mcphost`.
- The root `agent` imports `loop`, `hook`, `models`, `toolkit`, `clock` and
  `ctxengine`; it holds no loop and starts no goroutine.
- `mcphost` imports `toolkit`, `toolspec`, `hook` and mcp-go; it is the only
  package that imports mcp-go, and nothing imports it.

## Logging

Every package logs through a `*slog.Logger` the host passes in, usually with
`WithLogger`, and logs nothing without one. Records carry the attribute
`component` naming the package that wrote them. Message content is logged
only when the host allows it (`agent.WithLogContent`).

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
