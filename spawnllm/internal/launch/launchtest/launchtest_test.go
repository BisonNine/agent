package launchtest

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/accounting"
)

// fakeTB records the failures a helper reports instead of failing the real
// test. Fatalf ends the calling goroutine, as testing.T's does, so helpers
// are run through run.
type fakeTB struct {
	testing.TB

	mu     sync.Mutex
	errors []string
	fatal  bool
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *fakeTB) Error(args ...any) { f.Errorf("%s", fmt.Sprint(args...)) }

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.Errorf(format, args...)
	f.mu.Lock()
	f.fatal = true
	f.mu.Unlock()
	runtime.Goexit()
}

func (f *fakeTB) failures() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.errors, "\n")
}

// run calls fn on its own goroutine, so a Fatalf ends only fn.
func run(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
}

func TestLogs(t *testing.T) {
	var logs Logs
	log := logs.Logger().With("component", "x").WithGroup("ignored")
	log.Info("first", "a", 1)
	log.Warn("second", "b", "two")
	entries := logs.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want 2", entries)
	}
	if e := entries[0]; e.Level != slog.LevelInfo || e.Message != "first" || e.Attrs["component"] != "x" || e.Attrs["a"] != int64(1) {
		t.Errorf("first = %+v", e)
	}
	if e, ok := logs.Find("second"); !ok || e.Level != slog.LevelWarn || e.Attrs["b"] != "two" {
		t.Errorf("Find(second) = %+v, %v", e, ok)
	}
	if _, ok := logs.Find("missing"); ok {
		t.Error("Find found a message never logged")
	}
	// Entries is a copy.
	entries[0].Message = "changed"
	if logs.Entries()[0].Message != "first" {
		t.Error("Entries returned the recorder's own slice")
	}
}

func TestRecorder(t *testing.T) {
	r := Capture(t)
	accounting.Report(accounting.Record{Agent: "alice", Provider: "p", Success: true})

	// Records that are not accounting records are skipped.
	r.logs.Logger().Info("accounting record", "component", "other")
	r.logs.Logger().Info("something else", "component", "accounting")

	recs := r.Records()
	if len(recs) != 1 || recs[0].Agent != "alice" || recs[0].Provider != "p" {
		t.Fatalf("records = %+v, want alice's", recs)
	}
	if got := r.One(); got.Agent != "alice" {
		t.Errorf("One = %+v", got)
	}
}

func TestRecorder_MalformedRecords(t *testing.T) {
	fake := &fakeTB{}
	r := &Recorder{t: fake}
	log := r.logs.Logger().With("component", "accounting")
	log.Info("accounting record", "accounting", 7)
	log.Info("accounting record", "accounting", "{not json")
	if recs := r.Records(); len(recs) != 0 {
		t.Errorf("records = %+v, want none", recs)
	}
	got := fake.failures()
	if !strings.Contains(got, "without a string accounting attribute") || !strings.Contains(got, "not valid JSON") {
		t.Errorf("failures = %q, want both malformed records reported", got)
	}

	run(func() { r.One() })
	if !fake.fatal || !strings.Contains(fake.failures(), "got 0 accounting records, want 1") {
		t.Errorf("One with no records: fatal %v, failures %q", fake.fatal, fake.failures())
	}
}

func TestKnownUnknown(t *testing.T) {
	if n := Known(t, "n", accounting.KnownInt(4)); n != 4 {
		t.Errorf("Known = %d, want 4", n)
	}
	Unknown(t, "n", accounting.Int{})

	fake := &fakeTB{}
	Unknown(fake, "n", accounting.KnownInt(4))
	if !strings.Contains(fake.failures(), "n = 4, want unknown") {
		t.Errorf("failures = %q", fake.failures())
	}
	run(func() { Known(fake, "m", accounting.Int{}) })
	if !fake.fatal || !strings.Contains(fake.failures(), "m is unknown") {
		t.Errorf("Known of an unknown: fatal %v, failures %q", fake.fatal, fake.failures())
	}
}

func TestServerAndClosedURL(t *testing.T) {
	url, hits := Server(t, http.StatusTeapot, "text/plain", "short and stout")
	for range 2 {
		resp, err := http.Get(url) //nolint:noctx // test server
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close() //nolint:errcheck // test
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if resp.StatusCode != http.StatusTeapot || resp.Header.Get("Content-Type") != "text/plain" || string(body) != "short and stout" {
			t.Errorf("response = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}

	resp, err := http.Get(ClosedURL(t)) //nolint:noctx // test server
	if err == nil {
		_ = resp.Body.Close() //nolint:errcheck // test
		t.Fatal("a closed server answered")
	}
}

func TestRefusedAndFailed(t *testing.T) {
	good := Capture(t)
	accounting.Report(accounting.Record{Provider: "p", Error: "refused"})
	if rec := good.Refused("p"); rec.Error != "refused" {
		t.Errorf("Refused = %+v", rec)
	}

	good = Capture(t)
	accounting.Report(accounting.Record{Provider: "p", Error: "no response"})
	good.Failed(0)

	good = Capture(t)
	accounting.Report(accounting.Record{Provider: "p", Error: "boom", HTTPStatus: accounting.KnownInt(503)})
	good.Failed(503)

	// A record that is not a refusal fails every check.
	fake := &fakeTB{}
	bad := &Recorder{t: fake}
	accounting.SetLogger(bad.logs.Logger())
	t.Cleanup(func() { accounting.SetLogger(nil) })
	cost := 1.0
	accounting.Report(accounting.Record{
		Provider: "q", Success: true, HTTPStatus: accounting.KnownInt(200),
		InputTokens: accounting.KnownInt(1), OutputTokens: accounting.KnownInt(1),
		CostUSD: accounting.KnownFloat(cost),
	})
	bad.Refused("p")
	got := fake.failures()
	for _, want := range []string{"want a failure", `provider = "q"`, "http_status = 200", "input_tokens = 1", "output_tokens = 1", "cost_usd is known"} {
		if !strings.Contains(got, want) {
			t.Errorf("Refused failures %q lack %q", got, want)
		}
	}

	fake.errors = nil
	bad.Failed(503)
	if got := fake.failures(); !strings.Contains(got, "want a failure") || !strings.Contains(got, "http_status = 200, want 503") {
		t.Errorf("Failed failures = %q", got)
	}
	fake.errors = nil
	bad.Failed(0)
	if got := fake.failures(); !strings.Contains(got, "http_status = 200, want unknown") {
		t.Errorf("Failed(0) failures = %q", got)
	}
}
