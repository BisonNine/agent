package toolkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/toolspec"
)

// --- mock types ---

type mockTool struct {
	name   string
	desc   string
	params map[string]any
	result *Result
}

func (m *mockTool) Name() string               { return m.name }
func (m *mockTool) Description() string        { return m.desc }
func (m *mockTool) Parameters() map[string]any { return m.params }
func (m *mockTool) Execute(_ context.Context, _ map[string]any) *Result {
	return m.result
}

type mockContextTool struct {
	mockTool

	mu      sync.Mutex
	lastCtx context.Context
}

func (m *mockContextTool) Execute(ctx context.Context, _ map[string]any) *Result {
	m.mu.Lock()
	m.lastCtx = ctx
	m.mu.Unlock()
	return m.result
}

type mockAsyncTool struct {
	mockTool

	lastCB AsyncCallback
}

func (m *mockAsyncTool) ExecuteAsync(_ context.Context, _ map[string]any, cb AsyncCallback) *Result {
	m.lastCB = cb
	return m.result
}

// mockExternalTool mimics an upstream MCP tool: registered under an
// internal "mcp_"-prefixed name, offered under a bare ExternalName.
type mockExternalTool struct {
	mockTool

	external string
}

func (m *mockExternalTool) ExternalName() string { return m.external }

type mockSessionTool struct{ mockTool }

func (*mockSessionTool) IsSessionScoped() bool { return true }

func newMockTool(name, desc string) *mockTool {
	return &mockTool{
		name:   name,
		desc:   desc,
		params: map[string]any{"type": "object"},
		result: SilentResult("ok"),
	}
}

func listed(names ...string) []Entry {
	out := make([]Entry, 0, len(names))
	for _, n := range names {
		out = append(out, Entry{Tool: newMockTool(n, n)})
	}
	return out
}

func infoNames(infos []Info) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Name)
	}
	return out
}

func mustAdd(t *testing.T, c *Catalog, entries ...Entry) {
	t.Helper()
	if err := c.AddEntries(entries...); err != nil {
		t.Fatalf("AddEntries: %v", err)
	}
}

// --- tests ported from the host registry ---

func TestNew_Empty(t *testing.T) {
	c := New()
	if len(c.Names()) != 0 || len(c.List()) != 0 || len(c.Snapshot().Names()) != 0 {
		t.Fatalf("new catalogue not empty: %v", c.Names())
	}
	if NewRegistry().Version() != 0 {
		t.Fatal("a new registry starts at version 0")
	}
}

func TestAddAndGet(t *testing.T) {
	c := New()
	mustAdd(t, c, Entry{Tool: newMockTool("echo", "echoes input")})
	got, ok := c.Get("echo")
	if !ok || got.Tool.Name() != "echo" || got.Published != "echo" || got.Hidden || !got.Enabled {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if _, ok := c.Get("nonexistent"); ok {
		t.Fatal("Get of an unknown tool must fail")
	}
}

// Add refuses a duplicate name, already registered or twice in one call,
// and changes nothing.
func TestAdd_RefusesDuplicate(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a")...)
	v := c.Version()
	if err := c.AddEntries(listed("b", "a")...); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("AddEntries with a registered name = %v, want ErrDuplicate", err)
	}
	if err := c.AddEntries(listed("c", "c")...); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("AddEntries with a name twice = %v, want ErrDuplicate", err)
	}
	def := toolspec.ToolDefinition{Name: "a"}
	if err := c.Add(def); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Add of a registered definition = %v, want ErrDuplicate", err)
	}
	if got := c.Names(); !slices.Equal(got, []string{"a"}) || c.Version() != v {
		t.Fatalf("a refused call changed the catalogue: %v, version %d → %d", got, v, c.Version())
	}
	if err := c.AddEntries(Entry{}); err == nil {
		t.Fatal("an entry with no tool must be refused")
	}
	if err := c.AddEntries(Entry{Tool: newMockTool("", "")}); err == nil {
		t.Fatal("a tool with no name must be refused")
	}
}

func TestAdd_FromSpec(t *testing.T) {
	c := New()
	defs := fakeSpecDefs()
	if err := c.Add(defs...); err != nil {
		t.Fatal(err)
	}
	if got := c.Names(); !slices.Equal(got, []string{"read", "watch"}) {
		t.Fatalf("Names = %v", got)
	}
	info, _ := c.Get("read")
	if info.Group != "files" || !info.RevealTogether || info.Hidden {
		t.Fatalf("read = %+v, want listed, group files, reveal together", info)
	}
}

// Put is the host's overwriting registration.
func TestPut_Overwrites(t *testing.T) {
	c := New()
	if err := c.Put(Entry{Tool: newMockTool("dup", "first")}); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(Entry{Tool: newMockTool("dup", "second")}); err != nil {
		t.Fatal(err)
	}
	if len(c.Names()) != 1 {
		t.Fatalf("expected one tool after overwrite, got %v", c.Names())
	}
	info, _ := c.Get("dup")
	if info.Tool.Description() != "second" {
		t.Fatalf("expected overwritten description 'second', got %q", info.Tool.Description())
	}
}

// A tool registered as mcp_<server>_<tool> is offered under its bare
// ExternalName, and a call by that name dispatches to it.
func TestExternalName_PublishedAndDispatched(t *testing.T) {
	c := New()
	mustAdd(t, c, Entry{Tool: &mockExternalTool{
		mockTool: *newMockTool("mcp_playwright_browser_navigate", "navigate"),
		external: "playwright_browser_navigate",
	}})
	info, _ := c.Get("mcp_playwright_browser_navigate")
	if info.Published != "playwright_browser_navigate" {
		t.Fatalf("Published = %q, want the bare name", info.Published)
	}
	if res := c.Execute(context.Background(), "playwright_browser_navigate", nil, nil); res.IsError {
		t.Fatalf("a bare-name call should dispatch, got %s", res.ForLLM)
	}
	looked, ok := c.Lookup("playwright_browser_navigate")
	if !ok || looked.Name != "mcp_playwright_browser_navigate" {
		t.Fatalf("Lookup = %+v, %v", looked, ok)
	}
	if _, ok := c.Get("playwright_browser_navigate"); ok {
		t.Fatal("Get takes the internal name only")
	}
}

// An ExternalName that would shadow another tool, or that two tools want,
// is not used.
func TestExternalName_NoShadowing(t *testing.T) {
	c := New()
	ext := func(name, external string) Entry {
		return Entry{Tool: &mockExternalTool{mockTool: *newMockTool(name, ""), external: external}}
	}
	mustAdd(t, c,
		Entry{Tool: newMockTool("search", "")},
		ext("mcp_alice_search", "search"),
		ext("mcp_alice_fetch", "fetch"),
		ext("mcp_bob_fetch", "fetch"),
		ext("mcp_bob_lookup", "lookup"),
	)
	want := map[string]string{
		"search":           "search",
		"mcp_alice_search": "mcp_alice_search",
		"mcp_alice_fetch":  "mcp_alice_fetch",
		"mcp_bob_fetch":    "mcp_bob_fetch",
		"mcp_bob_lookup":   "lookup",
	}
	for _, info := range c.List() {
		if info.Published != want[info.Name] {
			t.Errorf("%s published as %q, want %q", info.Name, info.Published, want[info.Name])
		}
	}
	// A call by an internal name always reaches that tool.
	if info, _ := c.Lookup("search"); info.Name != "search" {
		t.Fatalf("Lookup(search) = %s", info.Name)
	}
}

func TestExecute_Success(t *testing.T) {
	c := New()
	mustAdd(t, c, Entry{Tool: &mockTool{name: "greet", params: map[string]any{}, result: SilentResult("hello")}})
	res := c.Execute(context.Background(), "greet", nil, nil)
	if res.IsError || res.ForLLM != "hello" {
		t.Fatalf("result = %+v", res)
	}
}

func TestExecute_NotFound(t *testing.T) {
	res := New().Execute(context.Background(), "missing", nil, nil)
	if !res.IsError || res.ForLLM != `tool "missing" not found` || !errors.Is(res.Err, ErrToolNotFound) {
		t.Fatalf("result = %+v", res)
	}
}

// The context the caller sets reaches the tool.
func TestExecute_PassesContext(t *testing.T) {
	c := New()
	ct := &mockContextTool{mockTool: *newMockTool("ctx_tool", "needs context")}
	mustAdd(t, c, Entry{Tool: ct})
	ctx := WithChat(context.Background(), "web", "chat-42")
	c.Execute(ctx, "ctx_tool", nil, nil)
	if ct.lastCtx == nil || Channel(ct.lastCtx) != "web" || ChatID(ct.lastCtx) != "chat-42" {
		t.Fatalf("context not passed: %v", ct.lastCtx)
	}
}

func TestExecute_AsyncCallback(t *testing.T) {
	c := New()
	at := &mockAsyncTool{mockTool: *newMockTool("async_tool", "async work")}
	at.result = AsyncResult("started")
	mustAdd(t, c, Entry{Tool: at})

	called := false
	cb := func(_ context.Context, _ *Result) { called = true }
	res := c.Execute(context.Background(), "async_tool", nil, cb)
	if at.lastCB == nil || !res.Async {
		t.Fatalf("ExecuteAsync not used: %+v", res)
	}
	at.lastCB(context.Background(), SilentResult("done"))
	if !called {
		t.Fatal("expected the callback to be invoked")
	}
	// Without a callback the tool runs synchronously.
	at.lastCB = nil
	c.Execute(context.Background(), "async_tool", nil, nil)
	if at.lastCB != nil {
		t.Fatal("ExecuteAsync must not be used without a callback")
	}
}

// A tool that returns nil gives an error result, not a panic.
func TestExecute_NilResult(t *testing.T) {
	c := New()
	mustAdd(t, c, Entry{Tool: &mockTool{name: "nil_tool"}})
	res := c.Execute(context.Background(), "nil_tool", nil, nil)
	if res == nil || !res.IsError || res.ForLLM != "tool returned no result" {
		t.Fatalf("result = %+v", res)
	}
}

func TestToolToSchema(t *testing.T) {
	schema := ToolToSchema(newMockTool("demo", "demo tool"))
	if schema["type"] != "function" {
		t.Errorf("expected type 'function', got %v", schema["type"])
	}
	fn, ok := schema["function"].(map[string]any)
	if !ok {
		t.Fatal("expected 'function' to be a map")
	}
	if fn["name"] != "demo" || fn["description"] != "demo tool" || fn["parameters"] == nil {
		t.Errorf("function = %v", fn)
	}
}

// Removing by prefix: every entry with the prefix goes, listed or hidden,
// the version moves, unrelated tools stay; a removal of nothing does not
// move the version.
func TestReplace_RemoveByPrefix(t *testing.T) {
	c := New()
	mustAdd(t, c,
		Entry{Tool: &mockTool{name: "mcp_alice_search"}},
		Entry{Tool: &mockTool{name: "mcp_alice_fetch"}, Hidden: true},
		Entry{Tool: &mockTool{name: "mcp_bob_search"}},
		Entry{Tool: &mockTool{name: "file_read"}},
	)
	before := c.Version()
	if added, removed := c.ReplaceEntries("mcp_alice_", nil); added != 0 || removed != 2 {
		t.Fatalf("ReplaceEntries = %d, %d, want 0, 2", added, removed)
	}
	if c.Version() == before {
		t.Fatal("a removal must bump the version")
	}
	if got := c.Names(); !slices.Equal(got, []string{"file_read", "mcp_bob_search"}) {
		t.Fatalf("Names = %v", got)
	}
	after := c.Version()
	if _, removed := c.ReplaceEntries("mcp_alice_", nil); removed != 0 || c.Version() != after {
		t.Fatal("a removal that removed nothing must not bump the version")
	}
}

// The prefix's old tools are swapped for the new entries (listed and
// hidden kept as given), unrelated tools stay, and the version moves.
func TestReplaceEntries(t *testing.T) {
	c := New()
	mustAdd(t, c,
		Entry{Tool: &mockTool{name: "mcp_alice_search"}},
		Entry{Tool: &mockTool{name: "mcp_alice_fetch"}, Hidden: true},
		Entry{Tool: &mockTool{name: "mcp_bob_search"}},
	)
	before := c.Version()
	added, removed := c.ReplaceEntries("mcp_alice_", []Entry{
		{Tool: &mockTool{name: "mcp_alice_search"}},
		{Tool: &mockTool{name: "mcp_alice_lookup"}, Hidden: true, Group: "alice", RevealTogether: true},
		{}, // skipped
	})
	if added != 2 || removed != 2 {
		t.Fatalf("ReplaceEntries = %d, %d, want 2, 2", added, removed)
	}
	if c.Version() == before {
		t.Fatal("ReplaceEntries must bump the version")
	}
	want := []string{"mcp_alice_lookup", "mcp_alice_search", "mcp_bob_search"}
	if got := c.Names(); !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if got := infoNames(c.Hidden()); !slices.Equal(got, []string{"mcp_alice_lookup"}) {
		t.Fatalf("hidden = %v", got)
	}
	lookup, _ := c.Get("mcp_alice_lookup")
	if lookup.Group != "alice" || !lookup.RevealTogether {
		t.Fatalf("lookup = %+v", lookup)
	}

	c.ReplaceEntries("mcp_carol_", nil)
	after := c.Version()
	if a, r := c.ReplaceEntries("mcp_carol_", nil); a != 0 || r != 0 || c.Version() != after {
		t.Fatal("an empty replacement must change nothing and leave the version alone")
	}
}

func TestReplace_FromSpec(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("x_old")...)
	added, removed := c.Replace("x_", []toolspec.ToolDefinition{{Name: "x_new"}})
	if added != 1 || removed != 1 || !slices.Equal(c.Names(), []string{"x_new"}) {
		t.Fatalf("Replace = %d, %d; names %v", added, removed, c.Names())
	}
}

func TestRemove(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a", "b", "c")...)
	if n := c.Remove("a", "c", "zzz"); n != 2 {
		t.Fatalf("Remove = %d, want 2", n)
	}
	v := c.Version()
	if n := c.Remove("zzz"); n != 0 || c.Version() != v {
		t.Fatal("removing nothing must not bump the version")
	}
	if !slices.Equal(c.Names(), []string{"b"}) {
		t.Fatalf("Names = %v", c.Names())
	}
}

func TestExecuteUnattended(t *testing.T) {
	c := New()
	failing := newMockTool("bad_tool", "bad")
	failing.result = ErrorResult("the mailbox is locked")
	mustAdd(t, c,
		Entry{Tool: newMockTool("ok_tool", "ok")},
		Entry{Tool: failing},
		Entry{Tool: &mockSessionTool{mockTool: *newMockTool("session_tool", "s")}},
	)
	out, err := c.ExecuteUnattended(context.Background(), "ok_tool", nil)
	if err != nil || out != "ok" {
		t.Errorf("ok_tool = %q, %v", out, err)
	}
	if _, err = c.ExecuteUnattended(context.Background(), "missing", nil); !errors.Is(err, ErrToolNotRegistered) {
		t.Errorf("missing = %v, want ErrToolNotRegistered", err)
	}
	if _, err = c.ExecuteUnattended(context.Background(), "session_tool", nil); !errors.Is(err, ErrToolSessionScoped) {
		t.Errorf("session_tool = %v, want ErrToolSessionScoped", err)
	}
	_, err = c.ExecuteUnattended(context.Background(), "bad_tool", nil)
	if err == nil || !strings.Contains(err.Error(), `tool "bad_tool" failed: the mailbox is locked`) {
		t.Errorf("bad_tool = %v", err)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger and a test to share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An expected refusal is logged as a warning; a genuine failure as an
// error. Records carry component=toolkit.
func TestExecute_RefusalLoggedAtWarn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    *Result
		wantLevel string
		wantMsg   string
	}{
		{"refusal", ErrorResult("You may not message Bob.").WithError(Refusal(errors.New("not allowed"))), `"level":"WARN"`, "Tool call refused"},
		{"failure", ErrorResult("disk full").WithError(errors.New("disk full")), `"level":"ERROR"`, "Tool execution failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			c := New(WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
			mustAdd(t, c, Entry{Tool: &mockTool{name: "probe", params: map[string]any{}, result: tc.result}})
			c.Execute(context.Background(), "probe", map[string]any{"secret": "s3cret"}, nil)

			var line string
			for l := range strings.SplitSeq(buf.String(), "\n") {
				if strings.Contains(l, tc.wantMsg) {
					line = l
				}
			}
			if line == "" || !strings.Contains(line, tc.wantLevel) || !strings.Contains(line, `"component":"toolkit"`) {
				t.Fatalf("want %q at %s, got log:\n%s", tc.wantMsg, tc.wantLevel, buf.String())
			}
			if strings.Contains(buf.String(), "s3cret") {
				t.Fatal("tool arguments must never be logged")
			}
		})
	}
}

// The error of a failed call is logged through the catalogue's truncate
// function: by default cut to 500 runes plus "…", in full with an
// identity function, which receives the default limit.
func TestExecute_LogTruncate(t *testing.T) {
	long := strings.Repeat("x", 600)
	var gotMax int
	for _, tc := range []struct {
		name string
		opt  Option
		want string
	}{
		{"default", nil, strings.Repeat("x", 500) + "…"},
		{"nil", WithTruncate(nil), strings.Repeat("x", 500) + "…"},
		{"identity", WithTruncate(func(s string, limit int) string { gotMax = limit; return s }), long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			opts := []Option{WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))}
			if tc.opt != nil {
				opts = append(opts, tc.opt)
			}
			c := New(opts...)
			mustAdd(t, c, Entry{Tool: &mockTool{name: "probe", params: map[string]any{}, result: ErrorResult(long)}})
			c.Execute(context.Background(), "probe", nil, nil)

			var rec struct {
				Error string `json:"error"`
			}
			for l := range strings.SplitSeq(buf.String(), "\n") {
				if strings.Contains(l, "Tool execution failed") {
					if err := json.Unmarshal([]byte(l), &rec); err != nil {
						t.Fatal(err)
					}
				}
			}
			if rec.Error != tc.want {
				t.Fatalf("logged error has %d runes, want %d", len([]rune(rec.Error)), len([]rune(tc.want)))
			}
		})
	}
	if gotMax != 500 {
		t.Fatalf("truncate got max %d, want 500", gotMax)
	}
}

// Overwriting a tool warns with the host's text for a hidden or a listed
// registration.
func TestPut_OverwriteWarning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hidden bool
		want   string
	}{
		{"listed", false, "Tool registration overwrites existing tool"},
		{"hidden", true, "Hidden tool registration overwrites existing tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf syncBuffer
			c := New(WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
			for range 2 {
				if err := c.Put(Entry{Tool: newMockTool("dup", "d"), Hidden: tc.hidden}); err != nil {
					t.Fatal(err)
				}
			}
			var rec struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
				Name  string `json:"name"`
			}
			for l := range strings.SplitSeq(buf.String(), "\n") {
				if strings.Contains(l, "overwrites existing tool") {
					if err := json.Unmarshal([]byte(l), &rec); err != nil {
						t.Fatal(err)
					}
				}
			}
			if rec.Msg != tc.want || rec.Level != "WARN" || rec.Name != "dup" {
				t.Fatalf("got %+v, want WARN %q name=dup; log:\n%s", rec, tc.want, buf.String())
			}
		})
	}
}
