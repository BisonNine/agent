//go:build !windows

/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// codexHello is the conversation every codex Chat test sends; its prompt is
// the bare user text.
var codexHello = []Message{{Role: "user", Content: "hello"}}

func TestCodexCliProvider_Chat_Timeout(t *testing.T) {
	script := writeFakeCLI(t, "exec sleep 60\n")
	p := NewCodexCliProviderWithTimeout(script, "", 50*time.Millisecond, nil, nil)

	resp, err := p.Chat(context.Background(), testID, codexHello, nil, "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "codex cli timed out after 50ms") {
		t.Errorf("err = %q, want it to name the timeout", err)
	}
	if resp.Status.StopReason != "timeout" || resp.Status.BytesSent != int64(len("hello")) {
		t.Errorf("status = %+v, want a timeout with 5 bytes sent", resp.Status)
	}
}

func TestCodexCliProvider_Chat_SubprocessFailure(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		logContent bool
		wantErr    string
		wantExit   bool
	}{
		{
			name:       "stderr becomes the error",
			body:       "echo '  login required ' >&2\nexit 4\n",
			logContent: true,
			wantErr:    "codex cli error: login required",
		},
		{
			name:     "silent failure wraps the exit error",
			body:     "exit 4\n",
			wantErr:  "codex cli error: exit status 4",
			wantExit: true,
		},
		{
			name:    "jsonl without an answer does not hide the failure",
			body:    "echo '{\"type\":\"turn.completed\"}'\nexit 4\n",
			wantErr: "codex cli error: exit status 4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &launchtest.Logs{}
			p := NewCodexCliProvider(writeFakeCLI(t, tt.body), "", nil, nil)
			p.SetLogger(logs.Logger())
			p.SetLogContent(tt.logContent)

			resp, err := p.Chat(WithAgentID(context.Background(), "agent-alice"), testID, codexHello, nil, "gpt-5", nil)
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if exitErr, ok := errors.AsType[*exec.ExitError](err); tt.wantExit && (!ok || exitErr.ExitCode() != 4) {
				t.Errorf("err = %v, want it to wrap exit code 4", err)
			}
			if resp.Status.StopReason != "error" || resp.Status.Model != "gpt-5" || resp.Status.Success {
				t.Errorf("status = %+v, want a failed error status for gpt-5", resp.Status)
			}
			e, ok := logs.Find("codex-cli subprocess failed")
			if !ok {
				t.Fatalf("no failure record; got %+v", logs.Entries())
			}
			if e.Attrs["agent_id"] != "agent-alice" || e.Attrs["exit_code"] != int64(4) {
				t.Errorf("attrs = %v, want agent_id agent-alice and exit_code 4", e.Attrs)
			}
			if _, has := e.Attrs["stderr"]; has != tt.logContent {
				t.Errorf("stderr logged = %v, want %v", has, tt.logContent)
			}
		})
	}
}

func TestCodexCliProvider_Chat_SuccessfulExit(t *testing.T) {
	t.Run("turn failure is returned with its status", func(t *testing.T) {
		logs := &launchtest.Logs{}
		p := NewCodexCliProvider(writeFakeCLI(t, `echo 'warning: slow' >&2
echo '{"type":"turn.failed","error":{"message":"model overloaded"}}'
`), "", nil, nil)
		p.SetLogger(logs.Logger())

		resp, err := p.Chat(context.Background(), testID, codexHello, nil, "", nil)
		if err == nil || err.Error() != "codex cli: model overloaded" {
			t.Fatalf("err = %v, want the turn failure", err)
		}
		if resp.Status.StopReason != "error" || resp.Status.BytesSent != 5 || resp.Status.BytesReceived == 0 {
			t.Errorf("status = %+v, want an error status with byte counts", resp.Status)
		}
		if e, ok := logs.Find("codex-cli wrote to stderr on successful exit"); !ok || e.Attrs["stderr"] != "warning: slow" {
			t.Errorf("stderr warning = %+v (found %v), want stderr %q", e, ok, "warning: slow")
		}
	})

	t.Run("unreadable stream is a parse error", func(t *testing.T) {
		// One line longer than the 1 MiB scanner limit and no answer before it.
		p := NewCodexCliProvider(writeFakeCLI(t, "head -c 1100000 /dev/zero | tr '\\0' a\n"), "", nil, nil)

		resp, err := p.Chat(context.Background(), testID, codexHello, nil, "m", nil)
		if err == nil || !strings.Contains(err.Error(), "codex cli: scanner error") {
			t.Fatalf("err = %v, want a scanner error", err)
		}
		if resp.Status.StopReason != "parse_error" || resp.Status.BytesReceived != 1100000 {
			t.Errorf("status = %+v, want parse_error with 1100000 bytes received", resp.Status)
		}
	})

	t.Run("empty answer is returned with counts and logged", func(t *testing.T) {
		for _, allowed := range []bool{false, true} {
			logs := &launchtest.Logs{}
			// A long non-JSON line pushes the raw output past the 500-byte preview.
			noise := strings.Repeat("x", 600)
			p := NewCodexCliProvider(writeFakeCLI(t, fmt.Sprintf(`echo '%s'
echo ''
echo '{"type":"turn.completed","usage":{"input_tokens":9,"output_tokens":0}}'
`, noise)), "", nil, nil)
			p.SetLogger(logs.Logger())
			p.SetLogContent(allowed)

			resp, err := p.Chat(context.Background(), testID, codexHello, nil, "", nil)
			if err != nil || resp.Content != "" {
				t.Fatalf("resp = %+v, err = %v, want an empty answer", resp, err)
			}
			if resp.Status.BytesSent != 5 || resp.Status.BytesReceived == 0 || resp.Status.InputTokens != 9 {
				t.Errorf("status = %+v, want byte counts and 9 input tokens", resp.Status)
			}
			e, ok := logs.Find("codex-cli returned empty content")
			if !ok {
				t.Fatalf("no empty-content warning; got %+v", logs.Entries())
			}
			raw, has := e.Attrs["raw_stdout"]
			if has != allowed || (allowed && raw != noise[:500]) {
				t.Errorf("allowed=%v: raw_stdout present %v, value %q, want the first 500 bytes", allowed, has, raw)
			}
		}
	})
}

func TestCodexCliProvider_Chat_StdoutCapExceeded(t *testing.T) {
	overflow := cliStdoutCapBytes + 4096
	p := NewCodexCliProvider(writeFakeCLI(t, fmt.Sprintf("head -c %d /dev/zero\n", overflow)), "", nil, nil)

	resp, err := p.Chat(context.Background(), testID, codexHello, nil, "", nil)
	if !errors.Is(err, errCLIOutputCapExceeded) || !strings.HasPrefix(err.Error(), "codex cli: ") {
		t.Fatalf("err = %v, want a codex cli errCLIOutputCapExceeded", err)
	}
	if resp.Status.StopReason != "output_cap" || resp.Status.BytesReceived != int64(overflow) {
		t.Errorf("status = %+v, want output_cap with %d bytes received", resp.Status, overflow)
	}
}

func TestCLI_StderrCapExceededIsLoggedAndTruncated(t *testing.T) {
	logs := &launchtest.Logs{}
	script := writeFakeCLI(t, fmt.Sprintf(`head -c %d /dev/zero >&2
echo '{"type":"result","result":"done"}'
`, cliStderrCapBytes+10))
	p := NewClaudeCliProvider(script, "", nil, nil)
	p.SetLogger(logs.Logger())

	resp, err := p.Chat(context.Background(), testID, codexHello, nil, "", nil)
	if err != nil || resp.Content != "done" {
		t.Fatalf("resp = %+v, err = %v, want the answer despite noisy stderr", resp, err)
	}
	e, ok := logs.Find("cli stderr exceeded retention cap; output truncated")
	if !ok || e.Attrs["cap_bytes"] != int64(cliStderrCapBytes) {
		t.Errorf("warning = %+v (found %v), want cap_bytes %d", e, ok, cliStderrCapBytes)
	}
}
