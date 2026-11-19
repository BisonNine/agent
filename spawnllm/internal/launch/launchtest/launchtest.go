/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package launchtest captures the accounting records spawnllm's providers
// report, for tests. The accounting sink is never started in tests, so every
// record reaches the logger; Capture installs a logger backend that keeps
// them. Each test package runs in its own process, so captures in different
// packages cannot see each other's records.
package launchtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/logger"
)

// Recorder keeps the accounting records logged while it is installed.
type Recorder struct {
	t       testing.TB
	mu      sync.Mutex
	records []accounting.Record
}

// Capture installs a Recorder as the logger backend for the rest of the test.
// Tests using it must not run in parallel with other tests that log.
func Capture(tb testing.TB) *Recorder {
	tb.Helper()
	r := &Recorder{t: tb}
	logger.SetBackend(r)
	tb.Cleanup(func() { logger.SetBackend(nil) })
	return r
}

// Log implements logger.Backend.
func (r *Recorder) Log(_, component, message string, fields map[string]any) {
	if component != "accounting" || message != "accounting record" {
		return
	}
	raw, ok := fields["accounting"].(string)
	if !ok {
		r.t.Errorf("accounting record without a string accounting field: %v", fields)
		return
	}
	var rec accounting.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		r.t.Errorf("accounting record is not valid JSON: %v: %s", err, raw)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

// LogMessageContent implements logger.Backend.
func (r *Recorder) LogMessageContent() bool { return false }

// Records returns the records captured so far.
func (r *Recorder) Records() []accounting.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]accounting.Record(nil), r.records...)
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
	{Name: "missing agent", ID: accounting.Identity{Project: "garden"}, Want: accounting.ErrAgentRequired},
	{Name: "agent too long", ID: accounting.Identity{Agent: strings.Repeat("a", accounting.MaxNameLength+1)}, Want: accounting.ErrNameTooLong},
	{Name: "project too long", ID: accounting.Identity{Agent: "alice", Project: strings.Repeat("p", accounting.MaxNameLength+1)}, Want: accounting.ErrNameTooLong},
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
