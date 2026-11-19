package openai_responses

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// testID is the accounting identity the tests launch calls with.
var testID = accounting.Identity{Agent: "alice"}

var hello = []Message{{Role: "user", Content: "hi"}}

const responsesOK = `{"status":"completed","output":[{"type":"message","role":"assistant",` +
	`"content":[{"type":"output_text","text":"hi"}]}],` +
	`"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15,"input_tokens_details":{"cached_tokens":0}}}`

func TestChatRefusesInvalidIdentity(t *testing.T) {
	for _, r := range launchtest.Rejected {
		t.Run(r.Name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json", responsesOK)
			_, err := NewProvider("k", url, "").Chat(context.Background(), r.ID, hello, nil, "gpt-test", nil)
			if !errors.Is(err, r.Want) {
				t.Fatalf("err = %v, want %v", err, r.Want)
			}
			if hits.Load() != 0 {
				t.Errorf("requests = %d, want 0", hits.Load())
			}
			rec.Refused("openai-responses")
		})
	}
}

func TestChatAccountsSuccess(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json", responsesOK)
	if _, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-test", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	if !got.Success || got.Kind != accounting.KindText || got.Provider != "openai-responses" ||
		got.Transport != accounting.TransportAPI || got.Model != "gpt-test" || got.Project != accounting.DefaultProject {
		t.Errorf("record = %+v", got)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d", n)
	}
	if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 12 {
		t.Errorf("input_tokens = %d, want 12", n)
	}
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 3 {
		t.Errorf("output_tokens = %d, want 3", n)
	}
	if n := launchtest.Known(t, "cache_read_tokens", got.CacheReadTokens); n != 0 {
		t.Errorf("cache_read_tokens = %d, want a reported 0", n)
	}
	launchtest.Unknown(t, "cache_creation_tokens", got.CacheCreationTokens)
	if _, ok := got.CostUSD.Value(); ok {
		t.Error("cost_usd known, want unknown")
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
			rec.Failed(status)
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
