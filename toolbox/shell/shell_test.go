//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolbox/shell/proc/proctest"
	"go.bison9.com/agent/toolspec"
)

type harness struct {
	t       *testing.T
	tools   *Tools
	clk     *proctest.Clock
	ws      string
	notices chan Notice
	ended   chan string     // ids of ended jobs, after their notice (if any) was sent
	seen    map[string]bool // ended ids already read from ended

	mu     sync.Mutex
	audits []AuditEvent
}

// newHarness builds the tools over a temporary workspace and a fake clock.
// Agents named "<owner>-clone" act as their owner.
func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, ws: t.TempDir(), notices: make(chan Notice, 16), ended: make(chan string, 256)}
	h.clk = proctest.NewClock(time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC))
	cfg := Config{
		Jobs: proc.Options{
			Dir:   filepath.Join(h.ws, "state", "shell"),
			Clock: h.clk,
			// The host's hook runs after the tools have sent any notice.
			OnEnd: func(i proc.Info) { h.ended <- i.ID },
		},
		Workspace: func(*toolspec.ToolCall) string { return h.ws },
		Owner: func(c *toolspec.ToolCall) string {
			return strings.TrimSuffix(c.AgentID, "-clone")
		},
		Notice: func(n Notice) { h.notices <- n },
		Audit: func(e AuditEvent) {
			e.Call = nil
			h.mu.Lock()
			h.audits = append(h.audits, e)
			h.mu.Unlock()
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	tools, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.tools = tools
	t.Cleanup(h.close)
	return h
}

// close closes the tools. A process can escape the terminate signal (one
// being forked as it is sent) and is then killed after the grace period,
// which runs on the fake clock: while Close waits, the clock is moved on.
func (h *harness) close() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.tools.Close(ctx) }()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				h.t.Errorf("Close: %v", err)
			}
			return
		case <-tick.C:
			h.clk.Advance(proc.DefaultGrace)
		}
	}
}

// run calls a tool as agent in session "s1" with a bounded real-time context.
func (h *harness) run(tool, agent string, args map[string]any) *toolspec.Result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return h.runCtx(ctx, tool, agent, "s1", args)
}

func (h *harness) runCtx(ctx context.Context, tool, agent, session string, args map[string]any) *toolspec.Result {
	h.t.Helper()
	var def *toolspec.ToolDefinition
	for _, d := range h.tools.RegisterTools(toolspec.Deps{}) {
		if d.Name == tool {
			def = &d
		}
	}
	if def == nil {
		h.t.Fatalf("no tool %q", tool)
	}
	res, err := def.Handler(&toolspec.ToolCall{Ctx: ctx, Args: args, AgentID: agent, Session: session})
	if err != nil {
		h.t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func (h *harness) exec(agent, command string, extra map[string]any) *toolspec.Result {
	h.t.Helper()
	args := map[string]any{"command": command}
	maps.Copy(args, extra)
	return h.run("exec", agent, args)
}

func (h *harness) ctrl(agent, action string, extra map[string]any) *toolspec.Result {
	h.t.Helper()
	args := map[string]any{"action": action}
	maps.Copy(args, extra)
	return h.run("ctrl", agent, args)
}

func (h *harness) auditKinds() []AuditEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]AuditEvent(nil), h.audits...)
}

func (h *harness) notice() Notice {
	h.t.Helper()
	select {
	case n := <-h.notices:
		return n
	case <-time.After(10 * time.Second):
		h.t.Fatal("no end-of-job notice")
		return Notice{}
	}
}

// noNotice checks that no notice was sent for job id, once its end has
// been fully handled.
func (h *harness) noNotice(id string) {
	h.t.Helper()
	if h.seen == nil {
		h.seen = make(map[string]bool)
	}
	timeout := time.After(10 * time.Second)
	for !h.seen[id] {
		select {
		case got := <-h.ended:
			h.seen[got] = true
		case <-timeout:
			h.t.Fatalf("job %s did not end", id)
		}
	}
	select {
	case n := <-h.notices:
		h.t.Fatalf("unexpected notice: %+v", n)
	default:
	}
}

var jobIDPattern = regexp.MustCompile(`\bj[0-9a-f]{8}\b`)

func jobID(t *testing.T, res *toolspec.Result) string {
	t.Helper()
	id := jobIDPattern.FindString(res.ForLLM)
	if id == "" {
		t.Fatalf("no job id in %q", res.ForLLM)
	}
	return id
}

func want(t *testing.T, res *toolspec.Result, isError bool, parts ...string) {
	t.Helper()
	if res.IsError != isError {
		t.Errorf("IsError = %v, want %v: %s", res.IsError, isError, res.ForLLM)
	}
	for _, p := range parts {
		if !strings.Contains(res.ForLLM, p) {
			t.Errorf("result lacks %q:\n%s", p, res.ForLLM)
		}
	}
}

func sameDir(t *testing.T, got, wantDir string) {
	t.Helper()
	g, err := filepath.EvalSymlinks(strings.TrimSpace(got))
	if err != nil {
		t.Fatalf("%q: %v", got, err)
	}
	w, err := filepath.EvalSymlinks(wantDir)
	if err != nil {
		t.Fatal(err)
	}
	if g != w {
		t.Errorf("directory %q, want %q", got, wantDir)
	}
}

// startForeground runs a foreground command in the background of the test
// and returns its result channel once the command's wait timer is set.
func (h *harness) startForeground(ctx context.Context, command string, timeout int) <-chan *toolspec.Result {
	base := h.clk.Timers()
	ch := make(chan *toolspec.Result, 1)
	go func() {
		ch <- h.runCtx(ctx, "exec", "alice", "s1", map[string]any{"command": command, "timeout_seconds": float64(timeout)})
	}()
	h.clk.BlockUntil(h.t, base+2) // the job's time limit and the foreground wait
	return ch
}

func TestDefinitions(t *testing.T) {
	h := newHarness(t, nil)
	defs := h.tools.RegisterTools(toolspec.Deps{})
	params := map[string][]string{}
	for _, d := range defs {
		for _, p := range d.Parameters {
			params[d.Name] = append(params[d.Name], p.Name)
		}
		if d.DefaultAllowed() {
			t.Errorf("%s is allowed by default", d.Name)
		}
	}
	if got := strings.Join(params["exec"], ","); got != "command,working_dir,timeout_seconds,background,notify" {
		t.Errorf("exec parameters %s", got)
	}
	if got := strings.Join(params["ctrl"], ","); got != "action,id,seconds,until,text,raw,all" {
		t.Errorf("ctrl parameters %s", got)
	}
	if len(defs) != 2 || h.tools.Namespace() != "shell" {
		t.Errorf("%d tools under %q", len(defs), h.tools.Namespace())
	}
}

func TestForegroundFinished(t *testing.T) {
	h := newHarness(t, nil)
	want(t, h.exec("alice", "echo hi; echo err >&2", nil), false, "hi\nerr\n")
	res := h.exec("alice", "echo bad; exit 2", nil)
	want(t, res, true, "bad\n", "Exit code: 2")
	if strings.Contains(res.ForLLM, "Full output") {
		t.Errorf("short output names a file: %s", res.ForLLM)
	}
	if jobs := h.tools.Manager().List("alice"); len(jobs) != 0 {
		t.Errorf("finished foreground commands kept: %+v", jobs)
	}
	want(t, h.exec("alice", "true", nil), false, "(no output)")
}

// Section 3.1: long output returns its head and its tail and names the file.
func TestForegroundLongOutputHeadAndTail(t *testing.T) {
	h := newHarness(t, nil)
	res := h.exec("alice", "seq 1 20000", nil)
	want(t, res, false, "1\n2\n3\n", "19999\n20000", "bytes omitted", "Full output: ")
	file := strings.TrimSpace(res.ForLLM[strings.Index(res.ForLLM, "Full output: ")+len("Full output: "):])
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("named file: %v", err)
	}
	if !strings.HasPrefix(string(data), "1\n2\n") || !strings.HasSuffix(string(data), "\n20000\n") {
		t.Errorf("file does not hold the whole output")
	}
	if len(res.ForLLM) > resultLimit+200 {
		t.Errorf("result is %d bytes", len(res.ForLLM))
	}
}

// Section 3.1 and 7.2: a foreground command reaching its timeout is kept as a job.
func TestForegroundTimeoutKeptAsJob(t *testing.T) {
	h := newHarness(t, nil)
	ch := h.startForeground(context.Background(), "echo started; sleep 30", 5)
	h.clk.Advance(5 * time.Second)
	res := <-ch
	want(t, res, false, "Still running after 5s, so it was kept as job", "Output file: ", "Use shell_ctrl to wait on it or stop it.", "You will be told when it ends.")
	id := jobID(t, res)
	want(t, h.ctrl("alice", "list", nil), false, id+" running", "sleep 30")
	want(t, h.ctrl("alice", "input", map[string]any{"id": id, "text": "x"}), true, "does not take input")
	want(t, h.ctrl("alice", "stop", map[string]any{"id": id}), false, "Stopped job "+id)
	h.noNotice(id) // the agent stopped it itself
}

// Section 7.2: without shell_ctrl a foreground timeout stops the command.
func TestForegroundTimeoutStopsWithoutCtrl(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.CtrlAvailable = func(*toolspec.ToolCall) bool { return false }
	})
	ch := h.startForeground(context.Background(), "echo started; sleep 30", 5)
	h.clk.Advance(5 * time.Second)
	res := <-ch
	want(t, res, true, "Command timed out after 5s and was stopped.")
	if running := h.tools.Running("alice"); len(running) != 0 {
		t.Errorf("still running: %+v", running)
	}
	want(t, h.exec("alice", "sleep 1", map[string]any{"background": true}), true, "need shell_ctrl")
	want(t, h.ctrl("alice", "list", nil), true, "not available to you")
	stopped := ""
	for _, e := range h.auditKinds() {
		if e.Kind == AuditStop {
			stopped = e.JobID
		}
	}
	if stopped == "" {
		t.Fatal("the timeout stop was not audited")
	}
	h.noNotice(stopped)
}

// Section 7.3: unattended callers get no background jobs and no shell_ctrl.
func TestUnattendedCallers(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Unattended = func(*toolspec.ToolCall) bool { return true }
	})
	res := h.exec("alice", "echo hi", map[string]any{"background": true})
	want(t, res, true, "not available in unattended runs")
	if !errors.Is(res.Err, ErrRefused) {
		t.Errorf("Err = %v, want ErrRefused", res.Err)
	}
	want(t, h.ctrl("alice", "list", nil), true, "shell_ctrl is not available in unattended runs")
	ch := h.startForeground(context.Background(), "sleep 30", 5)
	h.clk.Advance(5 * time.Second)
	want(t, <-ch, true, "timed out after 5s and was stopped")
	want(t, h.exec("alice", "echo fine", nil), false, "fine")
}

// Section 3.1 and 3.3: background returns at once; the end is announced once.
func TestBackgroundJobAndNotice(t *testing.T) {
	h := newHarness(t, nil)
	res := h.exec("alice", "echo one; read x; echo two; exit 4", map[string]any{"background": true})
	want(t, res, false, "Started job", "Output file: ", "You will be told when it ends.")
	id := jobID(t, res)
	want(t, h.ctrl("alice", "wait", map[string]any{"id": id, "until": "one"}), false, "printed a line matching until: one", "It is still running.", "New output:\none")
	want(t, h.ctrl("alice", "input", map[string]any{"id": id, "text": "go"}), false, "Sent 3 bytes to job "+id)
	want(t, h.ctrl("alice", "wait", map[string]any{"id": id}), false, "Job "+id+" exited with code 4", "New output:\ntwo")
	n := h.notice()
	if n.JobID != id || n.Owner != "alice" || n.ExitCode != 4 || n.State != proc.StateExited ||
		n.LastLines != "one\ntwo" || !strings.HasSuffix(n.OutputFile, id+".log") {
		t.Errorf("notice = %+v", n)
	}
	for _, p := range []string{"Shell job " + id + " ended: exited with code 4", "Command: echo one", "Last output:\none\ntwo"} {
		if !strings.Contains(n.Text, p) {
			t.Errorf("notice text lacks %q:\n%s", p, n.Text)
		}
	}
	quiet := jobID(t, h.exec("alice", "true", map[string]any{"background": true, "notify": false}))
	want(t, h.ctrl("alice", "wait", map[string]any{"id": quiet}), false, "exited with code 0")
	h.noNotice(quiet)
}

// Rule 1: a job is not tied to the turn that started it.
func TestJobNotTiedToTurn(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	id := jobID(t, h.runCtx(ctx, "exec", "alice", "s1", map[string]any{"command": "read x", "background": true}))
	cancel()
	if info, _ := h.tools.Manager().Get(id); !info.Running() {
		t.Fatal("background job ended with its turn")
	}

	// Interrupting a foreground command keeps it as a job.
	fctx, fcancel := context.WithCancel(context.Background())
	ch := h.startForeground(fctx, "sleep 30", 60)
	fcancel()
	res := <-ch
	want(t, res, false, "Interrupted; the command is still running as job")
	if info, _ := h.tools.Manager().Get(jobID(t, res)); !info.Running() {
		t.Fatal("interrupted foreground command was stopped")
	}
}

// Rule 3: a job belongs to its owner; a clone acts as its source; another
// agent's job id is unknown.
func TestOwnership(t *testing.T) {
	h := newHarness(t, nil)
	id := jobID(t, h.exec("alice", "read x", map[string]any{"background": true}))
	want(t, h.ctrl("alice-clone", "list", nil), false, id+" running")
	want(t, h.ctrl("bob", "list", nil), false, "No jobs.")
	for _, action := range []string{"wait", "input", "stop"} {
		want(t, h.ctrl("bob", action, map[string]any{"id": id, "text": "x", "seconds": float64(1)}), true, "Unknown job id: "+id)
	}
	want(t, h.ctrl("bob", "stop", map[string]any{"all": true}), false, "No running jobs.")
	if info, _ := h.tools.Manager().Get(id); !info.Running() {
		t.Fatal("another agent stopped the job")
	}
	want(t, h.ctrl("alice-clone", "stop", map[string]any{"id": id}), false, "Stopped job "+id)
}

// Rule 6: the directory a foreground command ends in is the next default,
// per agent and conversation; nothing else carries over.
func TestWorkingDirectoryRemembered(t *testing.T) {
	h := newHarness(t, nil)
	sub := filepath.Join(h.ws, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	sameDir(t, h.exec("alice", "pwd", nil).ForLLM, h.ws)
	want(t, h.exec("alice", "cd sub; export GREETING=hi", nil), false)
	sameDir(t, h.exec("alice", "pwd", nil).ForLLM, sub)
	want(t, h.exec("alice", "echo [$GREETING]", nil), false, "[]")

	// Another conversation, and another agent, start at the workspace.
	other := h.runCtx(context.Background(), "exec", "alice", "s2", map[string]any{"command": "pwd"})
	sameDir(t, other.ForLLM, h.ws)
	sameDir(t, h.exec("bob", "pwd", nil).ForLLM, h.ws)

	// working_dir overrides it, relative to the workspace.
	sameDir(t, h.exec("bob", "pwd", map[string]any{"working_dir": "sub"}).ForLLM, sub)

	// A background job does not move it.
	id := jobID(t, h.exec("alice", "cd /; true", map[string]any{"background": true}))
	want(t, h.ctrl("alice", "wait", map[string]any{"id": id}), false, "exited with code 0")
	sameDir(t, h.exec("alice", "pwd", nil).ForLLM, sub)

	// A remembered directory that has gone falls back to the workspace.
	if err := os.Remove(sub); err != nil {
		t.Fatal(err)
	}
	sameDir(t, h.exec("alice", "pwd", nil).ForLLM, h.ws)
}

// Rule 7: every start goes through the guard and gets the child environment;
// input is not screened but is audited by length.
func TestGuardEnvironmentAndAudit(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Guard = func(cmd string) string {
			if strings.Contains(cmd, "forbidden") {
				return "Command blocked by safety guard (dangerous pattern detected)"
			}
			return ""
		}
		c.Env = func(*toolspec.ToolCall) []string {
			return []string{"GREETING=hello", "PATH=" + os.Getenv("PATH")}
		}
	})
	for _, bg := range []bool{false, true} {
		res := h.exec("alice", "echo forbidden", map[string]any{"background": bg})
		want(t, res, true, "Command blocked by safety guard")
		if !errors.Is(res.Err, ErrRefused) {
			t.Errorf("Err = %v, want ErrRefused", res.Err)
		}
	}
	if jobs := h.tools.Manager().List(""); len(jobs) != 0 || len(h.auditKinds()) != 0 {
		t.Fatalf("a refused command started: %+v", jobs)
	}

	want(t, h.exec("alice", "echo $GREETING", nil), false, "hello")
	id := jobID(t, h.exec("alice", "read x; echo got $x", map[string]any{"background": true}))
	want(t, h.ctrl("alice", "input", map[string]any{"id": id, "text": "forbidden", "raw": true}), false, "Sent 9 bytes")
	want(t, h.ctrl("alice", "input", map[string]any{"id": id, "text": ""}), false, "Sent 1 bytes")
	want(t, h.ctrl("alice", "wait", map[string]any{"id": id}), false, "got forbidden")

	audits := h.auditKinds()
	if len(audits) != 4 {
		t.Fatalf("audits = %+v", audits)
	}
	if a := audits[0]; a.Kind != AuditStart || a.Background || a.Command != "echo $GREETING" || a.Dir != h.ws || a.Owner != "alice" {
		t.Errorf("foreground start audit = %+v", a)
	}
	if a := audits[1]; a.Kind != AuditStart || !a.Background || a.JobID != id {
		t.Errorf("background start audit = %+v", a)
	}
	if a := audits[2]; a.Kind != AuditInput || a.Bytes != 9 || a.JobID != id {
		t.Errorf("input audit = %+v", a)
	}
}

// Section 6 and 7.4: a start beyond max_jobs is refused, naming the running jobs.
func TestLimitRefusedNamingRunningJobs(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Jobs.Limits.MaxJobs = 2 })
	a := jobID(t, h.exec("alice", "read x", map[string]any{"background": true}))
	b := jobID(t, h.exec("alice", "sleep 100", map[string]any{"background": true}))
	res := h.exec("alice", "echo three", nil)
	want(t, res, true, "Not started: 2 jobs are already running, the most allowed.", "- "+a+": read x", "- "+b+": sleep 100")
	want(t, h.exec("bob", "echo fine", nil), false, "fine")

	want(t, h.ctrl("alice", "stop", map[string]any{"all": true}), false, "Stopped 2 jobs: ", a, b)
	want(t, h.ctrl("alice", "stop", map[string]any{"all": true}), false, "No running jobs.")
	want(t, h.ctrl("alice", "stop", map[string]any{"id": a}), false, "had already ended: stopped")
	want(t, h.ctrl("alice", "input", map[string]any{"id": a, "text": "x"}), true, "has ended")
	stops := 0
	for _, e := range h.auditKinds() {
		if e.Kind == AuditStop {
			stops++
		}
	}
	if stops != 2 {
		t.Errorf("%d stop audits, want 2", stops)
	}
	h.noNotice(a)
	h.noNotice(b)
}

func TestWaitTimesOut(t *testing.T) {
	h := newHarness(t, nil)
	id := jobID(t, h.exec("alice", "read x", map[string]any{"background": true}))
	ch := make(chan *toolspec.Result, 1)
	go func() { ch <- h.ctrl("alice", "wait", map[string]any{"id": id, "seconds": float64(10)}) }()
	h.clk.BlockUntil(t, 2) // the job's time limit and the wait
	h.clk.Advance(10 * time.Second)
	want(t, <-ch, false, "Job "+id+" is still running after 10s.", "No new output.")
	want(t, h.ctrl("alice", "wait", map[string]any{"id": id, "until": "("}), true, "not a valid regular expression")
	want(t, h.ctrl("alice", "frobnicate", nil), true, "Unknown action")
}

// Section 6: a job past max_job_seconds is stopped and announced.
func TestTimeLimitAnnounced(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Jobs.Limits.MaxJobTime = time.Hour })
	id := jobID(t, h.exec("alice", "read x", map[string]any{"background": true}))
	h.clk.BlockUntil(t, 1)
	h.clk.Advance(time.Hour)
	n := h.notice()
	if n.JobID != id || n.State != proc.StateStopped || n.StopReason != proc.StopTimeLimit || n.Duration != time.Hour {
		t.Fatalf("notice = %+v", n)
	}
	if !strings.Contains(n.Text, "ended: stopped (time limit) after 1h0m0s") {
		t.Errorf("notice text:\n%s", n.Text)
	}
}

// Rule 8 and 7.4: after a restart a job recorded as running is lost and
// announced once; it is never relaunched.
func TestLostAfterRestartAnnounced(t *testing.T) {
	first := newHarness(t, nil)
	id := jobID(t, first.exec("alice", "echo before; read x", map[string]any{"background": true}))
	want(t, first.ctrl("alice", "wait", map[string]any{"id": id, "until": "before"}), false)

	second := newHarness(t, func(c *Config) {
		c.Jobs.Dir = first.tools.Manager().Dir()
		c.Workspace = func(*toolspec.ToolCall) string { return first.ws }
	})
	n := second.notice()
	if n.JobID != id || n.State != proc.StateLost || n.LastLines != "before" {
		t.Fatalf("notice = %+v", n)
	}
	if !strings.Contains(n.Text, "was lost: the service restarted while it ran. It was not restarted.") {
		t.Errorf("notice text:\n%s", n.Text)
	}
	want(t, second.ctrl("alice", "list", nil), false, id+" lost")
	if len(second.tools.Running("")) != 0 {
		t.Fatal("lost job relaunched")
	}
	select {
	case n := <-second.notices:
		t.Fatalf("lost job announced twice: %+v", n)
	default:
	}
}

// Rule 9: shutdown stops every job.
func TestCloseStopsJobs(t *testing.T) {
	h := newHarness(t, nil)
	a := jobID(t, h.exec("alice", "read x", map[string]any{"background": true}))
	b := jobID(t, h.exec("bob", "read x", map[string]any{"background": true}))
	if n := h.tools.StopAll("bob"); n != 1 {
		t.Fatalf("StopAll(bob) = %d", n)
	}
	h.close()
	for _, id := range []string{a, b} {
		if info, _ := h.tools.Manager().Get(id); info.Running() {
			t.Errorf("%s still running", id)
		}
	}
	want(t, h.exec("alice", "echo hi", nil), true, "shutting down")
}

// Section 8: without jobs, shell_exec runs and waits, and background and
// shell_ctrl are refused.
func TestWithoutJobSupport(t *testing.T) {
	h := newHarness(t, nil)
	h.tools.jobs = false
	want(t, h.exec("alice", "echo hi", nil), false, "hi")
	want(t, h.exec("alice", "echo bad; exit 3", nil), true, "bad", "Exit code: 3")
	res := h.exec("alice", "seq 1 20000", nil)
	want(t, res, false, "1\n2\n", "20000", "bytes omitted")
	want(t, h.exec("alice", "echo hi", map[string]any{"background": true}), true, "not supported on this platform")
	want(t, h.ctrl("alice", "list", nil), true, "shell_ctrl is not supported on this platform")
	want(t, h.exec("alice", "exec sleep 5", map[string]any{"timeout_seconds": float64(1)}), true, "Command timed out after 1s and was stopped.")
}

// With Jobs.Root set to the workspace, the workspace can be moved while the
// service is stopped: the tools opened on the new location list the jobs,
// announce the one the shutdown stopped, and no record names the old path.
func TestWorkspaceMovedWithRecords(t *testing.T) {
	oldWS := filepath.Join(t.TempDir(), "ws")
	first := newHarness(t, func(c *Config) {
		c.Jobs.Dir = filepath.Join(oldWS, "state", "shell")
		c.Jobs.Root = oldWS
		c.Workspace = func(*toolspec.ToolCall) string { return oldWS }
	})
	id := jobID(t, first.exec("alice", "read x", map[string]any{"background": true}))
	first.close()

	newWS := filepath.Join(filepath.Dir(oldWS), "moved")
	if err := os.Rename(oldWS, newWS); err != nil {
		t.Fatal(err)
	}
	second := newHarness(t, func(c *Config) {
		c.Jobs.Dir = filepath.Join(newWS, "state", "shell")
		c.Jobs.Root = newWS
		c.Workspace = func(*toolspec.ToolCall) string { return newWS }
	})
	if n := second.notice(); n.JobID != id || n.StopReason != proc.StopShutdown {
		t.Fatalf("notice = %+v", n)
	}
	want(t, second.ctrl("alice", "list", nil), false, id+" stopped")
	if info, _ := second.tools.Manager().Get(id); info.Dir != newWS {
		t.Errorf("dir = %q, want %q", info.Dir, newWS)
	}
	data, err := os.ReadFile(filepath.Join(newWS, "state", "shell", id+".json"))
	if err != nil || strings.Contains(string(data), oldWS) {
		t.Errorf("record = %s, %v; want no old path", data, err)
	}
}
