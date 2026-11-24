package modelspec

import (
	"slices"
	"testing"
)

// The permission-bypass flag is passed only when the provider's
// bypass_restrictions is on; the non-security flags a CLI needs to run
// headless are passed either way.
func TestCLIArgs(t *testing.T) {
	tests := []struct {
		name        string
		protocol    string
		bypass      bool
		extra       []string
		want        []string
		wantIgnored []string
	}{
		{name: "claude off", protocol: "claude-cli", want: []string{"--no-chrome"}},
		{name: "claude on", protocol: "claude-cli", bypass: true, want: []string{"--dangerously-skip-permissions", "--no-chrome"}},
		{name: "codex off", protocol: "codex-cli", want: []string{"--skip-git-repo-check"}},
		{name: "codex on", protocol: "codex-cli", bypass: true, want: []string{"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"}},
		{name: "antigravity off", protocol: "antigravity-cli", want: nil},
		{name: "antigravity on", protocol: "antigravity-cli", bypass: true, want: []string{"--dangerously-skip-permissions"}},
		{name: "cursor off", protocol: "cursor-cli", want: nil},
		{name: "cursor on", protocol: "cursor-cli", bypass: true, want: []string{"--yolo"}},
		{name: "opencode off", protocol: "opencode-cli", want: []string{"--standalone"}},
		{name: "opencode on", protocol: "opencode-cli", bypass: true, want: []string{"--auto", "--standalone"}},
		{
			name:        "opencode's --auto is stripped when bypass is off",
			protocol:    "opencode-cli",
			extra:       []string{"--auto", "--thinking"},
			want:        []string{"--standalone", "--thinking"},
			wantIgnored: []string{"--auto"},
		},
		{
			name:     "model args are appended, not substituted",
			protocol: "antigravity-cli",
			bypass:   true,
			extra:    []string{"--model-arg"},
			want:     []string{"--dangerously-skip-permissions", "--model-arg"},
		},
		{
			name:     "a flag given explicitly is not duplicated",
			protocol: "cursor-cli",
			bypass:   true,
			extra:    []string{"--yolo", "--other"},
			want:     []string{"--yolo", "--other"},
		},
		{
			name:        "a bypass flag in extra_args is stripped when bypass is off",
			protocol:    "codex-cli",
			extra:       []string{"--dangerously-bypass-approvals-and-sandbox", "--other"},
			want:        []string{"--skip-git-repo-check", "--other"},
			wantIgnored: []string{"--dangerously-bypass-approvals-and-sandbox"},
		},
		{
			name:     "the deprecated gemini-cli alias gets antigravity's flags",
			protocol: "gemini-cli",
			bypass:   true,
			want:     []string{"--dangerously-skip-permissions"},
		},
		{
			name:     "an HTTP protocol is left alone",
			protocol: "openai-chat",
			bypass:   true,
			extra:    []string{"--x"},
			want:     []string{"--x"},
		},
		{
			name:     "a bypass flag is kept when bypass is on",
			protocol: "claude-cli",
			bypass:   true,
			extra:    []string{"--dangerously-skip-permissions"},
			want:     []string{"--dangerously-skip-permissions", "--no-chrome"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ignored := CLIArgs(tc.protocol, tc.bypass, tc.extra)
			if (len(got) != 0 || len(tc.want) != 0) && !slices.Equal(got, tc.want) {
				t.Errorf("args = %v, want %v", got, tc.want)
			}
			if !slices.Equal(ignored, tc.wantIgnored) {
				t.Errorf("ignored = %v, want %v", ignored, tc.wantIgnored)
			}
		})
	}
}

func TestCLIEnv(t *testing.T) {
	got := CLIEnv("claude-cli", nil)
	if got["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] != "1" {
		t.Errorf("claude-cli env = %v, want the auto-memory opt-out", got)
	}
	// A model may override the catalogue, but must not lose the rest of it.
	got = CLIEnv("claude-cli", map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "0", "OTHER": "x"})
	if got["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] != "0" || got["OTHER"] != "x" {
		t.Errorf("model env did not override cleanly: %v", got)
	}
	if CLIAgentByProtocol("claude-cli").Env["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] != "1" {
		t.Error("CLIEnv changed the catalogue")
	}
	if got := CLIEnv("openai-chat", nil); got != nil {
		t.Errorf("HTTP protocol env = %v, want nil", got)
	}
}

// Every row has to be complete and every accepted CLI protocol has to have a
// row.
func TestCLIAgents_CatalogueIsComplete(t *testing.T) {
	for _, c := range CLIAgents {
		if c.Protocol == "" || c.Label == "" || c.Binary == "" {
			t.Errorf("incomplete row: %+v", c)
		}
		if len(c.BypassArgs) == 0 {
			t.Errorf("%s has no bypass arguments", c.Protocol)
		}
		for _, a := range c.RequiredArgs {
			if slices.Contains(c.BypassArgs, a) {
				t.Errorf("%s lists %s as both required and bypass", c.Protocol, a)
			}
		}
		if !slices.Contains(c.BaseArgs, "json") && !slices.Contains(c.BaseArgs, "--json") {
			t.Errorf("%s base args %v request no JSON output", c.Protocol, c.BaseArgs)
		}
		if c.RequestTimeout <= 0 {
			t.Errorf("%s has no request timeout", c.Protocol)
		}
		if !IsCLIProtocol(c.Protocol) {
			t.Errorf("%s is in the catalogue but not an accepted CLI protocol", c.Protocol)
		}
		if spec, ok := LookupProtocol(c.Protocol); !ok || !spec.Keyless {
			t.Errorf("%s is not a registered keyless protocol", c.Protocol)
		}
		if c.SentinelModel() != c.Protocol {
			t.Errorf("%s sentinel = %q", c.Protocol, c.SentinelModel())
		}
	}
	for _, protocol := range []string{"claude-cli", "codex-cli", "antigravity-cli", "cursor-cli", "gemini-cli", "opencode-cli"} {
		if CLIAgentByProtocol(protocol) == nil {
			t.Errorf("%s is an accepted CLI protocol with no catalogue row", protocol)
		}
	}
	if CLIAgentByProtocol("openai-chat") != nil {
		t.Error("an HTTP protocol resolved to a CLI agent")
	}
}

// The OpenCode row: JSON events, the prompt on stdin (no trailing marker), a
// private server (--standalone) and --auto as the bypass flag.
func TestCLIAgents_OpenCode(t *testing.T) {
	c := CLIAgentByProtocol("opencode-cli")
	if c == nil {
		t.Fatal("no opencode-cli row")
	}
	if c.Label != "OpenCode CLI" || c.Binary != "opencode" || c.RequestTimeout != 3600 {
		t.Errorf("row = %+v", *c)
	}
	if want := []string{"run", "--format", "json"}; !slices.Equal(c.BaseArgs, want) {
		t.Errorf("base args = %v, want %v", c.BaseArgs, want)
	}
	if !slices.Equal(c.RequiredArgs, []string{"--standalone"}) {
		t.Errorf("required args = %v, want --standalone", c.RequiredArgs)
	}
	if len(c.TrailingArgs) != 0 || len(c.Env) != 0 {
		t.Errorf("trailing %v, env %v; want none", c.TrailingArgs, c.Env)
	}
	if !slices.Equal(c.BypassArgs, []string{"--auto"}) || c.SentinelModel() != "opencode-cli" {
		t.Errorf("bypass %v, sentinel %q", c.BypassArgs, c.SentinelModel())
	}
}
