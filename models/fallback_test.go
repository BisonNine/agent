package models_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
)

// describe renders an attempt as "name kind reason".
func describe(a models.Attempt) string {
	kind := "failed"
	switch {
	case a.Disabled:
		kind = "disabled"
	case a.Skipped:
		kind = "skipped"
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s %s", a.Name, kind, a.Reason))
}

func describeAll(list []models.Attempt) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, describe(a))
	}
	return out
}

// notice renders a fallback notice: the models passed (a skipped one marked
// "~"), then ">" and the next.
func notice(passed []models.Attempt, next models.Candidate) string {
	names := make([]string, 0, len(passed))
	for _, a := range passed {
		n := a.Name
		if a.Skipped {
			n = "~" + n
		}
		names = append(names, n)
	}
	return strings.Join(names, "+") + ">" + next.Name
}

var (
	errRateLimit = errors.New("rate limit exceeded")
	isExhausted  = func(err error) bool { _, ok := errors.AsType[*models.ExhaustedError](err); return ok }
)

// The fallback cases of the chain: the calls made, the attempts recorded and
// the notices given must be exactly those of the fallback chain this package
// replaces.
func TestChainFallback(t *testing.T) {
	cases := []struct {
		name      string
		models    []spec
		parked    map[string]int // model name → status it is parked with
		script    map[string][]modelstest.Step
		retryWait time.Duration // a Retry-After the call waits out on the fake clock
		calls     []string      // wire models, in call order
		answer    string        // the model that answered; "" for an error
		err       func(error) bool
		attempts  []string
		notices   []string
	}{
		{
			name:     "single model answers",
			models:   []spec{m("gpt-4", "openai", "gpt-4")},
			calls:    []string{"gpt-4"},
			answer:   "gpt-4",
			attempts: []string{},
		},
		{
			name:     "second model answers after a failure, with one notice",
			models:   []spec{m("gpt-4", "openai", "gpt-4"), m("claude-opus", "anthropic", "claude-opus")},
			script:   map[string][]modelstest.Step{"gpt-4": {modelstest.Fail(errRateLimit)}},
			calls:    []string{"gpt-4", "claude-opus"},
			answer:   "claude-opus",
			attempts: []string{"gpt-4 failed rate_limit"},
			notices:  []string{"gpt-4>claude-opus"},
		},
		{
			name:     "a parked model in the middle is skipped and announced",
			models:   []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude"), m("grok", "xai", "grok")},
			parked:   map[string]int{"claude": 429},
			script:   map[string][]modelstest.Step{"gpt-4": {modelstest.Fail(errRateLimit)}},
			calls:    []string{"gpt-4", "grok"},
			answer:   "grok",
			attempts: []string{"gpt-4 failed rate_limit", "claude skipped rate_limit"},
			notices:  []string{"gpt-4>claude", "~claude>grok"},
		},
		{
			name:     "no notice for the last model",
			models:   []spec{m("gpt-4", "openai", "gpt-4")},
			script:   map[string][]modelstest.Step{"gpt-4": {modelstest.Fail(errRateLimit)}},
			calls:    []string{"gpt-4"},
			err:      isExhausted,
			attempts: []string{"gpt-4 failed rate_limit"},
		},
		{
			name:   "every model fails",
			models: []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude"), m("llama", "groq", "llama")},
			script: map[string][]modelstest.Step{
				"gpt-4": {modelstest.Fail(errRateLimit)}, "claude": {modelstest.Fail(errRateLimit)}, "llama": {modelstest.Fail(errRateLimit)},
			},
			calls:    []string{"gpt-4", "claude", "llama"},
			err:      isExhausted,
			attempts: []string{"gpt-4 failed rate_limit", "claude failed rate_limit", "llama failed rate_limit"},
			notices:  []string{"gpt-4>claude", "claude>llama"},
		},
		{
			name:     "a format error falls back",
			models:   []spec{m("deepseek-v4-pro", "deepseek", "deepseek-v4-pro"), m("grok", "xai", "grok")},
			script:   map[string][]modelstest.Step{"deepseek-v4-pro": {modelstest.Fail(errors.New("string should match pattern"))}},
			calls:    []string{"deepseek-v4-pro", "grok"},
			answer:   "grok",
			attempts: []string{"deepseek-v4-pro failed format"},
			notices:  []string{"deepseek-v4-pro>grok"},
		},
		{
			name:     "a parked first model is skipped",
			models:   []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude")},
			parked:   map[string]int{"gpt-4": 429},
			calls:    []string{"claude"},
			answer:   "claude",
			attempts: []string{"gpt-4 skipped rate_limit"},
			notices:  []string{"~gpt-4>claude"},
		},
		{
			name:     "every model parked",
			models:   []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude")},
			parked:   map[string]int{"gpt-4": 429, "claude": 402},
			calls:    []string{},
			err:      isExhausted,
			attempts: []string{"gpt-4 skipped rate_limit", "claude skipped billing"},
		},
		{
			name:     "an unclassified error ends the call",
			models:   []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude")},
			script:   map[string][]modelstest.Step{"gpt-4": {modelstest.Fail(errors.New("completely unknown internal error"))}},
			calls:    []string{"gpt-4"},
			err:      func(err error) bool { return !isExhausted(err) && strings.Contains(err.Error(), "unclassified error") },
			attempts: []string{"gpt-4 failed"},
		},
		{
			name:     "a CLI that is not signed in falls back as auth",
			models:   []spec{m("claude", "claude-cli", "claude"), m("gpt-4", "openai", "gpt-4")},
			script:   map[string][]modelstest.Step{"claude": {modelstest.Fail(errors.New("claude cli error: Not logged in · Please run /login"))}},
			calls:    []string{"claude", "gpt-4"},
			answer:   "gpt-4",
			attempts: []string{"claude failed auth"},
			notices:  []string{"claude>gpt-4"},
		},
		{
			name: "consecutive parked models are one notice",
			models: []spec{
				m("deepseek-flash", "or", "deepseek-flash"), m("deepseek-pro", "or", "deepseek-pro"),
				m("glm", "or", "glm"), m("grok", "xai", "grok"),
			},
			parked:   map[string]int{"deepseek-flash": 402, "deepseek-pro": 402, "glm": 402},
			calls:    []string{"grok"},
			answer:   "grok",
			attempts: []string{"deepseek-flash skipped billing", "deepseek-pro skipped billing", "glm skipped billing"},
			notices:  []string{"~deepseek-flash+~deepseek-pro+~glm>grok"},
		},
		{
			name:     "trailing parked models give no notice",
			models:   []spec{m("gpt-4", "openai", "gpt-4"), m("glm", "or", "glm")},
			parked:   map[string]int{"glm": 402},
			script:   map[string][]modelstest.Step{"gpt-4": {modelstest.Fail(errRateLimit)}},
			calls:    []string{"gpt-4"},
			err:      isExhausted,
			attempts: []string{"gpt-4 failed rate_limit", "glm skipped billing"},
			notices:  []string{"gpt-4>glm"},
		},
		{
			name:     "the answer names the model that gave it",
			models:   []spec{m("Alice GPT", "openai", "gpt-4"), m("Bob Opus", "anthropic", "claude-opus")},
			script:   map[string][]modelstest.Step{"gpt-4": {modelstest.Fail(errRateLimit)}},
			calls:    []string{"gpt-4", "claude-opus"},
			answer:   "Bob Opus",
			attempts: []string{"Alice GPT failed rate_limit"},
			notices:  []string{"Alice GPT>Bob Opus"},
		},
		{
			name:      "a short Retry-After is waited out once, then a sibling on the same provider is tried",
			models:    []spec{m("scout", "openrouter", "meta-llama/llama-4-scout"), m("auto", "openrouter", "openrouter/auto")},
			script:    map[string][]modelstest.Step{"meta-llama/llama-4-scout": {modelstest.Status(429, 2*time.Second)}},
			retryWait: 2 * time.Second,
			calls:     []string{"meta-llama/llama-4-scout", "meta-llama/llama-4-scout", "openrouter/auto"},
			answer:    "auto",
			attempts:  []string{"scout failed rate_limit"},
			notices:   []string{"scout>auto"},
		},
		{
			name:     "a parse error falls back",
			models:   []spec{m("scout", "openrouter", "scout"), m("auto", "openrouter", "auto")},
			script:   map[string][]modelstest.Step{"scout": {modelstest.Fail(errors.New("failed to parse JSON response: unexpected EOF"))}},
			calls:    []string{"scout", "auto"},
			answer:   "auto",
			attempts: []string{"scout failed timeout"},
			notices:  []string{"scout>auto"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.models)
			for name, status := range tc.parked {
				f.park(t, name, status)
			}
			for model, steps := range tc.script {
				f.fake.Script(model, steps...)
			}
			if tc.retryWait > 0 {
				go func() {
					if f.clock.BlockUntil(t.Context(), 1) == nil {
						f.clock.Advance(tc.retryWait)
					}
				}()
			}
			var notices []string
			res, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{},
				models.WithNotify(func(passed []models.Attempt, next models.Candidate) {
					notices = append(notices, notice(passed, next))
				}))

			if got := f.fake.Models(); !slices.Equal(got, tc.calls) && len(got)+len(tc.calls) > 0 {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
			if !slices.Equal(notices, tc.notices) {
				t.Errorf("notices = %q, want %q", notices, tc.notices)
			}
			var attempts []models.Attempt
			if tc.answer != "" {
				if err != nil {
					t.Fatalf("err = %v, want an answer from %s", err, tc.answer)
				}
				if res.Candidate.Name != tc.answer || res.Response == nil {
					t.Fatalf("answered by %q, want %q", res.Candidate.Name, tc.answer)
				}
				attempts = res.Attempts
			} else {
				if err == nil || !tc.err(err) {
					t.Fatalf("err = %v", err)
				}
				if ex, ok := errors.AsType[*models.ExhaustedError](err); ok {
					attempts = ex.Attempts
				} else {
					attempts = f.cat.Current(models.Chat).Attempts
				}
			}
			if got := describeAll(attempts); tc.attempts != nil && !slices.Equal(got, tc.attempts) {
				t.Errorf("attempts = %q, want %q", got, tc.attempts)
			}
		})
	}
}

func TestChainSkipCarriesTimeRemaining(t *testing.T) {
	f := newFixture(t, []spec{m("glm", "or", "glm"), m("grok", "xai", "grok")})
	f.park(t, "glm", 402)
	f.clock.Advance(10 * time.Minute)
	res, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Attempts[0]
	if a.Remaining != 20*time.Minute || a.Reason != spawnllm.FailoverBilling || !strings.Contains(a.Err.Error(), "20m0s remaining") {
		t.Fatalf("skip = %+v", a)
	}
}

func TestChainContextCanceled(t *testing.T) {
	f := newFixture(t, []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude")})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.fake.Script("gpt-4", modelstest.Fail(context.Canceled))
	_, err := f.cat.Begin(models.Chat).Chat(ctx, alice, models.Request{},
		models.WithFinish(func(models.Candidate, time.Duration, *spawnllm.LLMResponse, error) { cancel() }))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := f.fake.Models(); !slices.Equal(got, []string{"gpt-4"}) {
		t.Fatalf("calls = %q: a model was tried after the cancel", got)
	}
	if _, _, _, exhausted := f.obs.counts(); exhausted != 0 {
		t.Fatal("a cancelled call was reported exhausted")
	}
}

func TestChainNoModels(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{})
	if !errors.Is(err, models.ErrNoModelAvailable) || !strings.Contains(err.Error(), `"chat"`) {
		t.Fatalf("err = %v, want ErrNoModelAvailable naming the purpose", err)
	}
}

func TestChainRefusesMissingIdentity(t *testing.T) {
	f := newFixture(t, []spec{m("gpt-4", "openai", "gpt-4")})
	if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), accounting.Identity{}, models.Request{}); err == nil {
		t.Fatal("a call without an agent was made")
	}
	if len(f.fake.Calls()) != 0 {
		t.Fatal("the client was called")
	}
}

func TestChainPassesIdentityAndRequest(t *testing.T) {
	f := newFixture(t, []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude")})
	f.fake.Script("gpt-4", modelstest.Fail(errRateLimit))
	req := models.Request{Messages: []spawnllm.Message{{Role: "user", Content: "hi"}}, Options: map[string]any{"max_tokens": 100}}
	_, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, req,
		models.WithPrepare(func(c models.Candidate, _ spawnllm.LLMProvider, r models.Request) models.Request {
			return models.Request{Messages: r.Messages, Options: map[string]any{"for": c.Name}}
		}))
	if err != nil {
		t.Fatal(err)
	}
	calls := f.fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	for i, want := range []string{"gpt-4", "claude"} {
		if calls[i].Identity != alice || calls[i].Options["for"] != want || calls[i].Messages[0].Content != "hi" {
			t.Errorf("call %d = %+v", i, calls[i])
		}
	}
	if req.Options["max_tokens"] != 100 {
		t.Error("the request was changed")
	}
}

func TestChainSuccessClearsHealth(t *testing.T) {
	f := newFixture(t, []spec{m("gpt-4", "openai", "gpt-4")})
	f.park(t, "gpt-4", 429)
	f.clock.Advance(time.Minute)
	if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.cat.Get("gpt-4"); !h.Health.Available || h.Health.Failures != 0 {
		t.Fatalf("health after an answer = %+v", h.Health)
	}
}

// A request too large for every model fails with AllContextLimit, which the
// caller fixes by compacting; the models are not parked, so the retry on the
// same chain tries them again.
func TestChainContextLimit(t *testing.T) {
	f := newFixture(t, []spec{m("a", "openai", "a"), m("b", "anthropic", "b")})
	f.fake.Script("a", modelstest.Status(413, 0), modelstest.Reply("ok"))
	f.fake.Script("b", modelstest.Status(413, 0))
	ch := f.cat.Begin(models.Chat)
	_, err := ch.Chat(t.Context(), alice, models.Request{})
	ex, ok := errors.AsType[*models.ExhaustedError](err)
	if !ok || !ex.AllContextLimit() {
		t.Fatalf("err = %v, want an exhausted error with AllContextLimit", err)
	}
	res, err := ch.Chat(t.Context(), alice, models.Request{})
	if err != nil || res.Candidate.Name != "a" {
		t.Fatalf("retry: %v, %+v", err, res)
	}
	if got := f.fake.Models(); !slices.Equal(got, []string{"a", "b", "a"}) {
		t.Fatalf("calls = %q", got)
	}

	mixed := &models.ExhaustedError{Attempts: []models.Attempt{
		{Reason: spawnllm.FailoverContextLimit}, {Reason: spawnllm.FailoverRateLimit},
	}}
	if mixed.AllContextLimit() {
		t.Error("a rate limit counted as a context limit")
	}
	if (&models.ExhaustedError{Attempts: []models.Attempt{{Skipped: true}}}).AllContextLimit() {
		t.Error("skips alone counted as a context limit")
	}
}

func TestExhaustedErrorMessage(t *testing.T) {
	e := &models.ExhaustedError{Attempts: []models.Attempt{
		{Provider: "openai", Model: "gpt-4", Err: errors.New("rate limited"), Reason: spawnllm.FailoverRateLimit, Duration: 500 * time.Millisecond},
		{Provider: "anthropic", Model: "claude", Skipped: true},
		{Provider: "xai", Model: "grok", Skipped: true, Disabled: true},
	}}
	want := "fallback: all 3 candidates failed:" +
		"\n  [1] openai/gpt-4: rate limited (reason=rate_limit, 500ms)" +
		"\n  [2] anthropic/claude: skipped (cooldown)" +
		"\n  [3] xai/grok: skipped (disabled)"
	if got := e.Error(); got != want {
		t.Fatalf("message =\n%s\nwant\n%s", got, want)
	}
}

// Cooldown timing on the fake clock: a model parked for a minute is skipped
// until the minute is up, then tried again.
func TestChainCooldownTiming(t *testing.T) {
	f := newFixture(t, []spec{m("gpt-4", "openai", "gpt-4"), m("claude", "anthropic", "claude")})
	f.park(t, "gpt-4", 429)
	for _, tc := range []struct {
		advance time.Duration
		first   string
	}{
		{59 * time.Second, "claude"},
		{time.Second, "gpt-4"},
	} {
		f.clock.Advance(tc.advance)
		f.fake.Reset()
		if _, err := f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}); err != nil {
			t.Fatal(err)
		}
		if got := f.fake.Models(); got[0] != tc.first {
			t.Fatalf("after %v more: first call to %q, want %q", tc.advance, got[0], tc.first)
		}
	}
}

// When the caller's context ends, the call stops: no further model, no
// health record for the call cut short, no FellBack or Exhausted event.
func TestChainStopsWhenCallerGivesUp(t *testing.T) {
	never := make(chan struct{})
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"deadline during a call", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 20*time.Millisecond)
		}, context.DeadlineExceeded},
		{"already cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, three)
			f.fake.Script("claude-sonnet", modelstest.Step{Hold: never})
			ctx, cancel := tc.ctx()
			defer cancel()
			_, err := f.cat.Begin(models.Chat).Chat(ctx, alice, models.Request{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := len(f.fake.Calls()); n > 1 {
				t.Fatalf("%d models called after the caller gave up", n)
			}
			if p, _, fb, ex := f.obs.counts(); p+fb+ex != 0 {
				t.Fatalf("events: parked %d, fell back %d, exhausted %d", p, fb, ex)
			}
			if h, _ := f.cat.Get("sonnet"); h.Health.Failures != 0 {
				t.Fatalf("health = %+v", h.Health)
			}
			_, err = f.cat.Default().Caller(models.Chat, alice).Complete(ctx, models.CompleteRequest{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("complete: err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A model's own timeout, with the caller's context still live, is an
// ordinary failure: the next model is tried.
func TestChainModelTimeoutFallsBack(t *testing.T) {
	f := newFixture(t, three)
	f.fake.Script("claude-sonnet", modelstest.Fail(context.DeadlineExceeded))
	if got := answeredBy(t, f.cat.Begin(models.Chat)); got != "gpt" {
		t.Fatalf("answered by %q", got)
	}
}

// A Retry-After wait cut short by the caller still records the failure that
// caused it.
func TestChainRetryWaitCutShort(t *testing.T) {
	f := newFixture(t, three)
	f.fake.Script("claude-sonnet", modelstest.Status(429, 2*time.Second))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		if f.clock.BlockUntil(t.Context(), 1) == nil {
			cancel()
		}
	}()
	_, err := f.cat.Begin(models.Chat).Chat(ctx, alice, models.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if h, _ := f.cat.Get("sonnet"); h.Health.Available || h.Health.Failures != 1 {
		t.Fatalf("the 429 was not recorded: %+v", h.Health)
	}
}
