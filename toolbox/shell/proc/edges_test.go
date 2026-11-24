//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
)

// logLines is a slog handler that keeps every message and runs onMessage,
// when set, as each is logged.
type logLines struct {
	mu        sync.Mutex
	msgs      []string
	onMessage func(msg string)
}

func (l *logLines) Enabled(context.Context, slog.Level) bool { return true }
func (l *logLines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.msgs = append(l.msgs, r.Message)
	fn := l.onMessage
	l.mu.Unlock()
	if fn != nil {
		fn(r.Message)
	}
	return nil
}
func (l *logLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logLines) WithGroup(string) slog.Handler      { return l }

func (l *logLines) has(msg string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.msgs, msg)
}

func TestStartRefusesBadSpecs(t *testing.T) {
	if !proc.Supported() {
		t.Fatal("background jobs are not supported on a unix system")
	}
	m, _ := newManager(t, proc.Options{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		spec proc.Spec
	}{
		{"no owner", context.Background(), proc.Spec{Command: "true"}},
		{"no command", context.Background(), proc.Spec{Owner: "alice", Command: "  "}},
		{"ended context", cancelled, proc.Spec{Owner: "alice", Command: "true"}},
		{"missing directory", context.Background(), proc.Spec{Owner: "alice", Command: "true", Dir: filepath.Join(t.TempDir(), "gone")}},
		{"missing directory, tracked", context.Background(), proc.Spec{Owner: "alice", Command: "true", Input: true, TrackDir: true, Dir: filepath.Join(t.TempDir(), "gone")}},
	} {
		if _, err := m.Start(tc.ctx, tc.spec); err == nil {
			t.Errorf("%s: Start accepted", tc.name)
		}
	}
	if jobs := m.List(""); len(jobs) != 0 {
		t.Fatalf("a refused start left jobs: %+v", jobs)
	}
	if left, err := os.ReadDir(m.Dir()); err != nil || len(left) != 0 {
		t.Fatalf("a refused start left files: %v, %v", left, err)
	}
}

func TestNewRefusesAnUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.New(proc.Options{Dir: filepath.Join(file, "shell")}); err == nil {
		t.Fatal("New accepted a directory under a file")
	}
}

// Calls naming a job that does not exist fail with ErrUnknown; a running
// job is not removed; SetNotify changes a running job and refuses an ended
// one.
func TestJobCallsRefusals(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	ctx := testCtx(t)
	const nobody = "j00000000"
	if err := m.Stop(nobody, time.Second); !errors.Is(err, proc.ErrUnknown) {
		t.Errorf("Stop: %v", err)
	}
	if err := m.Remove(nobody); !errors.Is(err, proc.ErrUnknown) {
		t.Errorf("Remove: %v", err)
	}
	if err := m.SetNotify(nobody, true); !errors.Is(err, proc.ErrUnknown) {
		t.Errorf("SetNotify: %v", err)
	}
	if _, err := m.Wait(ctx, nobody, proc.Until{}); !errors.Is(err, proc.ErrUnknown) {
		t.Errorf("Wait: %v", err)
	}

	running := start(t, m, proc.Spec{Command: "echo up; sleep 100", Notify: true})
	waitMatch(t, m, running.ID, "up")
	if err := m.Remove(running.ID); err == nil {
		t.Fatal("a running job was removed")
	}
	if err := m.SetNotify(running.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Get(running.ID); got.Notify {
		t.Fatal("SetNotify(false) left Notify on")
	}
	if err := m.Stop(running.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := m.SetNotify(running.ID, true); !errors.Is(err, proc.ErrEnded) {
		t.Fatalf("SetNotify of an ended job: %v", err)
	}
	if err := m.Remove(running.ID); err != nil {
		t.Fatal(err)
	}
}

// Jobs are listed in the order they started.
func TestListInStartOrder(t *testing.T) {
	m, clk := newManager(t, proc.Options{})
	want := make([]string, 0, 5)
	for range 5 {
		info := start(t, m, proc.Spec{Command: "true"})
		want = append(want, info.ID)
		clk.Advance(time.Second)
	}
	got := make([]string, 0, len(want))
	for _, info := range m.List("alice") {
		got = append(got, info.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

// Close returns with its context's error when a job outlives it; the job
// is still stopped after the grace.
func TestCloseContextEnds(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "trap '' TERM; echo up; sleep 100"})
	waitMatch(t, m, info.ID, "up")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v", err)
	}
}

// A start that Close overtakes is stopped and reported closed, never
// announced; a start beyond MaxJobs is refused while another is still
// starting.
func TestStartRacingCloseAndLimit(t *testing.T) {
	logs := &logLines{}
	var ended []proc.Info
	var mu sync.Mutex
	m, _ := newManager(t, proc.Options{
		Logger: slog.New(logs),
		Limits: proc.Limits{MaxJobs: 1},
		OnEnd: func(i proc.Info) {
			mu.Lock()
			ended = append(ended, i)
			mu.Unlock()
		},
	})
	var limitErr error
	var once sync.Once
	logs.onMessage = func(msg string) {
		if msg != "started shell job" {
			return
		}
		once.Do(func() {
			// The first job is still starting: a second start of its owner
			// counts it.
			_, limitErr = m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "true"})
			go func() { _ = m.Close(context.Background()) }() //nolint:errcheck // the test waits on Start
			for !closedYet(m) {
				time.Sleep(time.Millisecond)
			}
		})
	}
	_, err := m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "sleep 100", Notify: true})
	if !errors.Is(err, proc.ErrClosed) {
		t.Fatalf("Start overtaken by Close = %v", err)
	}
	var le *proc.LimitError
	if !errors.As(limitErr, &le) || le.Max != 1 {
		t.Fatalf("a start while another was starting = %v", limitErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 0 {
		t.Fatalf("an overtaken start was announced: %+v", ended)
	}
}

// closedYet reports whether m refuses starts. It asks for alice, who is
// at her limit, so the probe never starts a job.
func closedYet(m *proc.Manager) bool {
	_, err := m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "true"})
	return errors.Is(err, proc.ErrClosed)
}

// With the real clock a job runs and a Wait with a timeout ends with it.
func TestRealClock(t *testing.T) {
	m, err := proc.New(proc.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := m.Close(testCtx(t)); cerr != nil {
			t.Error(cerr)
		}
	}()
	info, err := m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Wait(testCtx(t), info.ID, proc.Until{Timeout: time.Minute})
	if err != nil || r.Reason != proc.ReasonEnded || string(r.Output) != "hi\n" || info.Started.IsZero() {
		t.Fatalf("Wait = %+v, %v", r, err)
	}
}

// A job whose log is gone reports the read error rather than empty output.
func TestWaitReportsUnreadableLog(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo lost"})
	waitEndQuiet(t, m, info.ID)
	if err := os.Remove(info.OutputFile); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(testCtx(t), info.ID, proc.Until{}); err == nil {
		t.Fatal("Wait of a job whose log is gone succeeded")
	}
	if _, err := m.Wait(testCtx(t), info.ID, proc.Until{Match: regexp.MustCompile("lost")}); err == nil {
		t.Fatal("Wait with a match of a job whose log is gone succeeded")
	}
}

// waitEndQuiet waits until the job has ended without reading its output.
func waitEndQuiet(t *testing.T, m *proc.Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if got, _ := m.Get(id); !got.Running() && !got.Ended.IsZero() {
			if err := m.SetNotify(id, false); errors.Is(err, proc.ErrEnded) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not end", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A command that closes the descriptor its end directory is written to
// ends with no end directory.
func TestTrackDirUnwritten(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "exec 9>&-; echo done", TrackDir: true})
	if r := waitEnd(t, m, info.ID); r.Info.EndDir != "" || !strings.HasPrefix(string(r.Output), "done\n") {
		t.Fatalf("EndDir %q, output %q", r.Info.EndDir, r.Output)
	}
}

// A process that left the job's group and still holds its output does not
// keep Stop waiting: the output is let go and the job ends.
func TestStopLetsGoOfEscapedOutput(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	// The real clock: the grace runs out while the escaped process holds on.
	m, err := proc.New(proc.Options{Dir: t.TempDir(), Grace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cerr := m.Close(testCtx(t)); cerr != nil {
			t.Error(cerr)
		}
	}()
	// The escaped loop ends at its first write after the output is closed.
	info := start(t, m, proc.Spec{Command: "setsid sh -c 'while echo tick; do sleep 0.05; done' & echo started"})
	waitMatch(t, m, info.ID, "tick")
	done := make(chan error, 1)
	go func() { done <- m.Stop(info.ID, time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Stop waited for the escaped process")
	}
	if got, _ := m.Get(info.ID); got.Running() {
		t.Fatalf("job still running: %+v", got)
	}
}

// New removes leftover temporary files, skips records it cannot read or
// that are not valid, and lists the rest.
func TestLoadSkipsBadRecords(t *testing.T) {
	dir := t.TempDir()
	first, _ := newManager(t, proc.Options{Dir: dir})
	good := start(t, first, proc.Spec{Command: "echo kept"})
	waitEnd(t, first, good.ID)

	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("j11111111.cwd", "/tmp\n")
	write("j22222222.json.tmp", "{")
	write("j33333333.json", "not json")
	write("j44444444.json", `{"id":"j55555555"}`)
	write("notes.json", "{}")
	if err := os.Mkdir(filepath.Join(dir, "j66666666.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A leftover that cannot be removed is logged and skipped.
	if err := os.MkdirAll(filepath.Join(dir, "j77777777.cwd", "inner"), 0o700); err != nil {
		t.Fatal(err)
	}

	logs := &logLines{}
	second, _ := newManager(t, proc.Options{Dir: dir, Logger: slog.New(logs)})
	ids := make([]string, 0, 1)
	for _, info := range second.List("") {
		ids = append(ids, info.ID)
	}
	if !slices.Equal(ids, []string{good.ID}) {
		t.Fatalf("listed %v, want %s alone", ids, good.ID)
	}
	for _, name := range []string{"j11111111.cwd", "j22222222.json.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("leftover %s not removed: %v", name, err)
		}
	}
	for _, msg := range []string{"shell job record not valid", "shell job record not read", "leftover shell job file not removed"} {
		if !logs.has(msg) {
			t.Errorf("no %q logged", msg)
		}
	}
	if !strings.HasSuffix(good.OutputFile, ".log") {
		t.Fatalf("output file %q", good.OutputFile)
	}
}
