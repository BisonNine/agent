//go:build !windows

/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"errors"
	"log/slog"
	"os/exec"
	"syscall"
)

// setCLIProcessGroup starts the CLI as the leader of a new process group and
// makes context cancellation terminate that whole group, so MCP servers and
// shells the CLI spawned die with it instead of being orphaned.
func setCLIProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// killCLIProcessGroup sends SIGKILL to the CLI's process group. The group id
// equals the CLI's pid because of Setpgid; the kernel keeps that id reserved
// while any member survives, so the signal cannot reach an unrelated process
// as long as there is something left to kill. ESRCH (nothing left) is the
// normal outcome and is ignored.
func killCLIProcessGroup(cmd *exec.Cmd, log *slog.Logger) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Debug("killing CLI process group", "pid", cmd.Process.Pid, "error", err.Error())
	}
}
