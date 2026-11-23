// spawnllm
// License: MIT

package openai_responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/protocoltypes"
)

const okReply = `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`

var weatherTool = []ToolDefinition{{
	Type:     "function",
	Function: protocoltypes.ToolFunctionDefinition{Name: "get_weather", Parameters: map[string]any{"type": "object"}},
}}

// countingServer replies with status and body and counts the requests it gets.
func countingServer(t *testing.T, status int, body string) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

// truncatedServer sends body under a Content-Length larger than body, so the
// client's read ends in an unexpected EOF.
func truncatedServer(t *testing.T, contentType, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func streamOpt(deltas *[]string) map[string]any {
	return map[string]any{common.TextDeltaOption: func(d string) {
		if deltas != nil {
			*deltas = append(*deltas, d)
		}
	}}
}

func TestWithRequestTimeout(t *testing.T) {
	if got := NewProvider("k", "http://x", "", WithRequestTimeout(42*time.Second)).httpClient.Timeout; got != 42*time.Second {
		t.Errorf("timeout = %v, want 42s", got)
	}
	def := NewProvider("k", "http://x", "").httpClient.Timeout
	if got := NewProvider("k", "http://x", "", WithRequestTimeout(0)).httpClient.Timeout; got != def {
		t.Errorf("zero timeout: got %v, want the default %v", got, def)
	}
}

func TestWithLoggerTagsComponent(t *testing.T) {
	var buf bytes.Buffer
	p := NewProvider("k", "http://x", "", WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	p.log().Info("hello")
	if !strings.Contains(buf.String(), "component=openai_responses") {
		t.Fatalf("log output = %q, want component=openai_responses", buf.String())
	}
}

func TestGetDefaultModelIsEmpty(t *testing.T) {
	if got := NewProvider("k", "http://x", "").GetDefaultModel(); got != "" {
		t.Fatalf("GetDefaultModel() = %q, want empty", got)
	}
}

func TestChat_NoParallelToolCalls(t *testing.T) {
	p, body := captureServer(t, okReply, WithNoParallelToolCalls(true))
	if _, err := p.Chat(context.Background(), testID, hello, weatherTool, "gpt-5", nil); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if v, ok := (*body)["parallel_tool_calls"]; !ok || v != false {
		t.Fatalf("parallel_tool_calls = %v (present %v), want false", v, ok)
	}

	// Without tools there is nothing to parallelise and the field is not sent.
	p, body = captureServer(t, okReply, WithNoParallelToolCalls(true))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", nil); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if _, ok := (*body)["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls sent without tools")
	}
}

func TestChat_ExtraBodyMergedButNeverOverridesManagedFields(t *testing.T) {
	var buf bytes.Buffer
	p, body := captureServer(t, okReply,
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))),
		WithModelLabel("Alice's model"),
		WithExtraBody(map[string]any{"model": "evil", "plugins": []any{"web"}}))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", nil); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if (*body)["model"] != "gpt-5" {
		t.Errorf("model = %v, want gpt-5 (extra_body must not override it)", (*body)["model"])
	}
	plugins := asType[[]any](t, (*body)["plugins"], "plugins")
	if len(plugins) != 1 || plugins[0] != "web" {
		t.Errorf("plugins = %v, want [web]", plugins)
	}
	if !strings.Contains(buf.String(), "key=model") || !strings.Contains(buf.String(), `model="Alice's model"`) {
		t.Errorf("log = %q, want the colliding key named with the model label", buf.String())
	}
}

func TestWithDropParams_BlankAndEmpty(t *testing.T) {
	if p := NewProvider("k", "http://x", "", WithDropParams(nil)); p.dropParams != nil {
		t.Errorf("nil list: dropParams = %v, want nil", p.dropParams)
	}
	p, body := captureServer(t, okReply, WithDropParams([]string{" store ", "", "  "}))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", nil); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if _, ok := (*body)["store"]; ok {
		t.Error("store sent, want it dropped (name is trimmed)")
	}
	if len(p.dropParams) != 1 {
		t.Errorf("dropParams = %v, want only store", p.dropParams)
	}
}

func TestChat_JSONObjectCapableOverride(t *testing.T) {
	p, body := captureServer(t, okReply, WithProtocol("openrouter"), WithResponseFormatJSONCapable(true))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5",
		map[string]any{ResponseFormatJSONObjectOption: true}); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	text := asType[map[string]any](t, (*body)["text"], "text")
	format := asType[map[string]any](t, text["format"], "text.format")
	if format["type"] != "json_object" {
		t.Fatalf("text.format = %v, want json_object", format)
	}
}

func TestWantsJSONObjectOnlyForBoolTrue(t *testing.T) {
	tests := []struct {
		v    any
		want bool
	}{{true, true}, {false, false}, {"true", false}, {nil, false}}
	for _, tt := range tests {
		if got := wantsJSONObject(map[string]any{ResponseFormatJSONObjectOption: tt.v}); got != tt.want {
			t.Errorf("wantsJSONObject(%v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestChat_RequestNotSent(t *testing.T) {
	url, hits := countingServer(t, http.StatusOK, okReply)
	tests := []struct {
		name    string
		apiBase string
		options map[string]any
		wantErr string
	}{
		{name: "no API base", apiBase: "", wantErr: "API base not configured"},
		{name: "unencodable request", apiBase: url, options: map[string]any{"temperature": math.NaN()}, wantErr: "failed to marshal request"},
		{name: "invalid API base", apiBase: "http://bad host", wantErr: "failed to create request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewProvider("k", tt.apiBase, "").Chat(context.Background(), testID, hello, nil, "gpt-5", tt.options)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestChat_TruncatedBodyIsReadError(t *testing.T) {
	url := truncatedServer(t, "application/json", `{"status":"compl`)
	out, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-5", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to read response body") {
		t.Fatalf("err = %v, want read error", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "error" {
		t.Fatalf("out = %+v, want failed error status", out)
	}
	if out.Status.BytesReceived != int64(len(`{"status":"compl`)) {
		t.Errorf("BytesReceived = %d, want the bytes read", out.Status.BytesReceived)
	}
}

func TestChat_MalformedBodyIsParseError(t *testing.T) {
	url, _ := countingServer(t, http.StatusOK, `{"output":`)
	out, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-5", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to parse Responses API body") {
		t.Fatalf("err = %v, want parse error", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "parse_error" {
		t.Fatalf("out = %+v, want parse_error status", out)
	}
}

// readLog returns the JSONL records of a response log file.
func readLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var recs []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", sc.Text(), err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestChat_ResponseLogRecordsRequestAndResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	p, _ := captureServer(t, okReply, WithResponseLogFile(path))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", nil); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	recs := readLog(t, path)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want request and response", len(recs))
	}
	req, resp := recs[0], recs[1]
	if req["dir"] != "request" || req["model"] != "gpt-5" || req["status"] != 0.0 {
		t.Errorf("request record = %v", req)
	}
	if _, ok := req["duration_ms"]; ok {
		t.Errorf("request record has duration_ms: %v", req)
	}
	if reqBody := asType[map[string]any](t, req["body"], "request body"); reqBody["model"] != "gpt-5" {
		t.Errorf("request body = %v, want the sent request", reqBody)
	}
	if resp["dir"] != "response" || resp["status"] != 200.0 {
		t.Errorf("response record = %v", resp)
	}
	if respBody := asType[map[string]any](t, resp["body"], "response body"); respBody["status"] != "completed" {
		t.Errorf("response body = %v, want the received response", respBody)
	}
}

func TestChat_StreamingResponseLogRecordsCompletedResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	body := `data: {"type":"response.completed","response":` + okReply + "}\n\n"
	srv := sseResponsesServer(t, body, nil)
	p := NewProvider("k", srv.URL, "", WithResponseLogFile(path))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", streamOpt(nil)); err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	recs := readLog(t, path)
	if len(recs) != 2 || recs[1]["dir"] != "response" {
		t.Fatalf("records = %v, want request then response", recs)
	}
	if got := asType[map[string]any](t, recs[1]["body"], "response body"); got["status"] != "completed" {
		t.Errorf("response body = %v, want the completed response object", got)
	}
}

func TestAppendLog_UnwritableLocationDoesNotFailChat(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, path string }{
		{name: "parent is a file", path: filepath.Join(file, "log.jsonl")},
		{name: "log path is a directory", path: dir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := captureServer(t, okReply, WithResponseLogFile(tt.path))
			out, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", nil)
			if err != nil || out.Content != "ok" {
				t.Fatalf("Chat = %+v, %v; want ok despite the log failure", out, err)
			}
		})
	}
	if info, err := os.Stat(file); err != nil || info.Size() != 0 || info.IsDir() {
		t.Fatalf("blocking file changed: %v %v", info, err)
	}
}

func TestChat_StreamingEdgeCases(t *testing.T) {
	completed := `data: {"type":"response.completed","response":` + okReply + "}\n\n"
	tests := []struct {
		name       string
		body       string
		wantErr    string
		wantStop   string
		wantStatus bool // the error is a provider error (HTTPStatusError)
		wantText   string
	}{
		{
			name: "framing noise and empty completed are ignored",
			body: "data: not json\n\n" +
				`data: {"type":"response.completed"}` + "\n\n" +
				completed + "data: [DONE]\n\n",
			wantText: "ok",
		},
		{
			name:     "DONE without completed",
			body:     `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\ndata: [DONE]\n\n",
			wantErr:  "stream ended without response.completed event",
			wantStop: "parse_error",
		},
		{
			name:     "completed response is not an object",
			body:     `data: {"type":"response.completed","response":"oops"}` + "\n\n",
			wantErr:  "failed to parse Responses API body",
			wantStop: "parse_error",
		},
		{
			name:       "error event with top-level message",
			body:       `data: {"type":"error","message":"rate limited"}` + "\n\n",
			wantErr:    "rate limited",
			wantStop:   "error",
			wantStatus: true,
		},
		{
			name:       "error event without a message",
			body:       `data: {"type":"response.error"}` + "\n\n",
			wantErr:    "responses stream error",
			wantStop:   "error",
			wantStatus: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := sseResponsesServer(t, tt.body, nil)
			out, err := NewProvider("k", srv.URL, "").Chat(context.Background(), testID, hello, nil, "gpt-5", streamOpt(nil))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Chat error: %v", err)
				}
				if out.Content != tt.wantText || !out.Status.Success {
					t.Fatalf("out = %+v, want %q and success", out, tt.wantText)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if _, isStatus := errors.AsType[*common.HTTPStatusError](err); isStatus != tt.wantStatus {
				t.Errorf("err type %T, want HTTPStatusError %v", err, tt.wantStatus)
			}
			if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != tt.wantStop {
				t.Fatalf("out = %+v, want failed %s status", out, tt.wantStop)
			}
			if out.Status.BytesReceived != int64(len(tt.body)) {
				t.Errorf("BytesReceived = %d, want %d", out.Status.BytesReceived, len(tt.body))
			}
		})
	}
}

func TestChat_StreamingTruncatedIsReadError(t *testing.T) {
	chunk := `data: {"type":"response.output_text.delta","delta":"par"}` + "\n\n"
	url := truncatedServer(t, "text/event-stream", chunk)
	var deltas []string
	out, err := NewProvider("k", url, "").Chat(context.Background(), testID, hello, nil, "gpt-5", streamOpt(&deltas))
	if err == nil || !strings.Contains(err.Error(), "failed to read stream") {
		t.Fatalf("err = %v, want stream read error", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "error" {
		t.Fatalf("out = %+v, want failed error status", out)
	}
	if strings.Join(deltas, "") != "par" {
		t.Errorf("deltas = %q, want par", strings.Join(deltas, ""))
	}
}

func TestBuildInput_SystemTurnsJoinedAndAssistantTextReplayed(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "one"},
		{Role: "developer", Content: "two"},
		{Role: "system", Content: "   "},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	instructions, input := buildInput(msgs, nil, nil)
	if instructions != "one\n\ntwo" {
		t.Errorf("instructions = %q, want blank turns skipped and the rest joined", instructions)
	}
	if len(input) != 2 {
		t.Fatalf("input = %v, want user and assistant", input)
	}
	asst := asType[map[string]any](t, input[1], "assistant item")
	if asst["role"] != "assistant" || asst["content"] != "hello" {
		t.Errorf("assistant item = %v", asst)
	}
}

func TestUserInputMessage_Media(t *testing.T) {
	img := "data:image/png;base64,AAAA"
	got := userInputMessage(Message{Role: "user", Content: "look", Media: []string{img, "https://example.com/a.pdf"}})
	parts := asType[[]map[string]any](t, got["content"], "content")
	if len(parts) != 2 {
		t.Fatalf("parts = %v, want text and the one image", parts)
	}
	if parts[0]["type"] != "input_text" || parts[0]["text"] != "look" {
		t.Errorf("parts[0] = %v", parts[0])
	}
	if parts[1]["type"] != "input_image" || parts[1]["image_url"] != img {
		t.Errorf("parts[1] = %v", parts[1])
	}

	// An image with no text sends the image alone.
	got = userInputMessage(Message{Role: "user", Media: []string{img}})
	if parts := asType[[]map[string]any](t, got["content"], "content"); len(parts) != 1 || parts[0]["type"] != "input_image" {
		t.Errorf("image-only parts = %v", parts)
	}
}

func TestFunctionCallOutput_NonImageMedia(t *testing.T) {
	// Text with only non-image media is a single text part.
	got := functionCallOutput(Message{Role: "tool", Content: "done", Media: []string{"https://example.com/a.pdf"}})
	parts := asType[[]map[string]any](t, got, "output")
	if len(parts) != 1 || parts[0]["type"] != "input_text" || parts[0]["text"] != "done" {
		t.Errorf("output = %v, want the text part alone", parts)
	}
	// No text and no image leaves no parts: the output is the empty string,
	// never an empty content array.
	if got := functionCallOutput(Message{Role: "tool", Media: []string{"https://example.com/a.pdf"}}); got != "" {
		t.Errorf("output = %#v, want empty string", got)
	}
}

func TestSupportsToolSearch_NonNumericMinor(t *testing.T) {
	if supportsToolSearch("gpt-5.x") {
		t.Error(`supportsToolSearch("gpt-5.x") = true, want false`)
	}
}

func TestArgumentsJSON(t *testing.T) {
	tests := map[string]string{
		``:                `{}`,
		`null`:            `{}`,
		`"  "`:            `{}`,
		`"{\"a\":1}"`:     `{"a":1}`,
		`{"goal":"find"}`: `{"goal":"find"}`,
	}
	for raw, want := range tests {
		if got := argumentsJSON(json.RawMessage(raw)); got != want {
			t.Errorf("argumentsJSON(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestParseResponse_OutputItemEdgeCases(t *testing.T) {
	raw := `{"status":"incomplete","incomplete_details":{"reason":"something_new"},"output":[
		1,
		{"type":"reasoning","summary":[{"type":"summary_text","text":"think "},{"type":"summary_text","text":"hard"}]},
		{"type":"tool_search_call","execution":"server","call_id":"s1","arguments":{}},
		{"type":"message","content":[{"type":"output_text","text":"partial"}]}
	]}`
	out, err := parseResponse([]byte(raw), "gpt-5", "search_tools", nil)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if out.ReasoningContent != "think hard" {
		t.Errorf("ReasoningContent = %q", out.ReasoningContent)
	}
	if len(out.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %v, want a server-executed search ignored", out.ToolCalls)
	}
	if out.FinishReason != "incomplete" || out.Normal {
		t.Errorf("finish = %q normal = %v, want incomplete and not normal", out.FinishReason, out.Normal)
	}
	if out.Content != "partial" {
		t.Errorf("Content = %q", out.Content)
	}

	// A client tool_search_call is not a tool call when search was not sent.
	out, err = parseResponse([]byte(`{"output":[{"type":"tool_search_call","execution":"client","call_id":"s1"}]}`), "gpt-5", "", nil)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(out.ToolCalls) != 0 || out.FinishReason != "stop" {
		t.Errorf("out = %+v, want no tool calls", out)
	}
}

func TestCallItem_InvalidArgumentsSentAsEmptyObject(t *testing.T) {
	s := newToolSearch(searchTools(true), "gpt-5.4")
	item := s.callItem(ToolCall{ID: "c1", Function: &FunctionCall{Name: "search_tools", Arguments: "not json"}}, nil)
	if got := string(asType[json.RawMessage](t, item["arguments"], "arguments")); got != "{}" {
		t.Fatalf("arguments = %s, want {}", got)
	}
	if !s.isCall("c1") {
		t.Error("callItem must record the call id")
	}
}

// TestChat_ResponseLogRecordsNonJSONBody checks that a response that is not
// JSON (a proxy's HTML error page) is still logged, as a string.
func TestChat_ResponseLogRecordsNonJSONBody(t *testing.T) {
	const page = "<html><body>502 Bad Gateway</body></html>"
	path := filepath.Join(t.TempDir(), "log.jsonl")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		if _, err := w.Write([]byte(page)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	p := NewProvider("k", srv.URL, "", WithResponseLogFile(path))
	if _, err := p.Chat(context.Background(), testID, hello, nil, "gpt-5", nil); err == nil {
		t.Fatal("Chat accepted a 502")
	}
	recs := readLog(t, path)
	if len(recs) != 2 {
		t.Fatalf("records = %v, want request and response", recs)
	}
	if resp := recs[1]; resp["dir"] != "response" || resp["status"] != 502.0 || resp["body"] != page {
		t.Errorf("response record = %v, want the page as a string", resp)
	}
}
