package models

import (
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

var epoch = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newTestHealth(c modelspec.Cooldown) (*health, *clock.Fake) {
	f := clock.NewFake(epoch)
	return newHealth(f, newPolicy(c), newParkedBook(), nil), f
}

func statusFor(r spawnllm.FailoverReason) int {
	switch r {
	case spawnllm.FailoverBilling:
		return 402
	case spawnllm.FailoverAuth:
		return 401
	case spawnllm.FailoverRateLimit:
		return 429
	case spawnllm.FailoverContextLimit:
		return 413
	default:
		return 500
	}
}

func markFail(h *health, provider, model string, r spawnllm.FailoverReason) {
	h.failure(model, provider, model, r, statusFor(r), 0)
}

func remaining(h *health, provider, model string) time.Duration {
	d, _ := h.parkedFor(provider, model)
	return d
}

func failures(h *health, provider, model string) int {
	return h.state(model, provider, model).Failures
}

func TestHealthInitiallyAvailable(t *testing.T) {
	h, _ := newTestHealth(modelspec.Cooldown{})
	if remaining(h, "openai", "m") != 0 || failures(h, "openai", "m") != 0 {
		t.Fatal("a new model is not available")
	}
}

func TestHealthEscalation(t *testing.T) {
	h, f := newTestHealth(modelspec.Cooldown{})
	for i, want := range []time.Duration{time.Minute, 3 * time.Minute, 5 * time.Minute, 10 * time.Minute} {
		markFail(h, "openai", "m", spawnllm.FailoverRateLimit)
		if got := remaining(h, "openai", "m"); got != want {
			t.Fatalf("failure %d: cooldown = %v, want %v", i+1, got, want)
		}
		f.Advance(want + time.Second)
	}
}

func TestHealthBillingAndAuthSkipEscalation(t *testing.T) {
	for _, r := range []spawnllm.FailoverReason{spawnllm.FailoverBilling, spawnllm.FailoverAuth} {
		h, _ := newTestHealth(modelspec.Cooldown{})
		markFail(h, "openai", "m", r)
		if got := remaining(h, "openai", "m"); got != 30*time.Minute {
			t.Errorf("%s: first cooldown = %v, want 30m", r, got)
		}
	}
	h, _ := newTestHealth(modelspec.Cooldown{})
	markFail(h, "openai", "m", spawnllm.FailoverRateLimit)
	if got := remaining(h, "openai", "m"); got != time.Minute {
		t.Errorf("first rate-limit cooldown = %v, want 1m", got)
	}
}

func TestHealthStatusesThatNeverCool(t *testing.T) {
	cases := []struct {
		name   string
		reason spawnllm.FailoverReason
		status int
	}{
		{"413 context limit", spawnllm.FailoverContextLimit, 413},
		{"no HTTP status", spawnllm.FailoverTimeout, 0},
		{"400 bad request", spawnllm.FailoverFormat, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHealth(modelspec.Cooldown{})
			if _, parked := h.failure("m", "openai", "m", tc.reason, tc.status, 0); parked {
				t.Error("reported parked")
			}
			if remaining(h, "openai", "m") != 0 {
				t.Error("parked")
			}
			if tc.status != 400 && failures(h, "openai", "m") != 0 {
				t.Error("counted toward escalation")
			}
		})
	}
}

func TestHealthRetryAfter(t *testing.T) {
	h, _ := newTestHealth(modelspec.Cooldown{})
	h.failure("m", "openai", "m", spawnllm.FailoverRateLimit, 429, 2*time.Minute)
	if got := remaining(h, "openai", "m"); got != 2*time.Minute {
		t.Fatalf("cooldown = %v, want the 2m Retry-After", got)
	}

	h, f := newTestHealth(modelspec.Cooldown{})
	h.failure("m", "openrouter", "scout", spawnllm.FailoverRateLimit, 429, 3*time.Second)
	f.Advance(2 * time.Second)
	if remaining(h, "openrouter", "scout") == 0 {
		t.Fatal("available 2s into a 3s Retry-After")
	}
	f.Advance(2 * time.Second)
	if remaining(h, "openrouter", "scout") != 0 {
		t.Fatal("still parked 4s after a 3s Retry-After")
	}

	h, f = newTestHealth(modelspec.Cooldown{})
	h.failure("m", "openrouter", "scout", spawnllm.FailoverRateLimit, 429, 24*time.Hour)
	f.Advance(5*time.Minute + time.Second)
	if remaining(h, "openrouter", "scout") != 0 {
		t.Fatal("a 24h Retry-After was not capped at 5m")
	}
}

func TestHealthSuccessResets(t *testing.T) {
	h, _ := newTestHealth(modelspec.Cooldown{})
	markFail(h, "openai", "m", spawnllm.FailoverRateLimit)
	markFail(h, "openai", "m", spawnllm.FailoverBilling)
	if n := failures(h, "openai", "m"); n != 2 {
		t.Fatalf("failures = %d, want 2", n)
	}
	h.success("m", "openai", "m")
	st := h.state("m", "openai", "m")
	if st.Failures != 0 || !st.Available || st.Reason != "" {
		t.Fatalf("after success: %+v", st)
	}
	h.success("x", "nonexistent", "m") // no entry: no panic
}

func TestHealthFailureWindow(t *testing.T) {
	h, f := newTestHealth(modelspec.Cooldown{})
	for range 4 {
		markFail(h, "openai", "m", spawnllm.FailoverRateLimit)
		f.Advance(2 * time.Second)
	}
	if n := failures(h, "openai", "m"); n != 4 {
		t.Fatalf("failures = %d, want 4", n)
	}
	f.Set(epoch.Add(25 * time.Hour))
	markFail(h, "openai", "m", spawnllm.FailoverRateLimit)
	if n := failures(h, "openai", "m"); n != 1 {
		t.Fatalf("failures after the 24h window = %d, want 1", n)
	}
}

func TestHealthPerModel(t *testing.T) {
	h, _ := newTestHealth(modelspec.Cooldown{})
	markFail(h, "openai", "gpt-4", spawnllm.FailoverRateLimit)
	markFail(h, "anthropic", "claude-opus", spawnllm.FailoverBilling)
	if remaining(h, "openai", "gpt-4") == 0 || remaining(h, "anthropic", "claude-opus") == 0 {
		t.Error("a failed model is available")
	}
	if remaining(h, "openai", "gpt-3.5") != 0 || remaining(h, "groq", "m") != 0 {
		t.Error("a sibling model is parked")
	}
}

func TestPolicyCategories(t *testing.T) {
	p := newPolicy(modelspec.Cooldown{})
	cases := map[int]time.Duration{
		401: 30 * time.Minute, 402: 30 * time.Minute, 403: 30 * time.Minute,
		429: 10 * time.Minute, 400: 0, 404: 10 * time.Minute,
		500: 10 * time.Minute, 503: 10 * time.Minute, 413: 0, 0: 0,
	}
	for status, want := range cases {
		if got := p.category(status); got != want {
			t.Errorf("category(%d) = %v, want %v", status, got, want)
		}
	}
	custom := newPolicy(modelspec.Cooldown{RateLimitMinutes: 2, ServerErrorMinutes: -1})
	if custom.category(429) != 2*time.Minute || custom.category(500) != 0 {
		t.Errorf("custom policy = %+v", custom)
	}
}

func TestHealthLastReason(t *testing.T) {
	h, _ := newTestHealth(modelspec.Cooldown{})
	if _, r := h.parkedFor("or", "glm"); r != "" {
		t.Fatalf("reason of an unknown model = %q", r)
	}
	h.failure("glm", "or", "glm", spawnllm.FailoverBilling, 402, 0)
	if _, r := h.parkedFor("or", "glm"); r != spawnllm.FailoverBilling {
		t.Fatalf("reason = %q, want billing", r)
	}
	h.failure("glm", "or", "glm", spawnllm.FailoverOverloaded, 503, 0)
	if _, r := h.parkedFor("or", "glm"); r != spawnllm.FailoverOverloaded {
		t.Fatalf("reason = %q, want the most recent", r)
	}
	h.success("glm", "or", "glm")
	if _, r := h.parkedFor("or", "glm"); r != "" {
		t.Fatalf("reason after success = %q", r)
	}
}

func TestHealthRemainingCountsDown(t *testing.T) {
	h, f := newTestHealth(modelspec.Cooldown{})
	markFail(h, "openai", "m", spawnllm.FailoverRateLimit)
	f.Advance(30 * time.Second)
	if got := remaining(h, "openai", "m"); got != 30*time.Second {
		t.Fatalf("remaining = %v, want 30s", got)
	}
}

func TestHealthParkedEvent(t *testing.T) {
	h, f := newTestHealth(modelspec.Cooldown{})
	var events []Parked
	for range 4 {
		if ev, ok := h.failure("alice-gpt", "openai", "gpt-4", spawnllm.FailoverRateLimit, 429, 0); ok {
			events = append(events, ev)
		}
		f.Advance(10 * time.Minute)
	}
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4", len(events))
	}
	for i, ev := range events {
		if ev.Failures != i+1 || ev.Settled != (i == 3) || ev.Key != "openai/gpt-4" || ev.Name != "alice-gpt" || ev.Status != 429 {
			t.Errorf("event %d = %+v", i, ev)
		}
	}
	if events[3].Cooldown != 10*time.Minute || !events[3].Until.Equal(events[3].At.Add(10*time.Minute)) {
		t.Errorf("settled event = %+v", events[3])
	}
	rec, ok := h.success("alice-gpt", "openai", "gpt-4")
	if !ok || !rec.Since.Equal(epoch) {
		t.Fatalf("recovered = %+v, %v; want since the first park", rec, ok)
	}
	if _, again := h.success("alice-gpt", "openai", "gpt-4"); again {
		t.Fatal("recovered reported twice")
	}
}

func TestHealthConcurrent(t *testing.T) {
	h, _ := newTestHealth(modelspec.Cooldown{})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(3)
		go func() { defer wg.Done(); markFail(h, "openai", "m", spawnllm.FailoverRateLimit) }()
		go func() { defer wg.Done(); h.parkedFor("openai", "m") }()
		go func() { defer wg.Done(); h.success("m", "openai", "m") }()
	}
	wg.Wait()
}
