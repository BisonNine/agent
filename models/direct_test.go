package models_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
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
	if _, err := f.cat.Direct("", "x"); !errors.Is(err, models.ErrUnknownModel) {
		t.Fatalf("empty model name: %v", err)
	}
}

// An empty wire id is passed to the client as given: the client decides
// (a CLI runs its own default model).
func TestDirectChainEmptyModelID(t *testing.T) {
	f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire")})
	ch, err := f.cat.Direct("haiku", "")
	if err != nil {
		t.Fatalf("empty model id refused: %v", err)
	}
	if c := ch.Candidates()[0]; c.Name != "" || c.Model != "" || c.Provider != "p1" {
		t.Fatalf("candidate = %+v", c)
	}
	res, err := ch.Chat(t.Context(), alice, models.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidate.Model != "" {
		t.Fatalf("result candidate = %+v", res.Candidate)
	}
	calls := f.fake.Calls()
	if len(calls) != 1 || calls[0].Model != "" {
		t.Fatalf("calls = %+v, want one with model \"\"", calls)
	}
}

// A direct chain's other calls follow the same rules: Caller makes one call
// whatever the model's health and switch and records nothing; Do calls fn
// once; Client takes only the chain's own name; a call cut short by its
// context returns the context's error.
func TestDirectChainOtherCalls(t *testing.T) {
	f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire")})
	f.park(t, "haiku", 500)
	if err := f.cat.Disable("haiku"); err != nil {
		t.Fatal(err)
	}
	ch, err := f.cat.Direct("haiku", "custom-model")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Purpose() != "" {
		t.Fatalf("purpose = %q", ch.Purpose())
	}

	reply, err := ch.Caller(alice).Complete(t.Context(), models.CompleteRequest{System: "s", User: "u"})
	if err != nil || reply.Model != "custom-model" {
		t.Fatalf("Complete = %+v, %v", reply, err)
	}
	f.fake.Script("custom-model", modelstest.Status(500, 0))
	if _, err := ch.Caller(alice).Complete(t.Context(), models.CompleteRequest{}); err == nil {
		t.Fatal("Complete hid the client's error")
	}
	f.fake.Script("custom-model")
	if h, _ := f.cat.Get("haiku"); h.Health.Available {
		t.Fatal("haiku recovered through a direct call")
	}
	if cur := f.cat.Current(models.Chat); cur.Serving != "" {
		t.Fatalf("a direct call was recorded as serving: %+v", cur)
	}
	if p, r, fb, ex := f.obs.counts(); p+r+fb+ex != 0 {
		t.Fatalf("observer events: %d %d %d %d", p, r, fb, ex)
	}

	calls := 0
	if _, _, err := ch.Do(t.Context(), func(context.Context, models.Candidate) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("Do: %d calls, %v", calls, err)
	}

	if _, release, err := ch.Client("custom-model"); err != nil {
		t.Fatalf("Client(own name): %v", err)
	} else {
		release()
	}
	if _, _, err := ch.Client("haiku"); !errors.Is(err, models.ErrUnknownModel) {
		t.Fatalf("Client(other name): %v", err)
	}

	hold := make(chan struct{})
	started := make(chan struct{})
	f.fake.Script("custom-model", modelstest.Step{Hold: hold, Started: started})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := ch.Chat(ctx, alice, models.Request{})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cut short: %v, want context.Canceled", err)
	}
	close(hold)
}

// Without options a direct chain's candidate reports the client's provider,
// the catalogue's limit defaults and the provider's own reasoning-content
// setting.
func TestDirectChainDefaults(t *testing.T) {
	f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire")},
		models.WithLimitDefaults(modelspec.LimitDefaults{MaxTokens: 1234, ContextWindow: 5678}))
	ch, err := f.cat.Direct("haiku", "custom-model")
	if err != nil {
		t.Fatal(err)
	}
	c := ch.Candidates()[0]
	want := modelspec.ResolveLimits(nil, modelspec.LimitDefaults{MaxTokens: 1234, ContextWindow: 5678})
	if c.Provider != "p1" || c.Limits != want || ch.Limits() != want || c.ProviderSpec.RequireReasoningContent {
		t.Fatalf("candidate = %+v, limits %+v", c, ch.Limits())
	}
}

// Each option changes what the candidate reports, alone.
func TestDirectChainOptions(t *testing.T) {
	limits := modelspec.Limits{
		MaxTokens: 777, ContextWindow: 9000, ThinkingLevel: modelspec.ThinkingLevel("high"),
		NoTools: true, Vision: modelspec.VisionUserMessage, RequestTimeout: 42 * time.Second,
	}
	defaults := modelspec.ResolveLimits(nil, modelspec.LimitDefaults{})
	tests := []struct {
		name      string
		opt       models.DirectOption
		provider  string
		limits    modelspec.Limits
		reasoning bool
	}{
		{"provider", models.WithDirectProvider(""), "", defaults, false},
		{"provider renamed", models.WithDirectProvider("other"), "other", defaults, false},
		{"limits", models.WithDirectLimits(limits), "p1", limits, false},
		{"reasoning content", models.WithDirectRequireReasoningContent(true), "p1", defaults, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire")})
			ch, err := f.cat.Direct("haiku", "custom-model", tt.opt)
			if err != nil {
				t.Fatal(err)
			}
			c := ch.Candidates()[0]
			if c.Provider != tt.provider || c.Limits != tt.limits || ch.Limits() != tt.limits ||
				c.ProviderSpec.RequireReasoningContent != tt.reasoning {
				t.Fatalf("candidate = %+v", c)
			}
			// The client, its protocol and the wire id are name's whatever
			// the options.
			if c.Name != "custom-model" || c.Model != "custom-model" || c.Protocol != "openai-chat" || c.ProviderSpec.Name != "p1" {
				t.Fatalf("candidate identity = %+v", c)
			}
		})
	}
}

// The candidate every call reports carries the overrides: to WithPrepare
// and WithFinish, in Chat's Result, to Do's fn and in its Attempts. The
// call is still one call: no notify, no health, no observer event, no
// fallback, and a caller's cancel returns ctx.Err().
func TestDirectChainOptionsReported(t *testing.T) {
	f := newFixture(t, []spec{m("haiku", "p1", "haiku-wire"), m("gpt", "p2", "gpt-wire")})
	f.park(t, "haiku", 500)
	limits := modelspec.Limits{MaxTokens: 777, ContextWindow: 9000, Vision: modelspec.VisionOff}
	ch, err := f.cat.Direct("haiku", "custom-model",
		models.WithDirectProvider(""), models.WithDirectLimits(limits), models.WithDirectRequireReasoningContent(true))
	if err != nil {
		t.Fatal(err)
	}
	check := func(where string, c models.Candidate) {
		t.Helper()
		if c.Name != "custom-model" || c.Provider != "" || c.Limits != limits || !c.ProviderSpec.RequireReasoningContent {
			t.Errorf("%s: candidate = %+v", where, c)
		}
	}
	notified := 0
	call := func(ctx context.Context) (*models.Result, error) {
		return ch.Chat(ctx, alice, models.Request{},
			models.WithPrepare(func(c models.Candidate, _ spawnllm.LLMProvider, req models.Request) models.Request {
				check("prepare", c)
				return req
			}),
			models.WithNotify(func([]models.Attempt, models.Candidate) { notified++ }),
			models.WithFinish(func(c models.Candidate, _ time.Duration, _ *spawnllm.LLMResponse, _ error) {
				check("finish", c)
			}),
		)
	}

	res, err := call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	check("result", res.Candidate)

	boom := errors.New("boom")
	f.fake.Script("custom-model", modelstest.Fail(boom))
	if _, err = call(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the client's", err)
	}
	f.fake.Script("custom-model", modelstest.Fail(boom))
	_, attempts, err := ch.Do(t.Context(), func(ctx context.Context, c models.Candidate) error {
		check("do", c)
		_, cerr := f.fake.Chat(ctx, alice, nil, nil, c.Model, nil)
		return cerr
	})
	if !errors.Is(err, boom) || len(attempts) != 1 || attempts[0].Provider != "" || attempts[0].Name != "custom-model" {
		t.Fatalf("Do = %+v, %v", attempts, err)
	}

	if calls := f.fake.Calls(); len(calls) != 3 {
		t.Fatalf("calls = %+v, want 3 (no fallback to gpt)", calls)
	}
	if notified != 0 {
		t.Fatalf("notify called %d times", notified)
	}
	if p, r, fb, ex := f.obs.counts(); p+r+fb+ex != 0 {
		t.Fatalf("observer events: parked %d recovered %d fellBack %d exhausted %d", p, r, fb, ex)
	}
	if h, _ := f.cat.Get("haiku"); h.Health.Available {
		t.Fatal("haiku recovered through a direct call")
	}
	if h, _ := f.cat.Get("gpt"); !h.Health.Available {
		t.Fatal("gpt's health changed")
	}

	hold := make(chan struct{})
	started := make(chan struct{})
	f.fake.Script("custom-model", modelstest.Step{Hold: hold, Started: started})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := call(ctx)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cut short: %v, want context.Canceled", err)
	}
	close(hold)
}

// The candidate reports its client's provider's reasoning-content setting
// unless the option overrides it, either way.
func TestDirectChainReasoningContent(t *testing.T) {
	c := models.New()
	if err := c.AddProvider(modelspec.Provider{Name: "p1", Protocol: "openai-chat", BaseURL: "https://example.invalid/v1", RequireReasoningContent: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "haiku", Model: "haiku-wire", Provider: "p1"}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		opts []models.DirectOption
		want bool
	}{
		{"provider's", nil, true},
		{"overridden off", []models.DirectOption{models.WithDirectRequireReasoningContent(false)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := c.Direct("haiku", "custom-model", tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if got := ch.Candidates()[0].ProviderSpec.RequireReasoningContent; got != tt.want {
				t.Fatalf("RequireReasoningContent = %v, want %v", got, tt.want)
			}
		})
	}
}
