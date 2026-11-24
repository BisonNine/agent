/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package shell provides the shell tools as a toolspec provider: "exec"
// runs a command in the foreground or as a background job, and "ctrl"
// lists, waits on, sends input to, or stops jobs. A host mounts them under
// the namespace "shell" (shell_exec, shell_ctrl).
//
// Jobs are run by a proc.Manager that the provider owns. Everything that
// depends on the host (the command guard, the child environment, the
// workspace, who the caller is, the end-of-job notice, auditing, and
// whether the caller may use shell_ctrl) is injected through Config.
package shell

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"
)

// Namespace is the namespace the tools are published under.
const Namespace = "shell"

// The published names, used in the text the tools return.
const (
	execTool = Namespace + "_exec"
	ctrlTool = Namespace + "_ctrl"
)

// Defaults for Config.
const (
	DefaultTimeout     = 60 * time.Second
	DefaultMaxTimeout  = 600 * time.Second
	DefaultWaitSeconds = 30
)

const (
	// resultLimit bounds the output a result carries: its head and its tail.
	resultLimit = 10000
	// noticeLines and noticeBytes bound the output in an end-of-job notice.
	noticeLines = 20
	noticeBytes = 2000
	// deadlineMargin is kept free before a call's deadline to build the result.
	deadlineMargin = time.Second
	// maxRememberedDirs bounds the remembered working directories.
	maxRememberedDirs = 1024
)

// ErrRefused marks the Err of a result for a call the tools refused: the
// guard blocked the command, or the caller may not use the feature here.
// Hosts can treat it as an expected refusal rather than a failure.
var ErrRefused = errors.New("refused")

// AuditKind names an audited action.
type AuditKind string

// The audited actions.
const (
	AuditStart AuditKind = "start" // a command was started (foreground or background)
	AuditInput AuditKind = "input" // input was sent to a job
	AuditStop  AuditKind = "stop"  // a job was stopped by a tool call or a foreground timeout
)

// AuditEvent describes one audited action.
type AuditEvent struct {
	Kind       AuditKind
	Call       *toolspec.ToolCall // the call that did it; valid only during the Audit call
	Owner      string
	JobID      string
	Command    string
	Dir        string // AuditStart: the working directory
	Background bool   // AuditStart: started as a background job
	Bytes      int    // AuditInput: bytes sent (never the content)
	Route      any    // the job's Config.Route value
}

// Notice is the end-of-job notice for a job started with notify on (the
// default for background jobs and for a foreground command kept as a job).
type Notice struct {
	Owner      string
	JobID      string
	Command    string
	State      proc.State
	ExitCode   int    // -1 when lost or ended by a signal
	Signal     string // the signal that ended it
	StopReason string // for a stopped job
	Duration   time.Duration
	LastLines  string // the last lines of output
	OutputFile string
	// Route is what Config.Route returned when the job was about to start.
	// It is nil for a job announced after a restart (lost, or stopped by
	// the shutdown): route those by Owner.
	Route any
	// Text is the notice written for the agent, ready to deliver.
	Text string
}

// Config is what the tools need from the host. Every func field is
// optional; its zero behaviour is given.
type Config struct {
	// Jobs configures the process manager (log directory, limits, file
	// modes, clock). Its OnEnd, if set, is called after the tools' own.
	Jobs proc.Options
	// Timeout is the default foreground timeout (default 60s); MaxTimeout
	// the most timeout_seconds and a wait may ask for (default 600s).
	Timeout    time.Duration
	MaxTimeout time.Duration

	// Guard screens a command before it starts and returns why it is
	// refused, or "" to allow it. Nil allows every command.
	Guard func(command string) string
	// Env returns the child environment for a call. Nil, or a nil result,
	// inherits this process's environment.
	Env func(call *toolspec.ToolCall) []string
	// Workspace returns the default working directory for a call, which is
	// also where a relative working_dir starts. Nil or "": the process's
	// working directory.
	Workspace func(call *toolspec.ToolCall) string
	// Owner returns who owns the jobs a call starts and may see; a clone
	// should get its source's owner. Nil: call.AgentID.
	Owner func(call *toolspec.ToolCall) string
	// CtrlAvailable reports whether shell_ctrl is available to the caller.
	// When it is not, a foreground command reaching its timeout is stopped
	// rather than kept, and background is refused. Nil: available.
	CtrlAvailable func(call *toolspec.ToolCall) bool
	// Unattended reports whether the caller is unattended (nothing would
	// collect a job): background and shell_ctrl are refused, and a
	// foreground timeout stops the command. Nil: attended.
	Unattended func(call *toolspec.ToolCall) bool
	// Notice delivers an end-of-job notice. It is called once per job, from
	// the job's goroutine (or during New for jobs found lost), and must not
	// block for long. Nil: no notices.
	Notice func(Notice)
	// Audit records starts, input and stops. Nil: not audited.
	Audit func(AuditEvent)
	// Route returns the host's value for routing a job's notice (where to
	// deliver it, say). It is called once for every job about to start,
	// foreground commands included (one kept at its timeout becomes a job
	// whose end is announced), before the process starts; the value comes
	// back as Notice.Route and AuditEvent.Route. A Notice is never
	// delivered before Route has returned for its job. The value is kept in
	// memory only: a job announced after a restart has a nil Route. If the
	// start is then refused (a limit, shutdown) the value is dropped. Nil:
	// Route is always nil.
	Route func(call *toolspec.ToolCall) any
	// Logger receives diagnostics, and is the manager's logger unless
	// Jobs.Logger is set. Nil: discarded.
	Logger *slog.Logger
}

// Settings are what Update can change while the tools are in use. A zero
// field takes its default, as in Config and proc.Options.
type Settings struct {
	Timeout    time.Duration // the default foreground timeout
	MaxTimeout time.Duration // the most timeout_seconds and a wait may ask for
	Limits     proc.Limits   // the job limits (see proc.Manager.SetLimits)
}

// Tools is the shell tool provider.
type Tools struct {
	cfg   Config
	m     *proc.Manager
	log   *slog.Logger
	grace time.Duration
	jobs  bool // this platform runs jobs

	// tmu guards the timeouts, which Update changes; cfg's copies are not
	// read after New. Update holds it across Manager.SetLimits so that
	// concurrent updates apply as a whole.
	tmu        sync.Mutex
	timeout    time.Duration
	maxTimeout time.Duration

	mu   sync.Mutex
	dirs map[string]string // remembered working directory per agent and session
}

// New creates the tools and their process manager. Jobs found lost in
// cfg.Jobs.Dir, and jobs the last Close stopped with notify on, are
// announced through cfg.Notice before New returns.
func New(cfg Config) (*Tools, error) {
	timeout, maxTimeout, err := timeouts(cfg.Timeout, cfg.MaxTimeout)
	if err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	t := &Tools{
		cfg:        cfg,
		log:        cfg.Logger.With("component", "shell"),
		grace:      cfg.Jobs.Grace,
		jobs:       proc.Supported(),
		timeout:    timeout,
		maxTimeout: maxTimeout,
		dirs:       make(map[string]string),
	}
	if t.grace == 0 {
		t.grace = proc.DefaultGrace
	}
	opts := cfg.Jobs
	if opts.Logger == nil {
		opts.Logger = cfg.Logger
	}
	hostOnEnd := opts.OnEnd
	opts.OnEnd = func(info proc.Info) {
		t.jobEnded(info)
		if hostOnEnd != nil {
			hostOnEnd(info)
		}
	}
	m, err := proc.New(opts)
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	t.m = m
	return t, nil
}

// timeouts checks the foreground timeouts and applies their defaults.
func timeouts(timeout, maxTimeout time.Duration) (time.Duration, time.Duration, error) {
	if timeout < 0 || maxTimeout < 0 {
		return 0, 0, errors.New("shell: timeouts must not be negative")
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if maxTimeout == 0 {
		maxTimeout = DefaultMaxTimeout
	}
	if timeout > maxTimeout {
		return 0, 0, fmt.Errorf("shell: timeout %v exceeds the maximum %v", timeout, maxTimeout)
	}
	return timeout, maxTimeout, nil
}

// Update changes the timeouts and the job limits, checked as by New; on an
// error nothing changes. It is safe to call at any time, from any
// goroutine. The timeouts apply to calls that start after it returns: a
// foreground command or a wait already running keeps the timeout it began
// with. The limits apply as proc.Manager.SetLimits describes.
func (t *Tools) Update(s Settings) error {
	timeout, maxTimeout, err := timeouts(s.Timeout, s.MaxTimeout)
	if err != nil {
		return err
	}
	t.tmu.Lock()
	defer t.tmu.Unlock()
	if err := t.m.SetLimits(s.Limits); err != nil {
		return fmt.Errorf("shell: %w", err)
	}
	t.timeout, t.maxTimeout = timeout, maxTimeout
	return nil
}

// currentTimeouts returns the default and the maximum timeout in force.
func (t *Tools) currentTimeouts() (time.Duration, time.Duration) {
	t.tmu.Lock()
	defer t.tmu.Unlock()
	return t.timeout, t.maxTimeout
}

// Manager returns the process manager, for the host's own use (listing an
// owner's running jobs, stopping them all, shutdown).
func (t *Tools) Manager() *proc.Manager { return t.m }

// Running returns owner's running jobs, oldest first.
func (t *Tools) Running(owner string) []proc.Info { return t.m.Running(owner) }

// StopAll stops every running job of owner (terminate, grace, kill) and
// returns how many it stopped. Their end is announced as usual.
func (t *Tools) StopAll(owner string) int {
	if owner == "" {
		return 0
	}
	return t.m.StopAll(owner, t.grace)
}

// Close stops every job and refuses new ones (see proc.Manager.Close).
func (t *Tools) Close(ctx context.Context) error { return t.m.Close(ctx) }

// RegisterTools implements toolspec.ToolProvider.
func (t *Tools) RegisterTools(toolspec.Deps) []toolspec.ToolDefinition { return t.Definitions() }

// Namespace implements toolspec.HostMeta.
func (t *Tools) Namespace() string { return Namespace }

// Description implements toolspec.HostMeta.
func (t *Tools) Description() string { return "Shell commands and background jobs" }

// Available implements toolspec.HostMeta.
func (t *Tools) Available(any) (bool, string) { return true, "" }

// owner returns the caller's job owner.
func (t *Tools) owner(call *toolspec.ToolCall) string {
	if t.cfg.Owner != nil {
		return t.cfg.Owner(call)
	}
	return call.AgentID
}

func (t *Tools) ctrlAvailable(call *toolspec.ToolCall) bool {
	return t.cfg.CtrlAvailable == nil || t.cfg.CtrlAvailable(call)
}

func (t *Tools) unattended(call *toolspec.ToolCall) bool {
	return t.cfg.Unattended != nil && t.cfg.Unattended(call)
}

// canKeepJobs reports whether a foreground command reaching its timeout may
// be kept as a job: only for a caller that can collect it.
func (t *Tools) canKeepJobs(call *toolspec.ToolCall) bool {
	return t.ctrlAvailable(call) && !t.unattended(call)
}

func (t *Tools) env(call *toolspec.ToolCall) []string {
	if t.cfg.Env == nil {
		return nil
	}
	return t.cfg.Env(call)
}

func (t *Tools) workspace(call *toolspec.ToolCall) string {
	if t.cfg.Workspace == nil {
		return ""
	}
	return t.cfg.Workspace(call)
}

func (t *Tools) route(call *toolspec.ToolCall) any {
	if t.cfg.Route == nil {
		return nil
	}
	return t.cfg.Route(call)
}

func (t *Tools) audit(e AuditEvent) {
	if t.cfg.Audit != nil {
		t.cfg.Audit(e)
	}
}

// jobEnded delivers the end-of-job notice for a job that asked for one.
func (t *Tools) jobEnded(info proc.Info) {
	if !info.Notify || t.cfg.Notice == nil {
		return
	}
	n := Notice{
		Owner:      info.Owner,
		JobID:      info.ID,
		Command:    info.Command,
		State:      info.State,
		ExitCode:   info.ExitCode,
		Signal:     info.Signal,
		StopReason: info.StopReason,
		Duration:   info.Ended.Sub(info.Started),
		LastLines:  lastLines(info.OutputFile, noticeLines, noticeBytes),
		OutputFile: info.OutputFile,
		Route:      info.Route,
	}
	n.Text = noticeText(info, n.LastLines)
	t.cfg.Notice(n)
}

func callContext(call *toolspec.ToolCall) context.Context {
	if call.Ctx != nil {
		return call.Ctx
	}
	return context.Background()
}

// budget shortens d so a wait ends before the call's deadline, leaving
// time to return the result.
func budget(ctx context.Context, d time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return d
	}
	return max(min(d, time.Until(dl)-deadlineMargin), time.Millisecond)
}
