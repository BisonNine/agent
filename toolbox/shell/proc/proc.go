/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package proc runs shell commands as jobs: each runs `sh -c` in its own
// process group, writes its output (stdout and stderr interleaved) to a file
// as it is produced, and is tracked until it ends, is stopped, or is found
// lost after a restart. It knows nothing about agents or sessions: a job has
// an owner string, and the caller decides what that means.
//
// The manager keeps a small JSON record next to each job's log, so jobs that
// were running when the process died are marked lost at the next start
// (never relaunched), and jobs whose end Close kept quiet are announced
// then. Finished jobs' logs and records are removed after the retention
// period.
//
// Jobs need Unix process groups. On other platforms New works but Start
// returns ErrUnsupported (see Supported).
package proc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/fsmode"
)

// State is a job's lifecycle state.
type State string

// The job states.
const (
	StateRunning State = "running"
	StateExited  State = "exited"  // the command ended by itself
	StateStopped State = "stopped" // the command was stopped (StopReason says why)
	StateLost    State = "lost"    // it was running when the process died
)

// Why a job was stopped (Info.StopReason).
const (
	StopRequested = "stop"       // Stop or StopAll
	StopTimeLimit = "time limit" // it ran longer than Limits.MaxJobTime
	StopShutdown  = "shutdown"   // Close
)

// Reason says why Wait returned.
type Reason string

// The wait reasons.
const (
	ReasonEnded     Reason = "ended"     // the job has ended (or had already)
	ReasonMatch     Reason = "match"     // a new output line matched Until.Match
	ReasonTimeout   Reason = "timeout"   // Until.Timeout passed
	ReasonCancelled Reason = "cancelled" // the context ended
)

// Errors returned by the manager.
var (
	ErrUnsupported = errors.New("background jobs are not supported on this platform")
	ErrUnknown     = errors.New("unknown job")
	ErrEnded       = errors.New("job has ended")
	ErrNoInput     = errors.New("job does not take input")
	ErrClosed      = errors.New("job manager is closed")
)

// LimitError is returned by Start when the owner already has Max jobs
// running. Running lists them, oldest first.
type LimitError struct {
	Owner   string
	Max     int
	Running []Info
}

func (e *LimitError) Error() string {
	ids := make([]string, len(e.Running))
	for i, r := range e.Running {
		ids[i] = r.ID
	}
	return fmt.Sprintf("%d jobs already running (the limit): %s", e.Max, strings.Join(ids, ", "))
}

// Limits bound what jobs may use. A zero field takes its default.
type Limits struct {
	MaxJobs        int           // running jobs per owner; default 8
	MaxJobTime     time.Duration // a job is stopped after this long; default 24h
	MaxOutputBytes int64         // per job; past it the middle is dropped; default 10 MiB
	LogRetention   time.Duration // ended jobs' logs and records are removed after this; default 7 days
}

// Default limits.
const (
	DefaultMaxJobs        = 8
	DefaultMaxJobTime     = 24 * time.Hour
	DefaultMaxOutputBytes = 10 << 20
	DefaultLogRetention   = 7 * 24 * time.Hour
	DefaultGrace          = 5 * time.Second
)

// Options configure a Manager.
type Options struct {
	// Dir holds each job's <id>.log and <id>.json. Required; created if missing.
	Dir    string
	Limits Limits
	// Modes are the permissions of the log directory and of the files in
	// it. A zero field takes its default (fsmode.DefaultDir, DefaultFile).
	Modes fsmode.Modes
	// Grace is how long Close and the time limit wait between terminating a
	// job's process group and killing it. Default 5s.
	Grace time.Duration
	// Clock is the source of time. Nil: the real clock.
	Clock Clock
	// Logger receives diagnostics. Nil: discarded.
	Logger *slog.Logger
	// OnEnd is called once for each job that ends (exited or stopped), from
	// the job's own goroutine, and must not block for long. It is not called
	// for a job that ends once Close has begun: the caller is shutting down.
	// New calls it, before it returns, for each job found lost and for each
	// job with Notify on that ended while the previous manager was closing
	// (stopped with StopShutdown, or exited by itself), once.
	OnEnd func(Info)
}

// Spec describes a command to start.
type Spec struct {
	Owner   string // required
	Command string // run as `sh -c Command`; required
	Dir     string // working directory; "" is the manager's process directory
	// Env is the child's environment. Nil inherits this process's
	// environment (as os/exec does); an empty slice gives an empty one.
	Env []string
	// Notify is recorded on the job for the caller's end-of-job notice.
	Notify bool
	// Input keeps the job's standard input open for Write. Without it the
	// job reads an empty input.
	Input bool
	// TrackDir records the directory the command ends in (Info.EndDir).
	TrackDir bool
	// Route is an opaque value of the caller's, kept with the job and
	// returned as Info.Route (so OnEnd receives it). It is held in memory
	// only: a job loaded from its record by New has a nil Route.
	Route any
}

// Info is a snapshot of a job.
type Info struct {
	ID      string
	Owner   string
	Command string
	Dir     string
	State   State
	// ExitCode is the exit status once the job has ended by itself or by a
	// stop; -1 while running, when lost, or when a signal ended it.
	ExitCode     int
	Signal       string // the signal that ended it, e.g. "terminated"
	StopReason   string // set when State is StateStopped
	Started      time.Time
	Ended        time.Time // zero while running
	OutputBytes  int64     // bytes produced in all
	DroppedBytes int64     // bytes dropped from the middle of the log
	OutputFile   string
	Notify       bool
	Input        bool
	EndDir       string // with Spec.TrackDir: where the command ended, if known
	Route        any    // Spec.Route; nil for a job loaded from its record
}

// Running reports whether the job is still running.
func (i Info) Running() bool { return i.State == StateRunning }

// Until says when Wait returns. It always returns once the job has ended.
type Until struct {
	// Match returns as soon as an output line produced since the previous
	// Wait (or the job's start) matches. The unfinished last line counts.
	Match *regexp.Regexp
	// Timeout returns after this long. Zero or less: no timeout.
	Timeout time.Duration
	// MaxOutput bounds WaitResult.Output: when the new output is longer, its
	// first 30% and last 70% are returned with an "omitted" line between.
	// Zero or less: no bound.
	MaxOutput int
}

// WaitResult is what Wait returns.
type WaitResult struct {
	Info    Info
	Reason  Reason
	Matched string // the matching line, for ReasonMatch
	// Output is what the job produced since the previous Wait, or since its
	// start; the next Wait continues from here.
	Output []byte
	// Omitted counts bytes of that output not in Output: dropped from the log
	// by its size limit, or left out by MaxOutput.
	Omitted int64
}

// Manager runs and tracks jobs. Its methods are safe for concurrent use.
type Manager struct {
	dir   string
	modes fsmode.Modes
	grace time.Duration
	clock Clock
	log   *slog.Logger
	onEnd func(Info)

	mu      sync.Mutex
	jobs    map[string]*job
	pending map[string]*job // reserved by Start, not yet launched
	closed  bool
	wg      sync.WaitGroup // job goroutines and starts in progress

	// limMu guards the limits. It is a leaf lock: nothing else is taken
	// while it is held.
	limMu      sync.Mutex
	limits     Limits
	limChanged chan struct{} // closed and replaced by SetLimits
}

// Supported reports whether this platform can run jobs.
func Supported() bool { return supported }

// New creates a manager over opts.Dir. It loads the records of earlier
// jobs, marks any found running as lost (calling OnEnd for each), and
// removes those past the retention period.
func New(opts Options) (*Manager, error) {
	if opts.Dir == "" {
		return nil, errors.New("proc: Dir is required")
	}
	if opts.Grace < 0 {
		return nil, errors.New("proc: grace must not be negative")
	}
	l, err := withDefaults(opts.Limits)
	if err != nil {
		return nil, err
	}
	modes := opts.Modes.Or(fsmode.Modes{Dir: fsmode.DefaultDir, File: fsmode.DefaultFile})
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("proc: %w", err)
	}
	if err = os.MkdirAll(dir, modes.Dir); err != nil {
		return nil, fmt.Errorf("proc: create log directory: %w", err)
	}
	if err = os.Chmod(dir, modes.Dir); err != nil {
		return nil, fmt.Errorf("proc: set log directory mode: %w", err)
	}
	m := &Manager{
		dir:        dir,
		modes:      modes,
		grace:      opts.Grace,
		clock:      opts.Clock,
		log:        opts.Logger,
		onEnd:      opts.OnEnd,
		jobs:       make(map[string]*job),
		pending:    make(map[string]*job),
		limits:     l,
		limChanged: make(chan struct{}),
	}
	if m.grace == 0 {
		m.grace = DefaultGrace
	}
	if m.clock == nil {
		m.clock = realClock{}
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	m.log = m.log.With("component", "shell")
	announce, err := m.load()
	if err != nil {
		return nil, err
	}
	m.sweep()
	if m.onEnd != nil {
		for _, info := range announce {
			m.onEnd(info)
		}
	}
	return m, nil
}

// withDefaults refuses negative limits and gives zero ones their default.
func withDefaults(l Limits) (Limits, error) {
	if l.MaxJobs < 0 || l.MaxJobTime < 0 || l.MaxOutputBytes < 0 || l.LogRetention < 0 {
		return Limits{}, errors.New("proc: limits must not be negative")
	}
	if l.MaxJobs == 0 {
		l.MaxJobs = DefaultMaxJobs
	}
	if l.MaxJobTime == 0 {
		l.MaxJobTime = DefaultMaxJobTime
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if l.LogRetention == 0 {
		l.LogRetention = DefaultLogRetention
	}
	return l, nil
}

// Dir returns the absolute log directory.
func (m *Manager) Dir() string { return m.dir }

// Limits returns the limits in force, defaults applied.
func (m *Manager) Limits() Limits {
	m.limMu.Lock()
	defer m.limMu.Unlock()
	return m.limits
}

// SetLimits replaces the limits, checked as by New: a negative field is
// refused (changing nothing) and a zero one takes its default. Once it
// returns they apply as follows:
//   - MaxJobs to later starts; running jobs over a lowered limit are not
//     stopped, but no start succeeds until the owner is below it.
//   - MaxJobTime to running jobs too, measured from each job's start: one
//     already older than a shortened limit is stopped at once with
//     StopTimeLimit.
//   - MaxOutputBytes to output running jobs write from now on. A head a
//     log has already kept is not shrunk.
//   - LogRetention at the next sweep (the next Start).
func (m *Manager) SetLimits(l Limits) error {
	l, err := withDefaults(l)
	if err != nil {
		return err
	}
	m.limMu.Lock()
	m.limits = l
	close(m.limChanged)
	m.limChanged = make(chan struct{})
	m.limMu.Unlock()

	// Pending jobs are included: one that has not opened its log yet reads
	// the new limit when it does; one that has is updated here.
	m.mu.Lock()
	jobs := m.runningLocked("")
	for _, j := range m.pending {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		if j.out != nil && j.out.f != nil {
			// The limit in force now, not l: a later SetLimits may have
			// passed this one.
			j.out.setLimit(m.Limits().MaxOutputBytes)
		}
		j.mu.Unlock()
	}
	return nil
}

// jobTimeLimit returns MaxJobTime and a channel closed when it may change.
func (m *Manager) jobTimeLimit() (time.Duration, <-chan struct{}) {
	m.limMu.Lock()
	defer m.limMu.Unlock()
	return m.limits.MaxJobTime, m.limChanged
}

// isClosed reports whether Close has begun.
func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Start starts spec as a job and returns at once. The job does not depend on
// ctx: it runs until it ends, is stopped, or reaches the time limit. A start
// beyond the owner's MaxJobs returns a *LimitError.
func (m *Manager) Start(ctx context.Context, spec Spec) (Info, error) {
	if !supported {
		return Info{}, ErrUnsupported
	}
	if spec.Owner == "" {
		return Info{}, errors.New("proc: owner is required")
	}
	if strings.TrimSpace(spec.Command) == "" {
		return Info{}, errors.New("proc: command is required")
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	m.sweep()
	j, err := m.reserve(spec)
	if err != nil {
		return Info{}, err
	}
	defer m.wg.Done()
	if err := m.launch(j, spec); err != nil {
		m.mu.Lock()
		delete(m.pending, j.id)
		m.mu.Unlock()
		m.removeFiles(j.id)
		return Info{}, err
	}
	m.mu.Lock()
	delete(m.pending, j.id)
	m.jobs[j.id] = j
	closed := m.closed
	m.mu.Unlock()
	if closed {
		// The caller is told it did not start: never announce it.
		j.mu.Lock()
		j.notify = false
		j.mu.Unlock()
		j.stop(m.grace, StopShutdown)
		return Info{}, ErrClosed
	}
	return j.info(), nil
}

// reserve checks the limits and holds a place for the job, so concurrent
// starts cannot pass the limit together. On success it has added one to
// m.wg for the start in progress, under m.mu while the manager is open, so
// Close cannot begin waiting before it.
//
// m.mu is never held while taking a job's lock: that lock can be held
// through disk I/O (log compaction, reading output).
func (m *Manager) reserve(spec Spec) (*job, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	running := m.runningLocked(spec.Owner)
	reserved := 0
	for _, j := range m.pending {
		if j.owner == spec.Owner {
			reserved++
		}
	}
	if limit := m.Limits().MaxJobs; len(running)+reserved >= limit {
		m.mu.Unlock()
		return nil, &LimitError{Owner: spec.Owner, Max: limit, Running: infos(running)}
	}
	defer m.mu.Unlock()
	id, err := m.newID()
	if err != nil {
		return nil, err
	}
	j := &job{
		m:       m,
		id:      id,
		owner:   spec.Owner,
		command: spec.Command,
		dir:     spec.Dir,
		input:   spec.Input,
		state:   StateRunning,
		notify:  spec.Notify,
		route:   spec.Route,
		started: m.clock.Now(),
		done:    make(chan struct{}),
		changed: make(chan struct{}),
	}
	m.pending[id] = j
	m.wg.Add(1)
	return j, nil
}

var idPattern = regexp.MustCompile(`^j[0-9a-f]{8}$`)

// newID returns an id not in use by a job or a leftover file. Caller holds m.mu.
func (m *Manager) newID() (string, error) {
	for range 100 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("proc: job id: %w", err)
		}
		id := "j" + hex.EncodeToString(b[:])
		if _, ok := m.jobs[id]; ok {
			continue
		}
		if _, ok := m.pending[id]; ok {
			continue
		}
		if _, err := os.Lstat(m.logPath(id)); err == nil {
			continue
		}
		return id, nil
	}
	return "", errors.New("proc: could not find a free job id")
}

func (m *Manager) logPath(id string) string    { return filepath.Join(m.dir, id+".log") }
func (m *Manager) recordPath(id string) string { return filepath.Join(m.dir, id+".json") }
func (m *Manager) cwdPath(id string) string    { return filepath.Join(m.dir, id+".cwd") }

func (m *Manager) lookup(id string) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrUnknown
	}
	return j, nil
}

// Get returns a snapshot of job id.
func (m *Manager) Get(id string) (Info, bool) {
	j, err := m.lookup(id)
	if err != nil {
		return Info{}, false
	}
	return j.info(), true
}

// List returns owner's jobs (every job when owner is ""), oldest first.
func (m *Manager) List(owner string) []Info {
	m.mu.Lock()
	var jobs []*job
	for _, j := range m.jobs {
		if owner == "" || j.owner == owner {
			jobs = append(jobs, j)
		}
	}
	m.mu.Unlock()
	return infos(jobs)
}

// infos snapshots jobs, oldest first. Callers do not hold m.mu.
func infos(jobs []*job) []Info {
	out := make([]Info, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.info())
	}
	sortByStart(out)
	return out
}

// runningLocked returns owner's (or every owner's, for "") jobs that have
// not ended. Caller holds m.mu; job locks are not taken.
func (m *Manager) runningLocked(owner string) []*job {
	var out []*job
	for _, j := range m.jobs {
		if !j.isDone() && (owner == "" || j.owner == owner) {
			out = append(out, j)
		}
	}
	return out
}

// Running returns owner's running jobs (every owner's when owner is ""),
// oldest first.
func (m *Manager) Running(owner string) []Info {
	var out []Info
	for _, info := range m.List(owner) {
		if info.Running() {
			out = append(out, info)
		}
	}
	return out
}

func sortByStart(infos []Info) {
	slices.SortFunc(infos, func(a, b Info) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// SetNotify changes whether the job's end will be announced (Info.Notify).
// It returns ErrEnded, changing nothing, when the job has already ended:
// its end was then announced (or not) with the previous setting.
func (m *Manager) SetNotify(id string, notify bool) error {
	j, err := m.lookup(id)
	if err != nil {
		return err
	}
	j.mu.Lock()
	if j.isDone() {
		j.mu.Unlock()
		return ErrEnded
	}
	j.notify = notify
	j.mu.Unlock()
	m.saveRecord(j)
	return nil
}

// Wait blocks until the job ends, a new output line matches u.Match, u.Timeout
// passes or ctx ends, and returns the output produced since the previous
// Wait. A job that has already ended returns at once. When ctx ends the
// result is still filled in and the error is ctx's.
func (m *Manager) Wait(ctx context.Context, id string, u Until) (WaitResult, error) {
	j, err := m.lookup(id)
	if err != nil {
		return WaitResult{}, err
	}
	var timeout <-chan time.Time
	if u.Timeout > 0 {
		t := m.clock.NewTimer(u.Timeout)
		defer t.Stop()
		timeout = t.C()
	}
	j.mu.Lock()
	scan := j.cursor
	j.mu.Unlock()
	for {
		j.mu.Lock()
		ended := j.isDone()
		changed := j.changed
		var matched string
		var found bool
		if u.Match != nil {
			matched, found, scan, err = j.scanLocked(scan, u.Match)
		}
		j.mu.Unlock()
		switch {
		case err != nil:
			return WaitResult{}, err
		case found:
			return j.collect(ReasonMatch, matched, u.MaxOutput)
		case ended:
			return j.collect(ReasonEnded, "", u.MaxOutput)
		}
		select {
		case <-changed:
		case <-j.done:
		case <-timeout:
			return j.collect(ReasonTimeout, "", u.MaxOutput)
		case <-ctx.Done():
			r, err := j.collect(ReasonCancelled, "", u.MaxOutput)
			if err != nil {
				return r, err
			}
			return r, ctx.Err()
		}
	}
}

// Write sends data to the job's standard input. It fails with ErrEnded once
// the job has ended, ErrNoInput when the job was started without Input, and
// an error when the job does not read its input within a few seconds.
func (m *Manager) Write(id string, data []byte) error {
	j, err := m.lookup(id)
	if err != nil {
		return err
	}
	return j.write(data)
}

// Stop ends the job's whole process group: terminate, then kill after grace.
// It returns once the job has ended. Stopping an ended job does nothing.
func (m *Manager) Stop(id string, grace time.Duration) error {
	j, err := m.lookup(id)
	if err != nil {
		return err
	}
	j.stop(grace, StopRequested)
	return nil
}

// StopAll stops every running job of owner (of every owner when owner is
// ""), concurrently, and returns how many it stopped.
func (m *Manager) StopAll(owner string, grace time.Duration) int {
	return m.stopJobs(m.runningJobs(owner), grace, StopRequested)
}

func (m *Manager) runningJobs(owner string) []*job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runningLocked(owner)
}

func (m *Manager) stopJobs(jobs []*job, grace time.Duration, reason string) int {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Go(func() { j.stop(grace, reason) })
	}
	wg.Wait()
	return len(jobs)
}

// Remove deletes an ended job's log and record. A running job is not removed.
func (m *Manager) Remove(id string) error {
	j, err := m.lookup(id)
	if err != nil {
		return err
	}
	if !j.isDone() {
		return errors.New("proc: job is running")
	}
	m.mu.Lock()
	delete(m.jobs, id)
	m.mu.Unlock()
	m.removeJob(j)
	return nil
}

// removeJob deletes a job's files and makes sure no later record write
// brings its record back.
func (m *Manager) removeJob(j *job) {
	j.recMu.Lock()
	defer j.recMu.Unlock()
	j.mu.Lock()
	j.removed = true
	j.mu.Unlock()
	m.removeFiles(j.id)
}

// Close stops every job (terminate, grace, kill) and refuses new starts. It
// returns when they have all ended, or with ctx's error if ctx ends first.
// OnEnd is not called for the jobs that end from here on; those with Notify
// on are recorded as not yet announced, and the next New over the same
// directory announces them.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.stopJobs(m.runningJobs(""), m.grace, StopShutdown)
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sweep removes ended jobs older than the retention period.
func (m *Manager) sweep() {
	cutoff := m.clock.Now().Add(-m.Limits().LogRetention)
	m.mu.Lock()
	var ended []*job
	for _, j := range m.jobs {
		if j.isDone() {
			ended = append(ended, j)
		}
	}
	m.mu.Unlock()
	for _, j := range ended {
		if !j.info().Ended.Before(cutoff) {
			continue
		}
		m.mu.Lock()
		_, present := m.jobs[j.id]
		delete(m.jobs, j.id)
		m.mu.Unlock()
		if present {
			m.removeJob(j)
			m.log.Debug("removed expired shell job", "job", j.id)
		}
	}
}

func (m *Manager) removeFiles(id string) {
	for _, p := range []string{m.logPath(id), m.recordPath(id), m.cwdPath(id)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			m.log.Warn("could not remove shell job file", "path", p, "error", err)
		}
	}
}
