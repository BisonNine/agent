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
| [`logger`](logger) | The logging seam shared by the packages above; silent until the host installs a backend. |

Each package has its own README.

## Layering

Each package imports only the ones below it, and a guard test in each package
enforces that:

- `toolspec` imports only the standard library.
- `spawnllm` imports `toolspec`.
- `ctxengine` imports `spawnllm` and `toolspec`, never `cogmem`.
- `cogmem` imports `toolspec`, never `spawnllm` or `ctxengine`.

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
