package openai_compat

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
	"go.bison9.com/agent/spawnllm/protocoltypes"
)

var weatherTool = []ToolDefinition{{
	Type:     "function",
	Function: protocoltypes.ToolFunctionDefinition{Name: "get_weather", Parameters: map[string]any{"type": "object"}},
}}

// captureCompat returns a provider pointed at a server that records the last
// request body and replies with okReply.
func captureCompat(t *testing.T, opts ...Option) (*Provider, *map[string]any) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = nil
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(okReply)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return NewProvider("key", srv.URL, "", opts...), &body
}

// fakeTransport answers every request with status, contentType and a body
// that yields data and then fails with io.ErrUnexpectedEOF, without a network.
func fakeTransport(status int, contentType, data string) http.RoundTripper {
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       &errAfterDataReadCloser{data: []byte(data)},
			Request:    r,
		}, nil
	})
}

func TestAccessors(t *testing.T) {
	p := NewProvider("key", "https://example.com/v1", "",
		WithResponseLogFile("/var/log/x.jsonl"), WithReasoningEffort("high"))
	if p.ResponseLogFile() != "/var/log/x.jsonl" {
		t.Errorf("ResponseLogFile() = %q", p.ResponseLogFile())
	}
	if p.ReasoningEffort() != "high" {
		t.Errorf("ReasoningEffort() = %q", p.ReasoningEffort())
	}
}

func TestWithDropParams_NothingToDrop(t *testing.T) {
	for _, names := range [][]string{nil, {}, {"", ""}} {
		if p := NewProvider("key", "https://example.com/v1", "", WithDropParams(names)); p.dropParams != nil {
			t.Errorf("WithDropParams(%q): dropParams = %v, want nil", names, p.dropParams)
		}
	}
}

func TestProviderChat_NoParallelToolCalls(t *testing.T) {
	p, body := captureCompat(t, WithNoParallelToolCalls(true))
	if _, err := p.Chat(t.Context(), testID, hello, weatherTool, "gpt-4o", nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if v, ok := (*body)["parallel_tool_calls"]; !ok || v != false {
		t.Fatalf("parallel_tool_calls = %v (present %v), want false", v, ok)
	}
	if _, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o", nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if _, ok := (*body)["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls sent without tools")
	}
}

func TestProviderChat_StrictAlternationRewritesWire(t *testing.T) {
	p, body := captureCompat(t, WithStrictAlternation(true))
	msgs := []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "weather?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather"}}},
		{Role: "tool", ToolCallID: "c1", Content: "sunny"},
	}
	if _, err := p.Chat(t.Context(), testID, msgs, nil, "gemma", nil); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	sent := asType[[]any](t, (*body)["messages"], "messages")
	got := make([]string, 0, len(sent))
	for _, m := range sent {
		msg := asType[map[string]any](t, m, "message")
		role := asType[string](t, msg["role"], "role")
		content := asType[string](t, msg["content"], "content")
		got = append(got, role+": "+content)
		if _, ok := msg["tool_calls"]; ok {
			t.Errorf("message %v keeps tool_calls", msg)
		}
	}
	want := []string{
		"user: be brief\n\nweather?",
		"assistant: (requested tool calls)",
		"user: Tool result:\nsunny",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

func TestProviderChat_RequestNotSent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if _, err := w.Write([]byte(okReply)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name    string
		apiBase string
		options map[string]any
		wantErr string
	}{
		{name: "no API base", wantErr: "API base not configured"},
		{name: "unencodable request", apiBase: srv.URL, options: map[string]any{"temperature": math.Inf(1)}, wantErr: "failed to marshal request"},
		{name: "invalid API base", apiBase: "http://bad host", wantErr: "failed to create request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProvider("key", tt.apiBase, "").Chat(t.Context(), testID, hello, nil, "gpt-4o", tt.options)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestProviderChat_LoggedBodyReadError(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "rr.jsonl")
	p := NewProvider("key", "https://example.com/v1", "", WithResponseLogFile(logPath))
	p.httpClient = &http.Client{Transport: fakeTransport(http.StatusOK, "application/json", `{"choi`)}

	out, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to read response body") {
		t.Fatalf("err = %v, want read error", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "error" {
		t.Fatalf("out = %+v, want failed error status", out)
	}
	if out.Status.BytesReceived != int64(len(`{"choi`)) {
		t.Errorf("BytesReceived = %d, want the bytes read", out.Status.BytesReceived)
	}
	if recs := parseLogFile(t, logPath); len(recs) != 1 || recs[0].Dir != "request" {
		t.Errorf("log = %+v, want the request alone", recs)
	}
}

func TestProviderChat_StreamTruncatedIsReadError(t *testing.T) {
	chunk := "data: " + `{"choices":[{"delta":{"content":"par"}}]}` + "\n\n"
	p := NewProvider("key", "https://example.com/v1", "")
	p.httpClient = &http.Client{Transport: fakeTransport(http.StatusOK, "text/event-stream", chunk)}

	var deltas []string
	out, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o",
		map[string]any{common.TextDeltaOption: func(d string) { deltas = append(deltas, d) }})
	if err == nil || !strings.Contains(err.Error(), "failed to read stream") {
		t.Fatalf("err = %v, want stream read error", err)
	}
	// A broken stream is a read failure, not a parse failure.
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "error" {
		t.Fatalf("out = %+v, want a failed error status", out)
	}
	if out.Status.BytesReceived != int64(len(chunk)) {
		t.Errorf("BytesReceived = %d, want %d", out.Status.BytesReceived, len(chunk))
	}
	if strings.Join(deltas, "") != "par" {
		t.Errorf("deltas = %q, want par", strings.Join(deltas, ""))
	}
}

func TestProviderChat_StreamLoggedAndModelDefaulted(t *testing.T) {
	body := "data: " + `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	server := sseServer(t, body, nil)
	t.Cleanup(server.Close)

	logPath := filepath.Join(t.TempDir(), "rr.jsonl")
	p := NewProvider("key", server.URL, "", WithResponseLogFile(logPath))
	out, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o",
		map[string]any{common.TextDeltaOption: func(string) {}})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if out.Status == nil || out.Status.Model != "gpt-4o" {
		t.Errorf("Status = %+v, want Model gpt-4o when the stream names none", out.Status)
	}

	recs := parseLogFile(t, logPath)
	if len(recs) != 2 {
		t.Fatalf("log = %+v, want request and response", recs)
	}
	if recs[1].Dir != "response" || recs[1].Status != http.StatusOK || recs[1].CorrID != recs[0].CorrID {
		t.Errorf("response record = %+v, want 200 paired with the request", recs[1])
	}
	var logged map[string]any
	if err := json.Unmarshal(recs[1].Body, &logged); err != nil {
		t.Fatalf("logged body is not a JSON object: %s", recs[1].Body)
	}
	if !strings.Contains(string(recs[1].Body), `"ok"`) {
		t.Errorf("logged body = %s, want the reconstructed response", recs[1].Body)
	}
}

func TestResponseLogFile_NonJSONBodyLoggedAsString(t *testing.T) {
	const page = "<html>bad gateway</html>"
	logPath := filepath.Join(t.TempDir(), "rr.jsonl")
	p := NewProvider("key", "https://example.com/v1", "", WithResponseLogFile(logPath))
	p.httpClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(page)),
			Request:    r,
		}, nil
	})}
	if _, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o", nil); err == nil {
		t.Fatal("expected an error from a 502")
	}
	recs := parseLogFile(t, logPath)
	if len(recs) != 2 {
		t.Fatalf("log = %+v, want request and response", recs)
	}
	var got string
	if err := json.Unmarshal(recs[1].Body, &got); err != nil || got != page {
		t.Fatalf("logged body = %s (%v), want the page as a JSON string", recs[1].Body, err)
	}
}

func TestResponseLogFile_OpenFailureWarnsOnceAndIsSticky(t *testing.T) {
	p, _ := captureCompat(t)
	logs := &launchtest.Logs{}
	dir := t.TempDir()
	p.responseLogFile = dir // a directory: opening it for append fails
	p.logger = logs.Logger()

	for i := range 3 {
		if _, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o", nil); err != nil {
			t.Fatalf("Chat #%d error = %v", i+1, err)
		}
	}
	var warns int
	for _, w := range warnEntries(logs) {
		if strings.Contains(w.Message, "response_log_file open failed") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("open-failure warnings = %d, want 1", warns)
	}
	if p.logFile != nil || p.logOpenErr == nil {
		t.Fatalf("logFile = %v, logOpenErr = %v; want the failure kept", p.logFile, p.logOpenErr)
	}
}

func TestRedactAPIKey_LeavesOtherBodiesUnchanged(t *testing.T) {
	for _, body := range []string{`not json`, `[1,2]`, `{"model":"m"}`} {
		if got := string(redactAPIKey([]byte(body))); got != body {
			t.Errorf("redactAPIKey(%s) = %s, want unchanged", body, got)
		}
	}
}

func TestNormalizeAlternation_SystemTurnsJoinedAndEmptyToolResult(t *testing.T) {
	got := normalizeAlternation([]Message{
		{Role: "system", Content: "one"},
		{Role: "developer", Content: "two"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "calling"},
		{Role: "tool", ToolCallID: "c1"},
	})
	want := []Message{
		{Role: "user", Content: "one\n\ntwo\n\nhi"},
		{Role: "assistant", Content: "calling"},
		{Role: "user", Content: "Tool result:\n(tool result)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeAlternation = %+v, want %+v", got, want)
	}
}

func TestSerializeMessages_MediaKeepsToolCallsAndReasoning(t *testing.T) {
	img := "data:image/png;base64,AAAA"
	out := serializeMessages([]Message{{
		Role:             "assistant",
		Content:          "see",
		Media:            []string{img},
		ReasoningContent: "because",
		ToolCalls:        []ToolCall{{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "Paris"}}},
	}}, false, nil)
	msg := asType[map[string]any](t, out[0], "message")
	if msg["reasoning_content"] != "because" {
		t.Errorf("reasoning_content = %v", msg["reasoning_content"])
	}
	calls := asType[[]ToolCall](t, msg["tool_calls"], "tool_calls")
	if len(calls) != 1 || calls[0].Function == nil || calls[0].Function.Name != "get_weather" ||
		calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("tool_calls = %+v, want the call in wire form", calls)
	}
	parts := asType[[]map[string]any](t, msg["content"], "content")
	if len(parts) != 2 || parts[0]["text"] != "see" || parts[1]["type"] != "image_url" {
		t.Errorf("content = %v", parts)
	}

	// strict compat drops reasoning_content from a multipart message too.
	out = serializeMessages([]Message{{Role: "assistant", Media: []string{img}, ReasoningContent: "because"}}, true, nil)
	if _, ok := asType[map[string]any](t, out[0], "message")["reasoning_content"]; ok {
		t.Error("strict compat kept reasoning_content")
	}
}

func TestHostChecksRejectUnparsableBase(t *testing.T) {
	if supportsPromptCacheKey("://bad") {
		t.Error("supportsPromptCacheKey on an unparsable base = true")
	}
	if isOpenRouterHost("://bad") {
		t.Error("isOpenRouterHost on an unparsable base = true")
	}
}

func TestMergeProviderObject_NonObjectsSentAsOperatorSet(t *testing.T) {
	managed := map[string]any{"require_parameters": true}
	if got := mergeProviderObject("fast", managed); got != "fast" {
		t.Errorf("non-object operator value = %v, want it unchanged", got)
	}
	user := map[string]any{"order": []any{"a"}}
	got := asType[map[string]any](t, mergeProviderObject(user, nil), "merged")
	if !reflect.DeepEqual(got, user) {
		t.Errorf("merged with no managed object = %v, want %v", got, user)
	}
}

func TestWriteLogEntry_DisabledWritesNothing(t *testing.T) {
	p := NewProvider("key", "https://example.com/v1", "")
	p.writeLogEntry(logEntry{Dir: "request", Body: json.RawMessage(`{}`)})
	if p.logFile != nil || p.logOpenErr != nil {
		t.Fatalf("logFile = %v, logOpenErr = %v; want nothing opened", p.logFile, p.logOpenErr)
	}
}

func TestProviderChat_StreamMalformedIsParseError(t *testing.T) {
	stream := "data: " + `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"choices":[],"usage":"not an object"}` + "\n\n" +
		"data: [DONE]\n\n"
	p := NewProvider("key", "https://example.com/v1", "")
	p.httpClient = &http.Client{Transport: fakeTransport(http.StatusOK, "text/event-stream", stream)}

	out, err := p.Chat(t.Context(), testID, hello, nil, "gpt-4o",
		map[string]any{common.TextDeltaOption: func(string) {}})
	if err == nil || errors.Is(err, common.ErrStreamRead) {
		t.Fatalf("err = %v, want a parse error", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "parse_error" {
		t.Fatalf("out = %+v, want a failed parse_error status", out)
	}
}
