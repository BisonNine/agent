package models_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
)

// A call that is not a chat call (speech, images) walks the chain with its
// own client for each model, sharing health, fallback and events with chat.
func TestDo(t *testing.T) {
	f := newFixture(t, three)
	var seen []string
	var finished []string
	c, attempts, err := f.cat.Begin(models.Chat).Do(t.Context(), func(_ context.Context, c models.Candidate) error {
		seen = append(seen, c.Spec.Model+"@"+c.ProviderSpec.BaseURL)
		if c.Name == "sonnet" {
			return modelstest.Status(503, 0).Err
		}
		return nil
	}, models.WithFinish(func(c models.Candidate, _ time.Duration, resp *spawnllm.LLMResponse, err error) {
		if resp != nil {
			t.Error("Do passed a chat response to the finish hook")
		}
		finished = append(finished, c.Name)
	}))
	if err != nil || c.Name != "gpt" {
		t.Fatalf("%v %+v", err, c)
	}
	if !slices.Equal(seen, []string{"claude-sonnet@https://example.invalid/v1", "gpt-5@https://example.invalid/v1"}) {
		t.Fatalf("fn saw %q", seen)
	}
	if !slices.Equal(finished, []string{"sonnet", "gpt"}) || len(attempts) != 1 {
		t.Fatalf("finished %q, attempts %+v", finished, attempts)
	}
	if h, _ := f.cat.Get("sonnet"); h.Health.Available {
		t.Fatal("the 503 did not park the model")
	}
	if p, _, fb, _ := f.obs.counts(); p != 1 || fb != 1 {
		t.Fatalf("parked %d, fell back %d", p, fb)
	}
	// A chat call now skips the model the Do call parked.
	if got := answeredBy(t, f.cat.Begin(models.Chat)); got != "gpt" {
		t.Fatalf("chat after Do answered by %q", got)
	}
	if _, _, err := models.New().Begin(models.Chat).Do(t.Context(), func(context.Context, models.Candidate) error { return nil }); !errors.Is(err, models.ErrNoModelAvailable) {
		t.Fatalf("no models: %v", err)
	}
}

// The candidate given to fn is a copy.
func TestDoCandidateIsACopy(t *testing.T) {
	f := newFixture(t, three)
	discard(f.cat.Begin(models.Chat).Do(t.Context(), func(_ context.Context, c models.Candidate) error {
		c.Spec.ExtraArgs = append(c.Spec.ExtraArgs, "x")
		c.Spec.Model = "changed"
		return nil
	}))
	if info, _ := f.cat.Get("sonnet"); info.Model.Model != "claude-sonnet" || len(info.Model.ExtraArgs) != 0 {
		t.Fatalf("the definition was changed through a candidate: %+v", info.Model)
	}
}

// A host rule for which failures cool a model: only those that say the model
// cannot serve anyone; every failure tries the next model.
func TestWithClassify(t *testing.T) {
	worthy := func(fe *spawnllm.FailoverError) bool {
		if fe.Status >= 500 {
			return true
		}
		switch fe.Reason {
		case spawnllm.FailoverRateLimit, spawnllm.FailoverOverloaded, spawnllm.FailoverTimeout,
			spawnllm.FailoverAuth, spawnllm.FailoverBilling:
			return true
		default:
			return false
		}
	}
	classify := models.WithClassify(func(err error, c models.Candidate) models.Verdict {
		fe := spawnllm.ClassifyError(err, c.Provider, c.Model)
		if fe == nil {
			return models.Verdict{Next: true}
		}
		return models.Verdict{Reason: fe.Reason, Status: fe.Status, RetryAfter: fe.RetryAfter, Record: worthy(fe), Next: true}
	})
	cases := []struct {
		name   string
		err    error
		parked bool
	}{
		{"a refused request", modelstest.Status(404, 0).Err, false},
		{"a rate limit", modelstest.Status(429, 0).Err, true},
		{"an outage", modelstest.Status(502, 0).Err, true},
		{"an unclassified error", errors.New("completely unknown internal error"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, three)
			c, _, err := f.cat.Begin(models.Chat).Do(t.Context(), func(_ context.Context, c models.Candidate) error {
				if c.Name == "sonnet" {
					return tc.err
				}
				return nil
			}, classify)
			if err != nil || c.Name != "gpt" {
				t.Fatalf("%v %+v: the next model was not tried", err, c)
			}
			if h, _ := f.cat.Get("sonnet"); h.Health.Available == tc.parked {
				t.Fatalf("parked = %v, want %v", !h.Health.Available, tc.parked)
			}
		})
	}
}

// A model's chat client, for a call that records no health.
func TestSetClient(t *testing.T) {
	f := newFixture(t, three)
	f.fake.Script("claude-sonnet", modelstest.Status(429, 0))
	client, release, err := f.cat.Set("bob").Client("sonnet")
	if err != nil {
		t.Fatal(err)
	}
	discard(client.Chat(t.Context(), alice, nil, nil, "claude-sonnet", nil))
	release()
	if h, _ := f.cat.Get("sonnet"); !h.Health.Available || h.Health.Failures != 0 {
		t.Fatal("a direct client call touched health")
	}
	if _, _, err := f.cat.Default().Client("nope"); err == nil {
		t.Fatal("a client for an unknown model")
	}
}

// The summary caller's hooks: each model passed is reported to the host's
// notify hook (and the SDK logs nothing), each call to its finish hook.
func TestCallerHooks(t *testing.T) {
	var buf bytes.Buffer
	f := newFixture(t, append(three, m("opus", "anthropic", "claude-opus")), models.WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	f.park(t, "gpt", 429)
	if err := f.cat.Disable("haiku"); err != nil {
		t.Fatal(err)
	}
	f.fake.Script("claude-sonnet", modelstest.Fail(errors.New("completely unknown internal error")))
	var notes, finished []string
	caller := f.cat.Default().Caller(models.Chat, alice, "s",
		models.WithNotify(func(passed []models.Attempt, next models.Candidate) {
			notes = append(notes, notice(passed, next))
		}),
		models.WithFinish(func(c models.Candidate, _ time.Duration, _ *spawnllm.LLMResponse, err error) {
			finished = append(finished, c.Name)
		}))
	buf.Reset()
	reply, err := caller.Complete(t.Context(), models.CompleteRequest{})
	if err != nil || reply.Model != "claude-opus" {
		t.Fatalf("%v %+v", err, reply)
	}
	if want := []string{"sonnet>gpt", "~gpt>haiku", "~haiku>opus"}; !slices.Equal(notes, want) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
	if !slices.Equal(finished, []string{"sonnet", "opus"}) {
		t.Fatalf("finished = %q", finished)
	}
	if strings.Contains(buf.String(), "model in cooldown") || strings.Contains(buf.String(), "trying the next") {
		t.Fatalf("the SDK logged what the host's hook reported:\n%s", buf.String())
	}

	buf.Reset()
	discard(f.cat.Default().Caller(models.Chat, alice, "").Complete(t.Context(), models.CompleteRequest{}))
	if !strings.Contains(buf.String(), "model in cooldown") {
		t.Fatalf("without a hook the skip is not logged:\n%s", buf.String())
	}
}

// Every model parked, disabled or none assigned: ErrNoModelAvailable.
func TestCallerNoModel(t *testing.T) {
	f := newFixture(t, three)
	f.park(t, "sonnet", 429)
	f.park(t, "gpt", 429)
	if err := f.cat.Disable("haiku"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []models.Purpose{models.Chat, models.Memory} {
		_, err := f.cat.Default().Caller(p, alice, "").Complete(t.Context(), models.CompleteRequest{})
		if !errors.Is(err, models.ErrNoModelAvailable) {
			t.Errorf("%s: err = %v", p, err)
		}
	}
}
