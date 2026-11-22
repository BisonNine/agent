package mcphost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// perAgent is a host whose agents each have their own toolkit catalogue,
// served through Union on both transports.
type perAgent struct {
	*fixture
}

func newPerAgent(t *testing.T, cats map[string]*toolkit.Catalog, opts ...fixtureConfig) *perAgent {
	t.Helper()
	f := &fixture{t: t, store: NewStore(), sets: map[string]*toolkit.Set{}}
	order := []toolkit.Registry{}
	for _, id := range []string{"alice", "bob"} {
		if c, ok := cats[id]; ok {
			f.sets[id] = c.Default()
			order = append(order, c)
		}
	}
	auth := f.store.Authenticator(func(s hook.Scope) (*toolkit.Set, bool) {
		set, ok := f.sets[s.AgentID]
		return set, ok
	}, "invalid or missing session_token")
	var err error
	if f.aliceTok, err = f.store.Issue(Grant{Scope: hook.Scope{AgentID: "alice", Session: "s-alice"}}); err != nil {
		t.Fatal(err)
	}
	if f.bobTok, err = f.store.Issue(Grant{Scope: hook.Scope{AgentID: "bob", Session: "s-bob"}}); err != nil {
		t.Fatal(err)
	}
	build := func(tr Transport) *Endpoint {
		cfg := Config{
			Name: "test-host", Version: "1.2.3", Transport: tr, Authenticator: auth,
			Catalogue: Union(func() []toolkit.Registry { return order }),
		}
		for _, o := range opts {
			o(&cfg)
		}
		ep, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ep.Close)
		return ep
	}
	f.arg, f.bearer = build(Argument), build(Bearer)
	mux := http.NewServeMux()
	mux.Handle("/internal", f.arg)
	mux.Handle("/mcp", f.bearer)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return &perAgent{f}
}

func catalogOf(t *testing.T, tools ...toolkit.Tool) *toolkit.Catalog {
	t.Helper()
	return wireCatalog(t, tools...)
}

// h: a bearer caller is listed and described strictly from its own
// registry, even for a name another agent's tool also has.
func TestBearer_ListsTheCallersOwnTools(t *testing.T) {
	aliceSchema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	bobSchema := map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "integer"}}, "required": []any{"b"}}
	h := newPerAgent(t, map[string]*toolkit.Catalog{
		"alice": catalogOf(t,
			&wireTool{name: "lookup", desc: "Alice's lookup", params: aliceSchema, run: wireText("alice ran")},
			&wireTool{name: "alice_only", desc: "Alice only", params: nil, run: wireText("a")}),
		"bob": catalogOf(t,
			&wireTool{name: "lookup", desc: "Bob's lookup", params: bobSchema, run: wireText("bob ran")}),
	})
	alice := list(t, h.mustClient("/mcp", h.aliceTok))
	bob := list(t, h.mustClient("/mcp", h.bobTok))
	if got := names(bob); !slices.Equal(got, []string{"lookup"}) {
		t.Fatalf("Bob lists %v", got)
	}
	if bob["lookup"].Description != "Bob's lookup" || alice["lookup"].Description != "Alice's lookup" {
		t.Errorf("descriptions: Alice %q, Bob %q", alice["lookup"].Description, bob["lookup"].Description)
	}
	bs := schemaOf(t, bob["lookup"])
	if props, ok := bs["properties"].(map[string]any); !ok || props["b"] == nil || props["a"] != nil {
		t.Errorf("Bob's schema = %v", bs)
	}
	if r := callT(t, h.mustClient("/mcp", h.bobTok), "lookup", nil); r.text != "bob ran" {
		t.Errorf("Bob's call = %+v", r)
	}
	if r := callT(t, h.mustClient("/mcp", h.bobTok), "alice_only", nil); !r.isError || r.text != toolkit.NotEnabledMessage("alice_only") {
		t.Errorf("Bob calling Alice's tool = %+v", r)
	}
	// The Argument endpoint lists the union, each name from the first agent.
	arg := list(t, h.mustClient("/internal", ""))
	if got := names(arg); !slices.Equal(got, []string{"alice_only", "lookup"}) || arg["lookup"].Description != "Alice's lookup" {
		t.Errorf("/internal lists %v (%q)", got, arg["lookup"].Description)
	}
	if r := callT(t, h.mustClient("/internal", ""), "lookup", map[string]any{TokenArgument: h.bobTok}); r.text != "bob ran" {
		t.Errorf("Bob's call on /internal = %+v", r)
	}
}

// c: the name rule decides what is published, and a call runs exactly the
// tool published under the name it uses.
func TestNameRules(t *testing.T) {
	tools := func() *toolkit.Catalog {
		return catalogOf(t,
			&wireTool{name: "srv_write", desc: "Native write", params: map[string]any{}, run: wireText("native write")},
			&wireExternal{wireTool{name: "mcp_srv_write", desc: "Upstream write", params: map[string]any{}, run: wireText("upstream write")}, "srv_write"},
			&wireExternal{wireTool{name: "mcp_srv_read", desc: "Upstream read", params: map[string]any{}, run: wireText("upstream read")}, "srv_read"})
	}
	cases := []struct {
		rule  NameRule
		names []string
		calls map[string]string
	}{
		{
			PublishedNames,
			[]string{"mcp_srv_write", "srv_read", "srv_write"},
			map[string]string{"srv_write": "native write", "mcp_srv_write": "upstream write", "srv_read": "upstream read"},
		},
		{
			ExternalFirst,
			[]string{"srv_read", "srv_write"},
			map[string]string{"srv_write": "upstream write", "srv_read": "upstream read"},
		},
	}
	for _, tc := range cases {
		h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": tools()}, func(c *Config) { c.Names = tc.rule })
		for _, path := range []string{"/internal", "/mcp"} {
			tok := h.aliceTok
			c := h.mustClient(path, map[bool]string{true: tok, false: ""}[path == "/mcp"])
			if got := names(list(t, c)); !slices.Equal(got, tc.names) {
				t.Errorf("rule %d %s lists %v, want %v", tc.rule, path, got, tc.names)
			}
			for name, want := range tc.calls {
				args := map[string]any{}
				if path == "/internal" {
					args[TokenArgument] = tok
				}
				if r := callT(t, c, name, args); r.text != want {
					t.Errorf("rule %d %s %s = %+v, want %q", tc.rule, path, name, r, want)
				}
			}
		}
	}
}

// The Gate sees the tool that runs under a published name, not the one
// toolkit would find by that name.
func TestNameRules_GateSeesTheToolThatRuns(t *testing.T) {
	var seen []string
	h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": catalogOf(t,
		&wireTool{name: "srv_write", desc: "Native write", params: map[string]any{}, run: wireText("native write")},
		&wireExternal{wireTool{name: "mcp_srv_write", desc: "Upstream write", params: map[string]any{}, run: wireText("upstream write")}, "srv_write"},
	)}, func(c *Config) {
		c.Names = ExternalFirst
		c.Gate = func(_ context.Context, info toolkit.Info) *toolkit.Result {
			seen = append(seen, info.Name)
			return nil
		}
	})
	if r := callT(t, h.mustClient("/mcp", h.aliceTok), "srv_write", nil); r.text != "upstream write" {
		t.Fatalf("call = %+v", r)
	}
	if !slices.Equal(seen, []string{"mcp_srv_write"}) {
		t.Errorf("gate saw %v", seen)
	}
}

// a: with PanicAsProtocolError a panicking tool is answered with the
// protocol error, after the callbacks and events saw the panic.
func TestPanicAsProtocolError(t *testing.T) {
	rec := &recorder{}
	var onError string
	f := newFixture(t, func(c *Config) {
		c.PanicAsProtocolError = true
		c.Plugins = []hook.Plugin{rec.plugin(), {
			Name: "host",
			OnToolError: func(_ context.Context, _ *hook.ToolCall, r *toolkit.Result) *toolkit.Result {
				onError = r.ForLLM
				return nil
			},
		}}
	})
	r := callT(t, f.mustClient("/mcp", f.aliceTok), "panicky", nil)
	if r.err == nil || !strings.Contains(r.err.Error(), "panic recovered in panicky tool handler: tool exploded") {
		t.Fatalf("result = %+v, want the protocol error", r)
	}
	if onError != "Tool panicky failed with an internal error." {
		t.Errorf("OnToolError saw %q", onError)
	}
	if fin := rec.finished(); len(fin) != 1 || fin[0].Result.ForLLM != "Tool panicky failed with an internal error." {
		t.Errorf("finished = %+v", fin)
	}
	if n := f.bearer.InFlight(); n != 0 {
		t.Errorf("InFlight after a re-raised panic = %d", n)
	}
}

// d: ToolContext adjusts a call's context for the tool it resolved to; the
// scope keeps the session.
func TestToolContext(t *testing.T) {
	var scopeSession string
	f := newFixture(t, func(c *Config) {
		c.ToolContext = func(ctx context.Context, _ Caller, info toolkit.Info) context.Context {
			if info.Name == "whoami" {
				s, _ := hook.ScopeOf(ctx)
				scopeSession = s.Session
				return toolkit.WithSessionKey(ctx, "")
			}
			return ctx
		}
	})
	r := callT(t, f.mustClient("/mcp", f.aliceTok), "whoami", nil)
	if r.text != "agent=alice session=s-alice turn=t1 depth=1 key=" {
		t.Errorf("whoami = %+v", r)
	}
	if scopeSession != "s-alice" {
		t.Errorf("scope session = %q", scopeSession)
	}
	// Without ToolContext every tool gets the session key.
	plain := newFixture(t)
	if r := callT(t, plain.mustClient("/mcp", plain.aliceTok), "whoami", nil); !strings.HasSuffix(r.text, "key=s-alice") {
		t.Errorf("default whoami = %+v", r)
	}
}

// e: a caller whose Done is nil is never cancelled by revocation: a call in
// flight when its token is revoked finishes and returns its result.
func TestNilDone_CallInFlightFinishes(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	cat := catalogOf(t, &wireTool{name: "hold", desc: "waits", params: map[string]any{}, run: func(ctx context.Context, _ map[string]any) *toolkit.Result {
		started <- struct{}{}
		select {
		case <-release:
			return toolkit.NewResult("held to the end")
		case <-ctx.Done():
			return toolkit.ErrorResult("cancelled")
		}
	}})
	store := NewStore()
	tok, err := store.Issue(Grant{Scope: hook.Scope{AgentID: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	auth := AuthenticatorFunc(func(_ context.Context, token string) (Caller, error) {
		g, _, ok := store.Resolve(token)
		if !ok {
			return Caller{}, errors.New("invalid token")
		}
		return Caller{Set: cat.Default(), Scope: g.Scope}, nil // no Done
	})
	for _, tr := range []Transport{Argument, Bearer} {
		ep, err := New(Config{Name: "n", Version: "1", Transport: tr, Authenticator: auth, Catalogue: FromRegistry(cat)})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(ep)
		f := &fixture{t: t, srv: srv}
		bearer, args := "", map[string]any{TokenArgument: tok}
		if tr == Bearer {
			bearer, args = tok, nil
		}
		c := f.mustClient("", bearer)
		done := make(chan callResult, 1)
		go func() { done <- callT(t, c, "hold", args) }()
		<-started
		store.Revoke(tok)
		close(release)
		if r := <-done; r.err != nil || r.text != "held to the end" {
			t.Errorf("%s: call in flight = %+v", tr, r)
		}
		srv.Close()
		ep.Close()
		release = make(chan struct{})
		if tok, err = store.Issue(Grant{Scope: hook.Scope{AgentID: "alice"}}); err != nil {
			t.Fatal(err)
		}
	}
}

// f: with a Catalogue that has no version, what is published changes only
// on Refresh, which sends tools/list_changed only when it changed.
func TestRefresh(t *testing.T) {
	alice := catalogOf(t, &wireTool{name: "first", desc: "first", params: map[string]any{}, run: wireText("1")})
	h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": alice})
	c := h.mustClient("/internal", "", transport.WithContinuousListening()) //nolint:staticcheck // the legacy GET stream CLIs use
	changed := make(chan struct{}, 16)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			changed <- struct{}{}
		}
	})
	time.Sleep(200 * time.Millisecond) // the listening stream opens in the background

	if h.arg.Refresh(context.Background()) {
		t.Error("Refresh with nothing changed reported a notice")
	}
	if err := alice.AddEntries(toolkit.Entry{Tool: &wireTool{name: "second", desc: "second", params: map[string]any{}, run: wireText("2")}}); err != nil {
		t.Fatal(err)
	}
	// Not published until Refresh: neither listed nor callable, no notice.
	if _, ok := list(t, c)["second"]; ok {
		t.Error("listed before Refresh")
	}
	if r := callT(t, c, "second", map[string]any{TokenArgument: h.aliceTok}); r.err == nil {
		t.Errorf("callable before Refresh: %+v", r)
	}
	select {
	case <-changed:
		t.Fatal("notice before Refresh")
	case <-time.After(200 * time.Millisecond):
	}
	if !h.arg.Refresh(context.Background()) {
		t.Fatal("Refresh after a change reported no notice")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed after Refresh")
	}
	if _, ok := list(t, c)["second"]; !ok {
		t.Error("not listed after Refresh")
	}
	if h.arg.Refresh(context.Background()) {
		t.Error("a second Refresh reported a notice")
	}
	// A versioned catalogue needs no Refresh: it reports nothing new.
	f := newFixture(t)
	if f.arg.Refresh(context.Background()) {
		t.Error("Refresh of a followed catalogue reported a notice")
	}
}

// g: by default a background tool runs on the request's context; with
// DetachBackground its context outlives the request.
func TestBackgroundContext(t *testing.T) {
	for _, detach := range []bool{false, true} {
		got := make(chan string, 1)
		f := newFixture(t, func(c *Config) {
			c.DetachBackground = detach
			c.AsyncResult = func(_ context.Context, _ Caller, _ hook.ToolCall, r *toolkit.Result) { got <- r.ForLLM }
		})
		if r := callT(t, f.mustClient("/mcp", f.aliceTok), "job", map[string]any{"job": "j"}); r.text != "started" {
			t.Fatalf("job = %+v", r)
		}
		select {
		case text := <-got:
			want := "finished j"
			if !detach {
				want = "context ended: context canceled"
			}
			if text != want {
				t.Errorf("detach=%v: late result %q, want %q", detach, text, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no late result")
		}
	}
}
