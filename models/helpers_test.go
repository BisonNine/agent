package models_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

var (
	epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	alice = accounting.Identity{Agent: "alice", Name: "Alice"}
)

// spec is a test model: its name, its provider and its wire model id.
type spec struct{ name, provider, model string }

func m(name, provider, model string) spec { return spec{name, provider, model} }

// fixture is a catalogue whose models all answer from one scripted client.
type fixture struct {
	cat   *models.Catalog
	fake  *modelstest.Provider
	clock *clock.Fake
	obs   *recorder
}

// newFixture builds a catalogue with the models, assigned to the default
// set's Chat in the order given.
func newFixture(t *testing.T, specs []spec, opts ...models.Option) *fixture {
	t.Helper()
	f := &fixture{fake: modelstest.New(), clock: clock.NewFake(epoch), obs: &recorder{}}
	opts = append([]models.Option{models.WithClock(f.clock), models.WithObserver(f.obs)}, opts...)
	f.cat = build(t, f.fake, specs, opts...)
	return f
}

// build makes a catalogue of specs, each with client fake, assigned to the
// default set's Chat in order.
func build(t *testing.T, fake *modelstest.Provider, specs []spec, opts ...models.Option) *models.Catalog {
	t.Helper()
	c := models.New(opts...)
	seen := map[string]bool{}
	for i, s := range specs {
		if !seen[s.provider] {
			seen[s.provider] = true
			if err := c.AddProvider(modelspec.Provider{Name: s.provider, Protocol: "openai-chat", BaseURL: "https://example.invalid/v1"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Add(modelspec.Model{ModelName: s.name, Model: s.model, Provider: s.provider}, models.For(models.Chat, i)); err != nil {
			t.Fatal(err)
		}
		if fake != nil {
			if err := c.RegisterClient(s.name, fake); err != nil {
				t.Fatal(err)
			}
		}
	}
	return c
}

// park makes the model fail once with status, the other models switched off
// for the call, which parks it; the call is forgotten.
func (f *fixture) park(t *testing.T, name string, status int) {
	t.Helper()
	info, ok := f.cat.Get(name)
	if !ok {
		t.Fatalf("no model %q", name)
	}
	var others []string
	for _, n := range f.cat.Names() {
		if n != name && f.cat.Enabled(n) {
			others = append(others, n)
		}
	}
	if err := f.cat.Disable(others...); err != nil {
		t.Fatal(err)
	}
	f.fake.Script(info.Model.Model, modelstest.Status(status, 0))
	discard(f.cat.Begin(models.Chat).Chat(context.Background(), alice, models.Request{}))
	f.fake.Script(info.Model.Model)
	if err := f.cat.Enable(others...); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.cat.Get(name); h.Health.Available {
		t.Fatalf("%s is not parked after status %d", name, status)
	}
	f.fake.Reset()
	f.obs.reset()
}

// recorder is an Observer that keeps every event.
type recorder struct {
	mu        sync.Mutex
	parked    []models.Parked
	recovered []models.Recovered
	fellBack  []models.FellBack
	exhausted []models.Exhausted
}

func (r *recorder) Parked(e models.Parked) {
	r.mu.Lock()
	r.parked = append(r.parked, e)
	r.mu.Unlock()
}

func (r *recorder) Recovered(e models.Recovered) {
	r.mu.Lock()
	r.recovered = append(r.recovered, e)
	r.mu.Unlock()
}

func (r *recorder) FellBack(e models.FellBack) {
	r.mu.Lock()
	r.fellBack = append(r.fellBack, e)
	r.mu.Unlock()
}

func (r *recorder) Exhausted(e models.Exhausted) {
	r.mu.Lock()
	r.exhausted = append(r.exhausted, e)
	r.mu.Unlock()
}

func (r *recorder) counts() (parked, recovered, fellBack, exhausted int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.parked), len(r.recovered), len(r.fellBack), len(r.exhausted)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parked, r.recovered, r.fellBack, r.exhausted = nil, nil, nil, nil
}

// discard drops results a test does not look at.
func discard(...any) {}

// answeredBy makes one call on ch and returns the model that answered.
func answeredBy(t *testing.T, ch *models.Chain) string {
	t.Helper()
	res, err := ch.Chat(t.Context(), alice, models.Request{})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	return res.Candidate.Name
}

func names(list []models.Entry) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Name)
	}
	return out
}

func candNames(ch *models.Chain) []string {
	cands := ch.Candidates()
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Name)
	}
	return out
}

var three = []spec{m("sonnet", "anthropic", "claude-sonnet"), m("gpt", "openai", "gpt-5"), m("haiku", "anthropic", "claude-haiku")}
