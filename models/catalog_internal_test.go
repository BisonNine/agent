package models

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

func httpCatalog(t *testing.T, models ...modelspec.Model) *Catalog {
	t.Helper()
	c := New()
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://example.invalid/v1"}); err != nil {
		t.Fatal(err)
	}
	for i, m := range models {
		if err := c.Add(m, For(Chat, i)); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func builtClient(t *testing.T, c *Catalog, name string) spawnllm.LLMProvider {
	t.Helper()
	e := c.gen.Load().models[name]
	cl, release, err := e.slot.acquire()
	if err != nil {
		t.Fatal(err)
	}
	release()
	return cl
}

// A model whose definition and provider are unchanged keeps its client, and
// its connections, across Adopt; a changed one gets a new one.
func TestAdoptKeepsUnchangedClients(t *testing.T) {
	same := modelspec.Model{ModelName: "same", Model: "a", Provider: "p", ExtraBody: map[string]any{"vendor": map[string]any{"x": 1}}}
	changed := modelspec.Model{ModelName: "changed", Model: "b", Provider: "p"}
	c := httpCatalog(t, same, changed)
	keptBefore, changedBefore := builtClient(t, c, "same"), builtClient(t, c, "changed")

	changed.ReasoningEffort = "high"
	next := httpCatalog(t, cloneModel(same), changed)
	builtClient(t, next, "same") // a client next built for itself is dropped
	d := c.Adopt(next)
	if !slices.Equal(d.Changed, []string{"changed"}) {
		t.Fatalf("diff = %+v", d)
	}
	if builtClient(t, c, "same") != keptBefore {
		t.Error("the unchanged model's client was not kept")
	}
	if builtClient(t, c, "changed") == changedBefore {
		t.Error("the changed model kept its client")
	}
}

// swapSlot gives model name a slot that builds client.
func swapSlot(t *testing.T, c *Catalog, name string, client spawnllm.LLMProvider) *clientSlot {
	t.Helper()
	slot := newSlot(func() (spawnllm.LLMProvider, error) { return client, nil })
	if err := c.update(func(g *generation) ([]*clientSlot, error) {
		e := *g.models[name]
		e.slot = slot
		g.models[name] = &e
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	return slot
}

// The catalogue never closes a client the host registered; one it built is
// closed when replaced, once its last call has ended.
func TestRegisterClient(t *testing.T) {
	c := httpCatalog(t, modelspec.Model{ModelName: "h", Model: "h", Provider: "p"})
	built := &closeCounter{}
	swapSlot(t, c, "h", built)
	_, release, err := c.gen.Load().models["h"].slot.acquire()
	if err != nil {
		t.Fatal(err)
	}
	first := &closeCounter{}
	if err := c.RegisterClient("h", first); err != nil {
		t.Fatal(err)
	}
	if built.closed != 0 {
		t.Fatal("the built client was closed during its call")
	}
	release()
	if built.closed != 1 {
		t.Fatalf("the built client was closed %d times after its call, want 1", built.closed)
	}
	if builtClient(t, c, "h") != first {
		t.Fatal("the registered client is not used")
	}
	before := c.gen.Load()
	if err := c.RegisterClient("h", first); err != nil {
		t.Fatal(err)
	}
	if c.gen.Load() != before {
		t.Fatal("registering the same client again changed the catalogue")
	}
	if err := c.RegisterClient("h", &closeCounter{}); err != nil {
		t.Fatal(err)
	}
	if first.closed != 0 {
		t.Fatalf("a registered client was closed %d times", first.closed)
	}
}

// A client the catalogue built is closed by Adopt only once its last call in
// flight has ended.
func TestAdoptClosesBuiltClientAfterItsCall(t *testing.T) {
	c := httpCatalog(t, modelspec.Model{ModelName: "h", Model: "h", Provider: "p"})
	old := &closeCounter{}
	swapSlot(t, c, "h", old)
	_, release, err := c.gen.Load().models["h"].slot.acquire()
	if err != nil {
		t.Fatal(err)
	}
	c.Adopt(httpCatalog(t, modelspec.Model{ModelName: "h", Model: "h2", Provider: "p"}))
	if old.closed != 0 {
		t.Fatal("closed during its call")
	}
	release()
	if old.closed != 1 {
		t.Fatalf("closed %d times after its call, want 1", old.closed)
	}

	idle := &closeCounter{}
	c = httpCatalog(t, modelspec.Model{ModelName: "h", Model: "h", Provider: "p"})
	swapSlot(t, c, "h", idle)
	builtClient(t, c, "h")
	c.Adopt(httpCatalog(t, modelspec.Model{ModelName: "h", Model: "h2", Provider: "p"}))
	if idle.closed != 1 {
		t.Fatal("an idle built client was not closed at once")
	}
}

// An unchanged model keeps its built client only when the client options are
// the same; otherwise it is rebuilt and listed as changed. A client not yet
// built is not kept.
func TestAdoptClientOptions(t *testing.T) {
	m := modelspec.Model{ModelName: "h", Model: "h", Provider: "p", ResponseLogFile: "/var/log/h.log"}
	handler := &tagHandler{Handler: slog.DiscardHandler}
	mk := func(opts ...Option) *Catalog {
		c := New(opts...)
		if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://x"}); err != nil {
			t.Fatal(err)
		}
		if err := c.Add(m, For(Chat, 0)); err != nil {
			t.Fatal(err)
		}
		return c
	}
	modes := func(f fs.FileMode) Option { return WithFileModes(fsmode.Modes{File: f}) }
	cases := []struct {
		name    string
		opts    []Option
		kept    bool
		changed bool
	}{
		{"same options", []Option{WithLogger(slog.New(handler)), modes(0o600)}, true, false},
		{"another logger", []Option{WithLogger(slog.New(&tagHandler{Handler: slog.DiscardHandler})), modes(0o600)}, false, true},
		{"log content", []Option{WithLogger(slog.New(handler)), modes(0o600), WithLogContent(true)}, false, true},
		{"file modes", []Option{WithLogger(slog.New(handler)), modes(0o640)}, false, true},
		{"declined message", []Option{WithLogger(slog.New(handler)), modes(0o600), WithDeclinedMessage(func(Declined) string { return "x" })}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mk(WithLogger(slog.New(handler)), modes(0o600))
			before := builtClient(t, c, "h")
			d := c.Adopt(mk(tc.opts...))
			if kept := builtClient(t, c, "h") == before; kept != tc.kept {
				t.Errorf("client kept = %v, want %v", kept, tc.kept)
			}
			if changed := slices.Equal(d.Changed, []string{"h"}); changed != tc.changed {
				t.Errorf("diff = %+v", d)
			}
		})
	}
	c := mk()
	next := mk()
	nextSlot := next.gen.Load().models["h"].slot
	c.Adopt(next)
	if c.gen.Load().models["h"].slot != nextSlot {
		t.Error("an old slot with no client built was kept")
	}
}

// Select and Unassign at the same time never leave a selection of a model
// that is not assigned.
func TestSelectRacesUnassign(t *testing.T) {
	c := httpCatalog(t, modelspec.Model{ModelName: "a", Model: "a", Provider: "p"}, modelspec.Model{ModelName: "b", Model: "b", Provider: "p"})
	s := c.Default()
	k := selKey{purpose: Chat, scope: "s"}
	for range 2000 {
		done := make(chan struct{})
		go func() {
			defer close(done)
			discard(s.Select(Chat, "b", "s"))
		}()
		s.Unassign(Chat, "b")
		<-done
		c.state.mu.Lock()
		name := c.state.selected[k]
		c.state.mu.Unlock()
		if name == "b" {
			t.Fatal("a selection of an unassigned model was left")
		}
		if err := s.Assign(Chat, "b", 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloneModelDeep(t *testing.T) {
	n := 1
	m := modelspec.Model{ExtraBody: map[string]any{
		"list": []string{"x"}, "map": map[string]string{"k": "v"}, "ptr": &n,
		"nested": map[string]any{"any": []any{map[string]any{"d": 1}}},
	}}
	cp := cloneModel(m)
	list, okL := cp.ExtraBody["list"].([]string)
	mp, okM := cp.ExtraBody["map"].(map[string]string)
	ptr, okP := cp.ExtraBody["ptr"].(*int)
	if !okL || !okM || !okP {
		t.Fatalf("types were not kept: %#v", cp.ExtraBody)
	}
	list[0], mp["k"], *ptr = "changed", "changed", 2
	if nested, ok := cp.ExtraBody["nested"].(map[string]any); ok {
		if inner, ok := nested["any"].([]any); ok {
			if d, ok := inner[0].(map[string]any); ok {
				d["d"] = 2
			}
		}
	}
	if !reflect.DeepEqual(m.ExtraBody, map[string]any{
		"list": []string{"x"}, "map": map[string]string{"k": "v"}, "ptr": &n,
		"nested": map[string]any{"any": []any{map[string]any{"d": 1}}},
	}) || n != 1 {
		t.Fatalf("the copy shares data with the original: %#v, n=%d", m.ExtraBody, n)
	}
}

func TestIgnoredBypassNotice(t *testing.T) {
	m := modelspec.Model{ModelName: "cc", Model: "claude-cli", ExtraArgs: []string{"--dangerously-skip-permissions"}}
	prov := modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli"}
	var got []IgnoredBypass
	s := testSettings()
	s.bypassNotice = func(n IgnoredBypass) { got = append(got, n) }
	cliSpecFor(s, &m, &prov)
	want := []IgnoredBypass{{Model: "cc", Protocol: "claude-cli", Flags: []string{"--dangerously-skip-permissions"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %+v, want %+v", got, want)
	}
	var buf bytes.Buffer
	s = testSettings()
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	cliSpecFor(s, &m, &prov)
	if !strings.Contains(buf.String(), "ignoring permission-bypass flags") {
		t.Fatalf("default notice not logged: %q", buf.String())
	}
}

type closeCounter struct {
	answer

	closed int
}

func (c *closeCounter) Close() { c.closed++ }

// An isolated chain runs a CLI model in the workspace given, without the
// bypass flag the provider allows, and without the declined-tools guard; an
// HTTP model uses the shared client.
func TestIsolatedChain(t *testing.T) {
	cmd, out := fakeCLI(t)
	c := New(WithBaseEnv(func() []string { return []string{"PATH=" + os.Getenv("PATH")} }))
	if err := c.AddProvider(modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", Command: cmd, BypassRestrictions: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI", Env: map[string]string{"OUT": out}}, For(Chat, 0)); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	ch := c.Begin(Chat).Isolated(ws)
	discard(ch.Chat(context.Background(), alice, Request{Messages: []spawnllm.Message{{Role: "user", Content: "hi"}}}))
	args, err := os.ReadFile(filepath.Join(out, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--dangerously-skip-permissions") {
		t.Fatalf("the isolated CLI got the bypass flag: %q", args)
	}
	e := c.gen.Load().models["cc"]
	if e.slot.peek() != nil {
		t.Fatal("the isolated call built the shared client")
	}
	if _, isoErr := c.Begin(Chat).Isolated(" ").Chat(context.Background(), alice, Request{}); isoErr == nil {
		t.Fatal("an isolated call without a workspace was made")
	}
	cl, rel, err := c.Begin(Chat).client(Candidate{Name: "cc"})
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if _, guarded := cl.(*declinedGuard); guarded {
		t.Fatal("bypass on: the shared client is guarded")
	}
	iso, rel2, err := ch.client(Candidate{Name: "cc"})
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if _, ok := iso.(*spawnllm.ClaudeCliProvider); !ok {
		t.Fatalf("isolated client = %T, want the bare CLI client", iso)
	}
	if iso == cl {
		t.Fatal("the isolated client is the shared one")
	}

	h := httpCatalog(t, modelspec.Model{ModelName: "http", Model: "gpt", Provider: "p"})
	shared := builtClient(t, h, "http")
	got, rel3, err := h.Begin(Chat).Isolated("/tmp/ws").client(Candidate{Name: "http"})
	if err != nil {
		t.Fatal(err)
	}
	rel3()
	if got != shared {
		t.Fatal("an isolated chain did not use the shared HTTP client")
	}
}

// tagHandler is a handler distinct from every other.
type tagHandler struct{ slog.Handler }
