package mcphost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// defaultTokenDescription describes the TokenArgument in published schemas
// when Config.TokenDescription is empty.
const defaultTokenDescription = "Your session token. Required on every call. Supplied verbatim from your system prompt."

// errNoTools refuses a caller the Authenticator gave no set.
var errNoTools = errors.New("no tools are served to this caller")

// AsyncResultFunc receives the late result of a tool that finished in the
// background (a toolkit.AsyncExecutor), for the host to deliver. ctx is the
// context the call's tool ran on (with the scope, the caller, the Target,
// the tool call and what CallContext and ToolContext added), without its
// cancellation, so the values of the call's time are still on it when the
// result comes, however late; the context the tool hands its callback is
// not passed, since it carries none of them. The context keeps those values
// alive until the callback returns. info is the tool that ran;
// call is the call as it ran.
type AsyncResultFunc func(ctx context.Context, caller Caller, call hook.ToolCall, info toolkit.Info, result *toolkit.Result)

// Config is what an Endpoint serves and how.
type Config struct {
	// Name and Version are the server's name and version in the MCP
	// handshake. Required. Version should be a bare semantic version.
	Name, Version string
	// Transport is how clients carry their credential. Required.
	Transport Transport
	// Authenticator says who a credential is. Required.
	Authenticator Authenticator
	// Catalogue is the endpoint's tools (see Catalogue). A host with one
	// toolkit.Catalog passes FromRegistry of it, or of a View holding the
	// tools this endpoint may publish; a host whose agents each have their
	// own catalogue passes Union of them. Required.
	Catalogue Catalogue
	// Names is how published names are chosen. The default, PublishedNames,
	// is toolkit's rule.
	Names NameRule

	// Realm is the realm of the WWW-Authenticate header of a 401 (Bearer).
	// Empty sends no realm.
	Realm string
	// TokenDescription describes the TokenArgument in every published
	// schema (Argument). Empty uses a generic text.
	TokenDescription string

	// Gate is asked, for the exact tool a call resolved to, whether it may
	// run (toolkit.Gate); its refusal is the call's result. Nil allows every
	// call the caller's set allows.
	Gate toolkit.Gate
	// CallContext, when set, adds the host's own values (a reply chat, an
	// ask chain, a sub-agent depth key) to a call's context, after the scope
	// and the caller are on it and before the callbacks, the Gate and the
	// tool see it.
	CallContext func(ctx context.Context, c Caller) context.Context
	// ToolContext, when set, adjusts a call's context for the tool it
	// resolved to (info), after CallContext and before the callbacks, the
	// Gate and the tool see it: to give the session key only to
	// session-scoped tools, it returns toolkit.WithSessionKey(ctx, "") for
	// the others. The call's scope (hook.ScopeOf, the events) is not
	// changed by it.
	ToolContext func(ctx context.Context, c Caller, info toolkit.Info) context.Context
	// Plugins are the host's callbacks, in order. Their BeforeTool,
	// AfterTool and OnToolError run on every call, as the turn engine runs
	// them; OnEvent receives ToolCallStarted and ToolCallFinished. Other
	// fields are not used.
	Plugins []hook.Plugin
	// AsyncResult receives the late results of background tools. Nil runs
	// every tool synchronously (an AsyncExecutor's Execute).
	AsyncResult AsyncResultFunc
	// DetachBackground gives a background tool (an AsyncExecutor, with
	// AsyncResult set) a context that does not end with the MCP request, so
	// its work outlives a client that goes away. Off, it runs on the
	// request's context like every other tool.
	DetachBackground bool
	// RefuseWithInternalName words the refusal of a published tool the
	// caller's set does not hold with the tool's internal name, as a server
	// that dispatched by internal name did. Off, it names the tool as the
	// call did, so no other agent's internal name reaches a caller.
	RefuseWithInternalName bool
	// PanicAsProtocolError answers a call whose tool or host callback
	// panicked with MCP's protocol error (-32603 "panic recovered in <name>
	// tool handler: <value>") instead of the error result "Tool <name>
	// failed with an internal error.". The host callbacks are the
	// Authenticator (Argument), CallContext, ToolContext, the Gate and the
	// plugins' BeforeTool, OnToolError and AfterTool; a panic in OnEvent
	// changes nothing. The callbacks and events run as for any panic first:
	// ToolCallFinished carries the internal-error result with the panic as
	// its Err (a *hook.ToolPanicError for the tool or the Gate, a
	// *hook.PanicError for a callback), but for an Authenticator panic,
	// which comes before there is a caller and has no events. A result
	// OnToolError gave in its place is not sent.
	PanicAsProtocolError bool

	// OnSkip, when set, is told of every catalogue tool left out (a taken
	// name, a schema that cannot be encoded or that MCP refuses) on every
	// read of the catalogue: at New, on each Refresh, and on each change of a
	// followed catalogue, in order of the tools' internal names. The
	// endpoint logs them too.
	OnSkip func(Skip)
	// OnChange, when set, is told of every read of the catalogue that
	// changed what is published (the names registered and withdrawn): at
	// New, on each Refresh (which also returns it), and on each change of a
	// followed catalogue, after that read's OnSkip calls. The endpoint logs
	// only the counts.
	OnChange func(RefreshResult)

	// Logger is the endpoint's logger; records carry component=mcphost.
	// Nil discards. Token values and tool arguments are never logged.
	Logger *slog.Logger
}

// Endpoint serves one catalogue over MCP streamable HTTP. It is an
// http.Handler; the host mounts it at a path of its choosing on a server it
// runs. It is stateless (no MCP session ids). Create one with New; it is
// safe for concurrent use.
type Endpoint struct {
	cfg    Config
	logger *slog.Logger
	srv    *server.MCPServer
	http   http.Handler

	versioned versioned // the Catalogue's Version, or nil

	// syncMu serialises sync; version is under it. state is replaced, never
	// changed.
	syncMu  sync.Mutex
	state   atomic.Pointer[catalogueState]
	version uint64
	synced  atomic.Bool   // a sync has completed
	seen    atomic.Uint64 // the catalogue version of the last sync

	inflight  atomic.Int64
	closeOnce sync.Once
	unsub     func()
}

// New returns an endpoint serving cfg.
func New(cfg Config) (*Endpoint, error) {
	switch {
	case cfg.Name == "" || cfg.Version == "":
		return nil, errors.New("mcphost: Name and Version are required")
	case cfg.Transport != Bearer && cfg.Transport != Argument:
		return nil, fmt.Errorf("mcphost: unknown transport %d", cfg.Transport)
	case cfg.Authenticator == nil:
		return nil, errors.New("mcphost: an Authenticator is required")
	case cfg.Catalogue == nil:
		return nil, errors.New("mcphost: a Catalogue is required")
	}
	cfg.Plugins = slices.Clone(cfg.Plugins)
	if cfg.TokenDescription == "" {
		cfg.TokenDescription = defaultTokenDescription
	}
	logger := cfg.Logger
	if logger == nil {
		logger = discardLogger()
	}
	e := &Endpoint{
		cfg:    cfg,
		logger: logger.With("component", component),
		unsub:  func() {},
	}
	e.state.Store(&catalogueState{byName: map[string]registered{}})
	if v, ok := cfg.Catalogue.(versioned); ok {
		e.versioned = v
	}

	hooks := &server.Hooks{}
	hooks.AddAfterListTools(e.afterList)
	// A call is routed by the names registered with mcp-go: bring them in
	// step with a followed catalogue first, so a tool added a moment ago is
	// found.
	hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) { e.autoSync() })
	e.srv = server.NewMCPServer(cfg.Name, cfg.Version,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithHooks(hooks),
	)
	e.http = server.NewStreamableHTTPServer(e.srv, server.WithStateLess(true))

	// Subscribe before the first sync, so no change falls between them.
	if s, ok := cfg.Catalogue.(subscribable); ok {
		e.unsub = s.Subscribe(func(toolkit.Change) { e.sync() })
	}
	e.sync()
	return e, nil
}

// Refresh reads the catalogue again and brings what the endpoint publishes
// in step with it, synchronously, sending tools/list_changed only when what
// is published changed since the last notice. It reports what changed
// (Changed: a notice was sent). A host whose Catalogue has no Version calls
// it after each change; for one that has, it is not needed.
func (e *Endpoint) Refresh(ctx context.Context) RefreshResult {
	res := e.sync()
	if res.Changed {
		e.logger.InfoContext(ctx, "MCP catalogue refreshed", "added", len(res.Added), "deleted", len(res.Removed))
	}
	return res
}

// Close stops following the catalogue (a registry passed to Watch is
// released by its own unwatch). It does not end requests in flight; the
// host stops its listener for that.
func (e *Endpoint) Close() {
	e.closeOnce.Do(func() { e.unsub() })
}

// Watch sends tools/list_changed to every client of the endpoint (MCP over
// stateless HTTP cannot address one caller) on every change reg reports:
// for a Bearer endpoint, each caller's set whose grant or switches may
// change apart from the catalogue. Changes to the Catalogue need no Watch.
func (e *Endpoint) Watch(reg toolkit.Registry) (unwatch func()) {
	return reg.Subscribe(func(toolkit.Change) {
		e.srv.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
	})
}

// InFlight returns the number of tool calls running now, for a host that
// waits for them before it stops.
func (e *Endpoint) InFlight() int { return int(e.inflight.Load()) }

// ServeHTTP serves the MCP streamable HTTP transport. On a Bearer endpoint
// every request is authenticated first; one without a bearer token, or with
// one the Authenticator refuses, is answered 401.
func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if e.cfg.Transport != Bearer {
		e.http.ServeHTTP(w, r)
		return
	}
	token := bearerToken(r)
	if token == "" {
		e.logger.WarnContext(r.Context(), "MCP request without a bearer token")
		e.unauthorized(w, "missing bearer token")
		return
	}
	c, err := e.authenticate(r.Context(), token)
	if err != nil {
		e.unauthorized(w, "invalid bearer token")
		return
	}
	// The request, a notification stream included, ends when the
	// credential stops working.
	ctx, stop := cancelOnDone(withCaller(r.Context(), c), c.Done)
	defer stop()
	e.http.ServeHTTP(w, r.WithContext(ctx))
}

// bearerToken returns the token of an "Authorization: Bearer <token>"
// header (the scheme in any case), or "".
func bearerToken(r *http.Request) string {
	const prefix = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func (e *Endpoint) unauthorized(w http.ResponseWriter, msg string) {
	challenge := "Bearer"
	if e.cfg.Realm != "" {
		challenge += fmt.Sprintf(" realm=%q", e.cfg.Realm)
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, msg, http.StatusUnauthorized)
}

// authenticate asks the Authenticator and checks its answer. It logs a
// refusal, never the token.
func (e *Endpoint) authenticate(ctx context.Context, token string) (Caller, error) {
	c, err := e.cfg.Authenticator.Authenticate(ctx, token)
	switch {
	case err != nil:
		e.logger.WarnContext(ctx, "MCP authentication refused",
			"transport", e.cfg.Transport.String(), "token_given", token != "", "error", err.Error())
		return Caller{}, err
	case c.Set == nil:
		e.logger.ErrorContext(ctx, "MCP caller has no tool set", "agent", c.Scope.AgentID)
		return Caller{}, errNoTools
	case c.revoked():
		e.logger.WarnContext(ctx, "MCP credential revoked", "agent", c.Scope.AgentID)
		return Caller{}, ErrRevoked
	}
	return c, nil
}

// cancelOnDone returns a child of ctx cancelled with ErrRevoked when done
// is closed, and the function that releases it. A nil done changes nothing.
func cancelOnDone(ctx context.Context, done <-chan struct{}) (context.Context, func()) {
	if done == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	go func() {
		select {
		case <-done:
			cancel(ErrRevoked)
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancel(nil) }
}

// revoked reports whether c's credential has stopped working.
func (c Caller) revoked() bool {
	if c.Done == nil {
		return false
	}
	select {
	case <-c.Done:
		return true
	default:
		return false
	}
}

// withCaller returns a child of ctx carrying c.
func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}
