//go:build windows

/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"log/slog"
	"os/exec"
)

// setCLIProcessGroup is a no-op on Windows: there is no process-group signal,
// so cancellation falls back to exec's default of killing the CLI process, and
// WaitDelay (set by newCLICommand) still bounds Wait when a descendant holds
// the output pipes.
func setCLIProcessGroup(cmd *exec.Cmd) {}

// killCLIProcessGroup is a no-op on Windows; exec has already killed the CLI
// process by the time it is called.
func killCLIProcessGroup(cmd *exec.Cmd, log *slog.Logger) {}
