/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"go.bison9.com/agent/toolbox/shell/proc"
)

const (
	timeLayout     = "2006-01-02 15:04:05"
	commandDisplay = 100 // runes of a command shown in a list
)

// output renders job output for the model: valid UTF-8, or a placeholder.
func output(data []byte) string {
	if len(data) == 0 {
		return "(no output)"
	}
	return strings.ToValidUTF8(string(data), "�")
}

// human renders a duration in whole seconds: "45s", "3m2s", "1h0m0s".
func human(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	return d.Round(time.Second).String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// shortCommand is the command's first line, cut to commandDisplay runes.
func shortCommand(cmd string) string {
	line, _, multi := strings.Cut(strings.TrimSpace(cmd), "\n")
	if utf8.RuneCountInString(line) > commandDisplay {
		line = string([]rune(line)[:commandDisplay])
		multi = true
	}
	if multi {
		line += "…"
	}
	return line
}

// stateText describes a job's state: "running", "exited with code 0",
// "ended by signal killed", "stopped (time limit)", "lost …".
func stateText(i proc.Info) string {
	switch i.State {
	case proc.StateRunning:
		return "running"
	case proc.StateExited:
		if i.Signal != "" {
			return "ended by signal " + i.Signal
		}
		return fmt.Sprintf("exited with code %d", i.ExitCode)
	case proc.StateStopped:
		if i.StopReason != "" && i.StopReason != proc.StopRequested {
			return "stopped (" + i.StopReason + ")"
		}
		return "stopped"
	case proc.StateLost:
		return "lost: the service restarted while it ran"
	default:
		return string(i.State)
	}
}

// finishedText is a foreground command that ended in time.
func finishedText(r proc.WaitResult, keptFile bool) string {
	var b strings.Builder
	b.WriteString(output(r.Output))
	if keptFile {
		fmt.Fprintf(&b, "\nFull output: %s", r.Info.OutputFile)
	}
	switch {
	case r.Info.Signal != "":
		fmt.Fprintf(&b, "\nEnded by signal: %s", r.Info.Signal)
	case r.Info.ExitCode != 0:
		fmt.Fprintf(&b, "\nExit code: %d", r.Info.ExitCode)
	}
	return b.String()
}

// keptText is a foreground command still running at its timeout, kept as a job.
func keptText(r proc.WaitResult, waited time.Duration, notify bool) string {
	var b strings.Builder
	if r.Reason == proc.ReasonCancelled {
		fmt.Fprintf(&b, "Interrupted; the command is still running as job %s.\n", r.Info.ID)
	} else {
		fmt.Fprintf(&b, "Still running after %s, so it was kept as job %s.\n", human(waited), r.Info.ID)
	}
	fmt.Fprintf(&b, "Output file: %s\n", r.Info.OutputFile)
	b.WriteString("Use " + ctrlTool + " to wait on it or stop it.")
	if notify {
		b.WriteString(" You will be told when it ends.")
	}
	b.WriteString("\nOutput so far:\n")
	b.WriteString(output(r.Output))
	return b.String()
}

// timedOutText is a foreground command stopped at its timeout, for a caller
// that cannot collect jobs.
func timedOutText(r proc.WaitResult, waited time.Duration) string {
	var b strings.Builder
	if r.Reason == proc.ReasonCancelled {
		b.WriteString("Command interrupted and stopped.")
	} else {
		fmt.Fprintf(&b, "Command timed out after %s and was stopped.", human(waited))
	}
	b.WriteString("\nOutput:\n")
	b.WriteString(output(r.Output))
	if r.Omitted > 0 {
		fmt.Fprintf(&b, "\nFull output: %s", r.Info.OutputFile)
	}
	return b.String()
}

func backgroundText(i proc.Info) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Started job %s in the background.\nOutput file: %s\n", i.ID, i.OutputFile)
	if i.Notify {
		b.WriteString("You will be told when it ends.")
	} else {
		b.WriteString("You will not be told when it ends; check on it with " + ctrlTool + ".")
	}
	return b.String()
}

func limitText(e *proc.LimitError) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Not started: %d %s already running, the most allowed. Stop one with %s first:",
		e.Max, plural(e.Max, "job is", "jobs are"), ctrlTool)
	for _, r := range e.Running {
		fmt.Fprintf(&b, "\n- %s: %s", r.ID, shortCommand(r.Command))
	}
	return b.String()
}

func listText(jobs []proc.Info) string {
	if len(jobs) == 0 {
		return "No jobs."
	}
	var b strings.Builder
	for i, j := range jobs {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s %s, started %s", j.ID, stateText(j), j.Started.Format(timeLayout))
		if !j.Ended.IsZero() {
			fmt.Fprintf(&b, ", ended %s", j.Ended.Format(timeLayout))
		}
		fmt.Fprintf(&b, ", %d bytes, %s\n  %s", j.OutputBytes, j.OutputFile, shortCommand(j.Command))
	}
	return b.String()
}

func waitText(r proc.WaitResult, waited time.Duration) string {
	var b strings.Builder
	id := r.Info.ID
	switch r.Reason {
	case proc.ReasonEnded:
		fmt.Fprintf(&b, "Job %s %s after %s.", id, stateText(r.Info), human(r.Info.Ended.Sub(r.Info.Started)))
	case proc.ReasonMatch:
		fmt.Fprintf(&b, "Job %s printed a line matching until: %s", id, r.Matched)
		if r.Info.Running() {
			b.WriteString("\nIt is still running.")
		}
	case proc.ReasonTimeout:
		fmt.Fprintf(&b, "Job %s is still running after %s.", id, human(waited))
	case proc.ReasonCancelled:
		fmt.Fprintf(&b, "Wait interrupted; job %s is %s.", id, stateText(r.Info))
	}
	fmt.Fprintf(&b, "\nOutput file: %s\n", r.Info.OutputFile)
	if len(r.Output) == 0 {
		b.WriteString("No new output.")
	} else {
		b.WriteString("New output:\n")
		b.WriteString(output(r.Output))
	}
	return b.String()
}

// noticeText is the end-of-job notice for the agent.
func noticeText(i proc.Info, last string) string {
	var b strings.Builder
	switch {
	case i.State == proc.StateLost:
		fmt.Fprintf(&b, "Shell job %s was lost: the service restarted while it ran. It was not restarted.", i.ID)
	case i.State == proc.StateStopped && i.StopReason == proc.StopShutdown:
		fmt.Fprintf(&b, "Shell job %s was stopped when the service restarted, and was not started again.", i.ID)
	default:
		fmt.Fprintf(&b, "Shell job %s ended: %s after %s.", i.ID, stateText(i), human(i.Ended.Sub(i.Started)))
	}
	fmt.Fprintf(&b, "\nCommand: %s\nOutput file: %s", shortCommand(i.Command), i.OutputFile)
	if last != "" {
		b.WriteString("\nLast output:\n")
		b.WriteString(last)
	}
	return b.String()
}

// lastLines returns up to n lines from the end of the file, reading at most
// maxBytes. A line cut by that bound is left out.
func lastLines(path string, n, maxBytes int) string {
	f, err := os.Open(path) //nolint:gosec // a job's own log file, named by the manager
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only descriptor
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start := max(st.Size()-int64(maxBytes), 0)
	data, err := io.ReadAll(io.NewSectionReader(f, start, st.Size()-start))
	if err != nil {
		return ""
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(strings.ToValidUTF8(string(data), "�"), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
