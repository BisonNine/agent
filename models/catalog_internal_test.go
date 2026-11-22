package models

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestRegisterClientReplacesAndClosesOld(t *testing.T) {
	c := httpCatalog(t, modelspec.Model{ModelName: "h", Model: "h", Provider: "p"})
	first := &closeCounter{}
	if err := c.RegisterClient("h", first); err != nil {
		t.Fatal(err)
	}
	if builtClient(t, c, "h") != first {
		t.Fatal("the registered client is not used")
	}
	if err := c.RegisterClient("h", &closeCounter{}); err != nil {
		t.Fatal(err)
	}
	if first.closed != 1 {
		t.Fatalf("the replaced client was closed %d times", first.closed)
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
