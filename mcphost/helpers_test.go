package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
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

// fnTool is a tool whose behaviour is a function.
type fnTool struct {
	name   string
	desc   string
	params map[string]any
	fn     func(ctx context.Context, args map[string]any) *toolkit.Result
}

func (t *fnTool) Name() string               { return t.name }
func (t *fnTool) Description() string        { return t.desc }
func (t *fnTool) Parameters() map[string]any { return t.params }
func (t *fnTool) Execute(ctx context.Context, args map[string]any) *toolkit.Result {
	return t.fn(ctx, args)
}

// asyncTool finishes in the background and reports through its callback.
type asyncTool struct {
	fnTool
}

func (t *asyncTool) ExecuteAsync(ctx context.Context, args map[string]any, cb toolkit.AsyncCallback) *toolkit.Result {
	go func() {
		// Uses the context it was given: it must outlive the request.
		time.Sleep(50 * time.Millisecond)
		if err := ctx.Err(); err != nil {
			cb(ctx, toolkit.ErrorResult("context ended: "+err.Error()))
			return
		}
		cb(ctx, toolkit.NewResult("finished "+fmt.Sprint(args["job"])))
	}()
	return toolkit.AsyncResult("started")
}

func textTool(name, text string) *fnTool {
	return &fnTool{
		name:   name,
		desc:   name + " tool",
		params: map[string]any{"type": "object", "properties": map[string]any{}},
		fn:     func(context.Context, map[string]any) *toolkit.Result { return toolkit.NewResult(text) },
	}
}

// grant is an AllowChecker over a fixed list of names.
type grant []string

func (g grant) IsToolAllowed(name string) bool { return slices.Contains(g, name) }

// fixture is a catalogue served on an Argument endpoint (/internal) and a
// Bearer endpoint (/mcp), with Alice and Bob as callers.
type fixture struct {
	t        *testing.T
	cat      *toolkit.Catalog
	store    *Store
	sets     map[string]*toolkit.Set
	arg      *Endpoint
	bearer   *Endpoint
	srv      *httptest.Server
	aliceTok string
	bobTok   string
	started  chan string // the slow tool reports its agent here when it starts
	slowDone chan error  // and the cause of its context's end here
}

// fixtureConfig adjusts both endpoints' configuration before they are built.
type fixtureConfig func(*Config)

func newFixture(t *testing.T, opts ...fixtureConfig) *fixture {
	t.Helper()
	f := &fixture{
		t:        t,
		cat:      toolkit.New(),
		store:    NewStore(),
		sets:     map[string]*toolkit.Set{},
		started:  make(chan string, 8),
		slowDone: make(chan error, 8),
	}
	whoami := &fnTool{
		name: "whoami", desc: "who is calling",
		params: map[string]any{"type": "object", "properties": map[string]any{}},
		fn: func(ctx context.Context, _ map[string]any) *toolkit.Result {
			s, _ := hook.ScopeOf(ctx)
			return toolkit.NewResult(fmt.Sprintf("agent=%s session=%s turn=%s depth=%d key=%s",
				toolkit.AgentID(ctx), s.Session, s.TurnID, s.Depth, toolkit.SessionKey(ctx)))
		},
	}
	echo := &fnTool{
		name: "echo", desc: "echo the arguments",
		params: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
		fn: func(_ context.Context, args map[string]any) *toolkit.Result {
			keys := make([]string, 0, len(args))
			for k := range args {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			return toolkit.UserResult(fmt.Sprintf("keys=%s text=%v", strings.Join(keys, ","), args["text"]))
		},
	}
	fail := &fnTool{
		name: "fail", desc: "always fails", params: nil,
		fn: func(context.Context, map[string]any) *toolkit.Result { return toolkit.ErrorResult("boom") },
	}
	slow := &fnTool{
		name: "slow", desc: "runs until cancelled", params: map[string]any{"type": "object"},
		fn: func(ctx context.Context, _ map[string]any) *toolkit.Result {
			f.started <- toolkit.AgentID(ctx)
			select {
			case <-ctx.Done():
				f.slowDone <- context.Cause(ctx)
				return toolkit.ErrorResult("cancelled")
			case <-time.After(10 * time.Second):
				f.slowDone <- nil
				return toolkit.NewResult("slow finished")
			}
		},
	}
	panicky := &fnTool{
		name: "panicky", desc: "panics", params: map[string]any{"type": "object"},
		fn: func(context.Context, map[string]any) *toolkit.Result { panic("tool exploded") },
	}
	job := &asyncTool{fnTool: *textTool("job", "sync job")}
	entries := []toolkit.Entry{
		{Tool: whoami},
		{Tool: echo},
		{Tool: fail},
		{Tool: slow},
		{Tool: panicky},
		{Tool: job},
		{Tool: textTool("secret", "alice only")},
		{Tool: textTool("gated", "passed the gate")},
		{Tool: textTool("deep", "a hidden tool"), Hidden: true},
	}
	if err := f.cat.AddEntries(entries...); err != nil {
		t.Fatal(err)
	}
	all := grant{"whoami", "echo", "fail", "slow", "panicky", "job", "secret", "gated", "deep", "later"}
	f.sets["alice"] = f.cat.Set("alice")
	f.sets["alice"].Grant(all)
	f.sets["bob"] = f.cat.Set("bob")
	f.sets["bob"].Grant(grant{"whoami", "echo", "slow", "gated"})

	var err error
	f.aliceTok, err = f.store.Issue(Grant{Scope: hook.Scope{AgentID: "alice", Session: "s-alice", TurnID: "t1", Depth: 1}})
	if err != nil {
		t.Fatal(err)
	}
	f.bobTok, err = f.store.Issue(Grant{Scope: hook.Scope{AgentID: "bob", Session: "s-bob", TurnID: "t2"}})
	if err != nil {
		t.Fatal(err)
	}

	auth := f.store.Authenticator(func(s hook.Scope) (*toolkit.Set, bool) {
		set, ok := f.sets[s.AgentID]
		return set, ok
	}, "invalid or missing session_token")
	build := func(tr Transport) *Endpoint {
		cfg := Config{
			Name: "test-host", Version: "1.2.3", Transport: tr,
			Authenticator: auth, Catalogue: FromRegistry(f.cat), Realm: "test-realm",
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
	f.arg = build(Argument)
	f.bearer = build(Bearer)
	mux := http.NewServeMux()
	mux.Handle("/internal", f.arg)
	mux.Handle("/mcp", f.bearer)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client connects to path, sending a bearer token when token is not empty.
func (f *fixture) client(path, token string, opts ...transport.StreamableHTTPCOption) (*client.Client, error) {
	f.t.Helper()
	if token != "" {
		opts = append(opts, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	}
	opts = append(opts, transport.WithHTTPLogger(slog.New(slog.DiscardHandler)))
	tr, err := transport.NewStreamableHTTP(f.srv.URL+path, opts...)
	if err != nil {
		return nil, err
	}
	c := client.NewClient(tr, client.WithProtocolVersion(mcp.ProtocolVersion20250618))
	// Start's context lives as long as the client: it carries the listening
	// stream.
	if err := c.Start(context.Background()); err != nil {
		return nil, err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.ProtocolVersion20250618
	init.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Initialize(ctx, init); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	f.t.Cleanup(func() {
		if err := c.Close(); err != nil {
			f.t.Logf("close client: %v", err)
		}
	})
	return c, nil
}

func (f *fixture) mustClient(path, token string, opts ...transport.StreamableHTTPCOption) *client.Client {
	f.t.Helper()
	c, err := f.client(path, token, opts...)
	if err != nil {
		f.t.Fatalf("connect %s: %v", path, err)
	}
	return c
}

// list returns the tools listed by c, by name.
func list(t *testing.T, c *client.Client) map[string]mcp.Tool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]mcp.Tool, len(res.Tools))
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

func names(tools map[string]mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for n := range tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// callResult is a tools/call outcome: the text, the error flag, or the
// protocol error.
type callResult struct {
	text    string
	isError bool
	err     error
}

func call(ctx context.Context, c *client.Client, name string, args map[string]any) callResult {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err != nil {
		return callResult{err: err}
	}
	var b strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return callResult{text: b.String(), isError: res.IsError}
}

func callT(t *testing.T, c *client.Client, name string, args map[string]any) callResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return call(ctx, c, name, args)
}

// schemaOf decodes a listed tool's input schema.
func schemaOf(t *testing.T, tl mcp.Tool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		InputSchema map[string]any `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.InputSchema
}

func requiredOf(schema map[string]any) []string {
	return requiredNames(schema["required"])
}

// logBuffer is a concurrency-safe buffer for a captured log.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newLogger(b *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func textToolDesc(name, desc string) *fnTool {
	t := textTool(name, name)
	t.desc = desc
	return t
}

func toolkitEntry(t toolkit.Tool) toolkit.Entry { return toolkit.Entry{Tool: t} }
