/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.bison9.com/agent/toolspec"
)

// waitDelay bounds how long the simple runner waits for output after the
// command was killed (a child may still hold it open).
const waitDelay = 2 * time.Second

// runSimple runs a command and waits for it, for platforms without jobs:
// output is kept in memory (head and tail), and the command is killed at
// the timeout.
func (t *Tools) runSimple(ctx context.Context, command, dir string, env []string, timeout time.Duration) *toolspec.Result {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command) //nolint:gosec // running the caller's command is the purpose; the guard screened it
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", command) //nolint:gosec // running the caller's command is the purpose; the guard screened it
	}
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" && cmd.Process != nil {
			// Best effort for the children; the Kill below is what is reported.
			exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() //nolint:errcheck,gosec // fixed program and our own pid
		}
		return cmd.Process.Kill()
	}
	buf := &headTail{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Run()

	if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return userResult(fmt.Sprintf("Command timed out after %s and was stopped.\nOutput:\n%s", human(timeout), output(buf.bytes())), true)
	}
	var b strings.Builder
	b.WriteString(output(buf.bytes()))
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return userResult(b.String(), false)
	case errors.As(err, &exitErr):
		fmt.Fprintf(&b, "\nExit code: %d", exitErr.ExitCode())
	default:
		fmt.Fprintf(&b, "\nfailed to run command: %v", err)
	}
	return userResult(b.String(), true)
}

// headTail keeps the first and the last bytes written to it, bounded to
// resultLimit, and counts what it left out. os/exec writes to it from one
// goroutine at a time when it is both Stdout and Stderr.
type headTail struct {
	head, tail []byte
	total      int64
}

const (
	headKeep = resultLimit * 3 / 10
	tailKeep = resultLimit - headKeep
)

func (h *headTail) Write(p []byte) (int, error) {
	h.total += int64(len(p))
	rest := p
	if room := headKeep - len(h.head); room > 0 {
		n := min(room, len(rest))
		h.head = append(h.head, rest[:n]...)
		rest = rest[n:]
	}
	h.tail = append(h.tail, rest...)
	if len(h.tail) > 2*tailKeep {
		h.tail = append(h.tail[:0], h.tail[len(h.tail)-tailKeep:]...)
	}
	return len(p), nil
}

func (h *headTail) bytes() []byte {
	tail := h.tail
	if len(tail) > tailKeep {
		tail = tail[len(tail)-tailKeep:]
	}
	out := append([]byte{}, h.head...)
	if omitted := h.total - int64(len(h.head)+len(tail)); omitted > 0 {
		out = append(out, fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted)...)
	}
	return append(out, tail...)
}
