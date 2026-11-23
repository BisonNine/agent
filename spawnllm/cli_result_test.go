package spawnllm

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// finishedRun builds the cliRun a CLI that wrote stdout and stderr would leave.
func finishedRun(stdout, stderr string) *cliRun {
	run := &cliRun{
		stdout:  newCappedBuffer(cliStdoutCapBytes),
		stderr:  newCappedBuffer(cliStderrCapBytes),
		elapsed: 1500 * time.Millisecond,
	}
	// Both fit their caps, so this is what Write would retain.
	run.stdout.buf, run.stdout.received = []byte(stdout), int64(len(stdout))
	run.stderr.buf, run.stderr.received = []byte(stderr), int64(len(stderr))
	return run
}

// exitError returns the *exec.ExitError of a process that exited with code.
func exitError(t *testing.T, code string) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	err := exec.Command("/bin/sh", "-c", "exit "+code).Run()
	if _, ok := errors.AsType[*exec.ExitError](err); !ok {
		t.Fatalf("exit %s: err = %v, want an *exec.ExitError", code, err)
	}
	return err
}

// reportedParse returns a parse func that answers with the given content and a
// status carrying the CLI's reported usage, failing when failWith is set.
func reportedParse(content string, failWith error) func(string) (*LLMResponse, error) {
	return func(string) (*LLMResponse, error) {
		status := &DispatchStatus{
			Success:      failWith == nil,
			Model:        "claude-opus-reported",
			InputTokens:  11,
			OutputTokens: 7,
			ToolCalls:    2,
			CostUSD:      new(0.25),
		}
		status.Reported.InputTokens, status.Reported.OutputTokens = true, true
		return &LLMResponse{Content: content, Status: status}, failWith
	}
}

func TestJSONCLIResult_NonZeroExitWithValidContentSucceeds(t *testing.T) {
	logs := &launchtest.Logs{}
	c := &jsonCLICall{label: "alice", model: "m", logging: &logging{}, parse: reportedParse("the answer", nil)}
	c.logging.SetLogger(logs.Logger())

	resp, err := jsonCLIResult(context.Background(), c, 42, finishedRun(`{"ok":true}`, ""), exitError(t, "3"))
	if err != nil {
		t.Fatalf("err = %v, want nil: a usable answer wins over the exit code", err)
	}
	if resp.Content != "the answer" {
		t.Errorf("Content = %q, want %q", resp.Content, "the answer")
	}
	if resp.Status.BytesSent != 42 || resp.Status.BytesReceived != int64(len(`{"ok":true}`)) {
		t.Errorf("bytes = %d sent / %d received, want 42 / %d",
			resp.Status.BytesSent, resp.Status.BytesReceived, len(`{"ok":true}`))
	}
	e, ok := logs.Find("alice-cli exited non-zero but returned valid content")
	if !ok {
		t.Fatalf("no warning; got %+v", logs.Entries())
	}
	if got := e.Attrs["exit_code"]; got != int64(3) {
		t.Errorf("exit_code = %v (%T), want 3", got, got)
	}
}

func TestJSONCLIResult_FailureErrors(t *testing.T) {
	runErr := errors.New("exit status 9")
	tests := []struct {
		name         string
		stdout       string
		stderr       string
		parse        func(string) (*LLMResponse, error)
		wantErr      []string
		wantReported bool
	}{
		{
			name:         "stderr and stdout",
			stdout:       `{"is_error":true}`,
			stderr:       "quota gone",
			parse:        reportedParse("", errors.New("cli reported failure")),
			wantErr:      []string{"alice cli error: exit status 9", "stderr: quota gone", `stdout: {"is_error":true}`},
			wantReported: true,
		},
		{
			name:         "stdout only",
			stdout:       `{"is_error":true}`,
			parse:        reportedParse("", errors.New("cli reported failure")),
			wantErr:      []string{"alice cli error: exit status 9", `output: {"is_error":true}`},
			wantReported: true,
		},
		{
			name:   "unparseable stdout keeps no usage",
			stdout: "not json",
			parse: func(string) (*LLMResponse, error) {
				return nil, errors.New("bad json")
			},
			wantErr: []string{"alice cli error: exit status 9", "output: not json"},
		},
		{
			name:    "stderr only",
			stderr:  "boom",
			parse:   reportedParse("unused", nil),
			wantErr: []string{"alice cli error: boom"},
		},
		{
			name:    "nothing written",
			parse:   reportedParse("unused", nil),
			wantErr: []string{"alice cli error: exit status 9"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &jsonCLICall{label: "alice", model: "requested", logging: &logging{}, parse: tt.parse}
			resp, err := jsonCLIResult(context.Background(), c, 5, finishedRun(tt.stdout, tt.stderr), runErr)
			if err == nil {
				t.Fatal("err = nil, want the CLI's failure")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
			if tt.stderr == "" && !errors.Is(err, runErr) {
				t.Errorf("err = %v, want it to wrap the run error", err)
			}
			st := resp.Status
			if st.Success || st.StopReason != "error" || st.BytesSent != 5 || st.DurationMs != 1500 {
				t.Errorf("status = %+v, want a failed error status with 5 bytes sent over 1500ms", st)
			}
			if tt.wantReported {
				if st.Model != "claude-opus-reported" || st.InputTokens != 11 || st.OutputTokens != 7 ||
					st.ToolCalls != 2 || st.CostUSD == nil || *st.CostUSD != 0.25 || !st.Reported.InputTokens {
					t.Errorf("status = %+v, want the usage and cost the CLI reported", st)
				}
			} else if st.Model != "requested" || st.InputTokens != 0 || st.CostUSD != nil {
				t.Errorf("status = %+v, want the requested model and no usage", st)
			}
		})
	}
}

func TestJSONCLIResult_FailureLogsContentOnlyWhenAllowed(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		logs := &launchtest.Logs{}
		c := &jsonCLICall{label: "bob", logging: &logging{}, parse: reportedParse("", errors.New("x"))}
		c.logging.SetLogger(logs.Logger())
		c.logging.SetLogContent(allowed)

		ctx := WithAgentID(context.Background(), "agent-bob")
		if _, err := jsonCLIResult(ctx, c, 0, finishedRun("out", "err"), exitError(t, "2")); err == nil {
			t.Fatalf("allowed=%v: err = nil, want the CLI's failure", allowed)
		}

		e, ok := logs.Find("bob-cli subprocess failed")
		if !ok {
			t.Fatalf("allowed=%v: no failure record; got %+v", allowed, logs.Entries())
		}
		if e.Attrs["agent_id"] != "agent-bob" || e.Attrs["exit_code"] != int64(2) {
			t.Errorf("allowed=%v: attrs = %v, want agent_id agent-bob and exit_code 2", allowed, e.Attrs)
		}
		_, hasStdout := e.Attrs["stdout"]
		_, hasStderr := e.Attrs["stderr"]
		if hasStdout != allowed || hasStderr != allowed {
			t.Errorf("allowed=%v: stdout logged %v, stderr logged %v", allowed, hasStdout, hasStderr)
		}
	}
}

func TestJSONCLIResult_SuccessfulExit(t *testing.T) {
	t.Run("stderr is logged as a warning", func(t *testing.T) {
		logs := &launchtest.Logs{}
		c := &jsonCLICall{label: "alice", logging: &logging{}, parse: reportedParse("hi", nil)}
		c.logging.SetLogger(logs.Logger())

		resp, err := jsonCLIResult(context.Background(), c, 3, finishedRun("{}", "  deprecation notice \n"), nil)
		if err != nil || resp.Content != "hi" {
			t.Fatalf("resp = %+v, err = %v, want content hi", resp, err)
		}
		e, ok := logs.Find("alice-cli wrote to stderr on successful exit")
		if !ok || e.Attrs["stderr"] != "deprecation notice" {
			t.Errorf("warning = %+v (found %v), want stderr %q", e, ok, "deprecation notice")
		}
	})

	t.Run("parse failure keeps reported usage", func(t *testing.T) {
		c := &jsonCLICall{
			label: "alice", model: "requested", logging: &logging{},
			parse: reportedParse("", errors.New("cli returned error: denied")),
		}
		resp, err := jsonCLIResult(context.Background(), c, 9, finishedRun(`{"is_error":true}`, ""), nil)
		if err == nil || err.Error() != "cli returned error: denied" {
			t.Fatalf("err = %v, want the parse error unchanged", err)
		}
		st := resp.Status
		if st.Success || st.StopReason != "parse_error" || st.BytesSent != 9 ||
			st.Model != "claude-opus-reported" || st.InputTokens != 11 {
			t.Errorf("status = %+v, want a parse_error status with the reported usage", st)
		}
	})

	t.Run("empty content is returned and logged", func(t *testing.T) {
		for _, allowed := range []bool{false, true} {
			logs := &launchtest.Logs{}
			c := &jsonCLICall{label: "alice", logging: &logging{}, parse: reportedParse("", nil)}
			c.logging.SetLogger(logs.Logger())
			c.logging.SetLogContent(allowed)

			resp, err := jsonCLIResult(context.Background(), c, 4, finishedRun(" {} \n", ""), nil)
			if err != nil || resp.Content != "" {
				t.Fatalf("resp = %+v, err = %v, want an empty answer without error", resp, err)
			}
			if resp.Status.BytesSent != 4 || resp.Status.BytesReceived != 5 {
				t.Errorf("bytes = %d/%d, want 4/5", resp.Status.BytesSent, resp.Status.BytesReceived)
			}
			e, ok := logs.Find("alice-cli returned empty content")
			if !ok {
				t.Fatalf("no empty-content warning; got %+v", logs.Entries())
			}
			if raw, has := e.Attrs["raw_stdout"]; has != allowed || (allowed && raw != "{}") {
				t.Errorf("allowed=%v: raw_stdout = %v (present %v)", allowed, raw, has)
			}
		}
	})
}

func TestWithReportedUsage(t *testing.T) {
	t.Run("nil is ignored", func(t *testing.T) {
		withReportedUsage(nil, &DispatchStatus{InputTokens: 1})
		st := &DispatchStatus{Model: "m"}
		withReportedUsage(st, nil)
		if st.Model != "m" || st.InputTokens != 0 {
			t.Errorf("status = %+v, want unchanged", st)
		}
	})
	t.Run("empty reported model keeps the requested one", func(t *testing.T) {
		st := &DispatchStatus{Model: "requested", StopReason: "error", DurationMs: 8}
		withReportedUsage(st, &DispatchStatus{InputTokens: 3, CacheReadTokens: 4, CacheCreationTokens: 5, StopReason: "x"})
		if st.Model != "requested" || st.InputTokens != 3 || st.CacheReadTokens != 4 ||
			st.CacheCreationTokens != 5 || st.StopReason != "error" || st.DurationMs != 8 {
			t.Errorf("status = %+v, want counts copied and model, outcome and duration kept", st)
		}
	})
}

func TestJSONCLIResult_CanceledIgnoresOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &jsonCLICall{label: "alice", model: "m", logging: &logging{}, parse: reportedParse("partial", nil)}

	resp, err := jsonCLIResult(ctx, c, 6, finishedRun(`{"result":"partial"}`, ""), errors.New("signal: terminated"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if resp.Content != "" || resp.Status.StopReason != "canceled" || resp.Status.BytesSent != 6 {
		t.Errorf("resp = %+v status %+v, want a canceled status and no content", resp, resp.Status)
	}
}
