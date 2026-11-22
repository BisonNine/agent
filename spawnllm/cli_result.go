package spawnllm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// jsonCLICall describes one Chat call to a CLI that reads its prompt on stdin
// and answers with a single JSON document on stdout. The claude, cursor and
// antigravity providers share this path; they differ only in their arguments
// and in how stdout is parsed.
type jsonCLICall struct {
	// label names the CLI in returned errors ("<label> cli …") and in log
	// messages ("<label>-cli …").
	label     string
	command   string
	workspace string // working directory; empty keeps the caller's
	timeout   time.Duration
	args      []string
	prompt    string
	baseEnv   []string
	env       map[string]string
	model     string
	parse     func(stdout string) (*LLMResponse, error)
	// logging is the provider's logger and content setting.
	logging *logging
}

// runJSONCLI runs c.command with c.args, piping c.prompt on stdin, bounded by
// c.timeout when it is set, and turns the outcome into the provider response.
// The provider's environment is layered over c.baseEnv (see applyProviderEnv).
// A CLI that exits non-zero but still wrote a usable response is treated as a
// success.
func runJSONCLI(ctx context.Context, c *jsonCLICall) (*LLMResponse, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	cmd := newCLICommand(ctx, c.command, c.args...)
	if c.workspace != "" {
		cmd.Dir = c.workspace
	}
	cmd.Stdin = strings.NewReader(c.prompt)
	cmd.Env = applyProviderEnv(c.baseEnv, c.env)

	bytesSent := int64(len(c.prompt))
	run, runErr := runCLI(ctx, cmd, c.logging.log())
	return jsonCLIResult(ctx, c, bytesSent, run, runErr)
}

// jsonCLIResult maps a finished run of c to the provider response and error.
func jsonCLIResult(ctx context.Context, c *jsonCLICall, bytesSent int64, run *cliRun, runErr error) (*LLMResponse, error) {
	label, model := c.label, c.model
	stdout, stderr, elapsed := run.stdout, run.stderr, run.elapsed
	bytesReceived := stdout.Received()
	if errors.Is(runErr, errCLIOutputCapExceeded) {
		return cliErrorResponse(model, "output_cap", elapsed, bytesSent, bytesReceived),
			fmt.Errorf("%s cli: %w", label, runErr)
	}

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return cliErrorResponse(model, "timeout", elapsed, bytesSent, bytesReceived),
				fmt.Errorf("%s cli timed out after %s: %w", label, c.timeout, context.DeadlineExceeded)
		}
		if ctx.Err() == context.Canceled {
			return cliErrorResponse(model, "canceled", elapsed, bytesSent, bytesReceived), ctx.Err()
		}

		// Attempt to parse stdout before treating as error — the CLI may exit
		// non-zero but still write a valid JSON response to stdout. When it
		// did not, the usage and cost it reported are still kept for
		// accounting.
		var reported *DispatchStatus
		if stdoutStr := strings.TrimSpace(stdout.String()); stdoutStr != "" {
			resp, parseErr := c.parse(stdoutStr)
			if resp != nil {
				reported = resp.Status
			}
			if parseErr == nil && resp.Content != "" {
				exitCode := -1
				if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
					exitCode = exitErr.ExitCode()
				}
				if resp.Status != nil {
					resp.Status.BytesSent = bytesSent
					resp.Status.BytesReceived = bytesReceived
				}
				c.logging.log().WarnContext(ctx, label+"-cli exited non-zero but returned valid content",
					"exit_code", exitCode)
				return resp, nil
			}
		}

		exitCode := -1
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			exitCode = exitErr.ExitCode()
		}
		stderrStr := strings.TrimSpace(stderr.String())
		stdoutStr := strings.TrimSpace(stdout.String())
		attrs := []any{
			"agent_id", AgentIDFromContext(ctx),
			"exit_code", exitCode,
		}
		if c.logging.logContent.Load() {
			attrs = append(attrs, "stderr", stderrStr, "stdout", stdoutStr)
		}
		c.logging.log().ErrorContext(ctx, label+"-cli subprocess failed", attrs...)
		errResp := cliErrorResponse(model, "error", elapsed, bytesSent, bytesReceived)
		withReportedUsage(errResp.Status, reported)
		switch {
		case stderrStr != "" && stdoutStr != "":
			return errResp, fmt.Errorf("%s cli error: %w\nstderr: %s\nstdout: %s", label, runErr, stderrStr, stdoutStr)
		case stderrStr != "":
			return errResp, fmt.Errorf("%s cli error: %s", label, stderrStr)
		case stdoutStr != "":
			return errResp, fmt.Errorf("%s cli error: %w\noutput: %s", label, runErr, stdoutStr)
		default:
			return errResp, fmt.Errorf("%s cli error: %w", label, runErr)
		}
	}

	// Log non-empty stderr on successful exit.
	if stderrStr := strings.TrimSpace(stderr.String()); stderrStr != "" {
		c.logging.log().WarnContext(ctx, label+"-cli wrote to stderr on successful exit",
			"stderr", stderrStr)
	}

	resp, parseErr := c.parse(stdout.String())
	if parseErr != nil {
		// A CLI that reported its own failure (is_error) still reported
		// what the run used and cost; keep that for accounting.
		errResp := cliErrorResponse(model, "parse_error", elapsed, bytesSent, bytesReceived)
		if resp != nil {
			withReportedUsage(errResp.Status, resp.Status)
		}
		return errResp, parseErr
	}
	if resp.Status != nil {
		resp.Status.BytesSent = bytesSent
		resp.Status.BytesReceived = bytesReceived
	}
	if resp.Content == "" {
		var attrs []any
		if c.logging.logContent.Load() {
			attrs = append(attrs, "raw_stdout", strings.TrimSpace(stdout.String()))
		}
		c.logging.log().WarnContext(ctx, label+"-cli returned empty content", attrs...)
	}
	return resp, nil
}

// withReportedUsage copies onto the status of a failed run what the CLI
// reported about it: the token and tool call counts with their Reported
// flags, the cost and the model. The outcome, stop reason, measured duration and byte counts stay
// as they are.
func withReportedUsage(status, reported *DispatchStatus) {
	if status == nil || reported == nil {
		return
	}
	status.InputTokens = reported.InputTokens
	status.OutputTokens = reported.OutputTokens
	status.CacheReadTokens = reported.CacheReadTokens
	status.CacheCreationTokens = reported.CacheCreationTokens
	status.ToolCalls = reported.ToolCalls
	status.Reported = reported.Reported
	status.CostUSD = reported.CostUSD
	if reported.Model != "" {
		status.Model = reported.Model
	}
}
