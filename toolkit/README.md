# toolkit

`go.bison9.com/agent/toolkit` holds the tool core and the live registry an
agent reads its tools from. It imports only `toolspec` and the standard
library.

## The tool core

- `Tool` (`Name`, `Description`, `Parameters`, `Execute`) and its optional
  interfaces: `AsyncExecutor` (finishes later through a callback),
  `SessionScoped` (needs the session key), `ExternalNamer` (offered under
  another name, as upstream MCP tools are) and `DiscoveryGrouped`.
- `Result` and its constructors (`NewResult`, `SilentResult`,
  `AsyncResult`, `ErrorResult`, `UserResult`, `MediaResult`,
  `WithError`). `Refusal(err)` marks an expected refusal (a configured
  permission said no), which is logged as a warning, not an error;
  `IsExpectedRefusal` recognises it, and any error with a `Refusal() bool`
  method. `NotEnabledMessage(name)` is the refusal text for a tool an agent
  may not use.
- The toolspec bridge: `FromSpec(ns, def)` turns a toolspec definition
  into a `Tool` published as `<ns>_<name>`; `ResultFromSpec` and
  `ResultToSpec` convert results.
- `ToolToSchema` and `OpenObjectProperties`: a tool's definition in the
  function form models take, with free-form object parameters declared
  open.
- Who a call is for, on the context: `WithChat` / `Channel` / `ChatID`,
  `WithSessionKey` / `SessionKey`, `WithAgentID` / `AgentID`. The caller
  sets them before `Execute`; toolspec handlers read them off the call.
- `Tally`: counts the calls of runs that ask for it, with the last
  failure's text (redacted by a function the host supplies).
- `toolkit/untrusted`: `Wrap` marks tool output from outside the process as
  data for the model; `Unwrap` and `Neutralise`.

## The registry

```go
reg := toolkit.NewRegistry()
reg.Add(lookupOrder, refund)                       // toolspec definitions; a duplicate name is refused
remove, err := reg.AddToolset(ctx, crmTools)       // a named group, removable as a unit
reg.AddProvider("file", files.Provider, deps)      // published as file_read, file_write, ...

alice, _ := agent.New("alice", agent.WithTools(reg))

// from anywhere, at any time:
reg.Remove("refund")                               // takes effect at the next model call
reg.Disable("lookup_order")                        // off, still registered
```

Writes: `Add` and `AddEntries` (refuse a duplicate name), `Put`
(replaces), `Remove`, `Replace` and `ReplaceEntries` (every tool under a
prefix swapped in one step), `AddToolset`, `AddProvider`. An `Entry` is a
`Tool` plus how it is offered: listed or `Hidden`, and its discovery
`Group` and `RevealTogether`.

Rules:

- **Safe from any goroutine, at any time,** including while a turn runs.
- **A change takes effect at the next model call.** Each model call takes
  one `Snapshot`; a turn sees the new list on its next call.
  `Snapshot.Execute` runs what the model chose: a tool removed since the
  snapshot returns the tool error `tool "x" is no longer available`, never
  a crash. A call already running finishes on the tool it started with.
- **Subscribers** get one `Change{Added, Removed, Changed}` per change and
  none for a change that alters nothing they see. Notices are delivered in
  commit order on a delivery goroutine the catalogue starts when there is
  something to deliver and that exits when there is not. A writer only
  queues: it returns before its notice is delivered and is never held by a
  slow subscriber. To wait for a notice, wait in the subscriber (for
  example on a channel it sends to). A subscriber may call back into the
  registry, even to change it. A panic in a subscriber is logged and does
  not stop delivery to the others or of later notices. A callback may run
  once more after `unsubscribe` returns if that notice was already being
  delivered. `Subscribe(nil)` registers nothing.
- **One generation per read.** The catalogue's tools and every set's
  grant and switches are published together; each read (a snapshot, a
  call, `List`, `Get`) works from one of them, so it never pairs one
  configuration's tools with another's grant, even during `Adopt`.
- **Private state.** `Catalog` and `Set` have no exported fields; `List`,
  `Get` and the snapshot accessors return copies.

**Cost.** The tool list is part of the prompt-cache prefix: every add,
remove, enable or disable of a *listed* tool invalidates the cache for the
agents that see it. Hidden (discovery) tools do not have this cost, which
is the reason to put a large or changing set behind discovery.

## Two levels: catalogue and set

The same shape as `models`:

| | `toolkit` |
| --- | --- |
| Process-wide | `Catalog`: every tool, toolset and provider the host mounted |
| Per agent | `Set`: the tools this agent is granted, each listed or hidden |
| Small host | `toolkit.New()` (or `NewRegistry()`) has a default set granting everything |
| Given to the agent | anything satisfying `Registry`: the catalogue or a set |
| Config reload | build a new catalogue, `Adopt(next)` |
| Ask it | `List()`, `Listed()`, `Hidden()`, `Stats()` |

```go
c := toolkit.New()
// ... mount every provider and toolset ...
alice := c.Set("alice")          // named; grants nothing until Grant
alice.Grant(aliceGrants)         // an AllowChecker: the host's grant policy
bob := c.View(bobGrants)         // an unnamed filtered view
```

The catalogue's read methods (`Snapshot`, `Execute`, `List`, `Get`,
`Subscribe`, `Version`, ...) are its default set's. A call to a tool the
set is not granted is refused with `NotEnabledMessage`.

**Adopt.** On a configuration change the host builds a complete new
catalogue the way it built the first, so every error is returned while it
is built, and hands it over with `c.Adopt(next)`. Agents keep their
handles; each named set takes the grant and flags of next's set of the same
id (a named set next lacks, and every view, keeps its own); a call already
running finishes on its old tool; each subscriber gets one notice; the
version keeps rising. `Adopt` returns the change for the host to log.

## Enable and disable

`Disable`, `Enable` and `Enabled` on the catalogue (off for everyone) and
on a set (off for that agent); `DisableGroup` and `EnableGroup` by toolset
or provider name, or by name prefix. A tool is usable when it is on at both
levels. A disabled tool stays registered but is not offered: no snapshot
includes it, so neither the model, discovery nor an MCP host sees it, and a
call to it returns the tool error `tool "x" is disabled`. A flag belongs to
the name and survives the tool being refreshed or re-added, until enabled.

## For discovery and an MCP host

A snapshot carries what progressive discovery indexes: `Hidden()` (the
corpus), each `Info` with `Published` (the name to reference and call),
`Group`, `RevealTogether` and the `Tool` (description, schema). Rebuild the
index when `Subscribe` reports a change, or when `Version` moves. An MCP
host serves `Snapshot().List()` and sends `tools/list_changed` on each
notice. Search, `search_tools`, `get_tool_details` and deferred loading are
the `discovery` package's.

## Logging

`WithLogger(*slog.Logger)`; records carry `component=toolkit`. The default
discards. Tool arguments are never logged.

`WithTruncate(fn func(s string, limit int) string)` sets how the catalogue
shortens text it logs (the `error` of "Tool execution failed" and "Tool
call refused"); `fn` gets the text and the limit, 500 runes. The default,
or nil, cuts to the limit and appends "…"; pass a function that returns
`s` to log the full text. It does not change what `Tally` keeps.

Overwriting a tool with `Put` warns "Tool registration overwrites existing
tool", or "Hidden tool registration overwrites existing tool" for a hidden
entry, with field `name`.
