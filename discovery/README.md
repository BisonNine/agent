# discovery

`go.bison9.com/agent/discovery` is progressive tool discovery over a
`toolkit` registry: tools kept out of the model's tool list until a search
finds them. It imports only `toolkit`, `toolspec` and the standard library.

A hidden tool may always be **called**; discovery decides only what is
**listed**. An MCP host serves every tool and never applies discovery.

## Hiding tools

```go
reg := toolkit.NewRegistry()
reg.Add(lookupOrder, refund)                                     // listed
reg.AddToolset(ctx, discovery.Hidden(crmTools,                   // found by search
    discovery.AlwaysShown("crm_start_here")))                    // except this one
reg.ReplaceEntries("mcp_bob_", discovery.HideEntries(bobTools))  // entries, not a toolset
```

- `Hidden(toolset, opts...)` returns the toolset with its tools hidden;
  `HideEntries(entries, opts...)` does the same to a slice (a copy).
- A hidden entry with no `Group` takes the one its tool reports through
  `toolkit.DiscoveryGrouped`. `RevealTogether` groups load as a unit.
- `AlwaysShown(patterns...)` keeps matching tools listed. A pattern is a
  name prefix, compared without regard to case or runs of underscores,
  against the internal name and the `ExternalName`; `*` matches everything
  and a trailing `*` is ignored. `AlwaysShownFunc(fn)` is for a host with
  its own rule.

Hiding is fixed when the tools are mounted, in the catalogue, so it holds
for every agent that is granted the tool.

## The index and the meta-tools

```go
ix := discovery.NewIndex(alice, discovery.MaxResults(10))   // one per Registry (agent set)
reg.AddToolset(ctx, ix.MetaTools())                          // search_tools, get_tool_details
```

`search_tools` (BM25 by default, `regex: true` for a regular expression)
and `get_tool_details` have the same names, descriptions, argument schemas
and result text as before they moved here. Their results carry
`Result.ToolReferences`: the hits, or the described hidden tool followed by
the rest of its reveal-together group.

They answer from the registry's current snapshot, so a tool the agent is
not granted, or a disabled one, is never found. With one catalogue shared
by several agents, mount `discovery.MetaTools(resolve)` once, where
`resolve(ctx)` returns the calling agent's `Index` (for instance by
`toolkit.AgentID(ctx)`).

`Index.Search`, `SearchRegex`, `Details` and `Namespaces` take the
snapshot they answer for. The index is built from that snapshot when its
tools differ from the last one built (its `Version` and offered names), so
a change is seen by the next query however late its change notice arrives,
and an older snapshot is never answered from a newer index.

Ranking: BM25 over the published name and description (k1 1.2, b 0.75),
equal scores in internal-name order. Regex search matches name or
description, case-insensitively, in internal-name order.

## Tool definitions for a model call

```go
sess := discovery.NewSession(refsInHistory...)                // per conversation
// each model call:
native := discovery.Native(provider, model)                   // loads deferred tools itself?
defs := discovery.ToolDefs(reg.Snapshot(), sess, native)
send(defs.Send)                                               // to the provider
cost(defs.InContext)                                          // what reaches the context
// each tool result:
sess.Record(result.ToolReferences)
```

- `native`: every offered tool, sorted by internal name, hidden ones
  `Deferred`: the same list on every call while the tools are unchanged,
  so the prompt-cache prefix holds.
- Not `native`: the listed tools, then the hidden tools the session has
  revealed, in order of first reveal.
- `InContext` is the listed tools plus the revealed ones, either way.
- `search_tools` is marked `ToolSearch`, with the snapshot's hidden
  namespaces in its description.
- No hidden tools (discovery off): the registry's full list, no flags.

`Def` is the package's own type; the caller copies it into its provider
type (`spawnllm.ToolDefinition`). `spawnllm.DeferredToolsCapable` satisfies
`DeferredLoader`.

A `Session` must mirror the history the model is sent: record each tool
result's references as it is added, and rebuild it with `NewSession` from
the kept messages when the history is rebuilt (a restart, a compaction that
drops messages).

## Logging

`WithLogger(*slog.Logger)` on the index; records carry
`component=discovery`. The default discards. Search queries and tool
arguments are not logged.
