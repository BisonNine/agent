/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"
)

// Refusal texts.
const (
	msgNoOwner               = "No agent identity for this call."
	msgUnattendedBackground  = "Background jobs are not available in unattended runs."
	msgUnattendedCtrl        = ctrlTool + " is not available in unattended runs."
	msgUnsupportedBackground = "Background jobs are not supported on this platform."
	msgUnsupportedCtrl       = ctrlTool + " is not supported on this platform."
	msgNoCtrlBackground      = "Background jobs need " + ctrlTool + ", which is not available to you."
	msgNoCtrl                = ctrlTool + " is not available to you."
)

func (t *Tools) execHandler(call *toolspec.ToolCall) (*toolspec.Result, error) {
	command := stringArg(call.Args, "command")
	if strings.TrimSpace(command) == "" {
		return errResult("command is required"), nil
	}
	owner := t.owner(call)
	if owner == "" {
		return refused(msgNoOwner), nil
	}
	background := boolArg(call.Args, "background", false)
	notify := boolArg(call.Args, "notify", true)
	defTimeout, _ := t.currentTimeouts()
	timeout := t.timeoutArg(call.Args, "timeout_seconds", defTimeout)
	if background {
		switch {
		case t.unattended(call):
			return refused(msgUnattendedBackground), nil
		case !t.jobs:
			return refused(msgUnsupportedBackground), nil
		case !t.ctrlAvailable(call):
			return refused(msgNoCtrlBackground), nil
		}
	}
	if t.cfg.Guard != nil {
		if msg := t.cfg.Guard(command); msg != "" {
			t.log.Warn("shell command refused by the guard", "owner", owner, "reason", msg)
			return refused(msg), nil
		}
	}
	dir := t.workDir(call, stringArg(call.Args, "working_dir"))
	env := t.env(call)
	ctx := callContext(call)

	if !t.jobs {
		t.audit(AuditEvent{Kind: AuditStart, Call: call, Owner: owner, Command: command, Dir: dir})
		return t.runSimple(ctx, command, dir, env, budget(ctx, timeout)), nil
	}

	// The route is taken before the process starts, so the job's notice can
	// never come before it.
	info, err := t.m.Start(ctx, proc.Spec{
		Owner:    owner,
		Command:  command,
		Dir:      dir,
		Env:      env,
		Notify:   background && notify,
		Input:    background,
		TrackDir: !background,
		Route:    t.route(call),
	})
	if err != nil {
		return startError(err), nil
	}
	t.audit(AuditEvent{Kind: AuditStart, Call: call, Owner: owner, JobID: info.ID, Command: command, Dir: dir, Background: background, Route: info.Route})
	if background {
		return textResult(backgroundText(info)), nil
	}
	return t.foreground(call, owner, info, timeout, notify), nil
}

// foreground waits for a command. Finished in time: its output. Still
// running: kept as a job when the caller can collect it, otherwise stopped.
func (t *Tools) foreground(call *toolspec.ToolCall, owner string, info proc.Info, timeout time.Duration, notify bool) *toolspec.Result {
	ctx := callContext(call)
	wait := budget(ctx, timeout)
	r, err := t.m.Wait(ctx, info.ID, proc.Until{Timeout: wait, MaxOutput: resultLimit})
	if err != nil && r.Reason == "" {
		return errResult("could not wait for the command: " + err.Error())
	}
	if r.Reason == proc.ReasonEnded {
		return t.finished(call, r)
	}

	if t.canKeepJobs(call) {
		err := t.m.SetNotify(info.ID, notify)
		if errors.Is(err, proc.ErrEnded) {
			// It ended just after the wait: report it as finished, since its
			// end was not announced.
			return t.finished(call, t.rest(ctx, r))
		}
		if err != nil {
			t.log.Warn("shell job notify not set", "job", info.ID, "error", err)
		}
		return userResult(keptText(r, wait, notify), false)
	}

	if err := t.m.Stop(info.ID, stopGrace(ctx, t.grace)); err != nil {
		t.log.Warn("timed-out shell command not stopped", "job", info.ID, "error", err)
	}
	r = t.rest(ctx, r)
	if r.Info.State != proc.StateStopped {
		return t.finished(call, r) // it ended by itself before the stop
	}
	t.audit(AuditEvent{Kind: AuditStop, Call: call, Owner: owner, JobID: info.ID, Command: info.Command, Route: info.Route})
	if r.Omitted == 0 {
		t.remove(info.ID)
	}
	return userResult(timedOutText(r, wait), true)
}

// finished is the result of a foreground command that has ended.
func (t *Tools) finished(call *toolspec.ToolCall, r proc.WaitResult) *toolspec.Result {
	t.rememberDir(call, r.Info.EndDir)
	keep := r.Omitted > 0
	if !keep {
		// Everything was shown: the log has nothing more to offer.
		t.remove(r.Info.ID)
	}
	return userResult(finishedText(r, keep), r.Info.Signal != "" || r.Info.ExitCode != 0)
}

// rest adds the output a job produced after r to it, once the job has
// ended, and brings its Info up to date.
func (t *Tools) rest(ctx context.Context, r proc.WaitResult) proc.WaitResult {
	more, err := t.m.Wait(ctx, r.Info.ID, proc.Until{MaxOutput: resultLimit})
	if err != nil && more.Reason == "" {
		return r
	}
	more.Output = append(r.Output, more.Output...)
	more.Omitted += r.Omitted
	return more
}

// stopGrace shortens the stop grace so a stop (terminate, grace, kill,
// grace) ends before the call's deadline.
func stopGrace(ctx context.Context, grace time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return grace
	}
	return max(min(grace, time.Until(dl)/4), 0)
}

// workDir resolves the directory for a call: working_dir (relative to the
// workspace), else where the agent's previous foreground command in this
// conversation ended, else the workspace, else the process's directory.
func (t *Tools) workDir(call *toolspec.ToolCall, requested string) string {
	base := t.workspace(call)
	if requested != "" {
		if !filepath.IsAbs(requested) && base != "" {
			return filepath.Join(base, requested)
		}
		return requested
	}
	key := dirKey(call)
	t.mu.Lock()
	dir, ok := t.dirs[key]
	t.mu.Unlock()
	if ok {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
		// It has gone since: forget it rather than fail every later call.
		t.mu.Lock()
		delete(t.dirs, key)
		t.mu.Unlock()
	}
	if base != "" {
		return base
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// rememberDir keeps where a foreground command ended as the default for the
// agent's next command in the same conversation.
func (t *Tools) rememberDir(call *toolspec.ToolCall, dir string) {
	if dir == "" {
		return
	}
	key := dirKey(call)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.dirs[key]; !ok && len(t.dirs) >= maxRememberedDirs {
		for k := range t.dirs { // drop an arbitrary entry to stay bounded
			delete(t.dirs, k)
			break
		}
	}
	t.dirs[key] = dir
}

func dirKey(call *toolspec.ToolCall) string {
	return call.AgentID + "\x00" + call.Session
}

// startError turns a failed start into the tool's answer.
func startError(err error) *toolspec.Result {
	var limit *proc.LimitError
	switch {
	case errors.As(err, &limit):
		return refused(limitText(limit))
	case errors.Is(err, proc.ErrClosed):
		return errResult("The shell is shutting down; the command was not started.")
	default:
		return errResult("failed to start command: " + err.Error())
	}
}

func textResult(text string) *toolspec.Result { return &toolspec.Result{ForLLM: text} }

func errResult(text string) *toolspec.Result { return &toolspec.Result{ForLLM: text, IsError: true} }

// userResult is a shell_exec result: as before, the output is also offered to
// the person.
func userResult(text string, isError bool) *toolspec.Result {
	return &toolspec.Result{ForLLM: text, ForUser: text, IsError: isError}
}

func refused(msg string) *toolspec.Result {
	return &toolspec.Result{ForLLM: msg, IsError: true, Err: fmt.Errorf("%w: %s", ErrRefused, msg)}
}

// remove drops an ended job whose output was returned in full.
func (t *Tools) remove(id string) {
	if err := t.m.Remove(id); err != nil {
		t.log.Debug("shell job not removed", "job", id, "error", err)
	}
}

// stringArg returns args[key] when it is a string, and "" otherwise.
func stringArg(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func boolArg(args map[string]any, key string, def bool) bool {
	if b, ok := args[key].(bool); ok {
		return b
	}
	return def
}

// intArg reads an integer argument; JSON numbers arrive as float64.
func intArg(args map[string]any, key string) (int64, bool) {
	switch n := args[key].(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// timeoutArg reads a seconds argument: absent or not positive is def, and
// more than the maximum is the maximum.
func (t *Tools) timeoutArg(args map[string]any, key string, def time.Duration) time.Duration {
	n, ok := intArg(args, key)
	if !ok || n <= 0 {
		return def
	}
	if _, maxTimeout := t.currentTimeouts(); n > int64(maxTimeout/time.Second) {
		return maxTimeout
	}
	return time.Duration(n) * time.Second
}
