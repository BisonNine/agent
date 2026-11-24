package mcphost

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// lateTool finishes in the background when release is closed, reporting
// on an isolated context, as a sub-agent spawn does.
type lateTool struct {
	wireTool

	release chan struct{}
}

func (t *lateTool) ExecuteAsync(_ context.Context, _ map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	go func() { //nolint:contextcheck // an isolated context, on purpose
		<-t.release
		// An isolated context, carrying nothing of the call.
		cb(context.Background(), toolkit.NewResult("late result"))
	}()
	return toolkit.AsyncResult("started")
}

type chatKey struct{}

// 1: a background result is delivered on the call's own context, detached
// from the request: the values CallContext put there at call time, the
// tool call and the target, and the tool that ran.
func TestAsyncResult_KeepsTheCallsContext(t *testing.T) {
	late := &lateTool{release: make(chan struct{})}
	late.name, late.desc, late.params = "mcp_spawn", "spawn", map[string]any{}
	ext := &externalLate{lateTool: late, external: "spawn"}
	cat := catalogOf(t, ext)
	var chat atomic.Value
	chat.Store("chat-1")
	type delivery struct {
		chat, called, internal, info string
		live                         bool
		hasCall                      bool
	}
	got := make(chan delivery, 1)
	h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": cat}, func(c *Config) {
		c.CallContext = func(ctx context.Context, _ Caller) context.Context {
			return context.WithValue(ctx, chatKey{}, chat.Load())
		}
		c.AsyncResult = func(ctx context.Context, _ Caller, call hook.ToolCall, info toolkit.Info, _ *toolkit.Result) {
			v, isString := ctx.Value(chatKey{}).(string)
			if !isString {
				v = "none"
			}
			tg, _ := TargetOf(ctx)
			tc, hasCall := hook.ToolCallFrom(ctx)
			got <- delivery{
				chat: v, called: call.Name, internal: tg.Internal, info: info.Name,
				live: ctx.Err() == nil, hasCall: hasCall && tc.Name == call.Name,
			}
		}
	})
	for _, path := range []string{"/internal", "/mcp"} {
		bearer, args := "", map[string]any{TokenArgument: h.aliceTok}
		if path == "/mcp" {
			bearer, args = h.aliceTok, nil
		}
		late.release = make(chan struct{})
		chat.Store("chat-1")
		if r := callT(t, h.mustClient(path, bearer), "spawn", args); r.text != "started" {
			t.Fatalf("%s: spawn = %+v", path, r)
		}
		// The request has ended and the turn's chat has moved on.
		chat.Store("chat-2")
		close(late.release)
		select {
		case d := <-got:
			want := delivery{chat: "chat-1", called: "spawn", internal: "mcp_spawn", info: "mcp_spawn", live: true, hasCall: true}
			if d != want {
				t.Errorf("%s: delivered %+v, want %+v", path, d, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no late result", path)
		}
	}
}

type externalLate struct {
	*lateTool

	external string
}

func (t *externalLate) ExternalName() string { return t.external }

// 2: every call carries the internal name of what it names, whether or not
// the caller's set holds it, on CallContext, ToolContext, the callbacks and
// the events; the refusal of a tool the caller does not hold names the
// called tool, or with RefuseWithInternalName the internal one.
func TestTarget_InternalNameOnEveryCall(t *testing.T) {
	for _, internalRefusal := range []bool{false, true} {
		alice := catalogOf(t,
			&wireExternal{wireTool{name: "mcp_srv_read", desc: "Upstream read", params: map[string]any{}, run: wireText("upstream read")}, "srv_read"})
		bob := catalogOf(t, &wireTool{name: "bob_only", desc: "Bob", params: map[string]any{}, run: wireText("bob")})
		var mu sync.Mutex
		seen := map[string][]Target{}
		note := func(where string, ctx context.Context) {
			if tg, ok := TargetOf(ctx); ok {
				mu.Lock()
				seen[where] = append(seen[where], Target{Published: tg.Published, Internal: tg.Internal, Held: tg.Held})
				mu.Unlock()
			}
		}
		h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": alice, "bob": bob}, func(c *Config) {
			c.Names = ExternalFirst
			c.RefuseWithInternalName = internalRefusal
			c.CallContext = func(ctx context.Context, _ Caller) context.Context { note("call", ctx); return ctx }
			c.Plugins = []hook.Plugin{{
				Name: "probe",
				BeforeTool: func(ctx context.Context, _ *hook.ToolCall) (*toolkit.Result, error) {
					note("before", ctx)
					return nil, nil //nolint:nilnil // (nil, nil): run the tool
				},
				OnEvent: func(ctx context.Context, ev hook.Event) {
					if _, ok := ev.(hook.ToolCallFinished); ok {
						note("event", ctx)
					}
				},
			}}
		})
		want := toolkit.NotEnabledMessage("srv_read")
		if internalRefusal {
			want = toolkit.NotEnabledMessage("mcp_srv_read")
		}
		for _, path := range []string{"/internal", "/mcp"} {
			bearer, args := "", map[string]any{TokenArgument: h.bobTok}
			if path == "/mcp" {
				bearer, args = h.bobTok, nil
			}
			c := h.mustClient(path, bearer)
			if r := callT(t, c, "srv_read", args); !r.isError || r.text != want {
				t.Errorf("internal=%v %s: Bob's srv_read = %+v, want %q", internalRefusal, path, r, want)
			}
		}
		aliceArgs := map[string]any{TokenArgument: h.aliceTok}
		if r := callT(t, h.mustClient("/internal", ""), "srv_read", aliceArgs); r.text != "upstream read" {
			t.Errorf("Alice's srv_read = %+v", r)
		}
		mu.Lock()
		for _, where := range []string{"call", "before", "event"} {
			wantSeen := []Target{
				{Published: "srv_read", Internal: "mcp_srv_read", Held: false},
				{Published: "srv_read", Internal: "mcp_srv_read", Held: false},
				{Published: "srv_read", Internal: "mcp_srv_read", Held: true},
			}
			if !slices.Equal(seen[where], wantSeen) {
				t.Errorf("internal=%v %s saw %+v, want %+v", internalRefusal, where, seen[where], wantSeen)
			}
		}
		mu.Unlock()
	}
}

// 3: Refresh reports what it added and removed, by published and internal
// name, to OnChange as well, and every tool it leaves out is reported to
// OnSkip on every read, as well as logged.
func TestRefresh_CountsAndSkips(t *testing.T) {
	alice := catalogOf(t,
		&wireTool{name: "srv_write", desc: "Native write", params: map[string]any{}, run: wireText("native")},
		&wireExternal{wireTool{name: "mcp_srv_write", desc: "Upstream write", params: map[string]any{}, run: wireText("upstream")}, "srv_write"},
		&wireTool{name: "bad_schema", desc: "bad", params: map[string]any{"type": "object", "default": func() {}}, run: wireText("x")},
		&wireTool{name: "refused", desc: "refused", params: map[string]any{"type": "object", "properties": map[string]any{
			"blob": map[string]any{"type": "object", "x-mcp-header": "X-Blob"},
		}}, run: wireText("x")})
	var mu sync.Mutex
	var skips []Skip
	var changes []RefreshResult
	h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": alice}, func(c *Config) {
		c.Names = ExternalFirst
		c.OnSkip = func(s Skip) {
			mu.Lock()
			skips = append(skips, s)
			mu.Unlock()
		}
		c.OnChange = func(r RefreshResult) {
			mu.Lock()
			changes = append(changes, r)
			mu.Unlock()
		}
	})
	takeChanges := func() []RefreshResult {
		mu.Lock()
		defer mu.Unlock()
		out := changes
		changes = nil
		return out
	}
	take := func() []Skip {
		mu.Lock()
		defer mu.Unlock()
		out := skips
		skips = nil
		return out
	}
	check := func(when string, got []Skip) {
		t.Helper()
		// Two endpoints share OnSkip: each reports each skip once.
		var taken, schema, refused int
		for _, s := range got {
			switch {
			case s.Reason == SkipNameTaken && s.Internal == "srv_write" && s.Published == "srv_write" && s.Other == "mcp_srv_write":
				taken++
			case s.Reason == SkipSchema && s.Internal == "bad_schema" && s.Err != nil:
				schema++
			case s.Reason == SkipRefused && s.Internal == "refused" && s.Published == "refused":
				refused++
			default:
				t.Errorf("%s: unexpected skip %+v", when, s)
			}
		}
		if taken != 2 || schema != 2 || refused != 2 {
			t.Errorf("%s: skips taken=%d schema=%d refused=%d, want 2 each", when, taken, schema, refused)
		}
	}
	check("at New", take())
	// Each endpoint reported what New registered.
	atNew := RefreshResult{Changed: true, Added: []PublishedTool{{"srv_write", "mcp_srv_write"}}}
	if got := takeChanges(); len(got) != 2 || !reflect.DeepEqual(got[0], atNew) || !reflect.DeepEqual(got[1], atNew) {
		t.Errorf("OnChange at New = %+v, want %+v from each endpoint", got, atNew)
	}

	if res := h.arg.Refresh(context.Background()); !reflect.DeepEqual(res, RefreshResult{}) {
		t.Errorf("Refresh with nothing changed = %+v", res)
	}
	if got := takeChanges(); len(got) != 0 {
		t.Errorf("OnChange told of an unchanged Refresh: %+v", got)
	}
	got := take()
	if len(got) != 3 {
		t.Errorf("an unchanged Refresh reported %d skips, want each of the 3 again", len(got))
	}
	// One read reports its skips in order of internal name, whatever the reason.
	if len(got) == 3 && (got[0].Internal != "bad_schema" || got[1].Internal != "refused" || got[2].Internal != "srv_write") {
		t.Errorf("skips reported as %s, %s, %s; want bad_schema, refused, srv_write", got[0].Internal, got[1].Internal, got[2].Internal)
	}

	if err := alice.AddEntries(toolkit.Entry{Tool: textTool("added_one", "a")}, toolkit.Entry{Tool: textTool("added_two", "b")}); err != nil {
		t.Fatal(err)
	}
	added := RefreshResult{Changed: true, Added: []PublishedTool{{"added_one", "added_one"}, {"added_two", "added_two"}}}
	if res := h.arg.Refresh(context.Background()); !reflect.DeepEqual(res, added) {
		t.Errorf("Refresh after two adds = %+v", res)
	}
	if got := takeChanges(); len(got) != 1 || !reflect.DeepEqual(got[0], added) {
		t.Errorf("OnChange after two adds = %+v", got)
	}
	alice.Remove("added_one")
	if err := alice.Put(toolkit.Entry{Tool: textToolDesc("added_two", "changed")}); err != nil {
		t.Fatal(err)
	}
	changed := RefreshResult{
		Changed: true,
		Added:   []PublishedTool{{"added_two", "added_two"}},
		Removed: []PublishedTool{{"added_one", "added_one"}},
	}
	if res := h.arg.Refresh(context.Background()); !reflect.DeepEqual(res, changed) {
		t.Errorf("Refresh after a remove and a change = %+v", res)
	}
}

// OnSkip runs after a sync is complete: it may call back into the endpoint,
// and a panic in it changes nothing.
func TestOnSkip_MayCallBack(t *testing.T) {
	alice := catalogOf(t,
		&wireTool{name: "srv_write", desc: "Native write", params: map[string]any{}, run: wireText("native")},
		&wireExternal{wireTool{name: "mcp_srv_write", desc: "Upstream write", params: map[string]any{}, run: wireText("upstream")}, "srv_write"})
	var ep atomic.Pointer[Endpoint]
	var calls atomic.Int64
	h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": alice}, func(c *Config) {
		c.Names = ExternalFirst
		c.OnSkip = func(Skip) {
			if calls.Add(1) > 2 {
				if e := ep.Load(); e != nil {
					e.InFlight()
					_ = e.state.Load()
				}
				panic("host callback exploded")
			}
		}
	})
	ep.Store(h.arg)
	done := make(chan RefreshResult, 1)
	go func() { done <- h.arg.Refresh(context.Background()) }()
	select {
	case res := <-done:
		if res.Changed {
			t.Errorf("Refresh = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh blocked in OnSkip")
	}
	if r := callT(t, h.mustClient("/mcp", h.aliceTok), "srv_write", nil); r.text != "upstream" {
		t.Errorf("srv_write after a panicking OnSkip = %+v", r)
	}
}

// On Bearer a name the caller's own naming gives a disabled tool resolves
// to that tool, refused as disabled, with its internal name on the Target.
func TestTarget_BearerDisabledOwnTool(t *testing.T) {
	alice := catalogOf(t, &wireTool{name: "lookup", desc: "Alice", params: map[string]any{}, run: wireText("alice")})
	bob := catalogOf(t, &wireExternal{wireTool{name: "mcp_lookup", desc: "Bob", params: map[string]any{}, run: wireText("bob")}, "lookup"})
	var seen Target
	h := newPerAgent(t, map[string]*toolkit.Catalog{"alice": alice, "bob": bob}, func(c *Config) {
		c.Names = ExternalFirst
		c.Plugins = []hook.Plugin{{Name: "probe", BeforeTool: func(ctx context.Context, _ *hook.ToolCall) (*toolkit.Result, error) {
			seen, _ = TargetOf(ctx)
			return nil, nil //nolint:nilnil // (nil, nil): run the tool
		}}}
	})
	if err := h.sets["bob"].Disable("mcp_lookup"); err != nil {
		t.Fatal(err)
	}
	if r := callT(t, h.mustClient("/mcp", h.bobTok), "lookup", nil); !r.isError || r.text != `tool "mcp_lookup" is disabled` {
		t.Errorf("Bob's lookup = %+v", r)
	}
	if seen.Internal != "mcp_lookup" || !seen.Held {
		t.Errorf("target = %+v", seen)
	}
}
