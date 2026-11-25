/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package anthropicmessages

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

// maxTokens is the max_tokens option the Messages API requires.
var maxTokens = map[string]any{"max_tokens": 64}

const messagesOK = `{"type":"message","role":"assistant","model":"claude-test-1",` +
	`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":7}}`

func TestChatRefusesInvalidIdentity(t *testing.T) {
	for _, r := range launchtest.Rejected {
		t.Run(r.Name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, hits := launchtest.Server(t, http.StatusOK, "application/json", messagesOK)
			_, err := NewProvider("k", url).Chat(context.Background(), r.ID, hello, nil, "claude-test", maxTokens)
			if !errors.Is(err, r.Want) {
				t.Fatalf("err = %v, want %v", err, r.Want)
			}
			if hits.Load() != 0 {
				t.Errorf("requests = %d, want 0", hits.Load())
			}
			rec.Refused("anthropic")
		})
	}
}

func TestChatAccountsSuccess(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json", messagesOK)
	if _, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	if !got.Success || got.Kind != accounting.KindText || got.Provider != "anthropic" ||
		got.Transport != accounting.TransportAPI || got.Model != "claude-test-1" || got.Project != accounting.DefaultProject {
		t.Errorf("record = %+v", got)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d", n)
	}
	for name, tc := range map[string]struct {
		v    accounting.Int
		want int64
	}{
		"input_tokens":          {got.InputTokens, 10},
		"output_tokens":         {got.OutputTokens, 5},
		"cache_read_tokens":     {got.CacheReadTokens, 0},
		"cache_creation_tokens": {got.CacheCreationTokens, 7},
	} {
		if n := launchtest.Known(t, name, tc.v); n != tc.want {
			t.Errorf("%s = %d, want %d", name, n, tc.want)
		}
	}
	if _, ok := got.CostUSD.Value(); ok {
		t.Error("cost_usd known, want unknown")
	}
	// The Messages API never returns images.
	launchtest.Unknown(t, "images", got.Images)
}

func TestChatPartialUsageLeavesRestUnknown(t *testing.T) {
	rec := launchtest.Capture(t)
	url, _ := launchtest.Server(t, http.StatusOK, "application/json",
		`{"type":"message","model":"claude-test-1","content":[{"type":"text","text":"hi"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	if _, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	launchtest.Known(t, "input_tokens", got.InputTokens)
	launchtest.Unknown(t, "cache_read_tokens", got.CacheReadTokens)
	launchtest.Unknown(t, "cache_creation_tokens", got.CacheCreationTokens)
}

func TestChatStreamingAccountsOnce(t *testing.T) {
	rec := launchtest.Capture(t)
	body := "event: message_start\ndata: " +
		`{"type":"message_start","message":{"model":"claude-test-1","usage":{"input_tokens":16,"output_tokens":1,"cache_read_input_tokens":4}}}` + "\n\n" +
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	url, _ := launchtest.Server(t, http.StatusOK, "text/event-stream", body)
	opts := map[string]any{"max_tokens": 64, common.TextDeltaOption: func(string) {}}
	if _, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", opts); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got := rec.One()
	if n := launchtest.Known(t, "input_tokens", got.InputTokens); n != 16 {
		t.Errorf("input_tokens = %d, want 16", n)
	}
	if n := launchtest.Known(t, "output_tokens", got.OutputTokens); n != 7 {
		t.Errorf("output_tokens = %d, want 7", n)
	}
	if n := launchtest.Known(t, "cache_read_tokens", got.CacheReadTokens); n != 4 {
		t.Errorf("cache_read_tokens = %d, want 4", n)
	}
	// message_start carried no cache_creation_input_tokens.
	launchtest.Unknown(t, "cache_creation_tokens", got.CacheCreationTokens)
}

func TestChatAccountsHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, status, "application/json", `{"type":"error","error":{"message":"no"}}`)
			if _, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens); err == nil {
				t.Fatal("Chat succeeded")
			}
			rec.Failed(status)
		})
	}
}

func TestChatAccountsConnectionError(t *testing.T) {
	rec := launchtest.Capture(t)
	if _, err := NewProvider("k", launchtest.ClosedURL(t)).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens); err == nil {
		t.Fatal("Chat succeeded")
	}
	rec.Failed(0)
}

func TestChatAccountsToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  int64
	}{
		{name: "two calls", want: 2, reply: `{"type":"message","model":"claude-test-1","stop_reason":"tool_use","content":[` +
			`{"type":"tool_use","id":"t1","name":"a","input":{}},{"type":"tool_use","id":"t2","name":"b","input":{}}],` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`},
		{name: "no calls is a known zero", want: 0, reply: messagesOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			url, _ := launchtest.Server(t, http.StatusOK, "application/json", tt.reply)
			if _, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens); err != nil {
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
	url, _ := launchtest.Server(t, http.StatusInternalServerError, "application/json", `{"type":"error"}`)
	if _, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens); err == nil {
		t.Fatal("Chat succeeded")
	}
	launchtest.Unknown(t, "tool_calls", rec.One().ToolCalls)
}
