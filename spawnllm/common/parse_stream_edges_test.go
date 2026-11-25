/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// --- SSE reader ---

func readAllSSE(t *testing.T, r io.Reader) ([]string, error) {
	t.Helper()
	sr := NewSSEReader(r)
	var out []string
	for {
		payload, ok, err := sr.Next()
		if !ok {
			return out, err
		}
		out = append(out, payload)
	}
}

func TestSSEReader_Framing(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []string
	}{
		{
			name:   "multi-line data joined with newlines",
			stream: "data: first\ndata:second\ndata:  third\n\n",
			want:   []string{"first\nsecond\n third"},
		},
		{
			name:   "final event without a trailing blank line",
			stream: "data: a\n\ndata: b",
			want:   []string{"a", "b"},
		},
		{
			name:   "non-data fields ignored",
			stream: "event: message\nid: 7\nretry: 100\ndata: x\n\n",
			want:   []string{"x"},
		},
		{
			name:   "event with only non-data fields dispatches nothing",
			stream: "event: ping\n\ndata: y\n\n",
			want:   []string{"y"},
		},
		{
			name:   "empty stream",
			stream: "",
			want:   nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readAllSSE(t, strings.NewReader(tc.stream))
			if !errors.Is(err, io.EOF) {
				t.Fatalf("end error = %v, want io.EOF", err)
			}
			if !equalStrings(got, tc.want) {
				t.Errorf("payloads = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSSEReader_ReadError(t *testing.T) {
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader("data: a\n\n"), iotest.ErrReader(boom))
	got, err := readAllSSE(t, r)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
	if !equalStrings(got, []string{"a"}) {
		t.Errorf("payloads before the error = %q, want [a]", got)
	}
}

// --- Chat stream accumulation ---

func TestAccumulateChatStream_ReadError(t *testing.T) {
	boom := errors.New("connection reset")
	r := io.MultiReader(strings.NewReader(buildSSE(`{"choices":[{"delta":{"content":"x"}}]}`)), iotest.ErrReader(boom))
	out, body, err := AccumulateChatStream(r, func(string) {}, nil)
	if out != nil || body != nil || !errors.Is(err, boom) || !errors.Is(err, ErrStreamRead) || !strings.Contains(err.Error(), "failed to read stream") {
		t.Fatalf("AccumulateChatStream = %+v, %q, %v; want a read error wrapping the cause", out, body, err)
	}
}

func TestAccumulateChatStream_ReasoningImagesAndNoise(t *testing.T) {
	stream := buildSSE(
		`not json at all`,
		`{"model":"m","choices":[{"delta":{"reasoning_content":"think "}}]}`,
		`{"model":"m","choices":[{"delta":{"reasoning_content":"hard","reasoning":"plan"}}]}`,
		`{"choices":[{"delta":{"images":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"look","arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		"[DONE]",
		`{"choices":[{"delta":{"content":"after done"}}]}`,
	)
	var deltas []string
	out, body, err := AccumulateChatStream(strings.NewReader(stream), func(d string) { deltas = append(deltas, d) }, nil)
	if err != nil {
		t.Fatalf("AccumulateChatStream: %v", err)
	}
	if len(deltas) != 0 {
		t.Errorf("deltas = %q, want none (no content, and nothing after [DONE])", deltas)
	}
	if out.ReasoningContent != "think hard" || out.Reasoning != "plan" {
		t.Errorf("reasoning_content = %q, reasoning = %q", out.ReasoningContent, out.Reasoning)
	}
	if len(out.Images) != 1 {
		t.Errorf("images = %d, want 1", len(out.Images))
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "c1" || out.ToolCalls[0].Name != "look" ||
		out.ToolCalls[0].Arguments["q"] != float64(1) {
		t.Fatalf("tool calls = %+v", out.ToolCalls)
	}
	if out.FinishReason != "tool_calls" || !out.Normal {
		t.Errorf("finish = %q normal = %v", out.FinishReason, out.Normal)
	}
	// The reconstructed body names the tool call's type even though the
	// stream never sent one.
	if !bytes.Contains(body, []byte(`"type":"function"`)) {
		t.Errorf("reconstructed body %s lacks the default tool call type", body)
	}
}

func TestAccumulateChatStream_MalformedUsage(t *testing.T) {
	stream := buildSSE(
		`{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":"not an object"}`,
	)
	out, body, err := AccumulateChatStream(strings.NewReader(stream), func(string) {}, nil)
	if out != nil || err == nil || !strings.Contains(err.Error(), "failed to parse reconstructed stream body") {
		t.Fatalf("AccumulateChatStream = %+v, %v; want a parse error", out, err)
	}
	if !bytes.Contains(body, []byte(`"not an object"`)) {
		t.Errorf("body = %s, want the partial reconstruction returned", body)
	}
}

func TestAccumulateChatStream_ErrorShapes(t *testing.T) {
	tests := []struct {
		name, chunk, want string
	}{
		{"object with message", `{"error":{"message":"overloaded"}}`, "overloaded"},
		{"bare string", `{"error":"quota exceeded"}`, "quota exceeded"},
		{"object without message", `{"error":{"code":500}}`, `{"code":500}`},
		{"empty string", `{"error":""}`, `""`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := AccumulateChatStream(strings.NewReader(buildSSE(tc.chunk)), func(string) {}, nil)
			var se *StreamChatError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want *StreamChatError", err, err)
			}
			if se.Message != tc.want || err.Error() != tc.want {
				t.Errorf("message = %q, Error() = %q, want %q", se.Message, err.Error(), tc.want)
			}
		})
	}
}

// --- ParseResponse options and edge cases ---

func TestParseResponse_UsageCostOnlyWhenAsked(t *testing.T) {
	body := `{"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"cost":0.0125,
		"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":30}}}`

	out, err := ParseResponse(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Status.CostUSD != nil {
		t.Errorf("cost = %v without WithUsageCost, want nil", *out.Status.CostUSD)
	}
	st := out.Status
	if st.InputTokens != 10 || st.CacheReadTokens != 60 || st.CacheCreationTokens != 30 || st.OutputTokens != 5 {
		t.Errorf("tokens in=%d read=%d write=%d out=%d, want 10/60/30/5",
			st.InputTokens, st.CacheReadTokens, st.CacheCreationTokens, st.OutputTokens)
	}
	if !st.Reported.InputTokens || !st.Reported.CacheReadTokens || !st.Reported.CacheCreationTokens || !st.Reported.OutputTokens {
		t.Errorf("reported = %+v, want all counts reported", st.Reported)
	}

	out, err = ParseResponse(strings.NewReader(body), nil, WithUsageCost())
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Status.CostUSD == nil || *out.Status.CostUSD != 0.0125 {
		t.Errorf("cost = %v, want 0.0125", out.Status.CostUSD)
	}

	// No cost field: nil even when asked.
	out, err = ParseResponse(strings.NewReader(`{"choices":[{"message":{"content":"x"}}],"usage":{}}`), nil, WithUsageCost())
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if out.Status.CostUSD != nil || out.Status.Reported.InputTokens || out.Status.Reported.CacheReadTokens {
		t.Errorf("status = %+v, want no cost and no reported counts", out.Status)
	}
}

func TestParseResponse_LegacyFunctionCallBadArgumentsLogged(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	body := `{"choices":[{"message":{"content":"","function_call":{"name":"get","arguments":"{not json"}},"finish_reason":"function_call"}]}`
	out, err := ParseResponse(strings.NewReader(body), nil, WithLogger(log))
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "get" || out.ToolCalls[0].Arguments["raw"] != "{not json" {
		t.Fatalf("tool calls = %+v, want get with the raw arguments", out.ToolCalls)
	}
	if !strings.Contains(logBuf.String(), "function_call arguments decode failed") ||
		!strings.Contains(logBuf.String(), "component=common") {
		t.Errorf("log = %q, want the decode warning tagged component=common", logBuf.String())
	}
}

func TestParseResponse_ContentSniffEdgeCases(t *testing.T) {
	tools := map[string]struct{}{"search": {}}
	tests := []struct {
		name     string
		content  string
		wantArgs map[string]any // nil: no tool call, content kept
	}{
		{name: "whitespace only", content: " \n\t"},
		{name: "malformed JSON", content: `{"type":"function","name":`},
		{name: "empty array", content: `[]`},
		{name: "array with a non-object", content: `[{"type":"function","name":"search"}, 3]`},
		{name: "no arguments", content: `{"type":"function","name":"search"}`, wantArgs: map[string]any{}},
		{name: "blank string arguments", content: `{"type":"function","name":"search","arguments":"  "}`, wantArgs: map[string]any{}},
		{name: "undecodable string arguments", content: `{"type":"function","name":"search","arguments":"q=1"}`, wantArgs: map[string]any{"raw": "q=1"}},
		{name: "numeric arguments", content: `{"type":"function","name":"search","parameters":42}`, wantArgs: map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"content": tc.content}, "finish_reason": "stop",
			}}})
			if err != nil {
				t.Fatal(err)
			}
			out, err := ParseResponse(bytes.NewReader(raw), tools)
			if err != nil {
				t.Fatalf("ParseResponse: %v", err)
			}
			if tc.wantArgs == nil {
				if len(out.ToolCalls) != 0 {
					t.Errorf("tool calls = %+v, want none", out.ToolCalls)
				}
				return
			}
			if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "search" || out.Content != "" {
				t.Fatalf("out = %+v, want one search call and no content", out)
			}
			got, err := json.Marshal(out.ToolCalls[0].Arguments)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(tc.wantArgs)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("arguments = %s, want %s", got, want)
			}
		})
	}
}

func TestDecodeToolCallArguments_Unusable(t *testing.T) {
	tests := []struct {
		name, raw, wantRaw, wantLog string
	}{
		{"string holding invalid JSON", `"{oops"`, "{oops", "failed to decode tool call arguments"},
		{"array", `[1,2]`, "[1,2]", "unsupported tool call arguments type"},
		{"number", `7`, "7", "unsupported tool call arguments type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			got := DecodeToolCallArguments(json.RawMessage(tc.raw), "tool", slog.New(slog.NewTextHandler(&logBuf, nil)))
			if len(got) != 1 || got["raw"] != tc.wantRaw {
				t.Errorf("arguments = %v, want raw %q", got, tc.wantRaw)
			}
			if !strings.Contains(logBuf.String(), tc.wantLog) || !strings.Contains(logBuf.String(), "name=tool") {
				t.Errorf("log = %q, want %q naming the tool", logBuf.String(), tc.wantLog)
			}
		})
	}
}

// --- HTTP helpers ---

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"", 0},
		{"  ", 0},
		{"30", 30 * time.Second},
		{" 1.5 ", 1500 * time.Millisecond},
		{"0", 0},
		{"-5", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{now.Format(http.TimeFormat), 0},
		{"soon", 0},
	}
	for _, tc := range tests {
		if got := ParseRetryAfter(tc.raw, now); got != tc.want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestReadParseAndMeasure_InspectError(t *testing.T) {
	boom := errors.New("reset")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(io.MultiReader(strings.NewReader(`{"cho`), iotest.ErrReader(boom))),
	}
	out, n, err := ReadParseAndMeasure(resp, "https://api.example.com", nil)
	if out != nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "failed to inspect response") {
		t.Fatalf("ReadParseAndMeasure = %+v, %v; want an inspect error", out, err)
	}
	if n != 5 {
		t.Errorf("bytes = %d, want the 5 read before the error", n)
	}
}

// --- Numeric helpers ---

func TestReportedNumbers(t *testing.T) {
	got := ReportedNumbers([]byte(`{"input_tokens":0,"output_tokens":-1,"cost":1.5e-3,
		"name":"x","nothing":null,"flag":true,"nested":{"a":1},"list":[1],"padded": 3 }`))
	want := map[string]bool{"input_tokens": true, "output_tokens": true, "cost": true, "padded": true}
	if len(got) != len(want) {
		t.Fatalf("ReportedNumbers = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("%q not reported in %v", k, got)
		}
	}
	for _, in := range []string{`[1,2]`, `"x"`, `null`, `{`, ``} {
		if got := ReportedNumbers([]byte(in)); len(got) != 0 {
			t.Errorf("ReportedNumbers(%q) = %v, want none", in, got)
		}
	}
	if got := ReportedNumbers([]byte(`[1]`)); got != nil {
		t.Errorf("ReportedNumbers of an array = %v, want nil", got)
	}
}

func TestReportedCount(t *testing.T) {
	if n, ok := ReportedCount(nil); n != 0 || ok {
		t.Errorf("ReportedCount(nil) = %d, %v; want 0, false", n, ok)
	}
	zero := 0
	if n, ok := ReportedCount(&zero); n != 0 || !ok {
		t.Errorf("ReportedCount(&0) = %d, %v; want a reported 0", n, ok)
	}
}

func TestToolChoice(t *testing.T) {
	tests := []struct {
		name    string
		options map[string]any
		want    string
	}{
		{"nil options", nil, ToolChoiceAuto},
		{"none", map[string]any{ToolChoiceOption: "none"}, ToolChoiceNone},
		{"auto", map[string]any{ToolChoiceOption: "auto"}, ToolChoiceAuto},
		{"unknown value", map[string]any{ToolChoiceOption: "required"}, ToolChoiceAuto},
		{"not a string", map[string]any{ToolChoiceOption: false}, ToolChoiceAuto},
	}
	for _, tc := range tests {
		if got := ToolChoice(tc.options); got != tc.want {
			t.Errorf("%s: ToolChoice = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// --- Serialization ---

func TestSerializeMessages_MediaWithToolCallsAndReasoning(t *testing.T) {
	msgs := []Message{{
		Role:             "assistant",
		Content:          "see image",
		Media:            []string{"data:image/png;base64,AAAA", "https://example.com/not-inline.png"},
		ReasoningContent: "because",
		ToolCalls:        []ToolCall{{ID: "c1", Name: "look", Arguments: map[string]any{"q": "x"}}},
	}}
	out := SerializeMessages(msgs, nil)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
		ToolCalls        []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if len(got) != 1 {
		t.Fatalf("messages = %s", raw)
	}
	m := got[0]
	// Only the inline data URL becomes an image part.
	if len(m.Content) != 2 || m.Content[0].Type != "text" || m.Content[1].Type != "image_url" {
		t.Errorf("content parts = %+v, want text then one image_url", m.Content)
	}
	if m.ReasoningContent != "because" {
		t.Errorf("reasoning_content = %q", m.ReasoningContent)
	}
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "c1" || m.ToolCalls[0].Function.Name != "look" {
		t.Errorf("tool_calls = %+v", m.ToolCalls)
	}
}

// --- HTTP client ---

// roundTripFunc is an http.RoundTripper that is not an *http.Transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestNewHTTPClient_ProxyWithCustomDefaultTransport replaces the process-wide
// http.DefaultTransport, so it does not run in parallel.
func TestNewHTTPClient_ProxyWithCustomDefaultTransport(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	t.Cleanup(func() { http.DefaultTransport = old })

	client := NewHTTPClient("http://127.0.0.1:3128", nil)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want a minimal *http.Transport", client.Transport)
	}
	proxy, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.example.com"}})
	if err != nil || proxy == nil || proxy.String() != "http://127.0.0.1:3128" {
		t.Errorf("proxy = %v, %v; want http://127.0.0.1:3128", proxy, err)
	}
	if client.Timeout != DefaultRequestTimeout {
		t.Errorf("timeout = %v, want %v", client.Timeout, DefaultRequestTimeout)
	}
}

// --- Deferred tools ---

func TestAnthropicSupportsDeferredTools_Degenerate(t *testing.T) {
	for _, model := range []string{"claude-", "claude-opus", "claude-opus-", "CLAUDE-sonnet-x"} {
		if AnthropicSupportsDeferredTools(model) {
			t.Errorf("AnthropicSupportsDeferredTools(%q) = true, want false", model)
		}
	}
}
