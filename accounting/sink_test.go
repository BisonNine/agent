package accounting

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// capture is a slog handler that keeps the accounting records and counts
// the error records it receives. Records without the component attribute
// "accounting" are ignored.
type capture struct {
	mu      sync.Mutex
	records []string
	errors  int
}

// captureHandler is the slog.Handler view of a capture, carrying the
// attributes added with WithAttrs.
type captureHandler struct {
	c     *capture
	attrs []slog.Attr
}

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]string{}
	for _, a := range h.attrs {
		fields[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.String()
		return true
	})
	if fields["component"] != "accounting" {
		return nil
	}
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	switch {
	case r.Level == slog.LevelError:
		h.c.errors++
	case r.Level == slog.LevelInfo && r.Message == "accounting record":
		if s, ok := fields["accounting"]; ok {
			h.c.records = append(h.c.records, s)
		}
	}
	return nil
}

func (h captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return captureHandler{c: h.c, attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h captureHandler) WithGroup(string) slog.Handler { return h }

func (c *capture) snapshot() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.records...), c.errors
}

// useCapture sets a capture as the accounting logger for the test and
// restores the default after.
func useCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	SetLogger(slog.New(captureHandler{c: c}))
	t.Cleanup(func() { SetLogger(nil) })
	return c
}

// useBuffer sets the queue size for sinks started by the test.
func useBuffer(t *testing.T, n int) {
	t.Helper()
	old := bufferSize
	bufferSize = n
	t.Cleanup(func() { bufferSize = old })
}

// startSink starts the sink in a temporary directory and closes it after
// the test if the test has not.
func startSink(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "acct")
	if err := Start(dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return dir
}

func testRecord(agent string, n int) Record {
	return Record{
		Time:         time.Now(),
		Agent:        agent,
		Project:      DefaultProject,
		Kind:         KindText,
		Provider:     "test",
		Transport:    TransportAPI,
		Model:        fmt.Sprintf("m%d", n),
		OutputTokens: KnownInt(int64(n)),
		Success:      true,
	}
}

// readLines parses every line of the accounting file as a Record.
func readLines(t *testing.T, dir string) []Record {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only file
	var out []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d is not a record: %v: %s", len(out)+1, err, sc.Text())
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStartCreatesPrivateFile(t *testing.T) {
	dir := startSink(t)
	Report(testRecord("alice", 1))
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("directory mode = %v, want no group or other access", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("file mode = %v, want no group or other access", perm)
	}
	if got := readLines(t, dir); len(got) != 1 || got[0].Agent != "alice" {
		t.Errorf("records = %+v", got)
	}
}

func TestStartTwiceFailsAndRestartAppends(t *testing.T) {
	dir := startSink(t)
	if err := Start(dir); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start = %v, want ErrAlreadyStarted", err)
	}
	Report(testRecord("alice", 1))
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	if err := Start(dir); err != nil {
		t.Fatalf("Start after Close: %v", err)
	}
	Report(testRecord("bob", 2))
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	got := readLines(t, dir)
	if len(got) != 2 || got[0].Agent != "alice" || got[1].Agent != "bob" {
		t.Errorf("records = %+v", got)
	}
}

func TestStartRequiresDirectory(t *testing.T) {
	if err := Start(""); err == nil {
		t.Fatal("Start(\"\") succeeded")
	}
}

func TestConcurrentReportsAllWritten(t *testing.T) {
	const goroutines, each = 50, 200
	c := useCapture(t)
	dir := startSink(t)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				Report(testRecord(fmt.Sprintf("agent%d", g), i))
			}
		})
	}
	wg.Wait()
	if err := Close(); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, dir)
	if len(got) != goroutines*each {
		t.Fatalf("file has %d records, want %d", len(got), goroutines*each)
	}
	perAgent := map[string]int{}
	for _, r := range got {
		perAgent[r.Agent]++
	}
	for g := range goroutines {
		if n := perAgent[fmt.Sprintf("agent%d", g)]; n != each {
			t.Errorf("agent%d has %d records, want %d", g, n, each)
		}
	}
	if logged, _ := c.snapshot(); len(logged) != 0 {
		t.Errorf("%d records went to the logger while the sink was running", len(logged))
	}
}

func TestReportWithoutSinkGoesToLogger(t *testing.T) {
	c := useCapture(t)
	rec := testRecord("alice", 7)
	Report(rec)

	logged, _ := c.snapshot()
	if len(logged) != 1 {
		t.Fatalf("logger got %d records, want 1", len(logged))
	}
	var back Record
	if err := json.Unmarshal([]byte(logged[0]), &back); err != nil {
		t.Fatalf("logged value is not a record: %v: %s", err, logged[0])
	}
	if back.Agent != "alice" || back.Model != "m7" {
		t.Errorf("logged record = %+v", back)
	}
}

func TestReportAfterCloseGoesToLogger(t *testing.T) {
	c := useCapture(t)
	dir := startSink(t)
	Report(testRecord("alice", 1))
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	Report(testRecord("bob", 2))

	if got := readLines(t, dir); len(got) != 1 || got[0].Agent != "alice" {
		t.Errorf("file records = %+v", got)
	}
	logged, _ := c.snapshot()
	if len(logged) != 1 {
		t.Fatalf("logger got %d records, want 1", len(logged))
	}
	var back Record
	if err := json.Unmarshal([]byte(logged[0]), &back); err != nil || back.Agent != "bob" {
		t.Errorf("logged record = %s (%v)", logged[0], err)
	}
}

func TestCloseWithoutSink(t *testing.T) {
	if err := Close(); err != nil {
		t.Fatalf("Close without Start = %v", err)
	}
}

func TestReportRacingCloseLosesNothing(t *testing.T) {
	const goroutines, each = 20, 100
	useBuffer(t, 4)
	c := useCapture(t)
	dir := startSink(t)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				Report(testRecord(fmt.Sprintf("agent%d", g), i))
			}
		})
	}
	// Close while the reporters are still running.
	time.Sleep(time.Millisecond)
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	inFile := len(readLines(t, dir))
	logged, _ := c.snapshot()
	if total := inFile + len(logged); total != goroutines*each {
		t.Errorf("file %d + logger %d = %d records, want %d", inFile, len(logged), total, goroutines*each)
	}
}

func TestBackPressureDoesNotDeadlock(t *testing.T) {
	const total = 500
	useBuffer(t, 1)
	dir := startSink(t)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var wg sync.WaitGroup
		for g := range 5 {
			wg.Go(func() {
				for i := range total / 5 {
					Report(testRecord(fmt.Sprintf("agent%d", g), i))
				}
			})
		}
		wg.Wait()
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("reporters blocked: back-pressure deadlocked")
	}
	if err := Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(readLines(t, dir)); got != total {
		t.Errorf("file has %d records, want %d", got, total)
	}
}

func TestWriteFailureFallsBackToLogger(t *testing.T) {
	c := useCapture(t)
	dir := startSink(t)

	// Close the file under the writer so its next write fails.
	mu.RLock()
	f := current.file
	mu.RUnlock()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	Report(testRecord("alice", 3))
	// Close reports the file's second close as an error; the record must
	// still have reached the logger.
	if err := Close(); err == nil {
		t.Error("Close did not report the failed file close")
	}

	logged, errs := c.snapshot()
	if len(logged) != 1 {
		t.Fatalf("logger got %d records, want 1", len(logged))
	}
	if errs == 0 {
		t.Error("write failure was not logged as an error")
	}
	if got := readLines(t, dir); len(got) != 0 {
		t.Errorf("file has %d records, want 0", len(got))
	}
}

func TestConcurrentCloseWaitsForDrain(t *testing.T) {
	const total = 20000
	useBuffer(t, total)
	dir := startSink(t)
	for i := range total {
		Report(testRecord("alice", i))
	}

	// Whichever Close returns, the file must already hold every record.
	counts := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			counts <- len(readLines(t, dir))
		})
	}
	wg.Wait()
	close(counts)
	for n := range counts {
		if n != total {
			t.Errorf("a Close returned with %d records in the file, want %d", n, total)
		}
	}
}

func TestStartWaitsForDrainingClose(t *testing.T) {
	const first, second = 20000, 100
	useBuffer(t, first)
	dir := startSink(t)
	for i := range first {
		Report(testRecord("first", i))
	}

	closed := make(chan error, 1)
	go func() { closed <- Close() }()
	// Restart as soon as the old sink is gone; Start must not begin a second
	// writer while the first is still draining.
	for {
		err := Start(dir)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrAlreadyStarted) {
			t.Fatalf("Start: %v", err)
		}
		runtime.Gosched()
	}
	for i := range second {
		Report(testRecord("second", i))
	}
	if err := <-closed; err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	got := readLines(t, dir)
	if len(got) != first+second {
		t.Fatalf("file has %d records, want %d", len(got), first+second)
	}
	for i, r := range got {
		want := "first"
		if i >= first {
			want = "second"
		}
		if r.Agent != want {
			t.Fatalf("record %d is from %q, want %q: the restarted writer ran during the drain", i, r.Agent, want)
		}
	}
}

func TestDefaultLoggerIsSilent(t *testing.T) {
	// Without SetLogger, a record with no sink goes nowhere: no panic, and
	// nothing on the process's default logger.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	SetLogger(nil)

	Report(testRecord("alice", 1))
	if buf.Len() != 0 {
		t.Errorf("default logger got output: %s", buf.String())
	}
}

func TestLoggerRecordsCarryComponent(t *testing.T) {
	// The capture keeps only records whose component is "accounting", so a
	// record seen at all carries it.
	c := useCapture(t)
	Report(testRecord("bob", 2))
	if logged, _ := c.snapshot(); len(logged) != 1 {
		t.Fatalf("logger got %d records with component accounting, want 1", len(logged))
	}
}
