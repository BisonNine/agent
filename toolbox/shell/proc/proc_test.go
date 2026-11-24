//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolbox/shell/proc/proctest"
)

var epoch = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

// testCtx bounds a blocking call in real time, so a broken test fails
// instead of hanging.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newManager(t *testing.T, opts proc.Options) (*proc.Manager, *proctest.Clock) {
	t.Helper()
	clk := proctest.NewClock(epoch)
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Clock == nil {
		opts.Clock = clk
	}
	m, err := proc.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { closeManager(t, m, clk) })
	return m, clk
}

// closeManager closes m. A process can escape the terminate signal (one
// being forked as it is sent) and is then killed after the grace period,
// which runs on the fake clock: while Close waits, the clock is moved on.
func closeManager(t *testing.T, m *proc.Manager, clk *proctest.Clock) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- m.Close(testCtx(t)) }()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Close: %v", err)
			}
			return
		case <-tick.C:
			clk.Advance(proc.DefaultGrace)
		}
	}
}

func start(t *testing.T, m *proc.Manager, spec proc.Spec) proc.Info {
	t.Helper()
	if spec.Owner == "" {
		spec.Owner = "alice"
	}
	info, err := m.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start(%q): %v", spec.Command, err)
	}
	return info
}

// waitEnd waits for the job to end and returns the result.
func waitEnd(t *testing.T, m *proc.Manager, id string) proc.WaitResult {
	t.Helper()
	r, err := m.Wait(testCtx(t), id, proc.Until{})
	if err != nil {
		t.Fatalf("Wait(%s): %v", id, err)
	}
	if r.Reason != proc.ReasonEnded {
		t.Fatalf("Wait(%s) reason = %s, want ended", id, r.Reason)
	}
	return r
}

func waitMatch(t *testing.T, m *proc.Manager, id, pattern string) proc.WaitResult {
	t.Helper()
	r, err := m.Wait(testCtx(t), id, proc.Until{Match: regexp.MustCompile(pattern)})
	if err != nil {
		t.Fatalf("Wait(%s, %q): %v", id, pattern, err)
	}
	if r.Reason != proc.ReasonMatch {
		t.Fatalf("Wait(%s, %q) reason = %s, output %q", id, pattern, r.Reason, r.Output)
	}
	return r
}

func processGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Rule 1: a job does not depend on the context that started it.
func TestJobOutlivesStartContext(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	info, err := m.Start(ctx, proc.Spec{Owner: "alice", Command: "read x; echo got $x", Input: true})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if got, _ := m.Get(info.ID); got.State != proc.StateRunning {
		t.Fatalf("state after cancel = %s, want running", got.State)
	}
	if err := m.Write(info.ID, []byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	r := waitEnd(t, m, info.ID)
	if r.Info.State != proc.StateExited || r.Info.ExitCode != 0 {
		t.Fatalf("ended as %s code %d", r.Info.State, r.Info.ExitCode)
	}
	if !strings.Contains(string(r.Output), "got hello") {
		t.Fatalf("output %q lacks the input", r.Output)
	}
}

// Rule 4: output goes to the file as it is produced, stdout and stderr
// interleaved in arrival order.
func TestOutputWrittenToFileAsProduced(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo one; echo two >&2; echo three; read x", Input: true})
	waitMatch(t, m, info.ID, "^three$")

	data, err := os.ReadFile(info.OutputFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(data) != "one\ntwo\nthree\n" {
		t.Fatalf("log while running = %q", data)
	}
	if got, _ := m.Get(info.ID); !got.Running() || got.OutputBytes != int64(len(data)) {
		t.Fatalf("info = %+v", got)
	}
	if err := m.Write(info.ID, []byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitEnd(t, m, info.ID)
}

// Rule 4 and 7.4: past the size limit the log keeps its head and its tail,
// records how much was dropped, and the job keeps running.
func TestOutputCapKeepsHeadAndTail(t *testing.T) {
	m, _ := newManager(t, proc.Options{Limits: proc.Limits{MaxOutputBytes: 1000}})
	info := start(t, m, proc.Spec{Command: "seq 1 5000; echo done"})
	r := waitEnd(t, m, info.ID)

	var full strings.Builder
	for i := 1; i <= 5000; i++ {
		full.WriteString(strconv.Itoa(i) + "\n")
	}
	full.WriteString("done\n")

	if r.Info.State != proc.StateExited || r.Info.ExitCode != 0 {
		t.Fatalf("job ended as %s code %d", r.Info.State, r.Info.ExitCode)
	}
	if r.Info.OutputBytes != int64(full.Len()) {
		t.Fatalf("OutputBytes = %d, want %d", r.Info.OutputBytes, full.Len())
	}
	data, err := os.ReadFile(info.OutputFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	marker := regexp.MustCompile(`\n\[\.\.\. (\d+) bytes dropped \.\.\.\]\n`)
	loc := marker.FindSubmatchIndex(data)
	if loc == nil {
		t.Fatalf("log has no dropped marker: %q", data)
	}
	head, tail := data[:loc[0]], data[loc[1]:]
	dropped, err := strconv.ParseInt(string(data[loc[2]:loc[3]]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full.String(), string(head)) {
		t.Fatalf("head is not the start of the output: %q", head)
	}
	if !strings.HasSuffix(full.String(), string(tail)) || !strings.HasSuffix(string(tail), "4999\n5000\ndone\n") {
		t.Fatalf("tail is not the end of the output: %q", tail)
	}
	if int64(len(head))+dropped+int64(len(tail)) != r.Info.OutputBytes || dropped != r.Info.DroppedBytes {
		t.Fatalf("head %d + dropped %d + tail %d != %d (info dropped %d)",
			len(head), dropped, len(tail), r.Info.OutputBytes, r.Info.DroppedBytes)
	}
	if len(data) > 1000+250+len(marker.Find(data)) {
		t.Fatalf("log is %d bytes, over the limit and slack", len(data))
	}
	if r.Omitted != dropped || !bytes.Equal(r.Output, data) {
		t.Fatalf("Wait returned %d bytes omitted %d, want the file and %d", len(r.Output), r.Omitted, dropped)
	}

	// After a restart the compacted log is read back the same way.
	again, _ := newManager(t, proc.Options{Dir: m.Dir(), Limits: proc.Limits{MaxOutputBytes: 1000}})
	got, ok := again.Get(info.ID)
	if !ok || got.OutputBytes != r.Info.OutputBytes || got.DroppedBytes != dropped {
		t.Fatalf("after restart: %+v", got)
	}
	r2, err := again.Wait(testCtx(t), info.ID, proc.Until{})
	if err != nil || !bytes.Equal(r2.Output, data) || r2.Omitted != dropped {
		t.Fatalf("after restart Wait returned %d bytes omitted %d, err %v", len(r2.Output), r2.Omitted, err)
	}
}

// Remove deletes an ended job's files for good: a restart does not find it.
func TestRemoveLeavesNothing(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo hi"})
	waitEnd(t, m, info.ID)
	if err := m.SetNotify(info.ID, true); !errors.Is(err, proc.ErrEnded) {
		t.Fatalf("SetNotify on an ended job: %v", err)
	}
	if err := m.Remove(info.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	entries, err := os.ReadDir(m.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	again, _ := newManager(t, proc.Options{Dir: m.Dir()})
	if jobs := again.List(""); len(jobs) != 0 {
		t.Fatalf("removed job back after restart: %+v", jobs)
	}
}

// A carriage return ends a line for until, so progress output can match.
func TestWaitUntilMatchesProgressLines(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: `printf 'step 10%%\rstep 100%%\r'; read x`, Input: true})
	r := waitMatch(t, m, info.ID, `^step 100%$`)
	if r.Matched != "step 100%" {
		t.Fatalf("matched %q", r.Matched)
	}
	if err := m.Stop(info.ID, time.Second); err != nil {
		t.Fatal(err)
	}
}

// 7.4: until matching, including an unfinished last line (a prompt).
func TestWaitUntilMatchesNewLines(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{
		Command: "printf 'Password: '; read p; echo ok $p; echo ready on 8080; read q",
		Input:   true,
	})
	r := waitMatch(t, m, info.ID, `^Password:`)
	if r.Matched != "Password: " || string(r.Output) != "Password: " {
		t.Fatalf("matched %q output %q, want the prompt", r.Matched, r.Output)
	}
	if err := m.Write(info.ID, []byte("secret\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	r = waitMatch(t, m, info.ID, `ready on \d+`)
	if r.Matched != "ready on 8080" || !r.Info.Running() || string(r.Output) != "ok secret\nready on 8080\n" {
		t.Fatalf("matched %q running %v output %q", r.Matched, r.Info.Running(), r.Output)
	}
	// Output already returned is not matched again: only new output counts.
	if err := m.Write(info.ID, []byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	r, err := m.Wait(testCtx(t), info.ID, proc.Until{Match: regexp.MustCompile("ready|Password")})
	if err != nil || r.Reason != proc.ReasonEnded || len(r.Output) != 0 {
		t.Fatalf("Wait = %s output %q err %v; want ended with no new output", r.Reason, r.Output, err)
	}
}

// 7.4: wait on a job that has already ended returns at once.
func TestWaitOnEndedJobReturnsAtOnce(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo hi; exit 3"})
	r := waitEnd(t, m, info.ID)
	if string(r.Output) != "hi\n" || r.Info.ExitCode != 3 {
		t.Fatalf("output %q code %d", r.Output, r.Info.ExitCode)
	}
	r, err := m.Wait(testCtx(t), info.ID, proc.Until{Timeout: time.Hour})
	if err != nil || r.Reason != proc.ReasonEnded || len(r.Output) != 0 {
		t.Fatalf("second Wait = %s %q %v", r.Reason, r.Output, err)
	}
}

func TestWaitTimeout(t *testing.T) {
	m, clk := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo hi; read x", Input: true})
	waitMatch(t, m, info.ID, "hi")

	type result struct {
		r   proc.WaitResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := m.Wait(testCtx(t), info.ID, proc.Until{Timeout: 30 * time.Second})
		ch <- result{r, err}
	}()
	clk.BlockUntil(t, 2) // the job's time limit and the wait
	clk.Advance(30 * time.Second)
	got := <-ch
	if got.err != nil || got.r.Reason != proc.ReasonTimeout || !got.r.Info.Running() || len(got.r.Output) != 0 {
		t.Fatalf("Wait = %s running %v output %q err %v", got.r.Reason, got.r.Info.Running(), got.r.Output, got.err)
	}
	if err := m.Stop(info.ID, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestWaitReturnsWhenContextEnds(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo hi; read x", Input: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := m.Wait(ctx, info.ID, proc.Until{})
	if !errors.Is(err, context.Canceled) || r.Reason != proc.ReasonCancelled {
		t.Fatalf("Wait = %s, %v", r.Reason, err)
	}
	if got, _ := m.Get(info.ID); !got.Running() {
		t.Fatal("the job stopped with the wait's context")
	}
}

// Rule 5 and 7.4: stop takes the whole process group, children included.
func TestStopTakesWholeProcessGroup(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "sleep 100 & echo a$!; sleep 100 & echo b$!; wait"})
	r := waitMatch(t, m, info.ID, `^b\d+$`)
	pids := regexp.MustCompile(`[ab](\d+)`).FindAllStringSubmatch(string(r.Output), -1)
	if len(pids) != 2 {
		t.Fatalf("child pids not found in %q", r.Output)
	}
	if err := m.Stop(info.ID, time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got, _ := m.Get(info.ID)
	if got.State != proc.StateStopped || got.StopReason != proc.StopRequested || got.Signal != "terminated" {
		t.Fatalf("after stop: state %s reason %q signal %q", got.State, got.StopReason, got.Signal)
	}
	for _, p := range pids {
		pid, err := strconv.Atoi(p[1])
		if err != nil {
			t.Fatal(err)
		}
		if !processGone(pid) {
			t.Errorf("child %d still running after stop", pid)
		}
	}
	// Stopping again is not an error.
	if err := m.Stop(info.ID, time.Second); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// Rule 5: a group that ignores the terminate signal is killed after the grace.
func TestStopKillsAfterGrace(t *testing.T) {
	m, clk := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "trap '' TERM; echo ready; read x", Input: true})
	waitMatch(t, m, info.ID, "ready")
	done := make(chan error, 1)
	go func() { done <- m.Stop(info.ID, 5*time.Second) }()
	clk.BlockUntil(t, 2) // the job's time limit and the grace
	select {
	case err := <-done:
		t.Fatalf("Stop returned before the grace: %v", err)
	default:
	}
	clk.Advance(5 * time.Second)
	if err := <-done; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got, _ := m.Get(info.ID)
	if got.State != proc.StateStopped || got.Signal != "killed" {
		t.Fatalf("after stop: state %s signal %q", got.State, got.Signal)
	}
}

// Section 6 and 7.4: a start beyond max_jobs is refused, naming the running jobs.
func TestMaxJobsRefusedNamingRunningJobs(t *testing.T) {
	m, _ := newManager(t, proc.Options{Limits: proc.Limits{MaxJobs: 2}})
	a := start(t, m, proc.Spec{Command: "read x", Input: true})
	b := start(t, m, proc.Spec{Command: "read x", Input: true})
	_, err := m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "echo three"})
	var limit *proc.LimitError
	if !errors.As(err, &limit) {
		t.Fatalf("third start: err = %v, want a LimitError", err)
	}
	if limit.Max != 2 || len(limit.Running) != 2 || !strings.Contains(err.Error(), a.ID) || !strings.Contains(err.Error(), b.ID) {
		t.Fatalf("limit error = %v (%+v)", err, limit)
	}
	// The limit is per owner.
	other := start(t, m, proc.Spec{Owner: "bob", Command: "echo hi"})
	waitEnd(t, m, other.ID)
	if n := m.StopAll("alice", time.Second); n != 2 {
		t.Fatalf("StopAll = %d, want 2", n)
	}
	if running := m.Running("alice"); len(running) != 0 {
		t.Fatalf("still running: %+v", running)
	}
	start(t, m, proc.Spec{Command: "true"})
}

// Section 6: a job is stopped once it runs past max_job_seconds.
func TestTimeLimitStopsJob(t *testing.T) {
	var ended []proc.Info
	var mu sync.Mutex
	m, clk := newManager(t, proc.Options{
		Limits: proc.Limits{MaxJobTime: time.Hour},
		OnEnd: func(i proc.Info) {
			mu.Lock()
			ended = append(ended, i)
			mu.Unlock()
		},
	})
	info := start(t, m, proc.Spec{Command: "echo up; read x", Input: true, Notify: true})
	waitMatch(t, m, info.ID, "up")
	clk.BlockUntil(t, 1)
	clk.Advance(time.Hour)
	r := waitEnd(t, m, info.ID)
	if r.Info.State != proc.StateStopped || r.Info.StopReason != proc.StopTimeLimit {
		t.Fatalf("state %s reason %q", r.Info.State, r.Info.StopReason)
	}
	if got := r.Info.Ended.Sub(r.Info.Started); got != time.Hour {
		t.Fatalf("ran %v by the clock, want 1h", got)
	}
	closeManager(t, m, clk)
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0].ID != info.ID || !ended[0].Notify {
		t.Fatalf("OnEnd calls = %+v, want one for %s", ended, info.ID)
	}
}

// Rule 8 and 7.4: a job recorded as running when the manager starts was
// lost: it is marked lost, announced once, and never relaunched.
func TestLostAfterRestart(t *testing.T) {
	dir := t.TempDir()
	first, _ := newManager(t, proc.Options{Dir: dir})
	info := start(t, first, proc.Spec{Command: "echo before; read x", Input: true, Notify: true})
	waitMatch(t, first, info.ID, "before")
	finished := start(t, first, proc.Spec{Command: "echo done"})
	waitEnd(t, first, finished.ID)

	// A second manager over the same directory stands for the restarted
	// service: the first one's job is still recorded as running.
	var lost []proc.Info
	second, _ := newManager(t, proc.Options{Dir: dir, OnEnd: func(i proc.Info) { lost = append(lost, i) }})
	if len(lost) != 1 || lost[0].ID != info.ID || lost[0].State != proc.StateLost || !lost[0].Notify {
		t.Fatalf("lost = %+v, want %s once", lost, info.ID)
	}
	got, ok := second.Get(info.ID)
	if !ok || got.State != proc.StateLost || got.Owner != "alice" || got.Ended.IsZero() {
		t.Fatalf("lost job = %+v", got)
	}
	if done, _ := second.Get(finished.ID); done.State != proc.StateExited {
		t.Fatalf("finished job after restart = %s", done.State)
	}
	if len(second.Running("")) != 0 {
		t.Fatal("a lost job was relaunched")
	}
	r, err := second.Wait(testCtx(t), info.ID, proc.Until{})
	if err != nil || string(r.Output) != "before\n" {
		t.Fatalf("lost job's log = %q, %v", r.Output, err)
	}

	// Announced once: a third start finds it lost, not running.
	var again []proc.Info
	newManager(t, proc.Options{Dir: dir, OnEnd: func(i proc.Info) { again = append(again, i) }})
	if len(again) != 0 {
		t.Fatalf("lost job announced again: %+v", again)
	}
}

// Rule 9: shutdown stops every job and refuses new ones.
func TestCloseStopsEveryJob(t *testing.T) {
	m, clk := newManager(t, proc.Options{})
	a := start(t, m, proc.Spec{Owner: "alice", Command: "echo a; sleep 100"})
	b := start(t, m, proc.Spec{Owner: "bob", Command: "echo b; sleep 100"})
	waitMatch(t, m, a.ID, "a")
	waitMatch(t, m, b.ID, "b")
	closeManager(t, m, clk)
	for _, id := range []string{a.ID, b.ID} {
		got, _ := m.Get(id)
		if got.State != proc.StateStopped || got.StopReason != proc.StopShutdown {
			t.Errorf("%s after Close: %s %q", id, got.State, got.StopReason)
		}
	}
	if _, err := m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "true"}); !errors.Is(err, proc.ErrClosed) {
		t.Fatalf("Start after Close: %v", err)
	}
}

// Section 6: ended jobs' logs and records are removed after the retention.
func TestExpiredLogsRemoved(t *testing.T) {
	m, clk := newManager(t, proc.Options{Limits: proc.Limits{LogRetention: time.Hour}})
	old := start(t, m, proc.Spec{Command: "echo old"})
	waitEnd(t, m, old.ID)
	record := strings.TrimSuffix(old.OutputFile, ".log") + ".json"
	for _, p := range []string{old.OutputFile, record} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing before expiry: %v", p, err)
		}
	}
	clk.Advance(30 * time.Minute)
	waitEnd(t, m, start(t, m, proc.Spec{Command: "true"}).ID)
	if _, ok := m.Get(old.ID); !ok {
		t.Fatal("job removed before its retention ended")
	}
	clk.Advance(31 * time.Minute)
	start(t, m, proc.Spec{Command: "true"})
	if _, ok := m.Get(old.ID); ok {
		t.Fatal("expired job still listed")
	}
	for _, p := range []string{old.OutputFile, record} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not removed: %v", p, err)
		}
	}
}

func TestTrackDirRecordsWhereCommandEnded(t *testing.T) {
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, _ := newManager(t, proc.Options{})
	// The command's own use of descriptor 3 is left alone.
	info := start(t, m, proc.Spec{Command: "exec 3>fd3.txt; echo three >&3; cd sub; echo here; exit 3", Dir: work, TrackDir: true})
	r := waitEnd(t, m, info.ID)
	if data, err := os.ReadFile(filepath.Join(work, "fd3.txt")); err != nil || string(data) != "three\n" {
		t.Fatalf("fd 3 file = %q, %v", data, err)
	}
	want := realPath(t, filepath.Join(work, "sub"))
	got := realPath(t, r.Info.EndDir)
	if got != want || r.Info.ExitCode != 3 || string(r.Output) != "here\n" {
		t.Fatalf("EndDir %q (want %q) code %d output %q", r.Info.EndDir, want, r.Info.ExitCode, r.Output)
	}
}

func TestWriteRefusals(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	if err := m.Write("j00000000", []byte("x")); !errors.Is(err, proc.ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	noInput := start(t, m, proc.Spec{Command: "echo x; sleep 100"})
	waitMatch(t, m, noInput.ID, "x")
	if err := m.Write(noInput.ID, []byte("x")); !errors.Is(err, proc.ErrNoInput) {
		t.Fatalf("no input: %v", err)
	}
	ended := start(t, m, proc.Spec{Command: "true", Input: true})
	waitEnd(t, m, ended.ID)
	if err := m.Write(ended.ID, []byte("x")); !errors.Is(err, proc.ErrEnded) {
		t.Fatalf("ended: %v", err)
	}
}

func TestEnvAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "shell")
	m, _ := newManager(t, proc.Options{Dir: dir, Modes: fsmode.Modes{Dir: 0o750, File: 0o640}})
	info := start(t, m, proc.Spec{Command: "echo $GREETING", Env: []string{"GREETING=hello", "PATH=" + os.Getenv("PATH")}})
	r := waitEnd(t, m, info.ID)
	if string(r.Output) != "hello\n" {
		t.Fatalf("output %q", r.Output)
	}
	checkMode(t, dir, 0o750)
	checkMode(t, info.OutputFile, 0o640)
	checkMode(t, strings.TrimSuffix(info.OutputFile, ".log")+".json", 0o640)
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("%q: %v", path, err)
	}
	return p
}

func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != want {
		t.Errorf("%s mode %o, want %o", path, st.Mode().Perm(), want)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	for _, opts := range []proc.Options{
		{},
		{Dir: t.TempDir(), Limits: proc.Limits{MaxJobs: -1}},
		{Dir: t.TempDir(), Limits: proc.Limits{MaxOutputBytes: -1}},
		{Dir: t.TempDir(), Grace: -time.Second},
	} {
		if _, err := proc.New(opts); err == nil {
			t.Errorf("New(%+v) accepted", opts)
		}
	}
}

// Records name a working directory inside Root relative to it, so the root
// can be moved: a manager opened on the new location names the new one.
func TestRecordsFollowMovedRoot(t *testing.T) {
	base := t.TempDir()
	oldRoot := filepath.Join(base, "data")
	work := filepath.Join(oldRoot, "work")
	outside := t.TempDir()
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	first, clk := newManager(t, proc.Options{Dir: filepath.Join(oldRoot, "state", "shell"), Root: oldRoot})
	running := start(t, first, proc.Spec{Command: "echo up; read x", Dir: work, Input: true, Notify: true})
	waitMatch(t, first, running.ID, "up")
	atRoot := start(t, first, proc.Spec{Command: "true", Dir: oldRoot})
	waitEnd(t, first, atRoot.ID)
	away := start(t, first, proc.Spec{Command: "true", Dir: outside})
	waitEnd(t, first, away.ID)
	noDir := start(t, first, proc.Spec{Command: "true"})
	waitEnd(t, first, noDir.ID)
	closeManager(t, first, clk) // the service stops; the running job is stopped

	newRoot := filepath.Join(base, "moved")
	if err := os.Rename(oldRoot, newRoot); err != nil {
		t.Fatal(err)
	}
	var announced []proc.Info
	second, _ := newManager(t, proc.Options{
		Dir:   filepath.Join(newRoot, "state", "shell"),
		Root:  newRoot,
		OnEnd: func(i proc.Info) { announced = append(announced, i) },
	})
	wantDir := map[string]string{
		running.ID: filepath.Join(newRoot, "work"),
		atRoot.ID:  newRoot,
		away.ID:    outside,
		noDir.ID:   "",
	}
	for id, dir := range wantDir {
		if got, ok := second.Get(id); !ok || got.Dir != dir {
			t.Errorf("job %s dir = %q (found %v), want %q", id, got.Dir, ok, dir)
		}
	}
	if len(announced) != 1 || announced[0].ID != running.ID || announced[0].Dir != wantDir[running.ID] {
		t.Fatalf("announced = %+v, want %s in %s", announced, running.ID, wantDir[running.ID])
	}

	stored := map[string]string{running.ID: "work", atRoot.ID: ".", away.ID: outside, noDir.ID: ""}
	for id, dir := range stored {
		data, err := os.ReadFile(filepath.Join(second.Dir(), id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(oldRoot)) {
			t.Errorf("record %s names the old root:\n%s", id, data)
		}
		var rec struct {
			Dir string `json:"dir"`
		}
		if err := json.Unmarshal(data, &rec); err != nil || rec.Dir != dir {
			t.Errorf("record %s dir = %q (%v), want %q", id, rec.Dir, err, dir)
		}
	}
}

// A record written with an absolute working directory (by a manager with
// no Root) names it as it is, and is written relative when next saved.
func TestAbsoluteRecordLoadsUnderRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state", "shell")
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	first, clk := newManager(t, proc.Options{Dir: dir})
	info := start(t, first, proc.Spec{Command: "echo up; read x", Dir: work, Input: true})
	waitMatch(t, first, info.ID, "up")
	ended := start(t, first, proc.Spec{Command: "true", Dir: work})
	waitEnd(t, first, ended.ID)
	if data, err := os.ReadFile(filepath.Join(dir, ended.ID+".json")); err != nil || !bytes.Contains(data, []byte(`"dir": "`+work+`"`)) {
		t.Fatalf("record without Root = %s, %v; want the absolute dir", data, err)
	}

	// The first manager's job is still recorded as running: the second
	// finds it lost and saves it.
	second, _ := newManager(t, proc.Options{Dir: dir, Root: root})
	for _, id := range []string{info.ID, ended.ID} {
		if got, _ := second.Get(id); got.Dir != work {
			t.Errorf("job %s dir = %q, want %q", id, got.Dir, work)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, info.ID+".json"))
	if err != nil || !bytes.Contains(data, []byte(`"dir": "work"`)) {
		t.Errorf("saved record = %s, %v; want dir relative to the root", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, ended.ID+".json")); err != nil || !bytes.Contains(data, []byte(`"dir": "`+work+`"`)) {
		t.Errorf("unsaved record rewritten: %s, %v", data, err)
	}
	closeManager(t, first, clk)
}
