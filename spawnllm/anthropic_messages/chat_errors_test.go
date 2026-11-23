// spawnllm
// License: MIT

package anthropicmessages

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

func TestNewProviderWithTimeout_SetsClientTimeout(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		want    time.Duration
	}{
		{"positive timeout is used", 7, 7 * time.Second},
		{"zero falls back to default", 0, defaultRequestTimeout},
		{"negative falls back to default", -3, defaultRequestTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProviderWithTimeout("k", "", tt.seconds, nil)
			if got := p.httpClient.Timeout; got != tt.want {
				t.Errorf("timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithLogger_TemperatureDroppedAndLogged(t *testing.T) {
	var captured []map[string]any
	srv := jsonMessagesServer(t, []string{messagesOK}, &captured)
	var logs launchtest.Logs
	p := NewProvider("k", srv.URL, WithLogger(logs.Logger()))

	opts := map[string]any{"max_tokens": 64, "temperature": 0.3}
	if _, err := p.Chat(context.Background(), testID, hello, nil, "claude-test", opts); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("requests = %d, want 1", len(captured))
	}
	if _, ok := captured[0]["temperature"]; ok {
		t.Errorf("temperature sent to the API: %v", captured[0]["temperature"])
	}
	e, ok := logs.Find("temperature not sent to Anthropic")
	if !ok {
		t.Fatalf("no log record; got %+v", logs.Entries())
	}
	if e.Attrs["component"] != "anthropic_messages" {
		t.Errorf("component = %v, want anthropic_messages", e.Attrs["component"])
	}
	if e.Attrs["temperature"] != 0.3 {
		t.Errorf("temperature attr = %v, want 0.3", e.Attrs["temperature"])
	}
}

func TestChat_MaxTokensNumericTypes(t *testing.T) {
	tests := []struct {
		name string
		val  any
	}{
		{"int", 64},
		{"float64 from decoded JSON", float64(64)},
		{"int64", int64(64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured []map[string]any
			srv := jsonMessagesServer(t, []string{messagesOK}, &captured)
			opts := map[string]any{"max_tokens": tt.val}
			if _, err := NewProvider("k", srv.URL).Chat(context.Background(), testID, hello, nil, "claude-test", opts); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := captured[0]["max_tokens"]; got != float64(64) {
				t.Errorf("max_tokens = %v, want 64", got)
			}
		})
	}
}

func TestChat_RequestBuildErrors(t *testing.T) {
	unserializable := []ToolDefinition{{
		Type: "function",
		Function: ToolFunctionDefinition{
			Name:       "bad",
			Parameters: map[string]any{"type": "object", "x": func() {}},
		},
	}}
	tests := []struct {
		name    string
		apiBase string
		tools   []ToolDefinition
		opts    map[string]any
		wantErr string
	}{
		{"max_tokens missing", "https://api.example.com", nil, map[string]any{}, "building request body: max_tokens is required"},
		{"max_tokens wrong type", "https://api.example.com", nil, map[string]any{"max_tokens": "64"}, "building request body: max_tokens is required"},
		{"tool schema cannot be serialized", "https://api.example.com", unserializable, maxTokens, "serializing request body"},
		{"base URL cannot be parsed", "://no-scheme", nil, maxTokens, "building endpoint URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProvider("k", tt.apiBase).Chat(context.Background(), testID, hello, tt.tools, "claude-test", tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestChat_HTTPStatusErrors(t *testing.T) {
	tests := []struct {
		status  int
		wantErr string
	}{
		{http.StatusUnauthorized, "authentication failed (401): check your API key"},
		{http.StatusBadRequest, "bad request (400): body-text"},
		{http.StatusNotFound, "endpoint not found (404): body-text"},
		{http.StatusTooManyRequests, "rate limited (429): body-text"},
		{http.StatusInternalServerError, "internal server error (500): body-text"},
		{http.StatusServiceUnavailable, "service unavailable (503): body-text"},
		{http.StatusTeapot, "API request failed with status 418: body-text"},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			url, _ := launchtest.Server(t, tt.status, "application/json", "body-text")
			out, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "error" {
				t.Fatalf("status = %+v, want failed with stop reason error", out)
			}
			if out.Status.BytesReceived != int64(len("body-text")) {
				t.Errorf("BytesReceived = %d, want %d", out.Status.BytesReceived, len("body-text"))
			}
		})
	}
}

func TestChat_MalformedResponseIsParseError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON", "not json"},
		{"usage counts of the wrong type", `{"type":"message","content":[],"usage":{"input_tokens":"many"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, _ := launchtest.Server(t, http.StatusOK, "application/json", tt.body)
			out, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens)
			if err == nil || !strings.Contains(err.Error(), "parsing JSON response") {
				t.Fatalf("err = %v, want parsing JSON response", err)
			}
			if out == nil || out.Status == nil || out.Status.StopReason != "parse_error" || out.Status.Success {
				t.Fatalf("status = %+v, want failed parse_error", out)
			}
		})
	}
}

// truncatingServer declares a longer body than it sends, so the client's read
// ends with an unexpected EOF.
func truncatingServer(t *testing.T, contentType, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestChat_TruncatedBody(t *testing.T) {
	url := truncatingServer(t, "application/json", `{"type":"mess`)
	out, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", maxTokens)
	if err == nil || !strings.Contains(err.Error(), "reading response body") {
		t.Fatalf("err = %v, want reading response body", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.BytesReceived == 0 {
		t.Fatalf("status = %+v, want failed with the bytes received", out)
	}
}

func TestChat_StreamingTruncated(t *testing.T) {
	body := `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"part"}}` + "\n\n"
	url := truncatingServer(t, "text/event-stream", body)
	var deltas []string
	opts := map[string]any{"max_tokens": 64, common.TextDeltaOption: func(d string) { deltas = append(deltas, d) }}
	out, err := NewProvider("k", url).Chat(context.Background(), testID, hello, nil, "claude-test", opts)
	if err == nil || !strings.Contains(err.Error(), "reading stream") {
		t.Fatalf("err = %v, want reading stream", err)
	}
	if out == nil || out.Status == nil || out.Status.Success {
		t.Fatalf("status = %+v, want failed", out)
	}
	if strings.Join(deltas, "") != "part" {
		t.Errorf("deltas = %q, want part", deltas)
	}
}

func TestChat_StreamingSkipsNoiseAndHandlesArglessTool(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":3}}}`,
		`data: [DONE-not-json]`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"now"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"stop_sequence"},"usage":{"output_tokens":2}}`,
	}, "\n\n") + "\n\n"
	srv := sseMessagesServer(t, body, nil)
	opts := map[string]any{"max_tokens": 64, common.TextDeltaOption: func(string) {}}
	out, err := NewProvider("k", srv.URL).Chat(context.Background(), testID, hello, nil, "claude-test", opts)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", out.ToolCalls)
	}
	tc := out.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Name != "now" || len(tc.Arguments) != 0 || tc.Function.Arguments != "{}" {
		t.Errorf("tool call = %+v / %+v, want toolu_1 now with empty arguments", tc, tc.Function)
	}
	if out.FinishReason != "stop" || !out.Normal || out.Status.StopReason != "stop_sequence" {
		t.Errorf("finish = %q normal=%v stop=%q, want stop/true/stop_sequence", out.FinishReason, out.Normal, out.Status.StopReason)
	}
	if out.Status.InputTokens != 3 || out.Status.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 3/2", out.Status.InputTokens, out.Status.OutputTokens)
	}
}

func TestStreamEventErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"absent", "", "messages stream error"},
		{"object with message", `{"type":"overloaded_error","message":"busy"}`, "busy"},
		{"bare string", `"gone away"`, "gone away"},
		{"object without message", `{"type":"x"}`, `{"type":"x"}`},
		{"empty string", `""`, `""`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamEventErrorMessage(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeBaseURL_BareV1(t *testing.T) {
	for _, in := range []string{"/v1", "/v1/", "  /v1  "} {
		if got := normalizeBaseURL(in); got != defaultBaseURL {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, defaultBaseURL)
		}
	}
}

// TestChat_StreamingKeepsUndecodableToolArguments checks that streamed tool
// call arguments that do not decode (cut short here) are kept under "raw" and
// logged, as on the OpenAI-compatible path, rather than becoming {}.
func TestChat_StreamingKeepsUndecodableToolArguments(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-test","usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"search"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"bo"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":2}}`,
	}, "\n\n") + "\n\n"
	srv := sseMessagesServer(t, body, nil)
	logs := &launchtest.Logs{}
	opts := map[string]any{"max_tokens": 64, common.TextDeltaOption: func(string) {}}
	out, err := NewProvider("k", srv.URL, WithLogger(logs.Logger())).
		Chat(context.Background(), testID, hello, nil, "claude-test", opts)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(out.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", out.ToolCalls)
	}
	tc := out.ToolCalls[0]
	if tc.Name != "search" || len(tc.Arguments) != 1 || tc.Arguments["raw"] != `{"q":"bo` {
		t.Errorf("arguments = %v, want the text kept under raw", tc.Arguments)
	}
	e, ok := logs.Find("failed to decode tool call arguments payload")
	if !ok || e.Level != slog.LevelWarn || e.Attrs["name"] != "search" {
		t.Errorf("entry = %+v, found %v; want the warning naming the tool", e, ok)
	}
}
