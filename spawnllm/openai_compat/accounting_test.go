package openai_compat

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// testID is the accounting identity the tests launch calls with.
var testID = accounting.Identity{Agent: "alice"}

var hello = []Message{{Role: "user", Content: "hi"}}

func TestChatRefusesInvalidIdentity(t *testing.T) {
	for _, r := range launchtest.Rejected {
		t.Run(r.Name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json", okReply)
			_, err := NewProvider("k", url, "").Chat(context.Background(), r.ID, hello, nil, "gpt-test", nil)
			if !errors.Is(err, r.Want) {
				t.Fatalf("err = %v, want %v", err, r.Want)
			}
			if hits.Load() != 0 {
				t.Errorf("requests = %d, want 0", hits.Load())
			}
			got := rec.Refused("openai-chat")
			if got.Kind != accounting.KindText || got.Transport != accounting.TransportAPI {
				t.Errorf("record = %s/%s", got.Kind, got.Transport)
			}
		})
	}
}

func TestChatAccountsSuccess(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json",
		`{"model":"gpt-test-0101","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":2}}}`)
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	if !got.Success || got.Kind != accounting.KindText || got.Provider != "openai-chat" ||
		got.Transport != accounting.TransportAPI || got.Model != "gpt-test-0101" {
		t.Errorf("record = %+v", got)
	}
	if got.Agent != "alice" || got.Project != accounting.DefaultProject {
		t.Errorf("identity = %q/%q", got.Agent, got.Project)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d", n)
	}
	if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 10 {
		t.Errorf("input_tokens = %d, want 10", n)
	}
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 0 {
		t.Errorf("output_tokens = %d, want a reported 0", n)
	}
	if n := launchtest.Known(t, "cache_read_tokens", got.CacheReadTokens); n != 2 {
		t.Errorf("cache_read_tokens = %d, want 2", n)
	}
	// Not reported by the response, and no cost from a host other than
	// OpenRouter.
	launchtest.Unknown(t, "cache_creation_tokens", got.CacheCreationTokens)
	if _, ok := got.CostUSD.Value(); ok {
		t.Error("cost_usd known, want unknown")
	}
}

func TestChatWithoutUsageLeavesTokensUnknown(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json", okReply)
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	launchtest.Unknown(t, "input_tokens", got.InputTokens)
	launchtest.Unknown(t, "output_tokens", got.OutputTokens)
	launchtest.Unknown(t, "cache_read_tokens", got.CacheReadTokens)
}

func TestChatAccountsOpenRouterCost(t *testing.T) {
	rec := launchtest.Capture(t)
	chatVia(t, "https://openrouter.ai/api/v1", "openai/gpt-test", nil,
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"cost":0.00123}}`)
	cost, ok := rec.One().CostUSD.Value()
	if !ok || cost != 0.00123 {
		t.Errorf("cost_usd = %v (known %v), want 0.00123", cost, ok)
	}
}

func TestChatAccountsHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, status, "application/json", `{"error":{"message":"no"}}`)
			if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err == nil {
				t.Fatal("Chat succeeded")
			}
			got := rec.Failed(status)
			launchtest.Unknown(t, "input_tokens", got.InputTokens)
		})
	}
}

func TestChatAccountsConnectionError(t *testing.T) {
	rec := launchtest.Capture(t)
	if _, err := NewProvider("k", launchtest.ClosedURL(t), "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err == nil {
		t.Fatal("Chat succeeded")
	}
	rec.Failed(0)
}

func TestChatStreamingAccountsOnce(t *testing.T) {
	rec := launchtest.Capture(t)
	body := "data: " + `{"model":"gpt-test","choices":[{"delta":{"content":"Hel"}}]}` + "\n\n" +
		"data: " + `{"model":"gpt-test","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"model":"gpt-test","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	url, _ := launchtest.Server(t, http.StatusOK, "text/event-stream", body)
	opts := map[string]any{common.TextDeltaOption: func(string) {}}
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", opts); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 1 {
		t.Errorf("output_tokens = %d, want 1", n)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d", n)
	}
}

func TestChatAccountsToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  int64
	}{
		{name: "two calls", want: 2, reply: `{"choices":[{"message":{"content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}},` +
			`{"id":"c2","type":"function","function":{"name":"b","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		{name: "no calls is a known zero", want: 0, reply: okReply},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, http.StatusOK, "application/json", tt.reply)
			if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if n := launchtest.Known(t, "tool_calls", rec.One().ToolCalls); n != tt.want {
				t.Errorf("tool_calls = %d, want %d", n, tt.want)
			}
		})
	}
}

func TestChatErrorLeavesToolCallsUnknown(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusInternalServerError, "application/json", `{"error":{"message":"no"}}`)
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err == nil {
		t.Fatal("Chat succeeded")
	}
	launchtest.Unknown(t, "tool_calls", rec.One().ToolCalls)
}
