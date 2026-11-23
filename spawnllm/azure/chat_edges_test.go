package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/spawnllm/common"
)

func TestWithLoggerTagsComponent(t *testing.T) {
	var buf bytes.Buffer
	p := NewProvider("k", "https://example.com", "", WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	p.log().Info("hello")
	if !strings.Contains(buf.String(), "component=azure") {
		t.Fatalf("log output = %q, want component=azure", buf.String())
	}
}

func TestLogWithoutLoggerDiscards(t *testing.T) {
	p := NewProvider("k", "https://example.com", "", WithLogger(nil))
	if p.log().Enabled(context.Background(), slog.LevelError) {
		t.Fatal("log() without a logger must discard every record")
	}
}

func TestGetDefaultModelIsEmpty(t *testing.T) {
	if got := NewProvider("k", "https://example.com", "").GetDefaultModel(); got != "" {
		t.Fatalf("GetDefaultModel() = %q, want empty: the deployment names the model", got)
	}
}

func TestChatInvalidAPIBase(t *testing.T) {
	p := NewProvider("k", "://not-a-url", "")
	_, err := p.Chat(t.Context(), testID, hello, nil, "deploy", nil)
	if err == nil || !strings.Contains(err.Error(), "failed to build Azure request URL") {
		t.Fatalf("err = %v, want URL build error", err)
	}
}

func TestChatSendsTemperature(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writeValidResponse(t, w)
	}))
	t.Cleanup(srv.Close)

	_, err := NewProvider("k", srv.URL, "").Chat(t.Context(), testID, hello, nil, "deploy",
		map[string]any{"temperature": 0.25})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if body["temperature"] != 0.25 {
		t.Fatalf("temperature = %v, want 0.25", body["temperature"])
	}
}

func TestChatUnencodableRequestIsNotSent(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeValidResponse(t, w)
	}))
	t.Cleanup(srv.Close)

	_, err := NewProvider("k", srv.URL, "").Chat(t.Context(), testID, hello, nil, "deploy",
		map[string]any{"temperature": math.NaN()})
	if err == nil || !strings.Contains(err.Error(), "failed to marshal request") {
		t.Fatalf("err = %v, want marshal error", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestChatMalformedJSONIsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"choices":[`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := NewProvider("k", srv.URL, "").Chat(t.Context(), testID, hello, nil, "deploy", nil)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if out == nil || out.Status == nil {
		t.Fatalf("out = %+v, want a failed status", out)
	}
	if out.Status.Success || out.Status.StopReason != "parse_error" || out.Status.Model != "deploy" {
		t.Fatalf("Status = %+v, want failed parse_error for deploy", out.Status)
	}
	if out.Status.BytesReceived == 0 || out.Status.BytesSent == 0 {
		t.Fatalf("Status = %+v, want bytes sent and received recorded", out.Status)
	}
}

func TestChatStreamTruncatedIsReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := "data: " + `{"choices":[{"delta":{"content":"par"}}]}` + "\n\n"
		w.Header().Set("Content-Type", "text/event-stream")
		// Promise more than is sent so the client sees an unexpected EOF.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	var deltas []string
	out, err := NewProvider("k", srv.URL, "").Chat(t.Context(), testID, hello, nil, "deploy",
		map[string]any{common.TextDeltaOption: func(d string) { deltas = append(deltas, d) }})
	if err == nil || !strings.Contains(err.Error(), "failed to read stream") {
		t.Fatalf("err = %v, want stream read error", err)
	}
	if _, ok := errors.AsType[*common.HTTPStatusError](err); ok {
		t.Fatalf("err = %T, a broken stream is not a provider error", err)
	}
	// A broken stream is a read failure, not a parse failure.
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "error" {
		t.Fatalf("out = %+v, want a failed error status", out)
	}
	if out.Status.BytesReceived == 0 {
		t.Fatalf("BytesReceived = 0, want the bytes read before the break")
	}
	if strings.Join(deltas, "") != "par" {
		t.Fatalf("deltas = %q, want par", strings.Join(deltas, ""))
	}
}

func TestChatStreamWithoutModelUsesDeployment(t *testing.T) {
	body := "data: " + `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	srv := sseAzureServer(t, body, nil)

	out, err := NewProvider("k", srv.URL, "").Chat(t.Context(), testID, hello, nil, "deploy",
		map[string]any{common.TextDeltaOption: func(string) {}})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if out.Status == nil || out.Status.Model != "deploy" {
		t.Fatalf("Status = %+v, want Model deploy", out.Status)
	}
}

func TestChatStreamMalformedIsParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		stream := "data: " + `{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\n" +
			"data: " + `{"choices":[],"usage":"not an object"}` + "\n\n" +
			"data: [DONE]\n\n"
		if _, err := w.Write([]byte(stream)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := NewProvider("k", srv.URL, "").Chat(t.Context(), testID, hello, nil, "deploy",
		map[string]any{common.TextDeltaOption: func(string) {}})
	if err == nil || errors.Is(err, common.ErrStreamRead) {
		t.Fatalf("err = %v, want a parse error", err)
	}
	if out == nil || out.Status == nil || out.Status.Success || out.Status.StopReason != "parse_error" {
		t.Fatalf("out = %+v, want a failed parse_error status", out)
	}
}
