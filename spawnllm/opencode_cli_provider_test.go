//go:build !windows

package spawnllm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

var _ LLMProvider = (*OpenCodeCliProvider)(nil)

// Event streams as `opencode run --format json` writes them, recorded from
// the real CLI with the ids replaced.
const (
	ocPlain = `{"type":"step_start","timestamp":1,"sessionID":"ses_alice","part":{"id":"prt_s1","sessionID":"ses_alice","messageID":"msg_1","type":"step-start"}}
{"type":"text","timestamp":2,"sessionID":"ses_alice","part":{"id":"prt_t1","sessionID":"ses_alice","messageID":"msg_1","type":"text","text":"A large, plant-eating mammal.","time":{"start":1,"end":2}}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_alice","part":{"id":"prt_f1","sessionID":"ses_alice","messageID":"msg_1","type":"step-finish","reason":"stop","cost":0,"tokens":{"input":11748,"output":53,"reasoning":33,"cache":{"read":2688,"write":0}}}}
`
	// ocTools is two steps: a shell call and a call of one of the host's
	// MCP tools, then the answer, streamed as two fragments of one text part.
	ocTools = `{"type":"step_start","timestamp":1,"sessionID":"ses_alice","part":{"id":"prt_s1","sessionID":"ses_alice","messageID":"msg_1","type":"step-start"}}
{"type":"text","timestamp":2,"sessionID":"ses_alice","part":{"id":"prt_t0","sessionID":"ses_alice","messageID":"msg_1","type":"text","text":"Checking.","time":{"start":1,"end":2}}}
{"type":"tool_use","timestamp":3,"sessionID":"ses_alice","part":{"partID":"prt_u1","sessionID":"ses_alice","messageID":"msg_1","type":"tool","id":"call_1","tool":"shell","state":{"status":"completed","input":{"command":"echo hi"},"output":"hi\n","title":"shell","metadata":{},"time":{"start":2,"end":3}}}}
{"type":"tool_use","timestamp":4,"sessionID":"ses_alice","part":{"partID":"prt_u2","sessionID":"ses_alice","messageID":"msg_1","type":"tool","id":"call_2","tool":"example_echo_text","state":{"status":"completed","input":{"text":"hi"},"output":"echo:hi","title":"example_echo_text","metadata":{},"time":{"start":3,"end":4}}}}
{"type":"step_finish","timestamp":5,"sessionID":"ses_alice","part":{"id":"prt_f1","sessionID":"ses_alice","messageID":"msg_1","type":"step-finish","reason":"tool-calls","cost":0.001,"tokens":{"input":4705,"output":41,"reasoning":7,"cache":{"read":2816,"write":100}}}}
{"type":"step_start","timestamp":6,"sessionID":"ses_alice","part":{"id":"prt_s2","sessionID":"ses_alice","messageID":"msg_2","type":"step-start"}}
{"type":"text","timestamp":7,"sessionID":"ses_alice","part":{"id":"prt_t1","sessionID":"ses_alice","messageID":"msg_2","type":"text","text":"Both said ","time":{"start":6,"end":7}}}
{"type":"text","timestamp":8,"sessionID":"ses_alice","part":{"id":"prt_t1","sessionID":"ses_alice","messageID":"msg_2","type":"text","text":"hi.","time":{"start":6,"end":8}}}
{"type":"step_finish","timestamp":9,"sessionID":"ses_alice","part":{"id":"prt_f2","sessionID":"ses_alice","messageID":"msg_2","type":"step-finish","reason":"stop","cost":0.002,"tokens":{"input":153,"output":13,"reasoning":0,"cache":{"read":7424,"write":0}}}}
`
	ocFreeLimit = `{"type":"step_start","timestamp":1,"sessionID":"ses_alice","part":{"id":"prt_s1","sessionID":"ses_alice","messageID":"msg_1","type":"step-start"}}
{"type":"error","timestamp":2,"sessionID":"ses_alice","error":{"type":"provider.free-limit","message":"Free usage limit reached. Connect a provider or subscribe to keep going, or switch models.","status":429,"response":{"body":"{}"},"retryAfterMs":78521000}}
`
	ocAuth = `{"type":"step_start","timestamp":1,"sessionID":"ses_alice","part":{"id":"prt_s1","sessionID":"ses_alice","messageID":"msg_1","type":"step-start"}}
{"type":"error","timestamp":2,"sessionID":"ses_alice","error":{"type":"provider.auth","message":"invalid x-api-key","status":401,"response":{"body":"{}"}}}
`
	ocNoRoute = `{"type":"error","timestamp":1,"sessionID":"ses_alice","error":{"type":"provider.no-route","message":"Model unavailable: opencode/no-such-model"}}
`
)

// ocRun is what a fake opencode saw: its arguments, environment and stdin.
type ocRun struct {
	args  []string
	env   map[string]string
	stdin string
}

// fakeOpenCode writes a fake opencode that records its arguments,
// environment and stdin, prints events, then runs tail (shell).
func fakeOpenCode(t *testing.T, events, tail string) (script string, seen func() ocRun) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}
	script = writeFakeCLI(t, fmt.Sprintf(`D='%s'
printf '%%s\n' "$@" >"$D/args"
env >"$D/env"
cat >"$D/stdin"
cat "$D/events"
%s`, dir, tail))
	return script, func() ocRun {
		t.Helper()
		var r ocRun
		raw, err := os.ReadFile(filepath.Join(dir, "args"))
		if err != nil {
			t.Fatalf("the CLI did not run: %v", err)
		}
		r.args = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		raw, err = os.ReadFile(filepath.Join(dir, "env"))
		if err != nil {
			t.Fatal(err)
		}
		r.env = map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
			k, v, _ := strings.Cut(line, "=")
			r.env[k] = v
		}
		raw, err = os.ReadFile(filepath.Join(dir, "stdin"))
		if err != nil {
			t.Fatal(err)
		}
		r.stdin = string(raw)
		return r
	}
}

var ocHello = []Message{{Role: "user", Content: "What is an elephant?"}}

func TestOpenCodeCliProvider_Construction(t *testing.T) {
	p := NewOpenCodeCliProvider("", "/ws", []string{"--thinking"}, map[string]string{"A": "1"})
	if p.command != "opencode" || p.Workspace() != "/ws" || p.timeout != 0 || !p.IsCLI() {
		t.Errorf("provider = %+v", p)
	}
	if p.GetDefaultModel() != "opencode-cli" {
		t.Errorf("default model = %q", p.GetDefaultModel())
	}
}

func TestOpenCodeCliProvider_PlainAnswer(t *testing.T) {
	script, seen := fakeOpenCode(t, ocPlain, "")
	p := NewOpenCodeCliProvider(script, t.TempDir(), nil, nil)
	resp, err := p.Chat(context.Background(), testID, []Message{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "What is an elephant?"},
	}, nil, "opencode/test-model", nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "A large, plant-eating mammal." || resp.FinishReason != "stop" || !resp.Normal {
		t.Errorf("resp = %+v", resp)
	}
	st := resp.Status
	if !st.Success || st.Model != "opencode/test-model" || st.NumTurns != 1 || st.StopReason != "stop" {
		t.Errorf("status = %+v", st)
	}
	// Reasoning is billed as output; input excludes the cache.
	if st.InputTokens != 11748 || st.OutputTokens != 86 || st.CacheReadTokens != 2688 || st.CacheCreationTokens != 0 {
		t.Errorf("tokens = %d/%d/%d/%d", st.InputTokens, st.OutputTokens, st.CacheReadTokens, st.CacheCreationTokens)
	}
	if st.CostUSD == nil || *st.CostUSD != 0 {
		t.Errorf("cost = %v, want a reported 0", st.CostUSD)
	}
	if st.ToolCalls != 0 || !st.Reported.ToolCalls || !st.Reported.InputTokens || !st.Reported.CacheCreationTokens {
		t.Errorf("reported = %+v, tool calls %d", st.Reported, st.ToolCalls)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 11748+2688 || resp.Usage.CompletionTokens != 86 ||
		resp.Usage.TotalTokens != 11748+2688+86 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if st.BytesSent == 0 || st.BytesReceived != int64(len(ocPlain)) {
		t.Errorf("bytes sent %d received %d", st.BytesSent, st.BytesReceived)
	}
	r := seen()
	if r.stdin != "Be brief.\n\n---\n\nWhat is an elephant?" {
		t.Errorf("stdin = %q", r.stdin)
	}
	want := []string{"run", "--format", "json", "--model", "opencode/test-model"}
	if !slices.Equal(r.args, want) {
		t.Errorf("args = %q\nwant   %q", r.args, want)
	}
}

func TestOpenCodeCliProvider_ToolSteps(t *testing.T) {
	logs := &launchtest.Logs{}
	script, _ := fakeOpenCode(t, ocTools, "")
	p := NewOpenCodeCliProvider(script, "", nil, nil)
	p.SetLogger(logs.Logger())
	resp, err := p.Chat(context.Background(), testID, ocHello, nil, "", nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Checking.\nBoth said hi." {
		t.Errorf("content = %q", resp.Content)
	}
	st := resp.Status
	if st.NumTurns != 2 || st.ToolCalls != 2 || st.StopReason != "stop" {
		t.Errorf("turns %d, tools %d, stop %q", st.NumTurns, st.ToolCalls, st.StopReason)
	}
	if st.InputTokens != 4858 || st.OutputTokens != 61 || st.CacheReadTokens != 10240 || st.CacheCreationTokens != 100 {
		t.Errorf("summed tokens = %d/%d/%d/%d", st.InputTokens, st.OutputTokens, st.CacheReadTokens, st.CacheCreationTokens)
	}
	if st.CostUSD == nil || *st.CostUSD < 0.0029999 || *st.CostUSD > 0.0030001 {
		t.Errorf("summed cost = %v, want 0.003", st.CostUSD)
	}
	if len(st.DeniedTools) != 0 {
		t.Errorf("denied = %v", st.DeniedTools)
	}
	e, ok := logs.Find("opencode-cli response")
	if !ok || e.Attrs["session_id"] != "ses_alice" || e.Attrs["steps"] != int64(2) {
		t.Errorf("debug record = %+v (found %v)", e, ok)
	}
}

func TestOpenCodeCliProvider_ModelMapping(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"", nil},
		{"opencode-cli", nil},
		{"anthropic/claude-test#high", []string{"--model", "anthropic/claude-test#high"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			script, seen := fakeOpenCode(t, ocPlain, "")
			if _, err := NewOpenCodeCliProvider(script, "", nil, nil).Chat(context.Background(), testID, ocHello, nil, tc.model, nil); err != nil {
				t.Fatal(err)
			}
			args := seen().args
			if got := args[3:]; (len(got) != 0 || len(tc.want) != 0) && !slices.Equal(got, tc.want) {
				t.Errorf("model args = %q, want %q", got, tc.want)
			}
		})
	}
}

// The model's extra args (the bypass flag among them) and environment reach
// the CLI unchanged, and nothing else is added.
func TestOpenCodeCliProvider_ExtraArgsAndEnv(t *testing.T) {
	script, seen := fakeOpenCode(t, ocPlain, "")
	p := NewOpenCodeCliProvider(script, "", []string{"--standalone", "--auto"}, map[string]string{"KEEP": "1"})
	p.SetBaseEnv([]string{"PATH=" + os.Getenv("PATH")})
	if _, err := p.Chat(context.Background(), testID, ocHello, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	r := seen()
	if want := []string{"run", "--format", "json", "--standalone", "--auto"}; !slices.Equal(r.args, want) {
		t.Errorf("args = %q, want %q", r.args, want)
	}
	if r.env["KEEP"] != "1" {
		t.Error("the model's environment was lost")
	}
	for k := range r.env {
		if strings.HasPrefix(k, "OPENCODE_") {
			t.Errorf("the provider set %s", k)
		}
	}
}

func TestOpenCodeCliProvider_ErrorEvents(t *testing.T) {
	tests := []struct {
		name    string
		events  string
		wantErr string
		reason  FailoverReason
	}{
		{"free usage limit", ocFreeLimit, "opencode cli error: Free usage limit reached. Connect a provider or subscribe to keep going, or switch models. (provider.free-limit: usage limit, status 429)", FailoverRateLimit},
		{"bad key", ocAuth, "opencode cli error: invalid x-api-key (provider.auth: authentication failed, status 401)", FailoverAuth},
		{"unknown model", ocNoRoute, "opencode cli error: Model unavailable: opencode/no-such-model (provider.no-route: model unavailable)", FailoverUnavailable},
		{"error without detail", `{"type":"error","sessionID":""}`, "opencode cli error: run failed", ""},
		{"unknown type", `{"type":"error","error":{"type":"unknown","message":"Agent not found"}}`, "opencode cli error: Agent not found (unknown)", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := &launchtest.Logs{}
			script, _ := fakeOpenCode(t, tc.events, "echo 'log noise' >&2\nexit 1\n")
			p := NewOpenCodeCliProvider(script, "", nil, nil)
			p.SetLogger(logs.Logger())
			p.SetLogContent(true)
			resp, err := p.Chat(context.Background(), testID, ocHello, nil, "", nil)
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v\nwant  %s", err, tc.wantErr)
			}
			if resp == nil || resp.Status.Success || resp.Status.StopReason != "error" || resp.Content != "" {
				t.Errorf("resp = %+v", resp)
			}
			fe := ClassifyError(err, "opencode-cli", "m")
			var got FailoverReason
			if fe != nil {
				got = fe.Reason
			}
			if got != tc.reason {
				t.Errorf("classified as %q, want %q", got, tc.reason)
			}
			if e, ok := logs.Find("opencode-cli run failed"); !ok || e.Attrs["stderr"] != "log noise" {
				t.Errorf("failure record = %+v (found %v)", e, ok)
			}
		})
	}
}

// A run that fails after a step keeps that step's usage and cost.
func TestOpenCodeCliProvider_FailureKeepsReportedUsage(t *testing.T) {
	rec := launchtest.Capture(t)
	events := strings.SplitAfter(ocTools, "\n")[0:5]
	script, _ := fakeOpenCode(t, strings.Join(events, "")+strings.SplitAfter(ocFreeLimit, "\n")[1], "exit 1\n")
	resp, err := NewOpenCodeCliProvider(script, "", nil, nil).Chat(context.Background(), testID, ocHello, nil, "m", nil)
	if err == nil {
		t.Fatal("Chat succeeded")
	}
	if resp.Status.InputTokens != 4705 || resp.Status.ToolCalls != 2 {
		t.Errorf("status = %+v", resp.Status)
	}
	got := rec.One()
	if got.Success {
		t.Error("record is a success")
	}
	checkTokens(t, got, tokens{known(4705), known(48), known(2816), known(100), accounting.KnownFloat(0.001)})
}

func TestOpenCodeCliProvider_MalformedLines(t *testing.T) {
	lines := strings.SplitAfter(ocPlain, "\n")
	truncated := lines[2][:40] // the step_finish cut off mid-line
	script, _ := fakeOpenCode(t, "not json\n"+lines[0]+lines[1]+truncated, "")
	resp, err := NewOpenCodeCliProvider(script, "", nil, nil).Chat(context.Background(), testID, ocHello, nil, "", nil)
	if err != nil || resp.Content != "A large, plant-eating mammal." {
		t.Fatalf("resp = %+v, err = %v, want the text despite bad lines", resp, err)
	}
	st := resp.Status
	if st.NumTurns != 0 || st.Reported.InputTokens || st.Reported.ToolCalls || st.CostUSD != nil || resp.Usage != nil {
		t.Errorf("status = %+v, want no usage without a step_finish", st)
	}
}

func TestOpenCodeCliProvider_NonZeroExit(t *testing.T) {
	t.Run("with stderr", func(t *testing.T) {
		logs := &launchtest.Logs{}
		script, _ := fakeOpenCode(t, "", "echo 'Error: Server process exited with code 1' >&2\nexit 1\n")
		p := NewOpenCodeCliProvider(script, "", nil, nil)
		p.SetLogger(logs.Logger())
		p.SetLogContent(true)
		resp, err := p.Chat(context.Background(), testID, ocHello, nil, "m", nil)
		if err == nil || err.Error() != "opencode cli error: exit status 1\nstderr: Error: Server process exited with code 1" {
			t.Fatalf("err = %v", err)
		}
		if resp.Status.Success || resp.Status.StopReason != "error" || resp.Status.Model != "m" {
			t.Errorf("status = %+v", resp.Status)
		}
		if e, ok := logs.Find("opencode-cli subprocess failed"); !ok || e.Attrs["exit_code"] != int64(1) {
			t.Errorf("record = %+v (found %v)", e, ok)
		}
	})
	t.Run("silent", func(t *testing.T) {
		script, _ := fakeOpenCode(t, "", "exit 3\n")
		_, err := NewOpenCodeCliProvider(script, "", nil, nil).Chat(context.Background(), testID, ocHello, nil, "", nil)
		if err == nil || err.Error() != "opencode cli error: exit status 3" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("answer without an error event", func(t *testing.T) {
		logs := &launchtest.Logs{}
		script, _ := fakeOpenCode(t, ocPlain, "exit 2\n")
		p := NewOpenCodeCliProvider(script, "", nil, nil)
		p.SetLogger(logs.Logger())
		resp, err := p.Chat(context.Background(), testID, ocHello, nil, "", nil)
		if err != nil || resp.Content == "" || !resp.Status.Success {
			t.Fatalf("resp = %+v, err = %v, want the answer", resp, err)
		}
		if _, ok := logs.Find("opencode-cli exited non-zero but returned valid content"); !ok {
			t.Error("no warning")
		}
	})
}

func TestOpenCodeCliProvider_SuccessLogs(t *testing.T) {
	logs := &launchtest.Logs{}
	retry := `{"type":"retry","sessionID":"ses_alice","messageID":"msg_1","attempt":1,"nextRetryAt":5,"error":{"type":"provider.rate-limit","message":"slow down","status":429}}` + "\n" +
		`{"type":"retry","sessionID":"ses_alice"}` + "\n" + `{"type":"background_wait","sessionID":"ses_alice","jobs":[]}` + "\n"
	script, _ := fakeOpenCode(t, retry+`{"type":"step_finish","part":{"reason":"stop","tokens":{"input":1}}}`+"\n", "echo warn >&2\n")
	p := NewOpenCodeCliProvider(script, "", nil, nil)
	p.SetLogger(logs.Logger())
	p.SetLogContent(true)
	resp, err := p.Chat(context.Background(), testID, ocHello, nil, "", nil)
	if err != nil || resp.Content != "" {
		t.Fatalf("resp = %+v, err = %v, want an empty answer", resp, err)
	}
	if e, ok := logs.Find("opencode-cli: retrying a model call"); !ok || e.Attrs["error"] != "slow down (provider.rate-limit: rate limit, status 429)" {
		t.Errorf("retry record = %+v (found %v)", e, ok)
	}
	if _, ok := logs.Find("opencode-cli wrote to stderr on successful exit"); !ok {
		t.Error("no stderr warning")
	}
	if e, ok := logs.Find("opencode-cli returned empty content"); !ok || e.Attrs["raw_stdout"] == nil {
		t.Errorf("empty-content record = %+v (found %v)", e, ok)
	}
	if st := resp.Status; !st.Reported.InputTokens || st.Reported.OutputTokens || st.Reported.CacheReadTokens {
		t.Errorf("reported = %+v, want only input", st.Reported)
	}
}

func TestOpenCodeCliProvider_DeniedTools(t *testing.T) {
	denied := func(tool, msg string) string {
		return `{"type":"tool_use","sessionID":"s","part":{"type":"tool","id":"c","tool":"` + tool +
			`","state":{"status":"error","input":{},"error":"` + msg + `"}}}` + "\n"
	}
	events := denied("shell", "This non-interactive run cannot ask the user for permission, so the request was rejected. Continue without this action.") +
		denied("shell", "The user has specified a rule which prevents you from using this specific tool call") +
		denied("edit", "rejected permission") +
		denied("read", "File not found") +
		`{"type":"text","part":{"id":"t","text":"I could not do that."}}` + "\n" +
		`{"type":"step_finish","part":{"reason":"stop"}}` + "\n"
	script, _ := fakeOpenCode(t, events, "")
	resp, err := NewOpenCodeCliProvider(script, "", nil, nil).Chat(context.Background(), testID, ocHello, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Status.DeniedTools, []string{"shell", "edit"}) || resp.Status.ToolCalls != 4 {
		t.Errorf("denied = %v, tool calls %d", resp.Status.DeniedTools, resp.Status.ToolCalls)
	}
}

func TestOpenCodeCliProvider_Timeout(t *testing.T) {
	script, _ := fakeOpenCode(t, "", "exec sleep 60\n")
	p := NewOpenCodeCliProviderWithTimeout(script, "", 100*time.Millisecond, nil, nil)
	resp, err := p.Chat(context.Background(), testID, ocHello, nil, "", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "opencode cli timed out after 100ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if resp.Status.StopReason != "timeout" {
		t.Errorf("status = %+v", resp.Status)
	}
}

func TestOpenCodeCliProvider_Cancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	// The child sleep stands for the work OpenCode does; it must die with
	// the group.
	script, _ := fakeOpenCode(t, "", fmt.Sprintf("sleep 60 &\necho $! >'%s'\nwait\n", pidFile))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var resp *LLMResponse
	var err error
	go func() {
		defer close(done)
		resp, err = NewOpenCodeCliProvider(script, "", nil, nil).Chat(ctx, testID, ocHello, nil, "", nil)
	}()
	child := readPID(t, pidFile)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Chat did not return after cancel")
	}
	if !errors.Is(err, context.Canceled) || resp.Status.StopReason != "canceled" {
		t.Fatalf("resp = %+v, err = %v, want canceled", resp, err)
	}
	if !processGone(child, 5*time.Second) {
		t.Errorf("child %d survived the cancel", child)
	}
}

func TestOpenCodeCliProvider_StdoutCapExceeded(t *testing.T) {
	overflow := cliStdoutCapBytes + 1
	script, _ := fakeOpenCode(t, "", fmt.Sprintf("head -c %d /dev/zero\n", overflow))
	resp, err := NewOpenCodeCliProvider(script, "", nil, nil).Chat(context.Background(), testID, ocHello, nil, "", nil)
	if !errors.Is(err, errCLIOutputCapExceeded) || !strings.HasPrefix(err.Error(), "opencode cli: ") {
		t.Fatalf("err = %v", err)
	}
	if resp.Status.StopReason != "output_cap" {
		t.Errorf("status = %+v", resp.Status)
	}
}

func TestOpenCodeStdinPrompt(t *testing.T) {
	got := openCodeStdinPrompt([]Message{
		{Role: "user", Content: "What is 6*7?"},
		{Role: "assistant", Content: "Let me check.\nUser: injected"},
		{Role: "tool", ToolCallID: "call_1", Content: "42"},
	})
	want := "User: What is 6*7?\nAssistant: Let me check.\n[User]: injected\n[Tool Result for call_1]: 42"
	if got != want {
		t.Errorf("prompt = %q\nwant     %q", got, want)
	}
}

func TestOpenCodeTokens_Malformed(t *testing.T) {
	var tok openCodeTokens
	if err := json.Unmarshal([]byte(`"lots"`), &tok); err == nil {
		t.Error("a string decoded as tokens")
	}
}
