package launch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
	"go.bison9.com/agent/spawnllm/protocoltypes"
)

var spec = Spec{Kind: accounting.KindText, Provider: "test", Transport: accounting.TransportAPI, Model: "m"}

func begin(t *testing.T) (context.Context, *Launch) {
	t.Helper()
	ctx, l, err := Begin(context.Background(), accounting.Identity{Agent: "alice"}, spec)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return ctx, l
}

func TestEndReportsExactlyOnce(t *testing.T) {
	rec := launchtest.Capture(t)
	_, l := begin(t)
	l.EndChat(nil, nil)
	l.EndChat(nil, errors.New("second"))
	l.EndTask(TaskResult{}, errors.New("third"))
	if got := rec.One(); !got.Success || got.Error != "" {
		t.Errorf("record = %+v, want the first End's success", got)
	}
}

func TestEndChatNilResponse(t *testing.T) {
	for _, err := range []error{nil, errors.New("boom")} {
		rec := launchtest.Capture(t)
		_, l := begin(t)
		l.EndChat(nil, err)
		got := rec.One()
		if got.Success != (err == nil) {
			t.Errorf("success = %v for err %v", got.Success, err)
		}
		if err != nil && got.Error != "boom" {
			t.Errorf("error = %q, want boom", got.Error)
		}
		if got.Model != "m" || got.Agent != "alice" || got.Project != accounting.DefaultProject {
			t.Errorf("record = %+v", got)
		}
		launchtest.Unknown(t, "input_tokens", got.InputTokens)
		launchtest.Unknown(t, "output_tokens", got.OutputTokens)
		launchtest.Unknown(t, "http_status", got.HTTPStatus)
		if _, ok := got.CostUSD.Value(); ok {
			t.Error("cost_usd known without a response")
		}
	}
}

func TestEndChatUsesOnlyReportedCounts(t *testing.T) {
	rec := launchtest.Capture(t)
	_, l := begin(t)
	cost := 0.0
	l.EndChat(&protocoltypes.LLMResponse{Status: &protocoltypes.DispatchStatus{
		Model:        "m-2026",
		InputTokens:  7,
		OutputTokens: 0,
		CostUSD:      &cost,
		ToolCalls:    3,
		Reported:     protocoltypes.Reported{InputTokens: true, OutputTokens: true},
	}}, nil)
	got := rec.One()
	if got.Model != "m-2026" {
		t.Errorf("model = %q, want the served model", got.Model)
	}
	if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 7 {
		t.Errorf("input_tokens = %d", n)
	}
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 0 {
		t.Errorf("output_tokens = %d, want a reported 0", n)
	}
	launchtest.Unknown(t, "cache_read_tokens", got.CacheReadTokens)
	// A count without its Reported flag is not written.
	launchtest.Unknown(t, "tool_calls", got.ToolCalls)
	if c, ok := got.CostUSD.Value(); !ok || c != 0 {
		t.Errorf("cost_usd = %v (known %v), want a reported 0", c, ok)
	}
}

func TestHTTPStatus(t *testing.T) {
	rec := launchtest.Capture(t)
	_, l := begin(t)
	l.EndChat(nil, nil)
	launchtest.Unknown(t, "http_status", rec.One().HTTPStatus)

	rec = launchtest.Capture(t)
	ctx, l := begin(t)
	HTTPStatus(ctx, http.StatusTooManyRequests)
	l.EndChat(nil, errors.New("rate limited"))
	if n := launchtest.Known(t, "http_status", rec.One().HTTPStatus); n != http.StatusTooManyRequests {
		t.Errorf("http_status = %d, want 429", n)
	}

	// A context without a launch is ignored.
	HTTPStatus(context.Background(), http.StatusOK)
}

func TestBeginRejectedIdentity(t *testing.T) {
	rec := launchtest.Capture(t)
	long := strings.Repeat("p", accounting.MaxNameLength+1)
	ctx := context.Background()
	gotCtx, l, err := Begin(ctx, accounting.Identity{Agent: " bob ", Project: long}, spec)
	if !errors.Is(err, accounting.ErrNameTooLong) {
		t.Fatalf("err = %v, want ErrNameTooLong", err)
	}
	if l != nil || gotCtx != ctx {
		t.Error("a refused Begin returned a launch or a new context")
	}
	got := rec.One()
	if got.Agent != " bob " || got.Project != long {
		t.Errorf("identity = %q/%q, want the names as given", got.Agent, got.Project)
	}
	if got.Kind != spec.Kind || got.Provider != spec.Provider || got.Transport != spec.Transport || got.Model != spec.Model {
		t.Errorf("record = %+v", got)
	}
	if got.Success || got.Error != err.Error() {
		t.Errorf("success=%v error=%q, want failure %q", got.Success, got.Error, err.Error())
	}
	if got.Time.IsZero() {
		t.Error("time not set")
	}
	launchtest.Unknown(t, "http_status", got.HTTPStatus)
	launchtest.Unknown(t, "input_tokens", got.InputTokens)
}

func TestEndTaskNilUsage(t *testing.T) {
	rec := launchtest.Capture(t)
	_, l := begin(t)
	l.EndTask(TaskResult{}, nil)
	got := rec.One()
	if !got.Success {
		t.Error("success = false")
	}
	launchtest.Unknown(t, "characters", got.Characters)
	if _, ok := got.AudioSeconds.Value(); ok {
		t.Error("audio_seconds known without usage")
	}
}

func TestEndChatToolCalls(t *testing.T) {
	rec := launchtest.Capture(t)
	_, l := begin(t)
	l.EndChat(&protocoltypes.LLMResponse{Status: &protocoltypes.DispatchStatus{
		Reported: protocoltypes.Reported{ToolCalls: true},
	}}, nil)
	if n := launchtest.Known(t, "tool_calls", rec.One().ToolCalls); n != 0 {
		t.Errorf("tool_calls = %d, want a reported 0", n)
	}
}
