/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// The agents' handle does not change: a set taken before Adopt sees the new
// contents.
func TestAdoptKeepsHandles(t *testing.T) {
	f := newFixture(t, three)
	bob := f.cat.Set("bob")
	if err := bob.Assign(models.Chat, "gpt", 0); err != nil {
		t.Fatal(err)
	}
	next := build(t, f.fake, three)
	if err := next.Set("bob").Assign(models.Chat, "haiku", 0); err != nil {
		t.Fatal(err)
	}
	f.cat.Adopt(next)
	if got := names(bob.List(models.Chat)); !slices.Equal(got, []string{"haiku"}) {
		t.Fatalf("bob after adopt = %q", got)
	}
	if f.cat.Set("bob") != bob {
		t.Fatal("the handle changed")
	}
	if err := next.AddProvider(modelspec.Provider{Name: "z", Protocol: "openai-chat", BaseURL: "https://x"}); !errors.Is(err, models.ErrAdopted) {
		t.Fatalf("the adopted catalogue is still usable: %v", err)
	}
	if err := next.Select(models.Chat, "sonnet"); !errors.Is(err, models.ErrAdopted) {
		t.Fatalf("select on the adopted catalogue: %v", err)
	}
	if d := f.cat.Adopt(next); d.Added != nil || d.Removed != nil {
		t.Fatal("a spent catalogue was adopted twice")
	}
}

func TestAdoptDiff(t *testing.T) {
	f := newFixture(t, three)
	nextSpecs := []spec{m("sonnet", "anthropic", "claude-sonnet"), m("gpt", "openai", "gpt-6"), m("opus", "anthropic", "claude-opus")}
	d := f.cat.Adopt(build(t, nil, nextSpecs))
	if !slices.Equal(d.Added, []string{"opus"}) || !slices.Equal(d.Removed, []string{"haiku"}) || !slices.Equal(d.Changed, []string{"gpt"}) {
		t.Fatalf("diff = %+v", d)
	}
	if got := f.cat.Names(); !slices.Equal(got, []string{"sonnet", "gpt", "opus"}) {
		t.Fatalf("names = %q", got)
	}
}

// A turn running when Adopt is called finishes on the chain it pinned, old
// client included; the next turn uses the new contents.
func TestAdoptInFlightTurnFinishesOnPinned(t *testing.T) {
	f := newFixture(t, three)
	pinned := f.cat.Begin(models.Chat)
	newer := modelstest.New()
	next := build(t, newer, []spec{m("sonnet", "anthropic", "claude-sonnet-new"), m("gpt", "openai", "gpt-5")})
	f.cat.Adopt(next)
	res, err := pinned.Chat(t.Context(), alice, models.Request{})
	if err != nil || res.Candidate.Model != "claude-sonnet" {
		t.Fatalf("pinned: %v %+v", err, res)
	}
	if len(f.fake.Calls()) != 1 || len(newer.Calls()) != 0 {
		t.Fatal("the pinned turn did not use its own client")
	}
	if got := candNames(pinned); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("the pinned chain changed: %q", got)
	}
	res, err = f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if err != nil || res.Candidate.Model != "claude-sonnet-new" || len(newer.Calls()) != 1 {
		t.Fatalf("next turn: %v %+v", err, res)
	}
}

// Selections are carried by model name; one whose model is no longer assigned
// reverts to the default and is listed.
func TestAdoptCarriesSelections(t *testing.T) {
	f := newFixture(t, three)
	if err := f.cat.Select(models.Chat, "haiku", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.Select(models.Chat, "gpt", "s2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cat.Begin(models.Chat, "s1").Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	d := f.cat.Adopt(build(t, f.fake, []spec{three[0], three[2]}))
	if got := f.cat.Selected(models.Chat, "s1").Name; got != "haiku" {
		t.Fatalf("s1 = %q, want the carried selection", got)
	}
	if got := f.cat.Selected(models.Chat, "s2").Name; got != "sonnet" {
		t.Fatalf("s2 = %q, want the default", got)
	}
	want := []models.Reverted{{Set: "", Purpose: models.Chat, Scope: "s2", Model: "gpt"}}
	if !slices.Equal(d.Reverted, want) {
		t.Fatalf("reverted = %+v, want %+v", d.Reverted, want)
	}
	if cur := f.cat.Current(models.Chat, "s1"); cur.Serving != "haiku" {
		t.Fatalf("serving now was not carried: %+v", cur)
	}
}

// Cooldowns are not carried: every model starts available. A model reported
// parked is still reported recovered when it answers.
func TestAdoptClearsCooldowns(t *testing.T) {
	f := newFixture(t, three)
	f.park(t, "sonnet", 402)
	f.cat.Adopt(build(t, f.fake, three, models.WithClock(f.clock), models.WithObserver(f.obs)))
	if h, _ := f.cat.Get("sonnet"); !h.Health.Available || h.Health.Failures != 0 {
		t.Fatalf("health after adopt = %+v", h.Health)
	}
	res, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if err != nil || res.Candidate.Name != "sonnet" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(f.obs.recovered) != 1 {
		t.Fatalf("recovered = %+v", f.obs.recovered)
	}
}

// A failure reported by a turn still on the old contents is dropped with
// them.
func TestAdoptDropsOldFailures(t *testing.T) {
	f := newFixture(t, three)
	pinned := f.cat.Begin(models.Chat)
	f.cat.Adopt(build(t, f.fake, three, models.WithClock(f.clock)))
	f.fake.Script("claude-sonnet", modelstest.Status(402, 0))
	if _, err := pinned.Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.cat.Get("sonnet"); !h.Health.Available {
		t.Fatal("an old turn's failure parked the model in the new contents")
	}
}

// A client the host registered is the host's: Adopt never closes it, whether
// next registers the same one or another, and a call in flight on it
// finishes.
func TestAdoptNeverClosesRegisteredClients(t *testing.T) {
	shared := modelstest.New()
	c := build(t, shared, []spec{m("sonnet", "anthropic", "claude-sonnet")})
	release, started := make(chan struct{}), make(chan struct{})
	shared.Script("claude-sonnet", modelstest.Step{Hold: release, Started: started, Response: modelstest.Reply("late").Response}, modelstest.Reply("ok"))
	done := make(chan error, 1)
	go func() {
		_, err := c.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
		done <- err
	}()
	<-started
	c.Adopt(build(t, shared, []spec{m("sonnet", "anthropic", "claude-sonnet")}))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := answeredBy(t, c.Begin(models.Chat)); got != "sonnet" {
		t.Fatalf("after adopt: %q", got)
	}
	if err := c.RegisterClient("sonnet", shared); err != nil {
		t.Fatal(err)
	}
	c.Adopt(build(t, modelstest.New(), []spec{m("sonnet", "anthropic", "claude-sonnet")}))
	if shared.Closed() != 0 {
		t.Fatalf("a registered client was closed %d times", shared.Closed())
	}
}

func TestBuild(t *testing.T) {
	t.Setenv("MODELS_TEST_KEY", "secret")
	off := false
	doc := modelspec.Document{
		Version: 1,
		Providers: []modelspec.Provider{
			{Name: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: "env:MODELS_TEST_KEY"},
		},
		Models: []modelspec.Model{
			{ModelName: "sonnet", Model: "claude-sonnet", Provider: "anthropic", Enabled: true},
			{ModelName: "haiku", Model: "claude-haiku", Provider: "anthropic", Enabled: true},
			{ModelName: "old", Model: "claude-old", Provider: "anthropic"},
		},
		Assignments: map[string]map[string][]modelspec.Assignment{
			"alice": {
				"chat":      {{Model: "sonnet"}, {Model: "old"}, {Model: "haiku", Enabled: &off}},
				"summarize": {{Model: "haiku"}},
			},
		},
	}
	c, err := models.Build(doc, models.WithSecrets(modelspec.DefaultResolver(0o600)))
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := c.Get("sonnet"); info.Provider.APIKey != "secret" {
		t.Fatalf("key = %q, want resolved", info.Provider.APIKey)
	}
	if doc.Providers[0].APIKey != "env:MODELS_TEST_KEY" {
		t.Fatal("the document was changed")
	}
	if c.Enabled("old") || !c.Enabled("sonnet") {
		t.Fatal("the model switches were not taken from the document")
	}
	alice := c.Set("alice")
	chat := alice.List(models.Chat)
	if got := names(chat); !slices.Equal(got, []string{"sonnet", "old", "haiku"}) {
		t.Fatalf("chat = %q", got)
	}
	if !chat[0].Enabled || chat[1].Enabled || chat[2].Enabled {
		t.Fatalf("switches = %+v", chat)
	}
	if !alice.List(models.Summarize)[0].Enabled {
		t.Fatal("the summarize assignment of haiku is off")
	}

	bad := doc
	bad.Models = append(slices.Clone(doc.Models), modelspec.Model{ModelName: "x", Model: "x", Provider: "nope", Enabled: true})
	if _, err := models.Build(bad); err == nil {
		t.Fatal("an invalid document was built")
	}
	empty := doc
	empty.Assignments = map[string]map[string][]modelspec.Assignment{"bob": {"chat": {}}}
	if _, err := models.Build(empty); err == nil {
		t.Fatal("a purpose with no model was accepted")
	}
	if _, err := models.Build(doc, models.WithSecrets(func(string) (string, error) { return "", errors.New("no") })); err == nil {
		t.Fatal("a secret that does not resolve was accepted")
	}
}

func TestAdoptTakesNewOptions(t *testing.T) {
	f := newFixture(t, three)
	f.park(t, "sonnet", 429)
	next := build(t, f.fake, three, models.WithClock(f.clock), models.WithCooldown(modelspec.Cooldown{RateLimitMinutes: -1}))
	f.cat.Adopt(next)
	f.fake.Script("claude-sonnet", modelstest.Status(429, 0))
	for range 5 {
		discard(f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}))
		f.clock.Advance(10 * time.Minute)
	}
	if h, _ := f.cat.Get("sonnet"); !h.Health.Available {
		t.Fatal("the new cooldown policy (rate limits never cool) was not taken")
	}
}
