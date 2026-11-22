# models

`go.bison9.com/agent/models` is everything about models at run time: the
providers and models, their clients, their health, and which models each
agent uses for which purpose. A host builds it from its configuration and
asks it questions; the fallback, cooldown and client handling are the
package's. It imports `modelspec`, `spawnllm` (with its provider packages),
`accounting`, `clock`, `fsmode` and the standard library, never a host.

## A single agent

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

c.Select(models.Chat, "haiku")                    // takes effect at the next Begin
```

`models.New()` has a default set (`Set("")`); `Add` with `For` assigns to
it, and the catalogue's `Assign`, `List`, `Select`, `Selected`, `Reset`,
`Current` and `Begin` act on it, so a single-agent host never sees the two
levels.

## Two levels: Catalog and Set

The **Catalog** is install-wide: the definitions, the clients and health. A
model out of credit is out of credit for every agent. A **Set** is one agent's
view onto it: which models serve which purpose, in priority order. Alice and
Bob may share a model at different priorities.

```go
doc, _ := modelspec.LoadFile("models.json")
c, _ := models.Build(doc, models.WithSecrets(modelspec.DefaultResolver(0o600)))
alice := c.Set("alice")                           // the document's assignments for alice
```

`Build` validates the document, resolves its secret references into a copy
(with `WithSecrets`), adds its providers and models, and assigns each agent's
lists in document order. A model or assignment the document marks disabled is
added disabled. It is a convenience over `New`, `AddProvider`, `Add` and
`Set.Assign`.

- `AddProvider(p)`, `Add(m, uses...)`, `Remove(name)`. `Add` validates the
  model, which starts enabled whatever `m.Enabled` says (`Build` honours it).
- `Get(name) (Info, bool)`, `Names()`, `Health()`, `ResetHealth(names...)`.
- `RegisterClient(name, client)`: a ready-made client for a model whose
  protocol the SDK does not build (a host-registered protocol). The host owns
  it: the catalogue never closes it. Registering the same client again changes
  nothing.
- `Set(id)`: the agent's set, created on first use; the handle stays valid.
- `RemoveSet(id) bool`: drops an agent's set and everything held for it
  (lists, switches, selections and serving records in memory) for an agent
  that is gone. The store is not written and no event is sent; chains already
  begun finish on what they pinned. The old handle's writes fail; a later
  `Set(id)` is new and empty. The default set cannot be removed.

On a Set:

- `Assign(purpose, name, priority, opts...)`: priority is an integer, lower
  first, ties in insertion order. `WithSettings(v)` attaches opaque
  per-assignment settings; `WithEnabled(false)` adds it switched off.
- `Unassign`, `Reorder(purpose, names)`, `List(purpose)` (entries with health,
  settings and whether usable), `Purposes()`.
- `Select(purpose, name, scope...)`, `Selected`, `Reset`: the model tried first,
  for a scope (such as a session) or, without one, the set's default. A scoped
  selection wins over the default one.
- `Current(purpose, scope...)`: `Serving{Selected, Serving, Since, Reason,
  Attempts}`, what answered last and why it is not the selected model.
- `Begin(purpose, scope...) *Chain`, `Caller(purpose, id, scope...)`.
- `Replace(purpose, assignments...)` and `ReplaceAll(map[Purpose][]Assignment)`:
  replace whole lists in one step (`Assignment{Name, Options}`, in priority
  order). Validated first; readers see the old lists or the new ones; selections
  and serving records whose model is still listed are kept; those whose model
  is gone revert and are returned; identical lists change nothing.
- `Forget(scope)`: drops the scope's selections and serving record from
  memory (not from the store), for a scope that has ended.

## Purposes

`Purpose` is an open string type. The SDK uses `Chat`, `Summarize`, `Memory`
and `Worker`; a host adds its own by using them. A purpose with no models of
its own falls back by a declared rule: `Worker` → `Chat` by default, others
with `WithFallback(from, to)` (and `WithFallback(p, "")` removes a rule).
`Chain.Purpose()` says whose list a chain holds. A purpose with no model fails
with an error wrapping `ErrNoModelAvailable` that names it.

## Chains

`Begin` pins the models for one turn: the selected model first, then the rest
in priority order. The order and clients do not change for the chain's life,
so a multi-step turn never changes model half-way. The one exception is the
enable switch, re-read before each call.

`chain.Chat(ctx, id, req, opts...)` tries each model in turn:

- a disabled model is skipped (`Attempt.Disabled`), a parked one too
  (`Attempt.Remaining`, `Attempt.Reason` its cause);
- when the caller's context ends (cancelled or past its deadline) the call
  stops with `ctx.Err()`: no further model is called, the call cut short is
  not held against the model, and no `FellBack` or `Exhausted` is sent. A
  model's own request timeout, with the context still live, is an ordinary
  failure;
- an error spawnllm cannot classify ends the call; a classified one is
  recorded against the model's health and the next model is tried; a
  `Retry-After` of up to 3 seconds is waited out and the same model tried once
  more first;
- when every model failed or was skipped the error is an `*ExhaustedError`
  (`AllContextLimit()` says whether compaction might help); when every model is
  disabled, a `*NoModelError`.

`chain.Do(ctx, fn, opts...)` is the same walk for a call that is not a chat
call (speech, transcription, image generation): `fn(ctx, candidate)` makes the
call with the caller's own client, built from `Candidate.Spec` and
`Candidate.ProviderSpec` (copies of the model's and provider's definitions,
secrets resolved). It shares health, switches, fallback and events with chat;
`Chat` is `Do` with a chat call. `WithClassify(fn)` replaces `DefaultVerdict`
for one call: a `Verdict{Reason, Status, RetryAfter, Record, Next, Wait}`
says whether the failure counts against the model's health, whether the next
model is tried, and whether to wait and retry the same model once first.

`chain.Client(name)`, `set.Client(name)` and `catalog.Client(name)` return a
model's chat client (the chain's is the pinned and, if `Isolated`, isolated
one) and a `release` function: for a call that records no health and ignores
the switches. The client stays usable until `release` is called, once; a
client replaced by `Adopt` or `RegisterClient` is closed only after that.

`WithPrepare` shapes the request for each model (its own `max_tokens`, images
it cannot take), `WithNotify` is called each time the call moves on (a run of
skipped models is one notice; none for the model that ends the call), and
`WithFinish` after each model call. `chain.Limits()` is the first model's
`modelspec.Limits` over `WithLimitDefaults`. `chain.Isolated(workspace)` runs
CLI models in the given workspace without a permission-bypass flag, for an
agent that has no tools to grant.

Every call is made for the `accounting.Identity` the caller passes; a call
without an agent is refused before any client is called.

## One model-caller interface

```go
type Caller interface {
	Complete(ctx context.Context, req CompleteRequest) (CompleteReply, error)
}
```

`set.Caller(purpose, id, scope, opts...)` and `chain.Caller(id, opts...)` take
the call options too: `WithFinish` (each model call), `WithPrepare`,
`WithClassify`, and `WithNotify`, which receives each model passed (one
attempt and the model considered next, or the zero `Candidate`) instead of
the SDK logging it. When no model could be called the error wraps
`ErrNoModelAvailable`.

`CompleteRequest{System, User, JSONObject, Exclude}` and
`CompleteReply{Content, FinishReason, Model}` are the shape the summarizer and
the memory consolidator use. `chain.Caller(id)` calls on one chain;
`set.Caller(purpose, id, scope...)` begins a new chain for each call. The walk
skips excluded (by wire model id), disabled and parked models, moves on after
any error (recording a classified one against the model's health), and returns
the wire model id of the model that answered.

## Updating: Adopt

A configuration change is a new catalogue built the same way as the first and
handed over: `c.Adopt(next) Diff`.

- Every error is returned while `next` is built; `Adopt` cannot fail.
- `c` and its Set handles do not change; agents keep what they hold.
- A chain begun before finishes on what it pinned, old clients included; every
  chain begun after uses the new contents. Failures reported by an old chain
  are dropped with the old contents.
- Selections and what is serving now are carried by model name; a selection
  whose model is no longer assigned reverts to the default and is listed in
  `Diff.Reverted`.
- Health is not carried: every model starts available.
- A model whose definition, provider and client options are unchanged keeps
  the client it has already built. Client options are the logger (the same
  one, or one over the same handler), `WithLogContent`, the
  `WithDeclinedMessage` function (by identity), and what `WithBaseEnv` and
  `WithFileModesFor` return now; a model whose client options changed is
  listed as changed and rebuilt. Any other client the catalogue built is
  closed (`spawnllm.StatefulProvider`) once its last call in flight ends; a
  registered client is never closed.
- A set `next` does not have is emptied: its lists and switches go, its
  selections revert (stored and listed in `Diff.Reverted`); its handle and
  serving records stay. Use `RemoveSet` to drop a set.
- `next`'s options replace `c`'s; `next` is spent (`ErrAdopted`).

`Diff` lists the models added, removed and changed.

## Shutting down

`c.Close(ctx)` shuts a catalogue down, for a host that exits or a catalogue
built for a short task. Calls and writes that start after it return
`ErrClosed`. It waits for the calls in flight and the unreleased `Client`
handles, then closes every client the catalogue built (never a registered
one), including clients an earlier `Adopt` replaced. If `ctx` ends first it
returns `ctx.Err()`: idle clients are closed at once, the rest when their last
call ends. A second `Close`, or closing a catalogue spent by `Adopt`, returns
nil. `Adopt` into or from a closed catalogue changes nothing.

## Enable and disable

`c.Disable(names...)`, `c.Enable`, `c.Enabled`: for every agent.
`s.Disable`, `s.Enable`, `s.Enabled`: for one agent. A model is used only when
on in both (and its assignment is on). A disabled model is skipped as a parked
one is and reported as `"disabled"` in attempts and `Current`; a selection of
it is kept. The default is on.

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
are called with no lock held and must not block. `Parked` and `Recovered` for
one model key arrive in the order the health changes happened (a `Recovered`
after the `Parked` it answers); to keep that order, one goroutine at a time
delivers a key's events, which may be another call's goroutine.

## Clients

The catalogue builds a model's client on first use and shares it.

- `WithBaseEnv(fn)`: the environment a CLI starts from. Default
  `DefaultBaseEnv`, an allowlist (the POSIX basics, locale, XDG, proxies, CA
  bundles and the CLIs' own key variables), never the whole environment.
- `WithFileModes`, `WithFileModesFor`: the modes of a response log file,
  read when a client is built. A host that wants a permissions change to apply
  includes it in what it compares before `Adopt`, which rebuilds the clients
  whose modes changed.
- `WithIgnoredBypassNotice(fn)`: what is done with the permission-bypass flags
  a CLI model's extra args carry but that are not passed (`IgnoredBypass{Model,
  Protocol, Flags}`). Default: a warning in the log.
- `WithDeclinedMessage(fn)`: the text of the `*CLIDeclinedError` a CLI without
  its permission bypass returns when it declined to use tools. `fn` gets the
  identity of the call, the model and the refused tools.
- `WithLogger`, `WithLogContent`.

## Errors

Match errors with `errors.Is` and `errors.As`, not their text, so a host can
show its own wording.

- `*ClientError{Model, Provider, Protocol, Err}`: a model's client could not
  be built or is not available (`Client`, `Chat`, `Caller`, an isolated
  chain with no workspace). `Err` is the bare cause, with no `models:`
  prefix. A model with no chat client (task-only or unknown protocol) is also
  `ErrNoClient`.
- `ErrUnknownModel`, `ErrUnknownProvider`, `ErrDuplicate`: a name not in the
  catalogue, a model's provider not added, a provider or model name taken.
- `ErrNoModelAvailable` (`*NoModelError` from a chain): a purpose with no
  model to try, or, from `Build`, an empty assignment list.
- `*ExhaustedError`, `*CLIDeclinedError`, `ErrClosed`, `ErrAdopted`.

## Locking

The catalogue and its sets are immutable snapshots behind an atomic pointer;
writers build the next one under one mutex. No lock is held across a model
call, an Observer call or a `SelectionStore` call. Health has its own lock.
Lists and definitions are returned as copies; nothing a caller holds changes
the live object.

## Persisting selections

`WithSelectionStore(st)`:

- `LoadSelection` is called the first time a selection is needed. An error
  means "cannot load now": the default is used and the selection is read again
  on the next use. A stored model that is not in the list is ignored.
- `SaveSelection` is called, with no lock held, on `Select` and `Reset`; with
  `""` for a selection reverted because its model left the list (`Remove`,
  `Unassign`, `Replace`, `Adopt`); and with the same name for a selection kept
  in a list whose members or order changed, so a store that keeps a position
  rather than a name can re-save it against the new list (`Set.List`). Writes
  are made one at a time, each with what memory holds then, so the store ends
  matching memory. Only
  selections loaded in this process are written. A write error is logged and
  the selection is kept in memory.

## Testing

`models/modelstest` is a scripted client: register it for a model with
`RegisterClient`, script each wire model's answers (`Reply`, `Fail`,
`Status`, or a `Step` that holds until released), and read back the calls.
