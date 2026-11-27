/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

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
		if got.Model != "m" || got.Agent != "alice" || got.OriginAgent != "alice" || got.BillingCode != accounting.DefaultBillingCode {
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

// An origin given is recorded, trimmed; none is the agent itself.
func TestBeginOrigin(t *testing.T) {
	for _, tt := range []struct {
		name   string
		origin string
		want   string
	}{
		{name: "defaults to agent", origin: "", want: "bob"},
		{name: "kept", origin: " alice ", want: "alice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			_, l, err := Begin(context.Background(), accounting.Identity{Origin: tt.origin, Agent: "bob"}, spec)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			l.EndChat(nil, nil)
			if got := rec.One(); got.OriginAgent != tt.want || got.Agent != "bob" {
				t.Errorf("origin, agent = %q, %q; want %q, bob", got.OriginAgent, got.Agent, tt.want)
			}
		})
	}
}

func TestBeginRejectedIdentity(t *testing.T) {
	rec := launchtest.Capture(t)
	long := strings.Repeat("p", accounting.MaxNameLength+1)
	ctx := context.Background()
	gotCtx, l, err := Begin(ctx, accounting.Identity{Origin: " alice ", Agent: " bob ", Name: " Bob ", BillingCode: long}, spec)
	if !errors.Is(err, accounting.ErrNameTooLong) {
		t.Fatalf("err = %v, want ErrNameTooLong", err)
	}
	if l != nil || gotCtx != ctx {
		t.Error("a refused Begin returned a launch or a new context")
	}
	got := rec.One()
	if got.OriginAgent != " alice " || got.Agent != " bob " || got.AgentName != " Bob " || got.BillingCode != long {
		t.Errorf("identity = %q/%q/%q/%q, want the names as given", got.OriginAgent, got.Agent, got.AgentName, got.BillingCode)
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

func TestEndTaskReportsUsage(t *testing.T) {
	rec := launchtest.Capture(t)
	ctx, l := begin(t)
	HTTPStatus(ctx, http.StatusOK)
	cost := 0.25
	l.EndTask(TaskResult{
		Usage: &protocoltypes.TaskUsage{
			InputTokens:  11,
			OutputTokens: 5,
			Seconds:      2.5,
			Characters:   42,
			Reported: protocoltypes.Reported{
				InputTokens: true, Seconds: true, Characters: true,
			},
		},
		CostUSD: &cost,
		Images:  accounting.KnownInt(2),
	}, nil)
	got := rec.One()
	if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 11 {
		t.Errorf("input_tokens = %d, want 11", n)
	}
	// OutputTokens was not reported, so its value is not written.
	launchtest.Unknown(t, "output_tokens", got.OutputTokens)
	if s, ok := got.AudioSeconds.Value(); !ok || s != 2.5 {
		t.Errorf("audio_seconds = %v (known %v), want 2.5", s, ok)
	}
	if n := launchtest.Known(t, "characters", got.Characters); n != 42 {
		t.Errorf("characters = %d, want 42", n)
	}
	if c, ok := got.CostUSD.Value(); !ok || c != 0.25 {
		t.Errorf("cost_usd = %v (known %v), want 0.25", c, ok)
	}
	if n := launchtest.Known(t, "images", got.Images); n != 2 {
		t.Errorf("images = %d, want 2", n)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d, want 200", n)
	}
}

func TestEndTaskUnreportedSeconds(t *testing.T) {
	rec := launchtest.Capture(t)
	_, l := begin(t)
	l.EndTask(TaskResult{Usage: &protocoltypes.TaskUsage{Seconds: 9}}, nil)
	if _, ok := rec.One().AudioSeconds.Value(); ok {
		t.Error("audio_seconds known although not reported")
	}
}

type taskOut struct{ chars int }

func TestRunTask(t *testing.T) {
	alice := accounting.Identity{Agent: "alice"}
	result := func(o *taskOut) TaskResult {
		return TaskResult{Usage: &protocoltypes.TaskUsage{
			Characters: o.chars, Reported: protocoltypes.Reported{Characters: true},
		}}
	}

	t.Run("success", func(t *testing.T) {
		rec := launchtest.Capture(t)
		out, err := RunTask(context.Background(), alice, spec,
			func(ctx context.Context) (*taskOut, error) {
				// The call's context carries the launch.
				HTTPStatus(ctx, http.StatusCreated)
				return &taskOut{chars: 3}, nil
			}, result)
		if err != nil || out == nil || out.chars != 3 {
			t.Fatalf("RunTask = %+v, %v", out, err)
		}
		got := rec.One()
		if !got.Success || got.Agent != "alice" {
			t.Errorf("record = %+v", got)
		}
		if n := launchtest.Known(t, "characters", got.Characters); n != 3 {
			t.Errorf("characters = %d, want 3", n)
		}
		if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusCreated {
			t.Errorf("http_status = %d, want 201", n)
		}
	})

	t.Run("call error with nil result", func(t *testing.T) {
		rec := launchtest.Capture(t)
		boom := errors.New("boom")
		out, err := RunTask(context.Background(), alice, spec,
			func(context.Context) (*taskOut, error) { return nil, boom },
			func(*taskOut) TaskResult {
				t.Error("result called for a nil output")
				return TaskResult{}
			})
		if out != nil || !errors.Is(err, boom) {
			t.Fatalf("RunTask = %+v, %v, want nil, boom", out, err)
		}
		got := rec.One()
		if got.Success || got.Error != "boom" {
			t.Errorf("record = %+v, want failure boom", got)
		}
		launchtest.Unknown(t, "characters", got.Characters)
	})

	t.Run("invalid identity runs nothing", func(t *testing.T) {
		rec := launchtest.Capture(t)
		bad := accounting.Identity{Agent: "bob", BillingCode: strings.Repeat("p", accounting.MaxNameLength+1)}
		out, err := RunTask(context.Background(), bad, spec,
			func(context.Context) (*taskOut, error) {
				t.Error("call ran for a refused identity")
				return &taskOut{}, nil
			}, result)
		if out != nil || !errors.Is(err, accounting.ErrNameTooLong) {
			t.Fatalf("RunTask = %+v, %v, want nil, ErrNameTooLong", out, err)
		}
		if got := rec.One(); got.Success {
			t.Errorf("record = %+v, want failure", got)
		}
	})
}
