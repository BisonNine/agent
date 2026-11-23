package mcphost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// The wire behaviour of an existing MCP server, recorded from it as 48 HTTP
// exchanges on its two endpoints (testdata/wire_parity.json), reproduced by
// configuration alone: one toolkit catalogue per agent (Union), the
// external-first naming rule, the protocol error for a panicking tool, the
// session key for session-scoped tools only, the server's own refusal
// texts, realm and token description, and its redaction as an AfterTool
// plugin. The recording's product names are replaced by neutral ones, and
// one exchange pair differs on purpose: a bearer caller is described from
// its own registry (the recorded server described Bob's fail_tool with
// Alice's description).

const (
	wireAliceToken = "SST" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	wireBobToken   = "SST" + "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	wireGoneToken  = "SST" + "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	wireBadToken   = "SST" + "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	wireBobService = "bob-service-token"
	wireLeaked     = "SST" + "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"
	wireSentinel   = "AGT" + "0000000000000000000000000000000000000000000000000000000000000000"

	wireInvalid  = "invalid or missing session_token; supply your assigned token (format: SST<64 hex>)"
	wireSubagent = "sub-agents are not granted Example MCP access; use the harness filesystem tools against your assigned working directory."
	wireTokenArg = "Your session_token (format: 'SST<64 hex>'). Required on every mcp__host__* call. Supplied verbatim from the agent's system prompt."
)

// wireTool is a fixture tool whose behaviour is a function of its call.
type wireTool struct {
	name, desc string
	params     map[string]any
	run        func(ctx context.Context, args map[string]any) *toolkit.Result
}

func (w *wireTool) Name() string               { return w.name }
func (w *wireTool) Description() string        { return w.desc }
func (w *wireTool) Parameters() map[string]any { return w.params }
func (w *wireTool) Execute(ctx context.Context, args map[string]any) *toolkit.Result {
	return w.run(ctx, args)
}

type wireExternal struct {
	wireTool

	external string
}

func (w *wireExternal) ExternalName() string { return w.external }

type wireScoped struct{ wireTool }

func (*wireScoped) IsSessionScoped() bool { return true }

func wireText(s string) func(context.Context, map[string]any) *toolkit.Result {
	return func(context.Context, map[string]any) *toolkit.Result { return toolkit.NewResult(s) }
}

func wireArgs(_ context.Context, args map[string]any) *toolkit.Result {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return toolkit.NewResult("args=" + strings.Join(keys, ","))
}

func wireSession(ctx context.Context, _ map[string]any) *toolkit.Result {
	return toolkit.NewResult("session=" + toolkit.SessionKey(ctx))
}

func wireCatalog(t *testing.T, tools ...toolkit.Tool) *toolkit.Catalog {
	t.Helper()
	c := toolkit.New()
	for _, tl := range tools {
		if err := c.AddEntries(toolkit.Entry{Tool: tl}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// allowNames grants the internal names it holds.
type allowNames map[string]bool

func (a allowNames) IsToolAllowed(name string) bool { return a[name] }

// denyNames grants every internal name but the ones it holds.
type denyNames map[string]bool

func (d denyNames) IsToolAllowed(name string) bool { return !d[name] }

var wireTokenPattern = regexp.MustCompile(`(?:AGT|SST)[0-9a-f]{64}`)

// wireHandler serves the recorded server's configuration with mcphost.
func wireHandler(t *testing.T) http.Handler {
	t.Helper()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"zone": map[string]any{"type": "string", "description": "Time zone"}},
		"required":   []any{"zone"},
	}
	open := map[string]any{
		"type":       "object",
		"properties": map[string]any{"meta": map[string]any{"type": "object"}},
	}
	alice := wireCatalog(t,
		&wireTool{name: "time_now", desc: "Alice's clock", params: schema, run: wireArgs},
		&wireTool{name: "open_args", desc: "Free-form object", params: open, run: wireArgs},
		&wireTool{name: "fail_tool", desc: "Always fails", params: nil, run: func(context.Context, map[string]any) *toolkit.Result {
			return toolkit.ErrorResult("boom")
		}},
		&wireTool{name: "nil_tool", desc: "Returns nothing", params: map[string]any{}, run: func(context.Context, map[string]any) *toolkit.Result {
			return nil
		}},
		&wireTool{name: "panic_tool", desc: "Panics", params: map[string]any{}, run: func(context.Context, map[string]any) *toolkit.Result {
			panic("wire fixture panic")
		}},
		&wireTool{name: "leak_tool", desc: "Echoes a token", params: map[string]any{}, run: wireText("token " + wireLeaked + " end")},
		&wireTool{name: "foruser_tool", desc: "Speaks to the user", params: map[string]any{}, run: func(context.Context, map[string]any) *toolkit.Result {
			return &toolkit.Result{ForLLM: "for the model", ForUser: "for the user"}
		}},
		&wireScoped{wireTool{name: "session_echo", desc: "Session-scoped", params: map[string]any{}, run: wireSession}},
		&wireTool{name: "ctx_probe", desc: "Not session-scoped", params: map[string]any{}, run: wireSession},
		&wireExternal{wireTool{name: "mcp_srv_read", desc: "Upstream read", params: map[string]any{}, run: wireText("upstream read")}, "srv_read"},
		&wireTool{name: "srv_write", desc: "Native write", params: map[string]any{}, run: wireText("native write")},
		&wireExternal{wireTool{name: "mcp_srv_write", desc: "Upstream write", params: map[string]any{}, run: wireText("upstream write")}, "srv_write"},
	)
	bob := wireCatalog(t,
		&wireTool{name: "time_now", desc: "Bob's clock", params: schema, run: wireArgs},
		&wireTool{name: "bob_only", desc: "Only Bob has it", params: map[string]any{}, run: wireText("bob only")},
		&wireTool{name: "fail_tool", desc: "Bob's failing tool", params: nil, run: wireText("bob's fail_tool ran")},
	)
	// The access policy (Bob may not call time_now) is each agent's view.
	sets := map[string]*toolkit.Set{"alice": alice.View(denyNames{}), "bob": bob.View(denyNames{"time_now": true})}

	store := NewStore()
	register := func(tok, agent, session string) {
		if err := store.Register(tok, Grant{Scope: hook.Scope{AgentID: agent, Session: session}}); err != nil {
			t.Fatal(err)
		}
	}
	register(wireAliceToken, "alice", "agent:alice:main")
	register(wireBobToken, "bob", "agent:bob:main")
	register(wireGoneToken, "alice", "agent:alice:gone")
	store.Revoke(wireGoneToken)
	serviceHash := sha256.Sum256([]byte(wireBobService))
	services := map[string]string{"sha256:" + hex.EncodeToString(serviceHash[:]): "bob"}

	auth := AuthenticatorFunc(func(_ context.Context, token string) (Caller, error) {
		if token == wireSentinel {
			return Caller{}, errors.New(wireSubagent)
		}
		scope, ok := hook.Scope{}, false
		if g, _, found := store.Resolve(token); found {
			scope, ok = g.Scope, true
		} else {
			sum := sha256.Sum256([]byte(token))
			if agent, found := services["sha256:"+hex.EncodeToString(sum[:])]; found {
				scope, ok = hook.Scope{AgentID: agent, Session: "agent:" + agent + ":main"}, true
			}
		}
		if !ok {
			return Caller{}, errors.New(wireInvalid)
		}
		// Done is left nil: a call in flight finishes whatever happens to
		// its token.
		return Caller{Set: sets[scope.AgentID], Scope: scope}, nil
	})
	redact := hook.Plugin{
		Name: "redact",
		AfterTool: func(_ context.Context, _ *hook.ToolCall, r *toolkit.Result) {
			r.ForLLM = wireTokenPattern.ReplaceAllStringFunc(r.ForLLM, func(m string) string {
				if m == wireSentinel {
					return m
				}
				return "AGT[REDACTED]"
			})
		},
	}
	onlyScoped := func(ctx context.Context, _ Caller, info toolkit.Info) context.Context {
		if s, ok := info.Tool.(toolkit.SessionScoped); ok && s.IsSessionScoped() {
			return ctx
		}
		return toolkit.WithSessionKey(ctx, "")
	}
	external := allowNames{
		"time_now": true, "fail_tool": true, "panic_tool": true, "leak_tool": true, "foruser_tool": true,
		"session_echo": true, "ctx_probe": true, "mcp_srv_read": true, "srv_write": true, "mcp_srv_write": true, "bob_only": true,
	}
	base := Config{
		Name: "Example", Version: "0.6.6", Authenticator: auth,
		Names: ExternalFirst, PanicAsProtocolError: true, RefuseWithInternalName: true, ToolContext: onlyScoped,
		Plugins: []hook.Plugin{redact},
	}
	internalCfg := base
	internalCfg.Transport = Argument
	internalCfg.TokenDescription = wireTokenArg
	internalCfg.Catalogue = Union(func() []toolkit.Registry { return []toolkit.Registry{alice, bob} })
	bearerCfg := base
	bearerCfg.Transport = Bearer
	bearerCfg.Realm = "host-mcp"
	bearerCfg.Catalogue = Union(func() []toolkit.Registry {
		return []toolkit.Registry{alice.View(external), bob.View(external)}
	})

	mux := http.NewServeMux()
	for path, cfg := range map[string]Config{"/internal": internalCfg, "/mcp": bearerCfg} {
		ep, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ep.Close)
		mux.Handle(path, ep)
	}
	return mux
}

type wireExchange struct {
	Name     string            `json:"name"`
	Path     string            `json:"path"`
	Auth     string            `json:"auth,omitempty"`
	Request  json.RawMessage   `json:"request"`
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	Response string            `json:"response"`
}

func TestWireParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/wire_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var exchanges []wireExchange
	if err := json.Unmarshal(raw, &exchanges); err != nil {
		t.Fatal(err)
	}
	if len(exchanges) != 48 {
		t.Fatalf("%d exchanges, want the 48 recorded", len(exchanges))
	}
	tokens := map[string]string{
		"alice": wireAliceToken, "bob": wireBobToken, "revoked": wireGoneToken,
		"bad": wireBadToken, "bob-service": wireBobService, "sub-agent": wireSentinel,
	}
	ts := httptest.NewServer(wireHandler(t))
	defer ts.Close()

	for _, ex := range exchanges {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+ex.Path, bytes.NewReader(ex.Request))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if ex.Auth != "" {
			scheme, who, _ := strings.Cut(ex.Auth, " ")
			req.Header.Set("Authorization", scheme+" "+tokens[strings.Trim(who, "<>")])
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", ex.Name, err)
		}
		body, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatalf("%s: %v", ex.Name, err)
		}
		headers := map[string]string{}
		for _, h := range []string{"Content-Type", "WWW-Authenticate", "Mcp-Session-Id"} {
			if v := resp.Header.Get(h); v != "" {
				headers[h] = v
			}
		}
		if resp.StatusCode != ex.Status {
			t.Errorf("%s: status %d, want %d", ex.Name, resp.StatusCode, ex.Status)
		}
		if !mapsEqual(headers, ex.Headers) {
			t.Errorf("%s: headers %v, want %v", ex.Name, headers, ex.Headers)
		}
		if string(body) != ex.Response {
			t.Errorf("%s:\n got %s\nwant %s", ex.Name, body, ex.Response)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
