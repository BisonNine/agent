# mcphost

`go.bison9.com/agent/mcphost` serves an agent's tools over MCP (streamable
HTTP) to a model that runs its own tool loop: a vendor CLI reaches a host's
tools only through an MCP server. It is the mechanism; the host keeps the
policy. It imports `toolkit`, `toolspec`, `hook`, the standard library and
`github.com/mark3labs/mcp-go`, and it is the only SDK package that imports
mcp-go: a host that serves no CLI model never links it.

## What it does

- **`Endpoint`** is an `http.Handler` the host mounts at a path of its
  choosing. It knows no listener, path, TLS or Host-header rule. It is
  stateless (no MCP session ids) and answers both the 2025 protocol (with
  the GET notification stream CLIs use) and the current one.
- **Tools.** `Config.Catalogue` is what a call may name and what
  `tools/list` shows on an `Argument` endpoint: `FromRegistry` of a toolkit
  catalogue, set or view (followed automatically: each list and call sees
  the registry as it is), or `Union` of several agents' own catalogues
  (changed by `Endpoint.Refresh`). On a `Bearer` endpoint each caller is
  listed and described from its own set alone, limited to the names the
  catalogue publishes. Every granted, enabled tool is listed, hidden ones
  included (discovery is never applied), under the name `Config.Names`
  gives it, with its description and its schema (free-form object
  arguments declared open, as `toolkit.ToolToSchema` does). A call runs
  exactly the tool published under the name it uses. A change sends
  `tools/list_changed`; `Watch` sends it (to every client: stateless HTTP
  cannot address one) for changes of a caller's own set.
- **Credentials.** `Transport` is `Bearer` (an `Authorization: Bearer`
  header; a request without one or with a refused one is answered 401 with
  `WWW-Authenticate: Bearer realm="<Config.Realm>"`) or `Argument` (the
  `session_token` argument of every call, added as a required string to
  every published schema and removed from the arguments before anything
  sees them; `tools/list` needs no credential). The host's `Authenticator`
  turns a token into a `Caller`: the `*toolkit.Set` to serve, the
  `hook.Scope` and a value of its own. mcphost implements no token format,
  hashing, service token or access policy.
- **Calls** run through `ExecuteGated` on the caller's set, in this order:
  the scope (`hook.WithScope`) and the caller (`CallerOf`) on the context,
  `Config.CallContext` for the host's own keys, `ToolCallStarted`,
  `BeforeTool`, the tool behind `Config.Gate`, `OnToolError`, `AfterTool`,
  `ToolCallFinished`. The callbacks are `hook.Plugin`s, the same the turn
  engine runs, so audit and redaction are the same for every model, and a
  panic in a callback or a tool fails only that call ("Tool x failed with an
  internal error."). A panic in `CallContext` or `ToolContext` fails it as a
  `BeforeTool` panic does; every panic during a call ends with the call's
  `ToolCallFinished` carrying it (`*hook.ToolPanicError` for the tool or the
  `Gate`, `*hook.PanicError` for a callback), so a host that counts panics
  counts them there. The MCP result is the final `ForLLM` text with the
  error flag. `ToolCallFinished` carries the whole result (`ForUser`,
  `Media`, ...) and `Config.AsyncResult` a background tool's late one, on
  the call's own context (the values of the call's time, not cancelled)
  with the tool that ran; the
  host delivers them, mcphost publishes nothing. The call's context ends
  with the MCP request (a background tool's too, unless
  `DetachBackground`).
- **`Store`** holds session tokens: `Issue` (64 hex characters from
  crypto/rand, or the host's `WithGenerator`), `Register`, `Resolve`,
  `Rescope`, `Revoke`, `RevokeFunc`, and an optional expiry per grant. A
  revoked or expired token stops resolving at once, and the calls running
  with it have their context cancelled (`ErrRevoked`) and their results
  withheld from the client. `Store.Authenticator` is an Authenticator over
  the store alone.
- **`CLIConfig.Access(token)`** is what a CLI-model turn needs: the prompt
  text giving the model its token (`Argument`) and the MCP server entry (name,
  URL, and the bearer header for `Bearer`), as plain values the host turns
  into its CLI's flags. The server name, URL and the tool prefix the CLI
  shows are the host's.

## Matching an existing server

Each default follows the SDK's own rules; these options reproduce a server
that behaved otherwise, exchange for exchange (`wire_test.go` replays 48
exchanges recorded from one):

| Behaviour | Default | Option |
| --- | --- | --- |
| One toolkit catalogue per agent | `FromRegistry(cat)` | `Catalogue: Union(func() []toolkit.Registry {...})`, agents in the order whose descriptions the `Argument` list uses |
| Notices only where the host says | automatic for `FromRegistry` | `Union` plus `Endpoint.Refresh(ctx)` where the host refreshed before; its `RefreshResult` says whether a notice was sent and which tools were added and removed (published and internal names) |
| Lines for tools registered and removed | counts logged by mcphost | `OnChange` is given the `RefreshResult` of every read that changed what is published (at `New`, on `Refresh`, on a followed change) |
| Tools left out | logged by mcphost | `OnSkip` is told of each (`SkipNameTaken`, `SkipSchema`, `SkipRefused`) on every read of the catalogue; a `SkipSchema` tool claims no name |
| The internal name of what a call names | `TargetOf(ctx)` on every call's context: `Internal`, and `Held` (false: published, but not in the caller's set) | the same |
| Refusing a published tool the caller lacks | `NotEnabledMessage` of the called name | `RefuseWithInternalName: true` (the internal name, on both transports) |
| Published names | `PublishedNames` (toolkit's no-shadow rule) | `Names: ExternalFirst` (external name always; a later tool by internal name wanting a taken name is left out) |
| A panicking tool or host callback | error result "Tool x failed with an internal error." | `PanicAsProtocolError: true` (-32603 "panic recovered in x tool handler: …", after the callbacks and events) |
| Session key | on every call (`hook.WithScope`) | `ToolContext` returning `toolkit.WithSessionKey(ctx, "")` for tools that are not `SessionScoped` |
| Background tools | the request's context | `DetachBackground: true` |
| Revocation | `Caller.Done` cancels calls in flight | leave `Done` nil: a call in flight finishes |
| Refusal texts | the Authenticator's errors | the same: they are the tool error text (`Argument`); 401 bodies are "missing bearer token" and "invalid bearer token" |
| 401 challenge, token schema, handshake | | `Realm`, `TokenDescription`, `Name` and `Version` |
| Redaction, access policy | | an `AfterTool` plugin; the caller's set (a `View` with the policy's grant) |

## What the host supplies

The listener and paths; the server name and version; an `Authenticator`
(over a `Store`, and any long-lived credentials of its own); the sets each
agent is granted and an optional `Gate`; `CallContext` for values only it
knows (the chat a reply goes to); the plugins that audit, redact and show
tool activity; and delivery of a tool's output to people.

## For the agent loop

`Served{Store, CLI}` is the agent's `hook.ServedTools` (`agent.WithServedTools`):
for each turn of a CLI model it issues a token for the turn's scope, returns
`CLI.Access(token).Prompt` for the system prompt, and revokes the token when
the turn ends or is stopped. For a `Bearer` endpoint the prompt is empty and
the CLI needs the token in its server entry's header, which the engine has no
way to hand it: use an `Argument` endpoint.

## Example

```go
cat := toolkit.New()
// ... mount tools ...
alice := cat.Set("alice")
alice.Grant(aliceGrants)
bob := cat.Set("bob")
bob.Grant(bobGrants)
sets := map[string]*toolkit.Set{"alice": alice, "bob": bob}

store := mcphost.NewStore(mcphost.WithLogger(logger))
auth := store.Authenticator(func(s hook.Scope) (*toolkit.Set, bool) {
	set, ok := sets[s.AgentID]
	return set, ok
}, "invalid or missing session_token")

ep, err := mcphost.New(mcphost.Config{
	Name: "example-host", Version: "1.0.0",
	Transport:     mcphost.Bearer,
	Authenticator: auth,
	Catalogue:     mcphost.FromRegistry(cat),
	Realm:         "example",
	Plugins:       []hook.Plugin{audit, redact},
	Logger:        logger,
})
if err != nil {
	return err
}
defer ep.Close()
http.Handle("/mcp", ep)

// A turn of Alice's on a CLI model:
token, err := store.Issue(mcphost.Grant{Scope: scope})
if err != nil {
	return err
}
defer store.Revoke(token) // at turn end, and on cancel
access := mcphost.CLIConfig{
	Server: "example", URL: "http://127.0.0.1:5911/mcp",
	Transport: mcphost.Bearer, ToolPrefix: "mcp__example__",
}.Access(token)
// access.Server goes into the CLI's MCP configuration for the turn.
```

## Logging

`Config.Logger` and `WithLogger` (for the store); records carry
`component=mcphost`; the default discards. Token values and tool arguments
are never logged. Catalogue changes are logged as counts; a host that wants
a line per tool registered or removed logs it from `OnChange`.
