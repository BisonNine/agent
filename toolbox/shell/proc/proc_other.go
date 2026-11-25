//go:build !unix

/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import (
	"os"
	"os/exec"
)

// Jobs need Unix process groups and signals; Start refuses elsewhere.
const supported = false

func setProcessGroup(*exec.Cmd) {}

func terminateGroup(int) {}

func killGroup(int) {}

func groupExists(int) bool { return false }

func exitStatus(ps *os.ProcessState) (int, string) {
	if ps == nil {
		return -1, ""
	}
	return ps.ExitCode(), ""
}
