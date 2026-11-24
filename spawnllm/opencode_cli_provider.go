package spawnllm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/protocoltypes"
)

// OpenCodeCliProvider implements LLMProvider using the OpenCode CLI
// (`opencode run --format json`) as a subprocess. The prompt is piped on
// stdin; the answer is read from the CLI's JSON event stream.
type OpenCodeCliProvider struct {
	logging

	command   string
	workspace string
	timeout   time.Duration
	extraArgs []string
	env       map[string]string
	baseEnv   []string
}

// SetBaseEnv implements BaseEnvSetter.
func (p *OpenCodeCliProvider) SetBaseEnv(env []string) { p.baseEnv = env }

// NewOpenCodeCliProvider creates a new OpenCode CLI provider.
// When command is empty, it defaults to "opencode".
func NewOpenCodeCliProvider(command, workspace string, extraArgs []string, env map[string]string) *OpenCodeCliProvider {
	return NewOpenCodeCliProviderWithTimeout(command, workspace, 0, extraArgs, env)
}

// NewOpenCodeCliProviderWithTimeout creates a new OpenCode CLI provider with
// a request timeout. When command is empty, it defaults to "opencode".
func NewOpenCodeCliProviderWithTimeout(command, workspace string, timeout time.Duration, extraArgs []string, env map[string]string) *OpenCodeCliProvider {
	if command == "" {
		command = "opencode"
	}
	return &OpenCodeCliProvider{
		command:   command,
		workspace: workspace,
		timeout:   timeout,
		extraArgs: extraArgs,
		env:       env,
	}
}

// Chat implements LLMProvider.Chat by executing `opencode run`.
func (p *OpenCodeCliProvider) Chat(
	ctx context.Context, id accounting.Identity, messages []Message, tools []ToolDefinition, model string,
	options map[string]any,
) (*LLMResponse, error) {
	return chatCLI(ctx, id, "opencode-cli", model, func(ctx context.Context) (*LLMResponse, error) {
		return p.chat(ctx, messages, tools, model, options)
	})
}

// GetDefaultModel returns the default model identifier: the sentinel that
// lets OpenCode use its own default model.
func (p *OpenCodeCliProvider) GetDefaultModel() string { return "opencode-cli" }

// IsCLI implements CLIProvider.
func (p *OpenCodeCliProvider) IsCLI() bool { return true }

// Workspace returns the working directory the CLI runs in. Exposed so hosts can
// verify provider construction.
func (p *OpenCodeCliProvider) Workspace() string { return p.workspace }

// chat is Chat without the identity check and accounting.
func (p *OpenCodeCliProvider) chat(
	ctx context.Context, messages []Message, tools []ToolDefinition, model string, options map[string]any,
) (*LLMResponse, error) {
	// CLI providers run their own internal agentic loop and return one final
	// answer per invocation. The `tools` parameter is intentionally ignored:
	// the host exposes its tools to the CLI over MCP instead.
	_ = tools
	// The system prompt and the conversation go on stdin, as for the other
	// CLIs: nothing of them appears in the argument list. Permission flags
	// (--auto) come from the extra args, not baked in here.
	prompt := applyCLIOptions(p.log(), "opencode-cli", openCodeStdinPrompt(messages), options)

	args := OpenCodeCliBaseArgs()
	args = append(args, p.extraArgs...)
	if model != "" && model != "opencode-cli" {
		args = append(args, "--model", model)
	}

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	cmd := newCLICommand(ctx, p.command, args...)
	if p.workspace != "" {
		cmd.Dir = p.workspace
	}
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = applyProviderEnv(p.baseEnv, p.env)

	run, runErr := runCLI(ctx, cmd, p.log())
	return p.result(ctx, model, int64(len(prompt)), run, runErr)
}

// result maps a finished run to the provider response and error.
func (p *OpenCodeCliProvider) result(ctx context.Context, model string, bytesSent int64, run *cliRun, runErr error) (*LLMResponse, error) {
	stdout, stderr, elapsed := run.stdout, run.stderr, run.elapsed
	bytesReceived := stdout.Received()
	if errors.Is(runErr, errCLIOutputCapExceeded) {
		return cliErrorResponse(model, "output_cap", elapsed, bytesSent, bytesReceived),
			fmt.Errorf("opencode cli: %w", runErr)
	}
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return cliErrorResponse(model, "timeout", elapsed, bytesSent, bytesReceived),
				fmt.Errorf("opencode cli timed out after %s: %w", p.timeout, context.DeadlineExceeded)
		}
		if ctx.Err() == context.Canceled {
			return cliErrorResponse(model, "canceled", elapsed, bytesSent, bytesReceived), ctx.Err()
		}
	}

	resp, ev := p.parseOpenCodeEvents(stdout.String(), model)
	resp.Status.DurationMs = elapsed.Milliseconds()
	resp.Status.BytesSent = bytesSent
	resp.Status.BytesReceived = bytesReceived
	stderrStr := strings.TrimSpace(stderr.String())

	if ev != nil {
		// OpenCode ends a failed run with an error event (and exit code 1).
		resp.Content = ""
		attrs := []any{"agent_id", AgentIDFromContext(ctx), "error_type", ev.Type}
		if p.logContent.Load() {
			attrs = append(attrs, "stderr", stderrStr)
		}
		p.log().ErrorContext(ctx, "opencode-cli run failed", attrs...)
		return resp, errors.New("opencode cli error: " + ev.describe())
	}

	if runErr != nil {
		exitCode := -1
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			exitCode = exitErr.ExitCode()
		}
		if resp.Content != "" {
			p.log().WarnContext(ctx, "opencode-cli exited non-zero but returned valid content", "exit_code", exitCode)
			return resp, nil
		}
		attrs := []any{"agent_id", AgentIDFromContext(ctx), "exit_code", exitCode}
		if p.logContent.Load() {
			attrs = append(attrs, "stderr", stderrStr, "stdout", strings.TrimSpace(stdout.String()))
		}
		p.log().ErrorContext(ctx, "opencode-cli subprocess failed", attrs...)
		errResp := cliErrorResponse(model, "error", elapsed, bytesSent, bytesReceived)
		withReportedUsage(errResp.Status, resp.Status)
		if stderrStr != "" {
			return errResp, fmt.Errorf("opencode cli error: %w\nstderr: %s", runErr, stderrStr)
		}
		return errResp, fmt.Errorf("opencode cli error: %w", runErr)
	}

	if stderrStr != "" {
		p.log().WarnContext(ctx, "opencode-cli wrote to stderr on successful exit", "stderr", stderrStr)
	}
	if resp.Content == "" {
		var attrs []any
		if p.logContent.Load() {
			attrs = append(attrs, "raw_stdout", strings.TrimSpace(stdout.String()))
		}
		p.log().WarnContext(ctx, "opencode-cli returned empty content", attrs...)
	}
	return resp, nil
}

// openCodeEvent is one line of `opencode run --format json`. The CLI writes
// step_start and step_finish around each model step, text (a fragment of a
// text part: the new text since the part's previous event), tool_use (a tool
// call once it completed or failed), reasoning (only with --thinking),
// retry, background_wait and, when the run fails, error.
type openCodeEvent struct {
	Type      string         `json:"type"`
	SessionID string         `json:"sessionID"`
	Part      openCodePart   `json:"part"`
	Error     *openCodeError `json:"error"`
}

type openCodePart struct {
	ID        string          `json:"id"`
	MessageID string          `json:"messageID"`
	Text      string          `json:"text"`
	Tool      string          `json:"tool"`
	State     openCodeState   `json:"state"`
	Reason    string          `json:"reason"`
	Cost      *float64        `json:"cost"`
	Tokens    *openCodeTokens `json:"tokens"`
}

type openCodeState struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

// openCodeTokens is a step's token usage. input excludes the cached part of
// the prompt (cache.read and cache.write); reasoning is counted apart from
// output.
type openCodeTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
	reported protocoltypes.Reported
}

// UnmarshalJSON decodes the counts and notes which ones were present.
func (t *openCodeTokens) UnmarshalJSON(b []byte) error {
	type plain openCodeTokens
	if err := json.Unmarshal(b, (*plain)(t)); err != nil {
		return err
	}
	var nested struct {
		Cache json.RawMessage `json:"cache"`
	}
	_ = json.Unmarshal(b, &nested) //nolint:errcheck // b decoded above
	top := cliUsageKeys{input: "input", output: "output"}.reported(b)
	cache := cliUsageKeys{cacheRead: "read", cacheCreation: "write"}.reported(nested.Cache)
	t.reported = protocoltypes.Reported{
		InputTokens:         top.InputTokens,
		OutputTokens:        top.OutputTokens,
		CacheReadTokens:     cache.CacheReadTokens,
		CacheCreationTokens: cache.CacheCreationTokens,
	}
	return nil
}

// openCodeError is the error of an error or retry event.
type openCodeError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

// openCodeErrorHints name the failure class of OpenCode's error types in
// the words ClassifyError matches, for errors that carry no HTTP status.
var openCodeErrorHints = map[string]string{
	"provider.auth":                    "authentication failed",
	"provider.rate-limit":              "rate limit",
	"provider.quota":                   "quota exceeded",
	"provider.free-limit":              "usage limit",
	"provider.go-limit":                "usage limit",
	"provider.chatgpt-limit":           "usage limit",
	"provider.chatgpt-workspace-limit": "usage limit",
	"provider.budget-limit":            "usage limit",
	"provider.model-budget-limit":      "usage limit",
	"provider.timeout":                 "timed out",
	"provider.no-route":                "model unavailable",
}

// describe renders the error for the returned error text: the message, then
// OpenCode's type with its failure class and the HTTP status, when known.
func (e *openCodeError) describe() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "run failed"
	}
	var detail []string
	if e.Type != "" {
		t := e.Type
		if hint := openCodeErrorHints[e.Type]; hint != "" {
			t += ": " + hint
		}
		detail = append(detail, t)
	}
	if e.Status > 0 {
		detail = append(detail, fmt.Sprintf("status %d", e.Status))
	}
	if len(detail) == 0 {
		return msg
	}
	return msg + " (" + strings.Join(detail, ", ") + ")"
}

// openCodeDenials are the phrases of a tool call OpenCode refused: one a
// permission rule denies, or one a non-interactive run cannot ask about.
var openCodeDenials = []string{
	"specified a rule",
	"rejected permission",
	"cannot ask the user for permission",
}

// parseOpenCodeEvents turns the JSON event stream of a run into the response.
// Every step's tokens and cost are summed. The text of every text part is
// joined in order, one part per line. The returned error event, if any, is
// the run's failure. A line that is not a JSON event is skipped.
func (p *OpenCodeCliProvider) parseOpenCodeEvents(output, model string) (*LLMResponse, *openCodeError) {
	var (
		partOrder []string
		parts     = map[string]string{}
		sessionID string
		steps     int
		tok       openCodeTokens
		cost      float64
		hasCost   bool
		tools     int
		denied    []string
		stop      string
		failure   *openCodeError
		skipped   int
	)
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 256*1024), cliStdoutCapBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev openCodeEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			skipped++
			p.log().Debug("opencode-cli: skipping malformed event line", "line_preview", truncateString(line, 120))
			continue
		}
		if ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		switch ev.Type {
		case "text":
			key := ev.Part.ID
			if _, seen := parts[key]; !seen {
				partOrder = append(partOrder, key)
			}
			parts[key] += ev.Part.Text
		case "tool_use":
			tools++
			if ev.Part.State.Status == "error" && isOpenCodeDenial(ev.Part.State.Error) &&
				ev.Part.Tool != "" && !slices.Contains(denied, ev.Part.Tool) {
				denied = append(denied, ev.Part.Tool)
			}
		case "step_finish":
			steps++
			stop = ev.Part.Reason
			if t := ev.Part.Tokens; t != nil {
				tok.Input += t.Input
				tok.Output += t.Output
				tok.Reasoning += t.Reasoning
				tok.Cache.Read += t.Cache.Read
				tok.Cache.Write += t.Cache.Write
				tok.reported.InputTokens = tok.reported.InputTokens || t.reported.InputTokens
				tok.reported.OutputTokens = tok.reported.OutputTokens || t.reported.OutputTokens
				tok.reported.CacheReadTokens = tok.reported.CacheReadTokens || t.reported.CacheReadTokens
				tok.reported.CacheCreationTokens = tok.reported.CacheCreationTokens || t.reported.CacheCreationTokens
			}
			if ev.Part.Cost != nil {
				cost += *ev.Part.Cost
				hasCost = true
			}
		case "error":
			failure = ev.Error
			if failure == nil {
				failure = &openCodeError{}
			}
		case "retry":
			if ev.Error != nil {
				p.log().Warn("opencode-cli: retrying a model call", "error", ev.Error.describe())
			}
		}
	}
	if err := scanner.Err(); err != nil {
		p.log().Warn("opencode-cli: event scanner error", "error", err.Error())
	}

	texts := make([]string, 0, len(partOrder))
	for _, key := range partOrder {
		if s := strings.TrimSpace(parts[key]); s != "" {
			texts = append(texts, s)
		}
	}
	content := strings.Join(texts, "\n")

	status := &DispatchStatus{
		Success:             failure == nil,
		Model:               model,
		NumTurns:            steps,
		InputTokens:         tok.Input,
		OutputTokens:        tok.Output + tok.Reasoning,
		CacheReadTokens:     tok.Cache.Read,
		CacheCreationTokens: tok.Cache.Write,
		StopReason:          stop,
		DeniedTools:         denied,
		Reported:            tok.reported,
	}
	if failure != nil {
		status.StopReason = "error"
	}
	if hasCost {
		status.CostUSD = &cost
	}
	if steps > 0 {
		status.ToolCalls, status.Reported.ToolCalls = tools, true
	}

	resp := &LLMResponse{Content: content, FinishReason: "stop", Normal: true, Status: status}
	if tok.reported.InputTokens || tok.reported.OutputTokens {
		prompt := tok.Input + tok.Cache.Read + tok.Cache.Write
		resp.Usage = &UsageInfo{
			PromptTokens:     prompt,
			CompletionTokens: tok.Output + tok.Reasoning,
			TotalTokens:      prompt + tok.Output + tok.Reasoning,
		}
	}
	p.log().Debug("opencode-cli response",
		"session_id", sessionID,
		"steps", steps,
		"tool_calls", tools,
		"skipped_lines", skipped,
		"cost_usd", costForLog(status.CostUSD),
		"content_chars", len(content),
	)
	return resp, failure
}

// openCodeStdinPrompt is the stdin payload of a run: the system messages,
// then the conversation, in the format the Claude and Cursor CLIs are given.
func openCodeStdinPrompt(messages []Message) string {
	var system, parts []string
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			system = append(system, msg.Content)
		case "user":
			parts = append(parts, "User: "+escapeConvMarkers(msg.Content))
		case "assistant":
			parts = append(parts, "Assistant: "+escapeConvMarkers(msg.Content))
		case "tool":
			parts = append(parts, fmt.Sprintf("[Tool Result for %s]: %s", msg.ToolCallID, common.ToolResultText(msg)))
		}
	}
	conversation := strings.Join(parts, "\n")
	if len(parts) == 1 && strings.HasPrefix(parts[0], "User: ") {
		conversation = strings.TrimPrefix(parts[0], "User: ")
	}
	if len(system) == 0 {
		return conversation
	}
	return strings.Join(system, "\n\n") + "\n\n---\n\n" + conversation
}

// isOpenCodeDenial reports whether a failed tool call's error is a refusal
// by OpenCode's permission system.
func isOpenCodeDenial(msg string) bool {
	msg = strings.ToLower(msg)
	for _, d := range openCodeDenials {
		if strings.Contains(msg, d) {
			return true
		}
	}
	return false
}
