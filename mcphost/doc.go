// Package mcphost serves an agent's tools over MCP (streamable HTTP), for a
// model that runs its own tool loop: a vendor CLI reaches the host's tools
// only through an MCP server. It is the mechanism; the host keeps the
// policy.
//
// What mcphost does:
//
//   - Endpoint is an http.Handler the host mounts at a path of its choosing.
//     It lists a toolkit registry's tools (every granted, enabled tool,
//     listed or hidden: discovery is never applied) under their published
//     names, with their own descriptions and schemas, and sends
//     tools/list_changed when the registry changes. Each list is built from
//     a snapshot taken for that request, never from a cache.
//   - The caller's credential travels as a bearer header (Bearer) or as the
//     session_token argument of every call (Argument). The host's
//     Authenticator says who it is: the toolkit set to serve, the hook.Scope
//     and a value of the host's own.
//   - A call runs through toolkit's ExecuteGated on the caller's set, with
//     the scope on its context, the host's Gate, and the host's tool
//     callbacks (hook.Plugin BeforeTool, AfterTool, OnToolError) and events
//     (ToolCallStarted, ToolCallFinished): the same contract as the turn
//     engine, so audit and redaction are the same for every model.
//   - Store issues, resolves and revokes session tokens; a revoked or
//     expired token stops working at once, calls in flight included.
//   - CLIConfig gives a CLI model's turn its prompt text and MCP server entry.
//
// What the host supplies: the listener, paths, TLS and Host-header rules,
// token formats beyond the default, hashing, service tokens, the access
// policy (as the sets it grants and the Gate), and the delivery of a tool's
// output to people (ToolCallFinished carries it; mcphost publishes nothing).
//
// Logging is the injected *slog.Logger (component=mcphost), discarded by
// default. Token values are never logged.
package mcphost

import "log/slog"

// component tags every record mcphost logs.
const component = "mcphost"

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
