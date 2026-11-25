/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package mcphost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

func TestNew_RequiresItsInputs(t *testing.T) {
	cat := toolkit.New()
	auth := AuthenticatorFunc(func(context.Context, string) (Caller, error) { return Caller{}, nil })
	good := Config{Name: "n", Version: "1.0.0", Transport: Argument, Authenticator: auth, Catalogue: FromRegistry(cat)}
	cases := map[string]func(*Config){
		"no name":          func(c *Config) { c.Name = "" },
		"no version":       func(c *Config) { c.Version = "" },
		"no transport":     func(c *Config) { c.Transport = 0 },
		"no authenticator": func(c *Config) { c.Authenticator = nil },
		"no catalogue":     func(c *Config) { c.Catalogue = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := good
			change(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted an incomplete configuration")
			}
		})
	}
	ep, err := New(good)
	if err != nil {
		t.Fatal(err)
	}
	ep.Close()
	ep.Close() // idempotent
}

func TestEndToEnd_Initialize(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ path, token string }{{"/internal", ""}, {"/mcp", f.aliceTok}} {
		c := f.mustClient(tc.path, tc.token)
		if res := c.GetServerCapabilities(); res.Tools == nil || !res.Tools.ListChanged {
			t.Errorf("%s: tools capability = %+v, want listChanged", tc.path, res.Tools)
		}
	}
}

// The Argument endpoint lists its catalogue to anyone, hidden tools
// included, each schema carrying the required session_token; the Bearer
// endpoint lists the caller's own tools with their own schemas.
func TestEndToEnd_List(t *testing.T) {
	f := newFixture(t)

	arg := list(t, f.mustClient("/internal", ""))
	want := []string{"deep", "echo", "fail", "gated", "job", "panicky", "secret", "slow", "whoami"}
	if got := names(arg); !slices.Equal(got, want) {
		t.Fatalf("/internal tools = %v, want %v", got, want)
	}
	for name, tl := range arg {
		schema := schemaOf(t, tl)
		if !slices.Contains(requiredOf(schema), TokenArgument) {
			t.Errorf("/internal %s: session_token not required: %v", name, schema)
		}
		props, isMap := schema["properties"].(map[string]any)
		if _, ok := props[TokenArgument]; !isMap || !ok {
			t.Errorf("/internal %s: no session_token property", name)
		}
	}
	echo := schemaOf(t, arg["echo"])
	if req := requiredOf(echo); !slices.Equal(req, []string{"text", TokenArgument}) {
		t.Errorf("echo required = %v, want [text session_token]", req)
	}
	if arg["echo"].Description != "echo the arguments" {
		t.Errorf("echo description = %q", arg["echo"].Description)
	}

	alice := list(t, f.mustClient("/mcp", f.aliceTok))
	if got := names(alice); !slices.Equal(got, want) {
		t.Errorf("/mcp Alice tools = %v, want %v", got, want)
	}
	for name, tl := range alice {
		schema := schemaOf(t, tl)
		if slices.Contains(requiredOf(schema), TokenArgument) {
			t.Errorf("/mcp %s: schema carries session_token", name)
		}
	}
	if req := requiredOf(schemaOf(t, alice["echo"])); !slices.Equal(req, []string{"text"}) {
		t.Errorf("/mcp echo required = %v, want [text]", req)
	}
	bob := list(t, f.mustClient("/mcp", f.bobTok))
	if got, want := names(bob), []string{"echo", "gated", "slow", "whoami"}; !slices.Equal(got, want) {
		t.Errorf("/mcp Bob tools = %v, want %v", got, want)
	}
}

// A tool the catalogue offers under an external name is published and
// called under it.
func TestEndToEnd_PublishedName(t *testing.T) {
	f := newFixture(t)
	ext := &externalTool{fnTool: *textTool("mcp_upstream_lookup", "looked up"), external: "upstream_lookup"}
	if err := f.cat.AddEntries(toolkit.Entry{Tool: ext}); err != nil {
		t.Fatal(err)
	}
	c := f.mustClient("/internal", "")
	tools := list(t, c)
	if _, ok := tools["upstream_lookup"]; !ok {
		t.Fatalf("external name not published: %v", names(tools))
	}
	if _, ok := tools["mcp_upstream_lookup"]; ok {
		t.Fatal("internal name published")
	}
	f.sets["alice"].Grant(grant{"mcp_upstream_lookup"})
	if r := callT(t, c, "upstream_lookup", map[string]any{TokenArgument: f.aliceTok}); r.err != nil || r.isError || r.text != "looked up" {
		t.Errorf("call by published name = %+v", r)
	}
}

type externalTool struct {
	fnTool

	external string
}

func (t *externalTool) ExternalName() string { return t.external }

func TestEndToEnd_Call(t *testing.T) {
	f := newFixture(t)
	argC := f.mustClient("/internal", "")
	aliceC := f.mustClient("/mcp", f.aliceTok)
	bobC := f.mustClient("/mcp", f.bobTok)
	withTok := func(tok string, args map[string]any) map[string]any {
		out := map[string]any{TokenArgument: tok}
		maps.Copy(out, args)
		return out
	}

	t.Run("success", func(t *testing.T) {
		r := callT(t, argC, "whoami", withTok(f.aliceTok, nil))
		if r.err != nil || r.isError || r.text != "agent=alice session=s-alice turn=t1 depth=1 key=s-alice" {
			t.Errorf("argument whoami = %+v", r)
		}
		r = callT(t, bobC, "whoami", nil)
		if r.err != nil || r.isError || r.text != "agent=bob session=s-bob turn=t2 depth=0 key=s-bob" {
			t.Errorf("bearer whoami = %+v", r)
		}
	})
	t.Run("session_token stripped", func(t *testing.T) {
		r := callT(t, argC, "echo", withTok(f.aliceTok, map[string]any{"text": "hi"}))
		if r.text != "keys=text text=hi" {
			t.Errorf("argument echo = %+v", r)
		}
		// A bearer client that sends one anyway does not reach the tool with it.
		r = callT(t, aliceC, "echo", map[string]any{"text": "hi", TokenArgument: "anything"})
		if r.text != "keys=text text=hi" {
			t.Errorf("bearer echo = %+v", r)
		}
	})
	t.Run("tool error", func(t *testing.T) {
		r := callT(t, aliceC, "fail", nil)
		if r.err != nil || !r.isError || r.text != "boom" {
			t.Errorf("fail = %+v", r)
		}
	})
	t.Run("not granted", func(t *testing.T) {
		r := callT(t, bobC, "secret", nil)
		if !r.isError || r.text != toolkit.NotEnabledMessage("secret") {
			t.Errorf("Bob calling secret = %+v", r)
		}
		r = callT(t, argC, "secret", withTok(f.bobTok, nil))
		if !r.isError || r.text != toolkit.NotEnabledMessage("secret") {
			t.Errorf("Bob calling secret on /internal = %+v", r)
		}
	})
	t.Run("hidden tool", func(t *testing.T) {
		if r := callT(t, aliceC, "deep", nil); r.isError || r.text != "a hidden tool" {
			t.Errorf("deep = %+v", r)
		}
	})
	t.Run("unknown tool", func(t *testing.T) {
		r := callT(t, aliceC, "nonesuch", nil)
		if r.err == nil || !strings.Contains(r.err.Error(), "not found") {
			t.Errorf("unknown tool = %+v, want a not-found error", r)
		}
	})
	t.Run("panic", func(t *testing.T) {
		r := callT(t, aliceC, "panicky", nil)
		if !r.isError || r.text != "Tool panicky failed with an internal error." {
			t.Errorf("panicky = %+v", r)
		}
	})
}

func TestEndToEnd_RemovedAndDisabled(t *testing.T) {
	f := newFixture(t)
	c := f.mustClient("/mcp", f.aliceTok)
	if _, ok := list(t, c)["gated"]; !ok {
		t.Fatal("gated not listed")
	}

	// Removed since the client listed it: the endpoint no longer offers it.
	f.cat.Remove("gated")
	if r := callT(t, c, "gated", nil); r.err == nil || !strings.Contains(r.err.Error(), "not found") {
		t.Errorf("removed tool = %+v, want a not-found error", r)
	}
	if _, ok := list(t, c)["gated"]; ok {
		t.Error("removed tool still listed")
	}

	// Disabled for everyone: no longer offered.
	if err := f.cat.Disable("echo"); err != nil {
		t.Fatal(err)
	}
	if r := callT(t, c, "echo", map[string]any{"text": "x"}); r.err == nil {
		t.Errorf("tool disabled in the catalogue = %+v, want a not-found error", r)
	}
	if _, ok := list(t, c)["echo"]; ok {
		t.Error("disabled tool still listed")
	}

	// Disabled for Alice only: still offered by the catalogue, refused for
	// her, unlisted for her.
	if err := f.sets["alice"].Disable("whoami"); err != nil {
		t.Fatal(err)
	}
	if r := callT(t, c, "whoami", nil); !r.isError || r.text != `tool "whoami" is disabled` {
		t.Errorf("tool disabled for Alice = %+v", r)
	}
	if _, ok := list(t, c)["whoami"]; ok {
		t.Error("tool disabled for Alice still listed for her")
	}
	if _, ok := list(t, f.mustClient("/mcp", f.bobTok))["whoami"]; !ok {
		t.Error("tool disabled for Alice is gone for Bob")
	}
}

func TestEndToEnd_Gate(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	f := newFixture(t, func(c *Config) {
		c.Gate = func(ctx context.Context, info toolkit.Info) *toolkit.Result {
			s, _ := hook.ScopeOf(ctx)
			if call, ok := hook.ToolCallFrom(ctx); !ok || call.Name != info.Name {
				t.Errorf("the gate's tool call = %+v, %v", call, ok)
			}
			mu.Lock()
			seen = append(seen, s.AgentID+":"+info.Name)
			mu.Unlock()
			if info.Name == "gated" && s.AgentID == "bob" {
				return toolkit.ErrorResult(toolkit.NotEnabledMessage(info.Name))
			}
			return nil
		}
	})
	if r := callT(t, f.mustClient("/mcp", f.bobTok), "gated", nil); !r.isError || r.text != toolkit.NotEnabledMessage("gated") {
		t.Errorf("gate refusal = %+v", r)
	}
	if r := callT(t, f.mustClient("/mcp", f.aliceTok), "gated", nil); r.isError || r.text != "passed the gate" {
		t.Errorf("gate allowed = %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(seen, "bob:gated") || !slices.Contains(seen, "alice:gated") {
		t.Errorf("gate saw %v", seen)
	}
}

// A list made right after a change shows it, whether or not the change's
// notice has been delivered yet; a call to the new tool is routed.
func TestEndToEnd_ListIsNeverStale(t *testing.T) {
	f := newFixture(t)
	argC := f.mustClient("/internal", "")
	aliceC := f.mustClient("/mcp", f.aliceTok)
	for i := range 20 {
		name := "later"
		if err := f.cat.AddEntries(toolkit.Entry{Tool: textTool(name, "added")}); err != nil {
			t.Fatal(err)
		}
		if _, ok := list(t, argC)[name]; !ok {
			t.Fatalf("round %d: /internal list misses a tool added before it", i)
		}
		if _, ok := list(t, aliceC)[name]; !ok {
			t.Fatalf("round %d: /mcp list misses a tool added before it", i)
		}
		if r := callT(t, aliceC, name, nil); r.err != nil || r.text != "added" {
			t.Fatalf("round %d: call to the new tool = %+v", i, r)
		}
		f.cat.Remove(name)
		if _, ok := list(t, argC)[name]; ok {
			t.Fatalf("round %d: /internal list shows a removed tool", i)
		}
	}
}

func TestEndToEnd_ListChanged(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ path, token string }{{"/internal", ""}, {"/mcp", f.aliceTok}} {
		t.Run(tc.path, func(t *testing.T) {
			// The legacy GET stream: what CLI clients use today.
			c := f.mustClient(tc.path, tc.token, transport.WithContinuousListening()) //nolint:staticcheck // see above
			changed := make(chan struct{}, 16)
			c.OnNotification(func(n mcp.JSONRPCNotification) {
				if n.Method == mcp.MethodNotificationToolsListChanged {
					changed <- struct{}{}
				}
			})
			// The listening stream is opened in the background: send changes
			// until one notice arrives.
			deadline := time.After(5 * time.Second)
			for i := 0; ; i++ {
				if f.cat.Remove("later") == 0 {
					if err := f.cat.AddEntries(toolkit.Entry{Tool: textTool("later", "added")}); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-changed:
					return
				case <-time.After(100 * time.Millisecond):
				case <-deadline:
					t.Fatal("no tools/list_changed")
				}
			}
		})
	}
}

// Watch sends tools/list_changed for a set's own change.
func TestEndToEnd_WatchSet(t *testing.T) {
	f := newFixture(t)
	unwatch := f.bearer.Watch(f.sets["bob"])
	defer unwatch()
	c := f.mustClient("/mcp", f.bobTok, transport.WithContinuousListening()) //nolint:staticcheck // the legacy GET stream CLIs use
	changed := make(chan struct{}, 16)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == mcp.MethodNotificationToolsListChanged {
			changed <- struct{}{}
		}
	})
	deadline := time.After(5 * time.Second)
	for i := 0; ; i++ {
		var err error
		if i%2 == 0 {
			err = f.sets["bob"].Disable("echo")
		} else {
			err = f.sets["bob"].Enable("echo")
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("no tools/list_changed for a watched set")
		}
	}
}

func TestEndToEnd_BadCredentials(t *testing.T) {
	f := newFixture(t)
	expired, err := f.store.Issue(Grant{Scope: hook.Scope{AgentID: "alice"}, Expires: time.Now().Add(50 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := f.store.Issue(Grant{Scope: hook.Scope{AgentID: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	f.store.Revoke(revoked)
	time.Sleep(100 * time.Millisecond)

	argC := f.mustClient("/internal", "")
	for name, tok := range map[string]string{"missing": "", "unknown": "nope", "expired": expired, "revoked": revoked} {
		t.Run("argument "+name, func(t *testing.T) {
			args := map[string]any{}
			if tok != "" {
				args[TokenArgument] = tok
			}
			r := callT(t, argC, "whoami", args)
			if r.err != nil || !r.isError || r.text != "invalid or missing session_token" {
				t.Errorf("call = %+v", r)
			}
		})
		t.Run("bearer "+name, func(t *testing.T) {
			_, err := f.client("/mcp", tok)
			if err == nil {
				t.Fatal("connected without a valid bearer token")
			}
		})
	}
}

func TestBearer_401(t *testing.T) {
	f := newFixture(t)
	for name, header := range map[string]string{
		"missing":    "",
		"not bearer": "Basic abc",
		"unknown":    "Bearer nope",
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, f.srv.URL+"/mcp",
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="test-realm"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			want := "invalid bearer token"
			if header == "" || header == "Basic abc" {
				want = "missing bearer token"
			}
			if strings.TrimSpace(string(body)) != want {
				t.Errorf("body = %q, want %q", body, want)
			}
		})
	}
}

func TestEndToEnd_Cancellation(t *testing.T) {
	f := newFixture(t)
	c := f.mustClient("/mcp", f.aliceTok)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan callResult, 1)
	go func() { done <- call(ctx, c, "slow", nil) }()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow tool did not start")
	}
	cancel()
	select {
	case cause := <-f.slowDone:
		if cause == nil {
			t.Fatal("slow tool ran to the end")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tool's context was not cancelled with the client's request")
	}
	<-done
}

// Revoking a token cancels the calls running with it, on both transports,
// and their results do not reach the client.
func TestEndToEnd_RevokeCancelsCallInFlight(t *testing.T) {
	for _, path := range []string{"/internal", "/mcp"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			tok, err := f.store.Issue(Grant{Scope: hook.Scope{AgentID: "alice", Session: "s-turn"}})
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{TokenArgument: tok}
			bearer := ""
			if path == "/mcp" {
				bearer, args = tok, nil
			}
			c := f.mustClient(path, bearer)
			done := make(chan callResult, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				done <- call(ctx, c, "slow", args)
			}()
			select {
			case <-f.started:
			case <-time.After(5 * time.Second):
				t.Fatal("slow tool did not start")
			}
			f.store.Revoke(tok)
			select {
			case cause := <-f.slowDone:
				if !errors.Is(cause, ErrRevoked) {
					t.Errorf("tool context cause = %v, want ErrRevoked", cause)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("revoking did not cancel the call")
			}
			r := <-done
			if r.err == nil && (!r.isError || r.text != ErrRevoked.Error()) {
				t.Errorf("client got %+v after revocation", r)
			}
		})
	}
}

// Calls of Alice and Bob in parallel, on both transports, each see their
// own set and scope.
func TestEndToEnd_ConcurrentCallersDoNotCross(t *testing.T) {
	f := newFixture(t)
	argC := f.mustClient("/internal", "")
	aliceC := f.mustClient("/mcp", f.aliceTok)
	bobC := f.mustClient("/mcp", f.bobTok)
	want := map[string]string{
		"alice": "agent=alice session=s-alice turn=t1 depth=1 key=s-alice",
		"bob":   "agent=bob session=s-bob turn=t2 depth=0 key=s-bob",
	}
	var wg sync.WaitGroup
	errs := make(chan string, 400)
	for i := range 50 {
		for _, who := range []string{"alice", "bob"} {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var r callResult
				if i%2 == 0 {
					tok := f.aliceTok
					if who == "bob" {
						tok = f.bobTok
					}
					r = call(ctx, argC, "whoami", map[string]any{TokenArgument: tok})
				} else if who == "alice" {
					r = call(ctx, aliceC, "whoami", nil)
				} else {
					r = call(ctx, bobC, "whoami", nil)
				}
				if r.err != nil || r.text != want[who] {
					errs <- who + ": " + r.text
				}
				// Bob is never served Alice's tools.
				if who == "bob" {
					var s callResult
					if i%2 == 0 {
						s = call(ctx, argC, "secret", map[string]any{TokenArgument: f.bobTok})
					} else {
						s = call(ctx, bobC, "secret", nil)
					}
					if !s.isError {
						errs <- "bob reached secret"
					}
				}
			})
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// A client on the current protocol (server/discover, subscriptions/listen)
// is served the same way on both transports.
func TestEndToEnd_ModernProtocol(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct{ path, token string }{{"/internal", ""}, {"/mcp", f.aliceTok}} {
		t.Run(tc.path, func(t *testing.T) {
			opts := []transport.StreamableHTTPCOption{transport.WithHTTPLogger(slog.New(slog.DiscardHandler))}
			if tc.token != "" {
				opts = append(opts, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + tc.token}))
			}
			tr, trErr := transport.NewStreamableHTTP(f.srv.URL+tc.path, opts...)
			if trErr != nil {
				t.Fatal(trErr)
			}
			c := client.NewClient(tr)
			t.Cleanup(func() {
				if err := c.Close(); err != nil {
					t.Logf("close: %v", err)
				}
			})
			if err := c.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			init := mcp.InitializeRequest{}
			init.Params.ClientInfo = mcp.Implementation{Name: "modern", Version: "0.0.1"}
			if _, err := c.Initialize(ctx, init); err != nil {
				t.Fatal(err)
			}
			if _, ok := list(t, c)["whoami"]; !ok {
				t.Fatal("whoami not listed")
			}
			args := map[string]any{}
			if tc.token == "" {
				args[TokenArgument] = f.aliceTok
			}
			if r := callT(t, c, "whoami", args); r.err != nil || !strings.HasPrefix(r.text, "agent=alice ") {
				t.Fatalf("whoami = %+v", r)
			}

			changed := make(chan struct{}, 16)
			c.OnNotification(func(n mcp.JSONRPCNotification) {
				if n.Method == mcp.MethodNotificationToolsListChanged {
					changed <- struct{}{}
				}
			})
			stop, listenErr := c.ListenAsync(context.Background(), mcp.SubscriptionFilter{ToolsListChanged: true}, nil)
			if listenErr != nil {
				t.Fatal(listenErr)
			}
			defer stop()
			deadline := time.After(5 * time.Second)
			for {
				if f.cat.Remove("later") == 0 {
					if err := f.cat.AddEntries(toolkit.Entry{Tool: textTool("later", "added")}); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-changed:
					return
				case <-time.After(100 * time.Millisecond):
				case <-deadline:
					t.Fatal("no tools/list_changed on a listen stream")
				}
			}
		})
	}
}
