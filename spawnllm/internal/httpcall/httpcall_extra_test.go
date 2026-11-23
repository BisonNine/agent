package httpcall

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

func TestNewPost(t *testing.T) {
	req, err := NewPost(context.Background(), "https://api.example.com/v1/x", []byte("body"), "application/json")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" || req.ContentLength != 4 {
		t.Errorf("request = %s %s type %q length %d", req.Method, req.URL, req.Header.Get("Content-Type"), req.ContentLength)
	}

	if _, err := NewPost(context.Background(), "http://bad host/\x7f", nil, "text/plain"); err == nil ||
		!strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("err = %v, want a create request error", err)
	}
}

func TestDo_TransportError(t *testing.T) {
	logs := &launchtest.Logs{}
	req, err := NewPost(context.Background(), launchtest.ClosedURL(t), nil, "text/plain")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	resp, err := Do(http.DefaultClient, req, Call{Logger: logs.Logger(), Backend: "b"})
	if resp != nil || err == nil || !strings.Contains(err.Error(), "failed to send request") {
		t.Fatalf("Do = %+v, %v; want a send error", resp, err)
	}
	if _, ok := logs.Find("Request failed"); !ok {
		t.Error("the transport failure was not logged")
	}
}

func TestDo_DeadlineStaysDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a request with a cancelled context reached the server")
	}))
	defer srv.Close()
	req, err := NewPost(ctx, srv.URL, nil, "text/plain")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	if _, err := Do(srv.Client(), req, Call{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// bodyTransport answers every request with a fixed response, so a body that
// fails mid-read can be tested without a network.
type bodyTransport struct {
	header http.Header
	body   io.Reader
}

func (b bodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     b.header,
		Body:       io.NopCloser(b.body),
		Request:    req,
	}, nil
}

func TestDo_ReadError(t *testing.T) {
	boom := errors.New("boom")
	client := &http.Client{Transport: bodyTransport{header: http.Header{}, body: iotest.ErrReader(boom)}}
	req, err := NewPost(context.Background(), "http://api.invalid/x", nil, "text/plain")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	resp, err := Do(client, req, Call{})
	if resp != nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "failed to read response") {
		t.Fatalf("Do = %+v, %v; want a read error wrapping boom", resp, err)
	}
}

func TestDo_HTMLWhereJSONExpected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		writeRaw(t, w, []byte("<html><body>Sign in</body></html>"))
	}))
	defer srv.Close()
	req, err := NewPost(context.Background(), srv.URL, nil, "application/json")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	resp, err := Do(srv.Client(), req, Call{APIBase: srv.URL, JSON: true})
	if resp != nil || err == nil {
		t.Fatalf("Do = %+v, %v; want an HTML error", resp, err)
	}
	want := common.WrapHTMLResponseError(http.StatusOK, []byte("<html><body>Sign in</body></html>"), "text/html", srv.URL)
	if err.Error() != want.Error() {
		t.Errorf("err = %q, want the chat providers' HTML error %q", err, want)
	}

	// Without JSON or Binary the same body is data.
	req, err = NewPost(context.Background(), srv.URL, nil, "application/json")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	if resp, err := Do(srv.Client(), req, Call{}); err != nil || !strings.Contains(string(resp.Body), "Sign in") {
		t.Errorf("Do = %+v, %v; want the body", resp, err)
	}
}

func TestIsTextType(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/plain":                      true,
		"application/json; charset=utf-8": true,
		"application/vnd.api+json":        true,
		"audio/mpeg":                      false,
		"":                                false,
		"not a media type;;; =":           false,
		"application/octet-stream":        false,
	} {
		if got := isTextType(ct); got != want {
			t.Errorf("isTextType(%q) = %v, want %v", ct, got, want)
		}
	}
}

// failWriter fails every write, so a multipart writer over it fails.
type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriteFilePart(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := WriteFilePart(w, `fi"le`, "", "", strings.NewReader("data")); err != nil {
		t.Fatalf("WriteFilePart: %v", err)
	}
	if err := WriteFilePart(w, "audio", "dir/clip.ogg", "audio/ogg", strings.NewReader("ogg")); err != nil {
		t.Fatalf("WriteFilePart: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r := multipart.NewReader(&buf, w.Boundary())
	for _, want := range []struct{ field, file, ctype, body string }{
		{`fi"le`, "file", "application/octet-stream", "data"},
		{"audio", "clip.ogg", "audio/ogg", "ogg"},
	} {
		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		body, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		if p.FormName() != want.field || p.FileName() != want.file ||
			p.Header.Get("Content-Type") != want.ctype || string(body) != want.body {
			t.Errorf("part = %q %q %q %q, want %+v", p.FormName(), p.FileName(), p.Header.Get("Content-Type"), body, want)
		}
	}
}

func TestWriteFilePart_Errors(t *testing.T) {
	boom := errors.New("boom")
	w := multipart.NewWriter(failWriter{err: boom})
	if err := WriteFilePart(w, "f", "a", "", strings.NewReader("x")); !errors.Is(err, boom) ||
		!strings.Contains(err.Error(), "failed to create form file") {
		t.Errorf("err = %v, want a create error wrapping boom", err)
	}

	var buf bytes.Buffer
	w = multipart.NewWriter(&buf)
	if err := WriteFilePart(w, "f", "a", "", iotest.ErrReader(boom)); !errors.Is(err, boom) ||
		!strings.Contains(err.Error(), "failed to read file part") {
		t.Errorf("err = %v, want a read error wrapping boom", err)
	}
}

func TestWriteFields(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := [][2]string{{"model", "m"}, {"language", "en"}, {"model", "again"}}
	if err := WriteFields(w, fields); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r := multipart.NewReader(&buf, w.Boundary())
	for _, want := range fields {
		p, err := r.NextPart()
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		v, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		if p.FormName() != want[0] || string(v) != want[1] {
			t.Errorf("field %q = %q, want %q = %q", p.FormName(), v, want[0], want[1])
		}
	}
	if _, err := r.NextPart(); !errors.Is(err, io.EOF) {
		t.Errorf("extra part: %v", err)
	}

	boom := errors.New("boom")
	err := WriteFields(multipart.NewWriter(failWriter{err: boom}), fields)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "failed to write model field") {
		t.Errorf("err = %v, want the model field's error wrapping boom", err)
	}
}

type failCloser struct {
	io.Reader

	err error
}

func (f failCloser) Close() error { return f.err }

type closeTransport struct{ body io.ReadCloser }

func (c closeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: c.body, Request: req}, nil
}

// TestDo_CloseErrorIsLogged checks that a body that fails to close does not
// fail a call whose body was read in full, and that the failure is logged.
func TestDo_CloseErrorIsLogged(t *testing.T) {
	logs := &launchtest.Logs{}
	client := &http.Client{Transport: closeTransport{body: failCloser{Reader: strings.NewReader("ok"), err: errors.New("close boom")}}}
	req, err := NewPost(context.Background(), "http://api.invalid/x", nil, "text/plain")
	if err != nil {
		t.Fatalf("NewPost: %v", err)
	}
	resp, err := Do(client, req, Call{Logger: logs.Logger()})
	if err != nil || string(resp.Body) != "ok" {
		t.Fatalf("Do = %+v, %v; want the body", resp, err)
	}
	e, ok := logs.Find("closing response body")
	if !ok || e.Attrs["error"] != "close boom" {
		t.Errorf("entry = %+v, found %v; want the close error logged", e, ok)
	}
}
