# models

`go.bison9.com/agent/models` is everything about models at run time: the
providers and models, their clients, their health, and which models serve
which purpose. A host builds it from its configuration and asks it
questions; the fallback, cooldown and client handling are the package's. It
imports `modelspec`, `spawnllm` (with its provider packages), `accounting`,
`clock`, `fsmode` and the standard library, never a host.

## The catalogue

```go
c := models.New()
c.AddProvider(modelspec.Provider{Name: "anthropic", Protocol: "anthropic",
	BaseURL: "https://api.anthropic.com", APIKey: key})
c.Add(modelspec.Model{ModelName: "sonnet", Model: "claude-sonnet-5-5", Provider: "anthropic"},
	models.For(models.Chat, 1))
c.Add(modelspec.Model{ModelName: "haiku", Model: "claude-haiku-5-5", Provider: "anthropic"},
	models.For(models.Chat, 2), models.For(models.Summarize, 1))

chain := c.Begin(models.Chat)                     // pinned for one turn
res, err := chain.Chat(ctx, accounting.Identity{Agent: "alice", Name: "Alice"},
	models.Request{Messages: msgs, Tools: tools})
```

- `AddProvider(p)`, `Add(m, uses...)`, `Remove(name)`. `Add` validates the
  model, which starts enabled whatever `m.Enabled` says. `For(purpose,
  priority)` assigns it: lower priority first, ties in the order added.
- `Get(name) (Info, bool)`, `Names()`, `Health()`, `ResetHealth(names...)`.
- `RegisterClient(name, client)`: a ready-made client for a model whose
  protocol the SDK does not build (a host-registered protocol).
- `Enable`, `Disable`, `Enabled`: a disabled model is skipped as a parked one
  is. The default is on.

`Purpose` is an open string type; the SDK uses `Chat`, `Summarize`, `Memory`
and `Worker`.

## Chains

`Begin` pins the models of a purpose for one turn. The order and clients do
not change for the chain's life; only the enable switch is re-read before
each call.

`chain.Chat(ctx, id, req, opts...)` tries each model in turn:

- a disabled model is skipped (`Attempt.Disabled`), a parked one too
  (`Attempt.Remaining`, `Attempt.Reason` its cause);
- a cancelled context ends the call with `context.Canceled`;
- an error spawnllm cannot classify ends the call; a classified one is
  recorded against the model's health and the next model is tried; a
  `Retry-After` of up to 3 seconds is waited out and the same model tried once
  more first;
- when every model failed or was skipped the error is an `*ExhaustedError`
  (`AllContextLimit()` says whether compaction might help); when every model is
  disabled, or there is none, an error wrapping `ErrNoModelAvailable` that
  names the purpose.

`WithPrepare` shapes the request for each model, `WithNotify` is called each
time the call moves on (a run of skipped models is one notice; none for the
model that ends the call), and `WithFinish` after each model call.
`chain.Limits()` is the first model's `modelspec.Limits` over
`WithLimitDefaults`. `chain.Isolated(workspace)` runs CLI models in the given
workspace without a permission-bypass flag, for an agent that has no tools to
grant.

Every call is made for the `accounting.Identity` the caller passes; a call
without an agent is refused before any client is called.

## Updating: Adopt

A configuration change is a new catalogue built the same way as the first and
handed over: `c.Adopt(next) Diff`.

- Every error is returned while `next` is built; `Adopt` cannot fail.
- `c` does not change; agents keep what they hold.
- A chain begun before finishes on what it pinned, old clients included; every
  chain begun after uses the new contents. Failures reported by an old chain
  are dropped with the old contents.
- Health is not carried: every model starts available.
- A model whose definition and provider are unchanged keeps its client. Any
  other old client is closed (`spawnllm.StatefulProvider`) once its last call
  in flight ends.
- `next`'s options replace `c`'s; `next` is spent (`ErrAdopted`).

`Diff` lists the models added, removed and changed.

## Health and cooldown

Health is kept per provider and wire model (`spawnllm.ModelKey`), so models
that share both share it. A failure with an HTTP status takes the model out of
rotation: 1, 3 and 5 minutes for the first three consecutive failures, then
the category cooldown of `WithCooldown(modelspec.Cooldown)`; billing and auth
failures (401, 402, 403) take the category cooldown at once. 413 and errors
without a status never cool. A `Retry-After` replaces the computed cooldown,
capped at 5 minutes. Failure counts reset after 24 hours without a failure and
on an answer. Time is read from `WithClock`.

## Events

`WithObserver(o)` receives `Parked` (each failure that parks a model, with its
reason, status, consecutive failures, whether past the escalation steps, and
for how long), `Recovered` (a model that answered after being parked, and since
when it had been failing; also after `ResetHealth` or `Adopt`), `FellBack` (a
chain moving on) and `Exhausted` (a call that ended without an answer). They
are called on the calling goroutine with no lock held and must not block.

## Clients

The catalogue builds a model's client on first use and shares it.

- `WithBaseEnv(fn)`: the environment a CLI starts from. Default
  `DefaultBaseEnv`, an allowlist (the POSIX basics, locale, XDG, proxies, CA
  bundles and the CLIs' own key variables), never the whole environment.
- `WithFileModes`, `WithFileModesFor`: the modes of a response log file.
- `WithDeclinedMessage(fn)`: the text of the `*CLIDeclinedError` a CLI without
  its permission bypass returns when it declined to use tools. `fn` gets the
  identity of the call, the model and the refused tools.
- `WithLogger`, `WithLogContent`.

## Locking

The catalogue is an immutable snapshot behind an atomic pointer; writers
build the next one under one mutex. No lock is held across a model call or an
Observer call. Health has its own lock. Definitions are returned as copies;
nothing a caller holds changes the live object.

## Testing

`models/modelstest` is a scripted client: register it for a model with
`RegisterClient`, script each wire model's answers (`Reply`, `Fail`,
`Status`, or a `Step` that holds until released), and read back the calls.
