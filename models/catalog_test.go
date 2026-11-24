package models_test

import (
	"slices"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

func TestChainLimits(t *testing.T) {
	c := models.New(models.WithLimitDefaults(modelspec.LimitDefaults{MaxTokens: 1000, ContextWindow: 50000}))
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "big", Model: "big", Provider: "p", ContextWindow: 200000, ThinkingLevel: "high", NoTools: true}, models.For(models.Chat, 0)); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "small", Model: "small", Provider: "p"}, models.For(models.Chat, 1)); err != nil {
		t.Fatal(err)
	}
	l := c.Begin(models.Chat).Limits()
	if l.ContextWindow != 200000 || l.MaxTokens != 1000 || l.ThinkingLevel != modelspec.ThinkingHigh || !l.NoTools {
		t.Fatalf("limits = %+v", l)
	}
	if err := c.Select(models.Chat, "small"); err != nil {
		t.Fatal(err)
	}
	if l := c.Begin(models.Chat).Limits(); l.ContextWindow != 50000 {
		t.Fatalf("selected model's limits = %+v", l)
	}
	if l := c.Begin(models.Memory).Limits(); l.MaxTokens != 1000 {
		t.Fatalf("no models: %+v", l)
	}
}

func TestObserverEvents(t *testing.T) {
	f := newFixture(t, three)
	f.fake.Script("claude-sonnet", modelstest.Status(402, 0), modelstest.Reply("back"))
	ch := f.cat.Set("").Begin(models.Chat, "s")
	if _, err := ch.Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if len(f.obs.parked) != 1 || f.obs.parked[0].Name != "sonnet" || f.obs.parked[0].Key != "anthropic/claude-sonnet" ||
		f.obs.parked[0].Reason != spawnllm.FailoverBilling || f.obs.parked[0].Cooldown != 30*time.Minute {
		t.Fatalf("parked = %+v", f.obs.parked)
	}
	if len(f.obs.fellBack) != 1 || f.obs.fellBack[0].Next.Name != "gpt" || f.obs.fellBack[0].Scope != "s" || f.obs.fellBack[0].Purpose != models.Chat {
		t.Fatalf("fell back = %+v", f.obs.fellBack)
	}
	f.clock.Advance(30 * time.Minute)
	if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if len(f.obs.recovered) != 1 || f.obs.recovered[0].Name != "sonnet" || !f.obs.recovered[0].Since.Equal(epoch) {
		t.Fatalf("recovered = %+v", f.obs.recovered)
	}
	if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if len(f.obs.recovered) != 1 {
		t.Fatal("recovered sent for a model that was not parked")
	}
	f.fake.Script("claude-sonnet", modelstest.Status(500, 0))
	f.fake.Script("gpt-5", modelstest.Status(500, 0))
	f.fake.Script("claude-haiku", modelstest.Status(500, 0))
	if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); err == nil {
		t.Fatal("no error")
	}
	if len(f.obs.exhausted) != 1 || len(f.obs.exhausted[0].Attempts) != 3 {
		t.Fatalf("exhausted = %+v", f.obs.exhausted)
	}
}

// A reset leaves the model reported parked, so its next answer still says
// it recovered.
func TestResetHealth(t *testing.T) {
	f := newFixture(t, append(three, m("sonnet-2", "anthropic", "claude-sonnet")))
	f.park(t, "sonnet", 429)
	f.park(t, "gpt", 429)
	if h, _ := f.cat.Get("sonnet-2"); h.Health.Available {
		t.Fatal("a model sharing provider and wire model is not parked with it")
	}
	if n := f.cat.ResetHealth("sonnet", "sonnet-2"); n != 1 {
		t.Fatalf("reset %d records, want 1 (shared)", n)
	}
	if n := f.cat.ResetHealth("unknown"); n != 0 {
		t.Fatalf("reset of an unknown model = %d", n)
	}
	if h, _ := f.cat.Get("gpt"); h.Health.Available {
		t.Fatal("another model was reset")
	}
	if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if len(f.obs.recovered) != 1 {
		t.Fatalf("recovered = %+v", f.obs.recovered)
	}
	f.cat.ResetHealth()
	for _, h := range f.cat.Health() {
		if !h.Available {
			t.Fatalf("after a full reset: %+v", h)
		}
	}
	if got := f.cat.Health(); len(got) != 4 || got[0].Name != "gpt" {
		t.Fatalf("health is not every model by name: %+v", got)
	}
}

func TestCatalogValidation(t *testing.T) {
	c := models.New()
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "nope"}); err == nil {
		t.Error("an unknown protocol was accepted")
	}
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://x"}); err == nil {
		t.Error("a duplicate provider was accepted")
	}
	if err := c.Add(modelspec.Model{ModelName: "a", Model: "a", Provider: "q"}); err == nil {
		t.Error("a model on an unknown provider was accepted")
	}
	if err := c.Add(modelspec.Model{ModelName: "a", Model: "a", Provider: "p", ReasoningEffort: "extreme"}); err == nil {
		t.Error("an invalid model was accepted")
	}
	if err := c.Add(modelspec.Model{ModelName: "a", Model: "a", Provider: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "a", Model: "b", Provider: "p"}); err == nil {
		t.Error("a duplicate model was accepted")
	}
	if err := c.Remove("zzz"); err == nil {
		t.Error("removing an unknown model succeeded")
	}
	if err := c.RegisterClient("zzz", modelstest.New()); err == nil {
		t.Error("a client was registered for an unknown model")
	}
	info, _ := c.Get("a")
	if !info.Enabled || info.Provider.Name != "p" {
		t.Fatalf("info = %+v", info)
	}
	if got := c.Names(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("names = %q", got)
	}
}

// What the catalogue returns is a copy: changing it changes nothing.
func TestCatalogReturnsCopies(t *testing.T) {
	c := models.New()
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://x"}); err != nil {
		t.Fatal(err)
	}
	m := modelspec.Model{
		ModelName: "a", Model: "a", Provider: "p", ExtraArgs: []string{"x"}, Env: map[string]string{"k": "v"},
		ExtraBody: map[string]any{"vendor": map[string]any{"deep": 1}},
	}
	if err := c.Add(m, models.For(models.Chat, 0)); err != nil {
		t.Fatal(err)
	}
	m.ExtraArgs[0] = "changed"
	info, _ := c.Get("a")
	info.Model.Env["k"] = "changed"
	if inner, ok := info.Model.ExtraBody["vendor"].(map[string]any); ok {
		inner["deep"] = 2
	}
	again, _ := c.Get("a")
	inner, ok := again.Model.ExtraBody["vendor"].(map[string]any)
	if again.Model.ExtraArgs[0] != "x" || again.Model.Env["k"] != "v" || !ok || inner["deep"] != 1 {
		t.Fatalf("the catalogue's model was changed through a copy: %+v", again.Model)
	}
	ch := c.Begin(models.Chat)
	ch.Candidates()[0].Name = "changed"
	if ch.Candidates()[0].Name != "a" {
		t.Fatal("the chain was changed through a copy")
	}
}
