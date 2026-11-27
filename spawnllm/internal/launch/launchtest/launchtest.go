/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package launchtest captures the accounting records spawnllm's providers
// report, and the log records of the code under test, for tests. The
// accounting sink is never started in tests, so every record reaches the
// accounting logger; Capture sets one that keeps them.
package launchtest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/accounting"
)

// Entry is one captured log record: its level, message and attributes, the
// ones added with Logger.With included.
type Entry struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// Logs is a slog handler that keeps every record it receives. It is safe for
// concurrent use.
type Logs struct {
	mu      sync.Mutex
	entries []Entry
}

// Logger returns a logger writing to l.
func (l *Logs) Logger() *slog.Logger { return slog.New(logsHandler{l: l}) }

// Entries returns the records captured so far.
func (l *Logs) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Entry(nil), l.entries...)
}

// Find returns the first record with message msg.
func (l *Logs) Find(msg string) (Entry, bool) {
	for _, e := range l.Entries() {
		if e.Message == msg {
			return e, true
		}
	}
	return Entry{}, false
}

type logsHandler struct {
	l     *Logs
	attrs []slog.Attr
}

func (logsHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h logsHandler) Handle(_ context.Context, r slog.Record) error {
	e := Entry{Level: r.Level, Message: r.Message, Attrs: map[string]any{}}
	for _, a := range h.attrs {
		e.Attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		e.Attrs[a.Key] = a.Value.Any()
		return true
	})
	h.l.mu.Lock()
	defer h.l.mu.Unlock()
	h.l.entries = append(h.l.entries, e)
	return nil
}

func (h logsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logsHandler{l: h.l, attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h logsHandler) WithGroup(string) slog.Handler { return h }

// Recorder keeps the accounting records logged while it is installed.
type Recorder struct {
	t    testing.TB
	logs Logs
}

// Capture sets a Recorder as the accounting logger for the rest of the test.
// The accounting logger is process-wide, so tests using it must not run in
// parallel with other tests that report accounting records.
func Capture(tb testing.TB) *Recorder {
	tb.Helper()
	r := &Recorder{t: tb}
	accounting.SetLogger(r.logs.Logger())
	tb.Cleanup(func() { accounting.SetLogger(nil) })
	return r
}

// Records returns the records captured so far.
func (r *Recorder) Records() []accounting.Record {
	var out []accounting.Record
	for _, e := range r.logs.Entries() {
		if e.Attrs["component"] != "accounting" || e.Message != "accounting record" {
			continue
		}
		raw, ok := e.Attrs["accounting"].(string)
		if !ok {
			r.t.Errorf("accounting record without a string accounting attribute: %v", e.Attrs)
			continue
		}
		var rec accounting.Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			r.t.Errorf("accounting record is not valid JSON: %v: %s", err, raw)
			continue
		}
		out = append(out, rec)
	}
	return out
}

// One returns the only record captured, failing the test unless exactly one
// was.
func (r *Recorder) One() accounting.Record {
	r.t.Helper()
	recs := r.Records()
	if len(recs) != 1 {
		r.t.Fatalf("got %d accounting records, want 1: %+v", len(recs), recs)
	}
	return recs[0]
}

// Known returns the value of a known integer, failing the test when it is
// unknown.
func Known(tb testing.TB, name string, v accounting.Int) int64 {
	tb.Helper()
	n, ok := v.Value()
	if !ok {
		tb.Fatalf("%s is unknown, want a value", name)
	}
	return n
}

// Unknown fails the test when the integer is known.
func Unknown(tb testing.TB, name string, v accounting.Int) {
	tb.Helper()
	if n, ok := v.Value(); ok {
		tb.Errorf("%s = %d, want unknown", name, n)
	}
}

// Rejected lists identities every entry point must refuse, with the error
// the refusal must wrap.
var Rejected = []struct {
	Name string
	ID   accounting.Identity
	Want error
}{
	{Name: "missing agent", ID: accounting.Identity{BillingCode: "garden"}, Want: accounting.ErrAgentRequired},
	{Name: "agent too long", ID: accounting.Identity{Agent: strings.Repeat("a", accounting.MaxNameLength+1)}, Want: accounting.ErrNameTooLong},
	{Name: "origin too long", ID: accounting.Identity{Origin: strings.Repeat("o", accounting.MaxNameLength+1), Agent: "alice"}, Want: accounting.ErrNameTooLong},
	{Name: "billing code too long", ID: accounting.Identity{Agent: "alice", BillingCode: strings.Repeat("p", accounting.MaxNameLength+1)}, Want: accounting.ErrNameTooLong},
}

// Server starts an HTTP server that answers every request with status,
// contentType and body, and returns its URL and a count of the requests it
// received.
func Server(tb testing.TB, status int, contentType, body string) (string, *atomic.Int32) {
	tb.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server
	}))
	tb.Cleanup(srv.Close)
	return srv.URL, &hits
}

// ClosedURL returns the URL of a server that is no longer listening, so a
// request to it gets no HTTP response.
func ClosedURL(tb testing.TB) string {
	tb.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// Refused checks the one record of a refused launch: a failure carrying the
// error, with no HTTP status and no usage.
func (r *Recorder) Refused(provider string) accounting.Record {
	r.t.Helper()
	rec := r.One()
	if rec.Success || rec.Error == "" {
		r.t.Errorf("record success=%v error=%q, want a failure with the error", rec.Success, rec.Error)
	}
	if rec.Provider != provider {
		r.t.Errorf("provider = %q, want %q", rec.Provider, provider)
	}
	Unknown(r.t, "http_status", rec.HTTPStatus)
	Unknown(r.t, "input_tokens", rec.InputTokens)
	Unknown(r.t, "output_tokens", rec.OutputTokens)
	if _, ok := rec.CostUSD.Value(); ok {
		r.t.Error("cost_usd is known for a refused launch")
	}
	return rec
}

// Failed checks the one record of a launch that failed with HTTP status
// (0: no response) and returns it.
func (r *Recorder) Failed(status int) accounting.Record {
	r.t.Helper()
	rec := r.One()
	if rec.Success || rec.Error == "" {
		r.t.Errorf("record success=%v error=%q, want a failure with the error", rec.Success, rec.Error)
	}
	if status == 0 {
		Unknown(r.t, "http_status", rec.HTTPStatus)
	} else if got := Known(r.t, "http_status", rec.HTTPStatus); got != int64(status) {
		r.t.Errorf("http_status = %d, want %d", got, status)
	}
	return rec
}
