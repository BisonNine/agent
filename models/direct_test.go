package models_test

import (
	"context"
	"errors"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// A direct chain calls the named model's client with the wire model id it
// was given, once, whatever the model's health or switch, and records and
// reports nothing.
func TestDirectChain(t *testing.T) {
	f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire"), m("gpt", "p2", "gpt-wire")})
	f.park(t, "haiku", 500)
	if err := f.cat.Disable("haiku"); err != nil {
		t.Fatal(err)
	}

	ch, err := f.cat.Direct("haiku", "custom-model")
	if err != nil {
		t.Fatal(err)
	}
	cands := ch.Candidates()
	if len(cands) != 1 || cands[0].Name != "custom-model" || cands[0].Model != "custom-model" ||
		cands[0].Provider != "p1" || cands[0].Protocol != "openai-chat" {
		t.Fatalf("candidates = %+v", cands)
	}
	if got, want := ch.Limits(), modelspec.ResolveLimits(nil, modelspec.LimitDefaults{}); got != want {
		t.Fatalf("limits = %+v, want the defaults %+v", got, want)
	}

	res, err := ch.Chat(t.Context(), alice, models.Request{})
	if err != nil {
		t.Fatalf("parked and disabled model not called: %v", err)
	}
	if res.Candidate.Name != "custom-model" || len(res.Attempts) != 0 {
		t.Fatalf("result = %+v", res)
	}
	calls := f.fake.Calls()
	if len(calls) != 1 || calls[0].Model != "custom-model" {
		t.Fatalf("calls = %+v", calls)
	}

	// A failure is returned as the client gave it: no fallback to gpt, no
	// health recorded, no observer event.
	boom := errors.New("boom")
	f.fake.Script("custom-model", modelstest.Fail(boom))
	_, err = ch.Chat(t.Context(), alice, models.Request{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the client's", err)
	}
	if _, isEx := errors.AsType[*models.ExhaustedError](err); isEx {
		t.Fatal("a direct call reported an exhausted chain")
	}
	if len(f.fake.Calls()) != 2 {
		t.Fatalf("calls = %d, want 2 (no fallback)", len(f.fake.Calls()))
	}
	if p, r, fb, ex := f.obs.counts(); p+r+fb+ex != 0 {
		t.Fatalf("observer events: parked %d recovered %d fellBack %d exhausted %d", p, r, fb, ex)
	}
	if h, _ := f.cat.Get("gpt"); !h.Health.Available {
		t.Fatal("gpt's health changed")
	}

	// It works isolated, and on a cancelled context calls nothing.
	if _, err := ch.Isolated(t.TempDir()).Chat(t.Context(), alice, models.Request{}); !errors.Is(err, boom) {
		t.Fatalf("isolated err = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ch.Chat(ctx, alice, models.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
	if len(f.fake.Calls()) != 3 {
		t.Fatalf("calls = %d, want 3 (none after the cancel)", len(f.fake.Calls()))
	}
}

func TestDirectChainErrors(t *testing.T) {
	f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire")})
	if _, err := f.cat.Direct("nope", "x"); !errors.Is(err, models.ErrUnknownModel) {
		t.Fatalf("unknown model: %v", err)
	}
	if _, err := f.cat.Direct("haiku", " "); !errors.Is(err, modelspec.ErrModelRequired) {
		t.Fatalf("empty model id: %v", err)
	}
}
