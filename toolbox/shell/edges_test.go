//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"
)

// Calls missing what they need are answered with what is missing.
func TestMissingArguments(t *testing.T) {
	h := newHarness(t, nil)
	want(t, h.exec("alice", "  ", nil), true, "command is required")
	want(t, h.exec("", "echo hi", nil), true, msgNoOwner)
	want(t, h.ctrl("", "list", nil), true, msgNoOwner)
	want(t, h.ctrl("alice", "", nil), true, "action is required")
	want(t, h.ctrl("alice", "wait", nil), true, "id is required")
	id := jobID(t, h.exec("alice", "cat", map[string]any{"background": true, "notify": false}))
	want(t, h.ctrl("alice", "input", map[string]any{"id": id}), true, "text is required")
}

// A job whose log is gone cannot be waited on; a job that closed its input
// cannot be sent any.
func TestCtrlFailures(t *testing.T) {
	h := newHarness(t, nil)
	ended := jobID(t, h.exec("alice", "echo done", map[string]any{"background": true, "notify": false}))
	h.noNotice(ended)
	info, _ := h.tools.Manager().Get(ended)
	if err := os.Remove(info.OutputFile); err != nil {
		t.Fatal(err)
	}
	want(t, h.ctrl("alice", "wait", map[string]any{"id": ended}), true, "could not wait for the job")

	closed := jobID(t, h.exec("alice", "exec 0<&-; echo ready; sleep 100", map[string]any{"background": true, "notify": false}))
	want(t, h.ctrl("alice", "wait", map[string]any{"id": closed, "until": "ready"}), false, "ready")
	want(t, h.ctrl("alice", "input", map[string]any{"id": closed, "text": "hello"}), true, "Could not send input to job "+closed)
}

// A command that cannot start is reported; an absolute working directory is
// used as given.
func TestExecStartFailureAndAbsoluteDir(t *testing.T) {
	h := newHarness(t, nil)
	want(t, h.exec("alice", "pwd", map[string]any{"working_dir": filepath.Join(h.ws, "missing")}), true, "failed to start command")
	abs := t.TempDir()
	res := h.exec("alice", "pwd", map[string]any{"working_dir": abs})
	want(t, res, false)
	sameDir(t, res.ForLLM, abs)
}

// Without a workspace or an owner function a command runs in the process's
// directory, owned by the calling agent, and a call without a context runs.
func TestDefaultsWithoutConfig(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Workspace = nil
		c.Owner = nil
	})
	var def toolspec.ToolDefinition
	for _, d := range h.tools.RegisterTools(toolspec.Deps{}) {
		if d.Name == "exec" {
			def = d
		}
	}
	res, err := def.Handler(&toolspec.ToolCall{Args: map[string]any{"command": "pwd"}, AgentID: "bob", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want(t, res, false)
	sameDir(t, res.ForLLM, wd)
	id := jobID(t, h.exec("bob", "sleep 100", map[string]any{"background": true, "notify": false}))
	if got, _ := h.tools.Manager().Get(id); got.Owner != "bob" {
		t.Fatalf("owner = %q, want the agent id", got.Owner)
	}
	if n := h.tools.StopAll(""); n != 0 {
		t.Fatalf("StopAll(\"\") stopped %d", n)
	}
	if n := h.tools.StopAll("bob"); n != 1 {
		t.Fatalf("StopAll(bob) stopped %d", n)
	}
}

// The provider describes itself to a host.
func TestHostMeta(t *testing.T) {
	h := newHarness(t, nil)
	if ok, reason := h.tools.Available(nil); !ok || reason != "" || h.tools.Description() == "" || h.tools.Namespace() != Namespace {
		t.Fatalf("Available = %v %q, Description %q, Namespace %q", ok, reason, h.tools.Description(), h.tools.Namespace())
	}
}

// New refuses bad timeouts and a job directory it cannot make.
func TestNewRefuses(t *testing.T) {
	if _, err := New(Config{Timeout: -time.Second, Jobs: proc.Options{Dir: t.TempDir()}}); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Jobs: proc.Options{Dir: filepath.Join(file, "shell")}}); err == nil {
		t.Fatal("a job directory under a file was accepted")
	}
}

// The simple runner reports a command it could not run.
func TestSimpleRunnerStartFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.tools.jobs = false
	want(t, h.exec("alice", "pwd", map[string]any{"working_dir": filepath.Join(h.ws, "missing")}), true, "failed to run command")
}

// The kept tail is cut to its size when more than it was written after
// the head.
func TestHeadTailBounds(t *testing.T) {
	var h headTail
	data := strings.Repeat("h", headKeep) + strings.Repeat("t", tailKeep+10)
	if _, err := h.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	got := string(h.bytes())
	wantText := strings.Repeat("h", headKeep) + "\n[... 10 bytes omitted ...]\n" + strings.Repeat("t", tailKeep)
	if got != wantText {
		t.Fatalf("bytes() has %d bytes, want %d", len(got), len(wantText))
	}
}

// Where a foreground command ended is remembered per conversation, never
// an empty directory, and at most maxRememberedDirs of them.
func TestRememberDirBounded(t *testing.T) {
	h := newHarness(t, nil)
	tools := h.tools
	call := &toolspec.ToolCall{AgentID: "alice", Session: "s1"}
	tools.rememberDir(call, "")
	if len(tools.dirs) != 0 {
		t.Fatal("an empty directory was remembered")
	}
	for i := range maxRememberedDirs {
		tools.rememberDir(&toolspec.ToolCall{AgentID: "alice", Session: strconv.Itoa(i)}, "/tmp")
	}
	tools.rememberDir(call, h.ws)
	if len(tools.dirs) != maxRememberedDirs || tools.dirs[dirKey(call)] != h.ws {
		t.Fatalf("%d directories remembered, latest %q", len(tools.dirs), tools.dirs[dirKey(call)])
	}
}

// The stop grace leaves room for a stop before the call's deadline.
func TestStopGrace(t *testing.T) {
	if got := stopGrace(context.Background(), time.Second); got != time.Second {
		t.Fatalf("no deadline: %v", got)
	}
	far, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got := stopGrace(far, time.Second); got != time.Second {
		t.Fatalf("far deadline: %v", got)
	}
	near, cancel2 := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel2()
	if got := stopGrace(near, time.Hour); got <= 0 || got > 100*time.Millisecond {
		t.Fatalf("near deadline: %v, want at most a quarter of it", got)
	}
	past, cancel3 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel3()
	if got := stopGrace(past, time.Second); got != 0 {
		t.Fatalf("past deadline: %v", got)
	}
}

// Integer arguments arrive as any of Go's integer forms or a JSON number.
func TestIntArg(t *testing.T) {
	for _, v := range []any{7, int64(7), float64(7)} {
		if n, ok := intArg(map[string]any{"n": v}, "n"); !ok || n != 7 {
			t.Errorf("intArg(%T) = %d, %v", v, n, ok)
		}
	}
	if _, ok := intArg(map[string]any{"n": "7"}, "n"); ok {
		t.Error("a string was taken for a number")
	}
}

func TestFormatting(t *testing.T) {
	long := strings.Repeat("é", commandDisplay+5)
	if got := shortCommand(long); got != strings.Repeat("é", commandDisplay)+"…" {
		t.Errorf("shortCommand(long) = %q", got)
	}
	if got := shortCommand("make\nmake test"); got != "make…" {
		t.Errorf("shortCommand(two lines) = %q", got)
	}
	if got := stateText(proc.Info{State: proc.StateExited, Signal: "killed"}); got != "ended by signal killed" {
		t.Errorf("stateText(signal) = %q", got)
	}
	if got := stateText(proc.Info{State: "paused"}); got != "paused" {
		t.Errorf("stateText(unknown) = %q", got)
	}
	r := proc.WaitResult{Info: proc.Info{ID: "j00000001", Signal: "killed", OutputFile: "/logs/j00000001.log"}, Output: []byte("partial")}
	if got := finishedText(r, false); !strings.Contains(got, "Ended by signal: killed") {
		t.Errorf("finishedText(signal) = %q", got)
	}
	r.Reason, r.Omitted = proc.ReasonCancelled, 5
	if got := timedOutText(r, time.Minute); !strings.HasPrefix(got, "Command interrupted and stopped.") || !strings.Contains(got, "Full output: /logs/j00000001.log") {
		t.Errorf("timedOutText(cancelled) = %q", got)
	}
	r.Info.State = proc.StateRunning
	if got := waitText(r, time.Minute); !strings.HasPrefix(got, "Wait interrupted; job j00000001 is running.") {
		t.Errorf("waitText(cancelled) = %q", got)
	}
	started := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	list := listText([]proc.Info{
		{ID: "j00000001", State: proc.StateRunning, Started: started, Command: "make"},
		{ID: "j00000002", State: proc.StateExited, Started: started, Ended: started.Add(time.Minute), Command: "make test"},
	})
	if lines := strings.Split(list, "\n"); len(lines) != 4 || !strings.HasPrefix(lines[2], "j00000002 exited with code 0") {
		t.Errorf("listText = %q", list)
	}
}

// The notice's last lines come from the end of the log, bounded in lines
// and bytes, with a line cut by the byte bound left out.
func TestLastLines(t *testing.T) {
	if got := lastLines(filepath.Join(t.TempDir(), "missing.log"), 5, 100); got != "" {
		t.Fatalf("lastLines(missing) = %q", got)
	}
	path := filepath.Join(t.TempDir(), "j.log")
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "line %02d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastLines(path, 3, 1000); got != "line 28\nline 29\nline 30" {
		t.Fatalf("lastLines(3 lines) = %q", got)
	}
	// 20 bytes reach into "line 28" (8 bytes a line): it is left out.
	if got := lastLines(path, 10, 20); got != "line 29\nline 30" {
		t.Fatalf("lastLines(20 bytes) = %q", got)
	}
}
