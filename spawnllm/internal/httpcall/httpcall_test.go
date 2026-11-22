package httpcall

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

func TestDo(t *testing.T) {
	logs := &launchtest.Logs{}
	log := logs.Logger().With("component", "test")

	tests := []struct {
		name       string
		status     int
		body       string
		expectJSON bool
		wantErr    bool
	}{
		{name: "200", status: http.StatusOK, body: "audio", wantErr: false},
		{name: "201 is success", status: http.StatusCreated, body: `{}`, expectJSON: true},
		{name: "302 is an error", status: http.StatusFound, body: "moved", wantErr: true},
		{name: "500 is an error", status: http.StatusInternalServerError, body: "boom", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(tc.status)
				writeRaw(t, w, []byte(tc.body))
			}))
			defer srv.Close()

			req, err := NewPost(context.Background(), srv.URL+"/x?key=secret", []byte("in"), "text/plain")
			if err != nil {
				t.Fatalf("NewPost: %v", err)
			}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := Do(client, req, Call{Logger: log, Backend: "b", Model: "m", APIBase: srv.URL, JSON: tc.expectJSON})
			if tc.wantErr {
				statusErr, ok := errors.AsType[*common.HTTPStatusError](err)
				if !ok || statusErr.StatusCode != tc.status {
					t.Fatalf("err = %v, want HTTPStatusError %d", err, tc.status)
				}
				return
			}
			if err != nil || string(resp.Body) != tc.body || resp.StatusCode != tc.status {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
		})
	}

	for _, msg := range []string{"Sending request", "Received response"} {
		e, ok := logs.Find(msg)
		if !ok || e.Level != slog.LevelDebug || e.Attrs["component"] != "test" {
			t.Errorf("%q: entry = %+v, found %v; want a debug record with component test", msg, e, ok)
		}
	}
	for _, e := range logs.Entries() {
		for k, v := range e.Attrs {
			if s, ok := v.(string); ok && strings.Contains(s, "secret") {
				t.Errorf("the query string was logged in %q attribute %s", e.Message, k)
			}
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	var v struct{ A int }
	if err := DecodeJSON([]byte(`{"A":1}`), &v); err != nil || v.A != 1 {
		t.Fatalf("DecodeJSON = %v, %+v", err, v)
	}
	if err := DecodeJSON([]byte(`{`), &v); err == nil || !strings.Contains(err.Error(), "failed to decode response") {
		t.Fatalf("err = %v", err)
	}
}

func writeRaw(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	if _, err := w.Write(data); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestDo_Binary(t *testing.T) {
	tests := []struct {
		name, contentType, body string
		wantErr                 bool
	}{
		{"audio", "audio/mpeg", "ID3", false},
		{"no type", "", "\x00\x01", false},
		{"octet-stream", "application/octet-stream", "\x00", false},
		{"JSON", "application/json", `{"error":"x"}`, true},
		{"problem JSON", "application/problem+json", `{"title":"x"}`, true},
		{"text", "text/plain", "nope", true},
		{"HTML", "text/html; charset=utf-8", "<html></html>", true},
		{"HTML body typed as audio", "audio/mpeg", "<!doctype html><html></html>", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType == "" {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", tc.contentType)
				}
				writeRaw(t, w, []byte(tc.body))
			}))
			defer srv.Close()
			req, err := NewPost(context.Background(), srv.URL, nil, "application/json")
			if err != nil {
				t.Fatalf("NewPost: %v", err)
			}
			resp, err := Do(srv.Client(), req, Call{APIBase: srv.URL, Binary: true})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && string(resp.Body) != tc.body {
				t.Errorf("body = %q", resp.Body)
			}
		})
	}
}

// TestDo_DrainsErrorBody checks that an error body longer than the 1 KiB
// excerpt is drained, so the next request reuses the connection.
func TestDo_DrainsErrorBody(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeRaw(t, w, []byte(strings.Repeat("e", 20<<10)))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	client := srv.Client()
	for range 3 {
		req, err := NewPost(context.Background(), srv.URL, nil, "text/plain")
		if err != nil {
			t.Fatalf("NewPost: %v", err)
		}
		if _, err := Do(client, req, Call{}); err == nil {
			t.Fatal("500 accepted")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Errorf("connections = %d, want 1 (body not drained)", conns)
	}
}

func TestWriteFilePart_ControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := WriteFilePart(w, "file", "evil\r\nX-Injected: 1\x00\x7f\".ogg", "", strings.NewReader("x")); err != nil {
		t.Fatalf("WriteFilePart: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r := multipart.NewReader(&buf, w.Boundary())
	p, err := r.NextPart()
	if err != nil {
		t.Fatalf("NextPart: %v", err)
	}
	if got, want := p.FileName(), `evil__X-Injected: 1__".ogg`; got != want {
		t.Errorf("filename = %q, want %q", got, want)
	}
	if got := p.Header.Get("X-Injected"); got != "" {
		t.Errorf("X-Injected = %q: the filename injected a header", got)
	}
}

func TestJSONBodyHelpers(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"s":`)
	WriteJSONString(&buf, "a\"b\n<é>")
	buf.WriteString(`,"d":"`)
	if err := WriteBase64(&buf, strings.NewReader("hello, Bob")); err != nil {
		t.Fatalf("WriteBase64: %v", err)
	}
	buf.WriteString(`"}`)
	var got struct {
		S string `json:"s"`
		D string `json:"d"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal %s: %v", buf.String(), err)
	}
	if got.S != "a\"b\n<é>" || got.D != base64.StdEncoding.EncodeToString([]byte("hello, Bob")) {
		t.Errorf("got %+v", got)
	}
	if err := WriteBase64(&buf, iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Error("read error not returned")
	}
}
