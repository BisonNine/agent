package spawnllm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// testID is the accounting identity the tests launch calls with.
var testID = accounting.Identity{Agent: "alice"}

// markerCLI writes a CLI script that creates a marker file when it runs and
// prints stdout. It returns the script and the marker's path.
func markerCLI(t *testing.T, stdout string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock CLI scripts not supported on Windows")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "stdout.txt")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "cli")
	body := fmt.Sprintf("#!/bin/sh\ntouch '%s'\ncat '%s'\n", marker, out)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, marker
}

func ran(marker string) bool {
	_, err := os.Stat(marker)
	return err == nil
}

// tokens is the token and cost part of a record a test expects.
type tokens struct {
	input, output, cacheRead, cacheCreation accounting.Int
	cost                                    accounting.Float
}

func checkTokens(t *testing.T, rec accounting.Record, want tokens) {
	t.Helper()
	got := tokens{rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens, rec.CacheCreationTokens, rec.CostUSD}
	if got != want {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}
}

func known(v int64) accounting.Int { return accounting.KnownInt(v) }

var cliCases = []struct {
	provider string
	build    func(script string) LLMProvider
	stdout   string
	model    string // model requested
	// wantModel is the model recorded: the one the CLI reports serving.
	wantModel string
	want      tokens
	// wantTools is the tool call count recorded; unknown for a CLI whose
	// output does not report its tool use.
	wantTools accounting.Int
}{
	{
		provider: "claude-cli",
		build:    func(s string) LLMProvider { return NewClaudeCliProvider(s, "", nil, nil) },
		stdout: `{"type":"result","subtype":"success","is_error":false,"result":"hi","total_cost_usd":0.005,` +
			`"num_turns":1,"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":100,` +
			`"cache_read_input_tokens":0},"modelUsage":{"claude-test-1":{"inputTokens":10,"outputTokens":5}}}`,
		model:     "sonnet",
		wantModel: "claude-test-1",
		// A reported 0 is a known 0; the cost is the CLI's own.
		want: tokens{known(10), known(5), known(0), known(100), accounting.KnownFloat(0.005)},
	},
	{
		provider: "codex-cli",
		build:    func(s string) LLMProvider { return NewCodexCliProvider(s, "", nil, nil) },
		stdout: `{"type":"item.completed","item":{"id":"i0","type":"command_execution","command":"ls","status":"completed"}}` + "\n" +
			`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"hi"}}` + "\n" +
			`{"type":"turn.completed","usage":{"input_tokens":50,"cached_input_tokens":10,"output_tokens":15}}` + "\n",
		model:     "gpt-test",
		wantModel: "gpt-test",
		// Codex reports no cache writes and no cost.
		want:      tokens{input: known(40), output: known(15), cacheRead: known(10)},
		wantTools: known(1),
	},
	{
		provider: "cursor-cli",
		build:    func(s string) LLMProvider { return NewCursorCliProvider(s, "", nil, nil) },
		stdout: `{"type":"result","is_error":false,"result":"hi","model":"cursor-test",` +
			`"usage":{"inputTokens":7,"outputTokens":3,"cacheReadTokens":0,"cacheWriteTokens":2}}`,
		model:     "auto",
		wantModel: "cursor-test",
		want:      tokens{input: known(7), output: known(3), cacheRead: known(0), cacheCreation: known(2)},
	},
	{
		provider: "antigravity-cli",
		build:    func(s string) LLMProvider { return NewAntigravityCliProvider(s, "", nil, nil) },
		stdout: `{"status":"SUCCESS","response":"hi","model":"gemini-test",` +
			`"usage":{"input_tokens":5,"output_tokens":2,"cache_read_tokens":1}}`,
		model:     "gemini-test",
		wantModel: "gemini-test",
		want:      tokens{input: known(5), output: known(2), cacheRead: known(1)},
	},
	{
		provider: "opencode-cli",
		build:    func(s string) LLMProvider { return NewOpenCodeCliProvider(s, "", nil, nil) },
		stdout: `{"type":"tool_use","part":{"type":"tool","id":"c1","tool":"example_echo","state":{"status":"completed"}}}` + "\n" +
			`{"type":"text","part":{"id":"t1","text":"hi"}}` + "\n" +
			`{"type":"step_finish","part":{"reason":"stop","cost":0.004,"tokens":{"input":9,"output":3,"reasoning":1,"cache":{"read":4,"write":2}}}}` + "\n",
		model:     "opencode/test-model",
		wantModel: "opencode/test-model",
		// Reasoning is billed as output.
		want:      tokens{known(9), known(4), known(4), known(2), accounting.KnownFloat(0.004)},
		wantTools: known(1),
	},
}

func TestCLIProvidersRejectInvalidIdentity(t *testing.T) {
	for _, c := range cliCases {
		for _, r := range launchtest.Rejected {
			t.Run(c.provider+"/"+r.Name, func(t *testing.T) {
				rec := launchtest.Capture(t)
				script, marker := markerCLI(t, c.stdout)
				_, err := c.build(script).Chat(context.Background(), r.ID,
					[]Message{{Role: "user", Content: "hi"}}, nil, c.model, nil)
				if !errors.Is(err, r.Want) {
					t.Fatalf("err = %v, want %v", err, r.Want)
				}
				if ran(marker) {
					t.Error("the CLI was started for a refused identity")
				}
				got := rec.One()
				if got.Success || got.Error == "" {
					t.Errorf("record success=%v error=%q, want a failure with the error", got.Success, got.Error)
				}
				if got.Provider != c.provider || got.Transport != accounting.TransportCLI || got.Kind != accounting.KindText {
					t.Errorf("record = %s/%s/%s", got.Provider, got.Transport, got.Kind)
				}
				launchtest.Unknown(t, "http_status", got.HTTPStatus)
				checkTokens(t, got, tokens{})
			})
		}
	}
}

func TestCLIProvidersAccountSuccess(t *testing.T) {
	for _, c := range cliCases {
		t.Run(c.provider, func(t *testing.T) {
			rec := launchtest.Capture(t)
			script, marker := markerCLI(t, c.stdout)
			if _, err := c.build(script).Chat(context.Background(), testID,
				[]Message{{Role: "user", Content: "hi"}}, nil, c.model, nil); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if !ran(marker) {
				t.Fatal("the CLI did not run")
			}
			got := rec.One()
			if !got.Success || got.Error != "" {
				t.Errorf("record success=%v error=%q, want success", got.Success, got.Error)
			}
			if got.Agent != "alice" || got.Project != accounting.DefaultProject {
				t.Errorf("identity = %q/%q, want alice/default", got.Agent, got.Project)
			}
			if got.Kind != accounting.KindText || got.Provider != c.provider || got.Transport != accounting.TransportCLI {
				t.Errorf("record = %s/%s/%s", got.Kind, got.Provider, got.Transport)
			}
			if got.Model != c.wantModel {
				t.Errorf("model = %q, want %q", got.Model, c.wantModel)
			}
			launchtest.Unknown(t, "http_status", got.HTTPStatus)
			checkTokens(t, got, c.want)
			if got.ToolCalls != c.wantTools {
				t.Errorf("tool_calls = %+v, want %+v", got.ToolCalls, c.wantTools)
			}
			launchtest.Unknown(t, "images", got.Images)
		})
	}
}

func TestCLIWithoutUsageLeavesTokensUnknown(t *testing.T) {
	rec := launchtest.Capture(t)
	script, _ := markerCLI(t, `{"type":"result","subtype":"success","is_error":false,"result":"hi"}`)
	if _, err := NewClaudeCliProvider(script, "", nil, nil).Chat(context.Background(), testID,
		[]Message{{Role: "user", Content: "hi"}}, nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	checkTokens(t, rec.One(), tokens{})
}

func TestIdentityIsNormalisedInRecord(t *testing.T) {
	rec := launchtest.Capture(t)
	script, _ := markerCLI(t, cliCases[0].stdout)
	id := accounting.Identity{Agent: "  bob ", Name: " Bob the builder ", Project: " shed "}
	if _, err := NewClaudeCliProvider(script, "", nil, nil).Chat(context.Background(), id,
		[]Message{{Role: "user", Content: "hi"}}, nil, "", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := rec.One(); got.Agent != "bob" || got.AgentName != "Bob the builder" || got.Project != "shed" {
		t.Errorf("identity = %q/%q/%q, want bob/Bob the builder/shed", got.Agent, got.AgentName, got.Project)
	}
}

// chatServer answers chat completions with body and counts the requests.
func chatServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const chatOK = `{"model":"gpt-test-0101","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":2}}}`

func TestHTTPProviderAccountsOnce(t *testing.T) {
	rec := launchtest.Capture(t)
	srv, hits := chatServer(t, http.StatusOK, chatOK)
	p := NewHTTPProviderWithOptions("k", srv.URL, "")
	if _, err := p.Chat(context.Background(), testID, []Message{{Role: "user", Content: "hi"}}, nil, "gpt-test", nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("requests = %d, want 1", hits.Load())
	}
	got := rec.One()
	if got.Provider != "openai-chat" || got.Transport != accounting.TransportAPI || got.Model != "gpt-test-0101" {
		t.Errorf("record = %s/%s/%s", got.Provider, got.Transport, got.Model)
	}
	if n := launchtest.Known(t, "http_status", got.HTTPStatus); n != http.StatusOK {
		t.Errorf("http_status = %d, want 200", n)
	}
	checkTokens(t, got, tokens{input: known(10), output: known(3), cacheRead: known(2)})
}

func TestUnconfiguredProviderIsAccounted(t *testing.T) {
	rec := launchtest.Capture(t)
	if _, err := NewUnconfiguredProvider().Chat(context.Background(), testID, nil, nil, "m", nil); err == nil {
		t.Fatal("Chat succeeded")
	}
	got := rec.One()
	if got.Provider != "unconfigured" || got.Success || got.Error == "" {
		t.Errorf("record = %+v", got)
	}

	if _, err := NewUnconfiguredProvider().Chat(context.Background(), accounting.Identity{}, nil, nil, "m", nil); !errors.Is(err, accounting.ErrAgentRequired) {
		t.Errorf("err = %v, want ErrAgentRequired", err)
	}
}

// exitCLI writes a CLI script that prints stdout and exits with code.
func exitCLI(t *testing.T, stdout string, code int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mock CLI scripts not supported on Windows")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "stdout.txt")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "cli")
	body := fmt.Sprintf("#!/bin/sh\ncat '%s'\nexit %d\n", out, code)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// A failed CLI run stays a failure in its status, with the measured outcome
// and stop reason, and only adds the usage and cost the CLI reported.
func TestCLIFailureKeepsReportedUsage(t *testing.T) {
	tests := []struct {
		name     string
		build    func(script string) LLMProvider
		stdout   string
		exitCode int
		wantStop string
		want     tokens
	}{
		{
			name:  "antigravity denied actions",
			build: func(s string) LLMProvider { return NewAntigravityCliProvider(s, "", nil, nil) },
			stdout: `{"status":"SUCCESS","response":"","denied_actions":[{"action":"run_command"}],` +
				`"usage":{"input_tokens":5,"output_tokens":0}}`,
			wantStop: "parse_error",
			want:     tokens{input: known(5), output: known(0)},
		},
		{
			name:     "non-zero exit with empty result",
			build:    func(s string) LLMProvider { return NewClaudeCliProvider(s, "", nil, nil) },
			stdout:   `{"type":"result","is_error":false,"result":"","usage":{"input_tokens":2,"output_tokens":0}}`,
			exitCode: 1,
			wantStop: "error",
			want:     tokens{input: known(2), output: known(0)},
		},
		{
			name:  "claude is_error with cost",
			build: func(s string) LLMProvider { return NewClaudeCliProvider(s, "", nil, nil) },
			stdout: `{"type":"result","subtype":"error","is_error":true,"result":"quota exceeded",` +
				`"total_cost_usd":0.01,"usage":{"input_tokens":4,"output_tokens":1}}`,
			wantStop: "parse_error",
			want:     tokens{input: known(4), output: known(1), cost: accounting.KnownFloat(0.01)},
		},
		{
			name:  "claude is_error with cost, non-zero exit",
			build: func(s string) LLMProvider { return NewClaudeCliProvider(s, "", nil, nil) },
			stdout: `{"type":"result","subtype":"error","is_error":true,"result":"overloaded",` +
				`"total_cost_usd":0.01,"usage":{"input_tokens":4,"output_tokens":1}}`,
			exitCode: 1,
			wantStop: "error",
			want:     tokens{input: known(4), output: known(1), cost: accounting.KnownFloat(0.01)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			script := exitCLI(t, tt.stdout, tt.exitCode)
			resp, err := tt.build(script).Chat(context.Background(), testID,
				[]Message{{Role: "user", Content: "hi"}}, nil, "", nil)
			if err == nil {
				t.Fatal("Chat succeeded, want an error")
			}
			if resp == nil || resp.Status == nil {
				t.Fatal("no status returned with the error")
			}
			if resp.Status.Success || resp.Status.StopReason != tt.wantStop {
				t.Errorf("status success=%v stop=%q, want false/%q", resp.Status.Success, resp.Status.StopReason, tt.wantStop)
			}
			got := rec.One()
			if got.Success || got.Error == "" {
				t.Errorf("record success=%v error=%q, want a failure", got.Success, got.Error)
			}
			checkTokens(t, got, tt.want)
		})
	}
}

func TestCodexToolCalls(t *testing.T) {
	const done = `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`
	msg := `{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"hi"}}`
	item := func(typ string) string {
		return `{"type":"item.completed","item":{"id":"x","type":"` + typ + `"}}`
	}
	tests := []struct {
		name   string
		events []string
		want   accounting.Int
	}{
		{name: "each tool item counts", want: known(4), events: []string{
			`{"type":"item.started","item":{"id":"x","type":"command_execution"}}`,
			item("command_execution"), item("file_change"), item("mcp_tool_call"), item("web_search"),
			item("reasoning"), item("todo_list"), msg, done,
		}},
		{name: "no tool items is a known zero", want: known(0), events: []string{item("reasoning"), msg, done}},
		{name: "unknown item type", events: []string{item("something_new"), msg, done}},
		{name: "no completed turn", events: []string{item("command_execution"), msg}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := launchtest.Capture(t)
			script, _ := markerCLI(t, strings.Join(tt.events, "\n")+"\n")
			if _, err := NewCodexCliProvider(script, "", nil, nil).Chat(context.Background(), testID,
				[]Message{{Role: "user", Content: "hi"}}, nil, "", nil); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := rec.One().ToolCalls; got != tt.want {
				t.Errorf("tool_calls = %+v, want %+v", got, tt.want)
			}
		})
	}
}
