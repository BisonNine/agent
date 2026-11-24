//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const supported = true

// setProcessGroup puts the command in a process group of its own, so a
// stop reaches everything it started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup and killGroup signal the whole group. An error means the
// group has already gone, which is what a stop wants.
func terminateGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGTERM) //nolint:errcheck // ESRCH: already gone
}

func killGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL) //nolint:errcheck // ESRCH: already gone
}

// groupExists reports whether the process group still has members.
func groupExists(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// exitStatus returns the exit code, or -1 and the signal's name when a
// signal ended the process.
func exitStatus(ps *os.ProcessState) (int, string) {
	if ps == nil {
		return -1, ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return -1, ws.Signal().String()
	}
	return ps.ExitCode(), ""
}
