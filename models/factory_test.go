package models

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	anthropicmessages "go.bison9.com/agent/spawnllm/anthropic_messages"
	"go.bison9.com/agent/spawnllm/azure"
	"go.bison9.com/agent/spawnllm/openai_responses"
)

var alice = accounting.Identity{Agent: "alice", Name: "Alice"}

func testSettings() *settings {
	s := defaultSettings()
	s.baseEnv = func() []string { return []string{"PATH=/usr/bin:/bin", "HOME=/home/alice"} }
	return s
}

// unwrapGuard returns the client behind the declined-tools guard.
func unwrapGuard(p spawnllm.LLMProvider) spawnllm.LLMProvider {
	if g, ok := p.(*declinedGuard); ok {
		return g.LLMProvider
	}
	return p
}

func TestBuildClientProtocols(t *testing.T) {
	cases := []struct {
		protocol string
		want     any
	}{
		{"openai-chat", &spawnllm.HTTPProvider{}},
		{"openai-responses", &openai_responses.Provider{}},
		{"azure", &azure.Provider{}},
		{"anthropic", &anthropicmessages.Provider{}},
		{"anthropic-messages", &anthropicmessages.Provider{}},
		{"claude-cli", &spawnllm.ClaudeCliProvider{}},
		{"codex-cli", &spawnllm.CodexCliProvider{}},
		{"antigravity-cli", &spawnllm.AntigravityCliProvider{}},
		{"gemini-cli", &spawnllm.AntigravityCliProvider{}},
		{"cursor-cli", &spawnllm.CursorCliProvider{}},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			m := modelspec.Model{ModelName: "m", Model: "raw/id-with/slash", Provider: "p"}
			prov := modelspec.Provider{Name: "p", Protocol: tc.protocol, BaseURL: "https://example.invalid/v1", APIKey: "k"}
			c, err := buildClient(testSettings(), &m, &prov)
			if err != nil {
				t.Fatal(err)
			}
			if got := unwrapGuard(c); reflect.TypeOf(got) != reflect.TypeOf(tc.want) {
				t.Fatalf("client = %T, want %T", got, tc.want)
			}
		})
	}
}

func TestBuildClientErrors(t *testing.T) {
	cases := []struct {
		name    string
		model   modelspec.Model
		prov    modelspec.Provider
		noChat  bool // ErrNoClient
		message string
	}{
		{"anthropic without key", modelspec.Model{ModelName: "m", Model: "x"}, modelspec.Provider{Name: "a", Protocol: "anthropic"}, false, "api_key required"},
		{"task-only protocol", modelspec.Model{ModelName: "m", Model: "x"}, modelspec.Provider{Name: "g", Protocol: "gemini", APIKey: "k"}, true, "task models only"},
		{"unknown protocol", modelspec.Model{ModelName: "m", Model: "x"}, modelspec.Provider{Name: "h", Protocol: "person"}, true, "person"},
		{"empty model id", modelspec.Model{ModelName: "m"}, modelspec.Provider{Name: "o", Protocol: "openai-chat", BaseURL: "https://x"}, false, "model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildClient(testSettings(), &tc.model, &tc.prov)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("err = %v, want one containing %q", err, tc.message)
			}
			if errors.Is(err, ErrNoClient) != tc.noChat {
				t.Fatalf("errors.Is(err, ErrNoClient) = %v, want %v", !tc.noChat, tc.noChat)
			}
		})
	}
}

// The command line and environment of a CLI model: what the host builds
// today for the same model and provider.
func TestCLISpec(t *testing.T) {
	cases := []struct {
		name  string
		model modelspec.Model
		prov  modelspec.Provider
		want  cliSpec
	}{
		{
			name:  "claude, bypass off, defaults",
			model: modelspec.Model{ModelName: "c", Model: "claude-cli"},
			prov:  modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli"},
			want: cliSpec{
				protocol: "claude-cli", workspace: ".", args: []string{"--no-chrome"},
				env: map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"},
			},
		},
		{
			name: "claude, bypass on, extra args, env and timeout",
			model: modelspec.Model{
				ModelName: "c", Model: "sonnet", Workspace: "/srv/ws", RequestTimeout: 90,
				ExtraArgs: []string{"--no-chrome", "--verbose"}, Env: map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "0", "X": "1"},
			},
			prov: modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", Command: "/opt/claude", BypassRestrictions: true},
			want: cliSpec{
				protocol: "claude-cli", command: "/opt/claude", workspace: "/srv/ws", timeout: 90 * time.Second,
				args: []string{"--dangerously-skip-permissions", "--no-chrome", "--verbose"},
				env:  map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "0", "X": "1"},
			},
		},
		{
			name:  "codex, bypass flag in extra args is dropped when bypass is off",
			model: modelspec.Model{ModelName: "x", Model: "gpt-5", ExtraArgs: []string{"--dangerously-bypass-approvals-and-sandbox", "-c", "k=v"}},
			prov:  modelspec.Provider{Name: "Codex", Protocol: "codex-cli"},
			want: cliSpec{
				protocol: "codex-cli", workspace: ".", args: []string{"--skip-git-repo-check", "-c", "k=v"},
				ignored: []string{"--dangerously-bypass-approvals-and-sandbox"},
			},
		},
		{
			name:  "gemini-cli runs Antigravity",
			model: modelspec.Model{ModelName: "g", Model: "gemini-2.5-flash"},
			prov:  modelspec.Provider{Name: "Gemini", Protocol: "gemini-cli", BypassRestrictions: true},
			want:  cliSpec{protocol: "antigravity-cli", workspace: ".", args: []string{"--dangerously-skip-permissions"}},
		},
		{
			name:  "cursor, bypass on",
			model: modelspec.Model{ModelName: "cu", Model: "cursor-cli", Env: map[string]string{"A": "b"}},
			prov:  modelspec.Provider{Name: "Cursor", Protocol: "cursor-cli", BypassRestrictions: true},
			want:  cliSpec{protocol: "cursor-cli", workspace: ".", args: []string{"--yolo"}, env: map[string]string{"A": "b"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testSettings()
			tc.want.baseEnv = s.baseEnv()
			got := cliSpecFor(s, &tc.model, &tc.prov)
			if len(got.args) == 0 {
				got.args = []string{}
			}
			if len(tc.want.args) == 0 {
				tc.want.args = []string{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("spec =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func TestIsolatedCLISpec(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		var buf bytes.Buffer
		s := testSettings()
		s.logger = slog.New(slog.NewTextHandler(&buf, nil))
		m := modelspec.Model{ModelName: "cc", Model: "claude-cli", Workspace: "/shared", ExtraArgs: []string{"--dangerously-skip-permissions", "--verbose"}}
		prov := modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", BypassRestrictions: bypass}
		warned := map[string]bool{}
		warn := func(name string) bool {
			seen := warned[name]
			warned[name] = true
			return !seen
		}
		for range 3 {
			got := isolatedCLISpec(s, &m, &prov, "/tmp/ws", warn)
			if got.workspace != "/tmp/ws" || !slices.Equal(got.args, []string{"--no-chrome", "--verbose"}) {
				t.Fatalf("bypass %v: spec = %+v", bypass, got)
			}
		}
		want := 1
		if bypass {
			want = 0
		}
		if n := strings.Count(buf.String(), "ignoring permission-bypass flags"); n != want {
			t.Errorf("bypass %v: logged %d times, want %d:\n%s", bypass, n, want, buf.String())
		}
		if m.Workspace != "/shared" || m.ExtraArgs[0] != "--dangerously-skip-permissions" || prov.BypassRestrictions != bypass {
			t.Fatal("the definition was changed")
		}
	}
}

func TestCLIBypassDecidesGuard(t *testing.T) {
	m := modelspec.Model{ModelName: "c", Model: "claude-cli"}
	off, err := buildClient(testSettings(), &m, &modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli"})
	if err != nil {
		t.Fatal(err)
	}
	g, ok := off.(*declinedGuard)
	if !ok {
		t.Fatalf("bypass off built %T, want the guard", off)
	}
	if _, isCLI := off.(spawnllm.CLIProvider); !isCLI {
		t.Error("the guard is not a CLI client")
	}
	if _, ok := g.LLMProvider.(*spawnllm.ClaudeCliProvider); !ok {
		t.Errorf("the guard wraps %T", g.LLMProvider)
	}
	on, err := buildClient(testSettings(), &m, &modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli", BypassRestrictions: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := on.(*spawnllm.ClaudeCliProvider); !ok {
		t.Errorf("bypass on built %T, want the bare client", on)
	}
}

type answer struct {
	resp *spawnllm.LLMResponse
	err  error
}

func (a answer) Chat(context.Context, accounting.Identity, []spawnllm.Message, []spawnllm.ToolDefinition, string, map[string]any) (*spawnllm.LLMResponse, error) {
	return a.resp, a.err
}
func (answer) GetDefaultModel() string { return "" }

func TestDeclinedGuard(t *testing.T) {
	const declined = "The Claude CLI declined to use tools. Allow its provider to bypass the CLI's restrictions, or allow the tools in the CLI's own settings."
	const denied = "The Claude CLI declined to use tools (maestro_file_get, mail_read, Bash). Allow its provider to bypass the CLI's restrictions, or allow the tools in the CLI's own settings."
	cases := []struct {
		name    string
		resp    *spawnllm.LLMResponse
		err     error
		wantErr string
		wantMsg string
	}{
		{name: "an empty answer", resp: &spawnllm.LLMResponse{Content: "  "}, wantErr: declined, wantMsg: declined},
		{
			name: "refused calls in the status",
			resp: &spawnllm.LLMResponse{Content: "I could not do it.", Status: &spawnllm.DispatchStatus{
				Success: true, DeniedTools: []string{"mcp__srv__maestro_file_get", "mcp__srv__mail_read", "Bash"},
			}},
			wantErr: denied, wantMsg: denied,
		},
		{
			name: "a denied-action error", resp: &spawnllm.LLMResponse{},
			err:     errors.New("antigravity cli denied RunCommand and produced no answer"),
			wantErr: declined + ": antigravity cli denied RunCommand and produced no answer", wantMsg: declined,
		},
		{name: "a real answer passes", resp: &spawnllm.LLMResponse{Content: "hello"}},
		{name: "an unrelated error passes", err: errors.New("exit status 1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &declinedGuard{LLMProvider: answer{tc.resp, tc.err}, format: DefaultDeclinedMessage, model: "c", label: "Claude CLI", protocol: "claude-cli"}
			resp, err := g.Chat(context.Background(), alice, nil, nil, "m", nil)
			if resp != tc.resp {
				t.Error("the response was replaced")
			}
			if tc.wantErr == "" {
				if !errors.Is(err, tc.err) {
					t.Errorf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("err = %v\nwant %q", err, tc.wantErr)
			}
			d, ok := errors.AsType[*CLIDeclinedError](err)
			if !ok || d.Message != tc.wantMsg {
				t.Fatalf("declined = %#v", d)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Error("the CLI's error is not wrapped")
			}
		})
	}
}

func TestDeclinedMessageFormatter(t *testing.T) {
	var got Declined
	g := &declinedGuard{
		LLMProvider: answer{resp: &spawnllm.LLMResponse{Status: &spawnllm.DispatchStatus{DeniedTools: []string{"mcp__a__b"}}}},
		format:      func(d Declined) string { got = d; return "custom" },
		model:       "alice-claude", label: "Claude CLI", protocol: "claude-cli",
	}
	_, err := g.Chat(context.Background(), alice, nil, nil, "m", nil)
	if err == nil || err.Error() != "custom" {
		t.Fatalf("err = %v", err)
	}
	want := Declined{Identity: alice, Model: "alice-claude", Label: "Claude CLI", Protocol: "claude-cli", Denied: []string{"b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatter got %+v, want %+v", got, want)
	}
}

func TestFilterEnv(t *testing.T) {
	environ := []string{"PATH=/bin", "HOME=/h", "SECRET_TOKEN=x", "LC_ALL=C", "ANTHROPIC_API_KEY=k", "Path=w", "AGENCY_HOME=/x"}
	if got, want := filterEnv(environ, "linux"), []string{"ANTHROPIC_API_KEY=k", "HOME=/h", "LC_ALL=C", "PATH=/bin"}; !slices.Equal(got, want) {
		t.Errorf("linux = %v, want %v", got, want)
	}
	if got := filterEnv(environ, "windows"); !slices.Contains(got, "Path=w") {
		t.Errorf("windows dropped Path: %v", got)
	}
}

// fakeCLI writes a script that records its arguments and environment, and
// returns its path and the directory it writes to.
func fakeCLI(t *testing.T) (cmd, out string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	cmd = filepath.Join(dir, "cli")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$@\" >\"$OUT/args\"\nenv >\"$OUT/env\"\necho '{}'\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return cmd, dir
}

func runFakeCLI(t *testing.T, s *settings, m modelspec.Model, prov modelspec.Provider) (args []string, env map[string]string) {
	t.Helper()
	cmd, out := fakeCLI(t)
	prov.Command = cmd
	m.Env = map[string]string{"OUT": out}
	c, err := buildClient(s, &m, &prov)
	if err != nil {
		t.Fatal(err)
	}
	discard(c.Chat(context.Background(), alice, []spawnllm.Message{{Role: "user", Content: "hi"}}, nil, m.Model, nil))
	rawArgs, err := os.ReadFile(filepath.Join(out, "args"))
	if err != nil {
		t.Fatalf("the CLI did not run: %v", err)
	}
	rawEnv, err := os.ReadFile(filepath.Join(out, "env"))
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Split(strings.TrimSuffix(string(rawArgs), "\n"), "\n")
	env = map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(rawEnv), "\n"), "\n") {
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	return args, env
}

// The command line a CLI model runs with, end to end: spawnllm's base
// arguments, the factory's (required, bypass, the model's own), the model
// flag and the trailing ones. The environment is the base environment and
// the protocol's and model's own, never the rest of the process's.
func TestCLICommandLineAndEnvironment(t *testing.T) {
	t.Setenv("MODELS_TEST_SECRET", "must-not-leak")
	cases := []struct {
		protocol string
		bypass   bool
		extra    []string
		want     []string
		env      map[string]string
	}{
		{"claude-cli", false, []string{"--verbose"}, []string{"-p", "--output-format", "json", "--no-chrome", "--verbose", "--model", "sonnet", "-"}, map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}},
		{"claude-cli", true, nil, []string{"-p", "--output-format", "json", "--dangerously-skip-permissions", "--no-chrome", "--model", "sonnet", "-"}, nil},
		{"codex-cli", true, nil, []string{"exec", "--json", "--color", "never", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "-m", "sonnet", "-C", ".", "-"}, nil},
		{"antigravity-cli", false, []string{"--dangerously-skip-permissions"}, []string{"--input-format", "text", "--output-format", "json", "--model", "sonnet"}, nil},
		{"gemini-cli", true, nil, []string{"--input-format", "text", "--output-format", "json", "--dangerously-skip-permissions", "--model", "sonnet"}, nil},
		{"cursor-cli", true, nil, []string{"-p", "--output-format", "json", "--yolo", "--model", "sonnet"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			s := testSettings()
			s.baseEnv = func() []string { return []string{"PATH=" + os.Getenv("PATH"), "KEEP=1"} }
			args, env := runFakeCLI(t, s,
				modelspec.Model{ModelName: "m", Model: "sonnet", ExtraArgs: tc.extra},
				modelspec.Provider{Name: "p", Protocol: tc.protocol, BypassRestrictions: tc.bypass})
			if !slices.Equal(args, tc.want) {
				t.Errorf("args = %q\nwant   %q", args, tc.want)
			}
			if env["KEEP"] != "1" || env["OUT"] == "" {
				t.Errorf("base or model environment missing: %v", env)
			}
			if _, leaked := env["MODELS_TEST_SECRET"]; leaked {
				t.Error("the process environment reached the CLI")
			}
			for k, v := range tc.env {
				if env[k] != v {
					t.Errorf("env %s = %q, want %q", k, env[k], v)
				}
			}
		})
	}
}

// modelspec.CLIAgents holds literal copies of spawnllm's base and trailing
// arguments; this keeps them from drifting apart.
func TestCLIAgentsMatchSpawnllm(t *testing.T) {
	for _, agent := range modelspec.CLIAgents {
		t.Run(agent.Protocol, func(t *testing.T) {
			s := testSettings()
			s.baseEnv = func() []string { return []string{"PATH=" + os.Getenv("PATH")} }
			args, _ := runFakeCLI(t, s,
				modelspec.Model{ModelName: "m", Model: agent.SentinelModel(), Workspace: "/"},
				modelspec.Provider{Name: "p", Protocol: agent.Protocol})
			if len(args) < len(agent.BaseArgs)+len(agent.TrailingArgs) {
				t.Fatalf("args = %q", args)
			}
			if got := args[:len(agent.BaseArgs)]; !slices.Equal(got, agent.BaseArgs) {
				t.Errorf("base args: spawnllm %q, modelspec %q", got, agent.BaseArgs)
			}
			if got := args[len(args)-len(agent.TrailingArgs):]; !slices.Equal(got, agent.TrailingArgs) {
				t.Errorf("trailing args: spawnllm %q, modelspec %q", got, agent.TrailingArgs)
			}
			middle := args[len(agent.BaseArgs) : len(args)-len(agent.TrailingArgs)]
			if !slices.Equal(middle[:len(agent.RequiredArgs)], agent.RequiredArgs) {
				t.Errorf("required args: got %q, want %q first", middle, agent.RequiredArgs)
			}
		})
	}
}

// discard drops results a test does not look at.
func discard(...any) {}
