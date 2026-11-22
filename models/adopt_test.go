package models_test

import (
	"slices"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

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

// An old client is closed only once its last call in flight has ended.
func TestAdoptClosesOldClientAfterItsCall(t *testing.T) {
	old := modelstest.New()
	c := build(t, old, []spec{m("sonnet", "anthropic", "claude-sonnet")})
	release, started := make(chan struct{}), make(chan struct{})
	old.Script("claude-sonnet", modelstest.Step{Hold: release, Started: started, Response: modelstest.Reply("late").Response})
	done := make(chan error, 1)
	go func() {
		_, err := c.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
		done <- err
	}()
	<-started
	c.Adopt(build(t, modelstest.New(), []spec{m("sonnet", "anthropic", "claude-sonnet")}))
	if old.Closed() != 0 {
		t.Fatal("the old client was closed during its call")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if old.Closed() != 1 {
		t.Fatalf("closed %d times after its call ended, want 1", old.Closed())
	}

	idle := modelstest.New()
	c = build(t, idle, []spec{m("sonnet", "anthropic", "claude-sonnet")})
	c.Adopt(build(t, modelstest.New(), []spec{m("sonnet", "anthropic", "claude-sonnet")}))
	if idle.Closed() != 1 {
		t.Fatal("an idle old client was not closed at once")
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
