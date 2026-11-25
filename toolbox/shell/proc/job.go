/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// inputTimeout bounds a write to a job's standard input when the job is not
// reading it. It is an operating-system deadline, so it is real time.
const inputTimeout = 5 * time.Second

// job is one command. The fields set before launch never change; the rest
// are guarded by mu.
type job struct {
	m       *Manager
	id      string
	owner   string
	command string
	dir     string
	input   bool
	route   any // the caller's Spec.Route; in memory only
	started time.Time

	pid     int
	output  *os.File // read end of the output pipe
	stdin   *os.File // write end of the input pipe; nil without Input
	stdinMu sync.Mutex
	cwd     *os.File      // receives the end directory with TrackDir
	recMu   sync.Mutex    // serialises record writes and removal
	done    chan struct{} // closed once the job has ended and its record is saved
	reaped  atomic.Bool   // sh has exited and been waited for

	mu         sync.Mutex
	state      State
	notify     bool
	exitCode   int
	signal     string
	stopReason string
	ended      time.Time
	endDir     string
	out        *logFile
	changed    chan struct{} // closed and replaced on new output and on end
	cursor     int64         // logical output offset the next Wait starts from
	removed    bool          // files deleted: never write the record again
	// unannounced: the job ended with notify on while the manager was
	// closing, so OnEnd was not called; the next New announces it.
	unannounced bool
}

// isDone reports whether the job has ended. It needs no lock.
func (j *job) isDone() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// trackDirPrefix makes sh print its working directory to descriptor 9 when
// the command exits, on the command's own line so error line numbers are
// unchanged. A high descriptor keeps clear of the ones scripts redirect.
const trackDirPrefix = "trap 'pwd >&9' EXIT; "

// trackDirFD is the descriptor the end directory is written to.
const trackDirFD = 9

// launch opens the job's files, starts the process and its goroutines.
func (m *Manager) launch(j *job, spec Spec) error {
	// opened is closed if the start fails; childEnds (the child's ends of
	// the pipes) are closed once the child has them.
	var opened, childEnds []*os.File
	fail := func(err error) error {
		closeFiles(childEnds)
		closeFiles(opened)
		return err
	}

	logf, err := m.createFile(m.logPath(j.id))
	if err != nil {
		return err
	}
	opened = append(opened, logf)

	cmd := exec.Command("sh", "-c", spec.Command) //nolint:gosec // running the caller's command is the purpose; the caller screens it
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	setProcessGroup(cmd)

	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(fmt.Errorf("proc: output pipe: %w", err))
	}
	opened = append(opened, outR)
	childEnds = append(childEnds, outW)
	cmd.Stdout = outW
	cmd.Stderr = outW

	var inW *os.File
	if spec.Input {
		inR, w, perr := os.Pipe()
		if perr != nil {
			return fail(fmt.Errorf("proc: input pipe: %w", perr))
		}
		opened = append(opened, w)
		childEnds = append(childEnds, inR)
		cmd.Stdin = inR
		inW = w
	}

	var cwdf *os.File
	if spec.TrackDir {
		if cwdf, err = m.createFile(m.cwdPath(j.id)); err != nil {
			return fail(err)
		}
		opened = append(opened, cwdf)
		// ExtraFiles[i] is descriptor 3+i; nil entries are closed in the child.
		cmd.ExtraFiles = make([]*os.File, trackDirFD-2)
		cmd.ExtraFiles[trackDirFD-3] = cwdf
		cmd.Args[2] = trackDirPrefix + spec.Command
	}

	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start command: %w", err))
	}
	closeFiles(childEnds)

	// The limit is read under j.mu so a concurrent SetLimits either is seen
	// here or updates the log afterwards.
	j.mu.Lock()
	j.out = newLogFile(logf.Name(), logf, m.Limits().MaxOutputBytes)
	j.mu.Unlock()
	j.pid = cmd.Process.Pid
	j.output = outR
	j.stdin = inW
	j.cwd = cwdf
	m.saveRecord(j)
	m.log.Debug("started shell job", "job", j.id, "owner", j.owner, "pid", j.pid, "dir", j.dir)

	readDone := make(chan struct{})
	m.wg.Add(3)
	go func() {
		defer m.wg.Done()
		defer close(readDone)
		j.copyOutput()
	}()
	go func() {
		defer m.wg.Done()
		waitErr := cmd.Wait()
		j.reaped.Store(true)
		<-readDone
		j.finish(cmd.ProcessState, waitErr)
	}()
	go func() {
		defer m.wg.Done()
		j.enforceTimeLimit()
	}()
	return nil
}

// closeFiles closes files whose close error has no consequence: pipe ends,
// and files abandoned after a failed start.
func closeFiles(files []*os.File) {
	for _, f := range files {
		f.Close() //nolint:errcheck // nothing was written that a failed close could lose
	}
}

// createFile creates path exclusively with the manager's file mode.
func (m *Manager) createFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, m.modes.File) //nolint:gosec // path is the manager's directory and a generated id
	if err != nil {
		return nil, fmt.Errorf("proc: create %s: %w", filepath.Base(path), err)
	}
	if err := f.Chmod(m.modes.File); err != nil {
		return nil, errors.Join(fmt.Errorf("proc: set mode of %s: %w", filepath.Base(path), err), f.Close())
	}
	return f, nil
}

// copyOutput moves output from the pipe to the log until the pipe closes
// (every holder of its write end has exited) or stop forces it shut.
func (j *job) copyOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := j.output.Read(buf)
		if n > 0 {
			j.mu.Lock()
			compacted, werr := j.out.write(buf[:n])
			close(j.changed)
			j.changed = make(chan struct{})
			j.mu.Unlock()
			if werr != nil {
				j.m.log.Warn("shell job output not written", "job", j.id, "error", werr)
			}
			if compacted {
				j.m.saveRecord(j)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				j.m.log.Debug("shell job output closed", "job", j.id, "error", err)
			}
			closeFiles([]*os.File{j.output})
			return
		}
	}
}

// finish records the end of the job and announces it.
func (j *job) finish(ps *os.ProcessState, waitErr error) {
	code, signal := exitStatus(ps)
	if ps == nil && waitErr != nil {
		j.m.log.Warn("shell job wait failed", "job", j.id, "error", waitErr)
	}
	j.stdinMu.Lock()
	if j.stdin != nil {
		closeFiles([]*os.File{j.stdin})
	}
	j.stdinMu.Unlock()

	// Read before taking j.mu: m.mu is never taken under a job's lock.
	closing := j.m.isClosed()

	// The final state is visible to info() from here, but the job counts as
	// ended (Wait, SetNotify) only once done closes, after the record is
	// saved, so a caller that saw the end never races the record.
	j.mu.Lock()
	j.state = StateExited
	if j.stopReason != "" {
		j.state = StateStopped
	}
	// A stop for shutdown implies Close has begun even if closing was read
	// just before it did. Its end is left for the next New to announce.
	quiet := closing || j.stopReason == StopShutdown
	j.unannounced = quiet && j.notify
	j.exitCode = code
	j.signal = signal
	j.ended = j.m.clock.Now()
	if j.cwd != nil {
		if j.state == StateExited {
			j.endDir = readEndDir(j.cwd)
		}
		closeFiles([]*os.File{j.cwd})
		if err := os.Remove(j.cwd.Name()); err != nil {
			j.m.log.Debug("shell job directory file not removed", "job", j.id, "error", err)
		}
	}
	if err := j.out.f.Close(); err != nil {
		j.m.log.Warn("shell job log not closed", "job", j.id, "error", err)
	}
	j.out.f = nil
	j.mu.Unlock()

	j.m.saveRecord(j)

	j.mu.Lock()
	close(j.done)
	close(j.changed)
	j.changed = make(chan struct{})
	info := j.infoLocked()
	j.mu.Unlock()
	j.m.log.Debug("shell job ended", "job", j.id, "state", info.State, "exit_code", info.ExitCode, "signal", info.Signal)
	if j.m.onEnd != nil && !quiet {
		j.m.onEnd(info)
	}
}

// readEndDir reads the directory the shell's exit trap wrote.
func readEndDir(f *os.File) string {
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 64*1024))
	if err != nil {
		return ""
	}
	dir := strings.TrimRight(string(data), "\n")
	if !filepath.IsAbs(dir) || strings.Contains(dir, "\n") {
		return ""
	}
	return dir
}

// enforceTimeLimit stops the job when it runs past MaxJobTime, measured from
// its start. A change of the limit (SetLimits) re-arms the timer, so a job
// already older than a shortened limit is stopped at once.
func (j *job) enforceTimeLimit() {
	for {
		limit, changed := j.m.jobTimeLimit()
		t := j.m.clock.NewTimer(j.started.Add(limit).Sub(j.m.clock.Now()))
		select {
		case <-j.done:
			t.Stop()
			return
		case <-changed:
			t.Stop()
		case <-t.C():
			// The limit may have been raised as the timer fired.
			if limit, _ = j.m.jobTimeLimit(); j.m.clock.Now().Before(j.started.Add(limit)) {
				continue
			}
			j.m.log.Info("shell job reached its time limit", "job", j.id, "limit", limit)
			j.stop(j.m.grace, StopTimeLimit)
			return
		}
	}
}

// stop terminates the process group, kills it after grace, and if the output
// is still held open (by a process that left the group) closes it, so the
// job always ends. It returns once the job has ended.
func (j *job) stop(grace time.Duration, reason string) {
	j.mu.Lock()
	if j.state != StateRunning {
		j.mu.Unlock()
		return
	}
	if j.stopReason == "" {
		j.stopReason = reason
	}
	j.mu.Unlock()

	// Once sh has been reaped its pid can be reused; signal the group only
	// while it still has members.
	if j.signalable() {
		terminateGroup(j.pid)
		if j.waitDone(grace) {
			return
		}
	}
	if j.signalable() {
		killGroup(j.pid)
		if j.waitDone(grace) {
			return
		}
	}
	// Something outside the group still holds the output open: let go of it.
	closeFiles([]*os.File{j.output})
	<-j.done
}

func (j *job) signalable() bool {
	return !j.reaped.Load() || groupExists(j.pid)
}

func (j *job) waitDone(d time.Duration) bool {
	t := j.m.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-j.done:
		return true
	case <-t.C():
		return false
	}
}

func (j *job) write(data []byte) error {
	if j.isDone() {
		return ErrEnded
	}
	if !j.input {
		return ErrNoInput
	}
	j.stdinMu.Lock()
	defer j.stdinMu.Unlock()
	if err := j.stdin.SetWriteDeadline(time.Now().Add(inputTimeout)); err != nil {
		return ErrEnded
	}
	if _, err := j.stdin.Write(data); err != nil {
		if errors.Is(err, os.ErrClosed) {
			return ErrEnded
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return errors.New("the job is not reading its input")
		}
		return fmt.Errorf("write input: %w", err)
	}
	return nil
}

// collect returns the output since the cursor and moves the cursor to the end.
func (j *job) collect(reason Reason, matched string, maxOutput int) (WaitResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	pieces, dropped := j.out.since(j.cursor)
	data, omitted, err := j.out.read(pieces, maxOutput)
	if err != nil {
		return WaitResult{}, err
	}
	j.cursor = j.out.produced
	return WaitResult{
		Info:    j.infoLocked(),
		Reason:  reason,
		Matched: matched,
		Output:  data,
		Omitted: dropped + omitted,
	}, nil
}

// maxCarry bounds how much of an unfinished line is scanned again when it
// grows, so output without line ends cannot make matching quadratic.
const maxCarry = 64 * 1024

// scanLocked looks for a line matching re in the output from scan on (lines
// end at \n or \r, so progress lines count). It returns the line and where
// the next scan starts: at the beginning of the unfinished last line, so
// that line is looked at again once it grows.
func (j *job) scanLocked(scan int64, re *regexp.Regexp) (string, bool, int64, error) {
	if scan >= j.out.produced {
		return "", false, scan, nil
	}
	pieces, _ := j.out.since(scan)
	data, _, err := j.out.read(pieces, 0)
	if err != nil {
		return "", false, scan, err
	}
	partial := len(data)
	if i := bytes.LastIndexAny(data, "\n\r"); i >= 0 {
		partial = len(data) - i - 1
	}
	next := j.out.produced - int64(partial)
	if partial > maxCarry {
		next = j.out.produced
	}
	for line := range strings.FieldsFuncSeq(string(data), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if re.MatchString(line) {
			return line, true, next, nil
		}
	}
	return "", false, next, nil
}

func (j *job) info() Info {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.infoLocked()
}

func (j *job) infoLocked() Info {
	info := Info{
		ID:         j.id,
		Owner:      j.owner,
		Command:    j.command,
		Dir:        j.dir,
		State:      j.state,
		ExitCode:   -1,
		Signal:     j.signal,
		StopReason: j.stopReason,
		Started:    j.started,
		Ended:      j.ended,
		OutputFile: j.m.logPath(j.id),
		Notify:     j.notify,
		Input:      j.input,
		EndDir:     j.endDir,
		Route:      j.route,
	}
	if j.state == StateExited || j.state == StateStopped {
		info.ExitCode = j.exitCode
	}
	if j.out != nil {
		info.OutputBytes = j.out.produced
		info.DroppedBytes = j.out.dropped
	}
	return info
}
