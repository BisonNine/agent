# modelspec

`go.bison9.com/agent/modelspec` is the one definition of a provider and a
model that hosts and the SDK share. It is plain data: it opens no
connection, reads no environment variable or file unless asked to, and does
not log. It imports only the standard library.

## Providers and models

`Provider` is an endpoint (name, protocol, base URL, credentials and
endpoint quirks); `Model` is one configured model (alias, model id, the
provider it is reached through, and per-model settings). Their JSON tags,
field order and omit rules are those of the host configuration they came
from, so a `providers` or `models` section of such a file reads and writes
back byte for byte.

```go
p := modelspec.Provider{Name: "example", Protocol: "openai-chat",
	BaseURL: "https://api.example.com/v1", APIKey: "env:EXAMPLE_KEY"}
m := modelspec.Model{ModelName: "alice-chat", Model: "gpt-5", Provider: "example", Enabled: true}
if err := m.Validate(); err != nil { ... }
if err := p.ValidateProtocol(); err != nil { ... }
```

- `Model.Validate` requires `model_name`, `model` and `provider`, accepts
  `reasoning_effort` of `none`, `low`, `medium`, `high` or empty, and refuses
  an `extra_body` key that the request builder sets itself
  (`ReservedBodyKeys`).
- `Provider.ValidateProtocol` refuses an unknown protocol, a missing
  `base_url` on an HTTP chat protocol, and a missing `api_key` on a
  task-only protocol. `Provider.HasCredentials` is true for a keyless
  protocol (the CLIs) or when an API key is set.
- Vision modes: `VisionOff`, `VisionUserMessage`, `VisionToolResponse`.

## Protocols

The SDK knows `openai-chat`, `openai-responses`, `azure`, `anthropic`,
`anthropic-messages`, the CLIs (`claude-cli`, `codex-cli`,
`antigravity-cli`, `cursor-cli`, and `gemini-cli`, kept as an alias of
Antigravity) and the task-only `gemini` and `elevenlabs`. A host adds its
own at start-up:

```go
err := modelspec.RegisterProtocol("person", modelspec.ProtocolSpec{Keyless: true})
```

`ProtocolSpec` has three flags: `BaseURLRequired`, `TaskOnly` (api_key
required, base_url optional, no chat client) and `Keyless` (has credentials
without an api_key). `Protocols`, `LookupProtocol`, `IsCLIProtocol` and
`TaskOnlyProtocol` answer questions about them.

## CLI agents

`CLIAgents` lists the supported CLIs with their binary, base, required,
trailing and permission-bypass arguments, environment and request timeout.
`CLIArgs(protocol, bypass, extraArgs)` returns the arguments to run a model
with, and the bypass flags it dropped from `extraArgs` because bypass is off,
so the host can tell the operator. `CLIEnv` merges a model's environment
over the CLI's own.

## Cooldown

`Cooldown` holds, per HTTP-status category, how many minutes a failing model
is taken out of rotation: 0 is the default (`DefaultCooldown…Minutes`), a
negative value disables it. `BillingAuth()`, `RateLimit()`, `BadRequest()`,
`ClientError()` and `ServerError()` return the durations. A host that keeps
its own struct with extra tags converts it: `modelspec.Cooldown(hostValue)`.

## Document

`Document` is a whole model configuration:

```json
{
  "version": 1,
  "providers": [ ... ],
  "models": [ ... ],
  "assignments": {
    "alice": {
      "chat": [{"model": "sonnet"}, {"model": "grok", "enabled": false}],
      "summarize": [{"model": "haiku"}]
    }
  }
}
```

`assignments` is optional: agent id → purpose → models in priority order.
An entry without `enabled` is enabled (`Assignment.IsEnabled`).

- `Parse(data)` and `LoadFile(path)` decode and validate. Unknown fields and
  trailing data are errors.
- `Validate()` checks the version (1), unique non-empty provider names and
  their protocols, each model's own rules, unique model names, that each
  model's provider exists, and that each assignment names a known model at
  most once per list.
- `Marshal()` writes indented JSON with a final newline; empty lists are
  written as `[]`.

## Secret references

A provider's `api_key` and `proxy`, the values of a model's `env`, and
credential-named fields inside `extra_body` may hold a reference instead of a
value: `env:NAME` or `file:/absolute/path`.

```go
resolved, err := modelspec.ResolveSecrets(doc, modelspec.DefaultResolver(0o600))
```

`ResolveSecrets` returns a resolved deep copy and leaves `doc` as it was, so
the document the application saves keeps its references. The resolver is
called only for references. `DefaultResolver(maxFileMode)` reads the
environment variable (which must be set) or the file (absolute path, not a
directory, no permission bit beyond `maxFileMode` except on Windows,
contents trimmed). Nothing reads the environment or a file unless the
caller passes that resolver.

## Limits

`ResolveLimits(model, defaults)` returns the per-request `Limits` of a model:
its `max_tokens`, `context_window` and `request_timeout` when positive,
otherwise the caller's `LimitDefaults`, otherwise the built-in 8192 tokens
and 128000-token window (no timeout); its thinking level
(`ParseThinkingLevel`, off when unknown), `no_tools`, and vision mode (off
unless `user_message` or `tool_response`). A nil model gets the defaults.
