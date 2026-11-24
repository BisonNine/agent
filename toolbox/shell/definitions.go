/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"fmt"

	"go.bison9.com/agent/toolspec"
)

// Tool descriptions are read by the model on every request: short and exact.
const (
	execDescription = "Run a command-line program: git, builds and tests, and Unix tools such as grep, sed, awk and jq for pipelines. " +
		"Long output is cut to its head and tail and the full log file is named. " +
		"A command still running at its timeout is kept as a job; background starts one and returns at once. " +
		"Manage jobs with " + ctrlTool + ". " +
		"To find, read or edit files, the file tools are usually better: their output is numbered and limited in size."

	ctrlDescription = "Manage jobs started by " + execTool + ". " +
		"list: your jobs. " +
		"wait: block until the job ends, a new output line matches until, or seconds pass; returns the new output. " +
		"input: write text to the job's standard input. " +
		"stop: end the job, or all your jobs with all. " +
		"A job's full output is a log file: read it with the file tools."
)

// Definitions returns the tool definitions with bare names ("exec", "ctrl").
// They state the timeouts in force when they are built.
func (t *Tools) Definitions() []toolspec.ToolDefinition {
	timeout, maxDuration := t.currentTimeouts()
	maxTimeout := float64(maxDuration.Seconds())
	minOne := float64(1)
	return []toolspec.ToolDefinition{
		{
			Name:        "exec",
			Description: execDescription,
			Parameters: []toolspec.Parameter{
				{Name: "command", Type: "string", Required: true, Description: "The shell command to run"},
				{Name: "working_dir", Type: "string", Description: "Directory to run in; relative paths start at your workspace. Default: where your previous command ended, else your workspace"},
				{
					Name: "timeout_seconds", Type: "integer", Minimum: &minOne, Maximum: &maxTimeout,
					Default:     int(timeout.Seconds()),
					Description: fmt.Sprintf("How long to wait for the command before returning (default %d, at most %d)", int(timeout.Seconds()), int(maxTimeout)),
				},
				{Name: "background", Type: "boolean", Description: "Start as a job and return at once"},
				{Name: "notify", Type: "boolean", Default: true, Description: "For a job: tell you when it ends"},
			},
			Hints:    &toolspec.ToolHints{Destructive: toolspec.Allow(true), OpenWorld: toolspec.Allow(true)},
			Handler:  t.execHandler,
			Category: "shell",
		},
		{
			Name:        "ctrl",
			Description: ctrlDescription,
			Parameters: []toolspec.Parameter{
				{Name: "action", Type: "string", Required: true, Enum: []any{"list", "wait", "input", "stop"}, Description: "What to do"},
				{Name: "id", Type: "string", Description: "Job id (wait, input, stop)"},
				{
					Name: "seconds", Type: "integer", Minimum: &minOne, Maximum: &maxTimeout, Default: DefaultWaitSeconds,
					Description: fmt.Sprintf("wait: most seconds to block (default %d)", DefaultWaitSeconds),
				},
				{Name: "until", Type: "string", Description: "wait: return when a new output line matches this regular expression"},
				{Name: "text", Type: "string", Description: "input: text for the job's standard input; a newline is added unless raw"},
				{Name: "raw", Type: "boolean", Description: "input: send text exactly as given"},
				{Name: "all", Type: "boolean", Description: "stop: stop all your running jobs"},
			},
			Hints:    &toolspec.ToolHints{Destructive: toolspec.Allow(true)},
			Handler:  t.ctrlHandler,
			Category: "shell",
		},
	}
}
