package azure

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

const azureOK = `{"model":"gpt-test-0101","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`

func TestChatRefusesInvalidIdentity(t *testing.T) {
	for _, r := range launchtest.Rejected {
		t.Run(r.Name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json", azureOK)
			_, err := NewProvider("k", url, "").Chat(context.Background(), r.ID, hello, nil, "deploy", nil)
			if !errors.Is(err, r.Want) {
				t.Fatalf("err = %v, want %v", err, r.Want)
			}
			if hits.Load() != 0 {
				t.Errorf("requests = %d, want 0", hits.Load())
			}
			rec.Refused("azure")
		})
	}
}

func TestChatAccountsSuccess(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json", azureOK)
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "deploy", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	if !got.Success || got.Kind != accounting.KindText || got.Provider != "azure" ||
		got.Transport != accounting.TransportAPI || got.Model != "gpt-test-0101" || got.Project != accounting.DefaultProject {
		t.Errorf("record = %+v", got)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d", n)
	}
	if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 9 {
		t.Errorf("input_tokens = %d, want 9", n)
	}
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 4 {
		t.Errorf("output_tokens = %d, want 4", n)
	}
	// The response has no prompt_tokens_details: the cache counts are unknown.
	launchtest.Unknown(t, "cache_read_tokens", got.CacheReadTokens)
	launchtest.Unknown(t, "cache_creation_tokens", got.CacheCreationTokens)
	// Azure chat never returns images.
	launchtest.Unknown(t, "images", got.Images)
	if _, ok := got.CostUSD.Value(); ok {
		t.Error("cost_usd known, want unknown")
	}
}

func TestChatAccountsHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, status, "application/json", `{"error":{"message":"no"}}`)
			if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "deploy", nil); err == nil {
				t.Fatal("Chat succeeded")
			}
			rec.Failed(status)
		})
	}
}

func TestChatAccountsConnectionError(t *testing.T) {
	rec := launchtest.Capture(t)
	if _, err := NewProvider("k", launchtest.ClosedURL(t), "").Chat(context.Background(), testID, hello, nil, "deploy", nil); err == nil {
		t.Fatal("Chat succeeded")
	}
	rec.Failed(0)
}

func TestChatStreamErrorKeepsHTTPStatus(t *testing.T) {
	rec := launchtest.Capture(t)
	body := "data: " + `{"error":{"message":"overloaded"}}` + "\n\n"
	url, _ := launchtest.Server(t, http.StatusOK, "text/event-stream", body)
	opts := map[string]any{common.TextDeltaOption: func(string) {}}
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "deploy", opts); err == nil {
		t.Fatal("Chat succeeded")
	}
	// The stream failed in-band after a 200 response.
	rec.Failed(http.StatusOK)
}

func TestChatAccountsToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  int64
	}{
		{name: "one call", want: 1, reply: `{"choices":[{"message":{"content":"","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		{name: "no calls is a known zero", want: 0, reply: azureOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, http.StatusOK, "application/json", tt.reply)
			if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "deploy", nil); err != nil {
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
	url, _ := launchtest.Server(t, http.StatusTooManyRequests, "application/json", `{"error":{"message":"no"}}`)
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "deploy", nil); err == nil {
		t.Fatal("Chat succeeded")
	}
	launchtest.Unknown(t, "tool_calls", rec.One().ToolCalls)
}
