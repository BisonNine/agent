package modelspec

import (
	"maps"
	"slices"
)

// CLI agents: the local binaries a host can drive as providers.
//
// Each of these needs a provider entry, a model entry, and a handful of flags
// that are not optional and not guessable: the flags the CLI needs to run
// headless, the permission-bypass flag it needs to use tools unattended, the
// sentinel model id that means "let the CLI pick its own model", and a timeout
// long enough for an agentic run. Getting any of them wrong fails quietly: a
// CLI denied permission to use tools returns success with an empty answer.
//
// The permission-bypass flag is the one an operator decides on: it lets the
// CLI run commands and edit files anywhere the service user can. It is passed
// only when the provider's bypass_restrictions is on.
//
// This table is the single source for all of it. Adding a CLI means adding a
// row.

// StdinArg is the trailing argument the Claude and Codex CLIs take to read
// the prompt from stdin. It has the value of spawnllm.StdinArg.
const StdinArg = "-"

// CLIAgent describes one supported CLI agent.
type CLIAgent struct {
	// Protocol is the wire protocol naming this CLI in a provider entry.
	Protocol string
	// Label is the human name, used for the provider entry and in a UI.
	Label string
	// Binary is the executable to look for on PATH when a provider sets no
	// explicit command.
	Binary string
	// BaseArgs are the arguments the provider itself always passes: print the
	// answer, encode it as JSON, and (where the CLI needs saying) read the
	// prompt from stdin. Not configurable, which is precisely why they have
	// to be shown: an operator asking what runs on their machine is owed the
	// whole command line. They hold the values of spawnllm's
	// <Name>CliBaseArgs, which builds the invocation.
	BaseArgs []string
	// TrailingArgs come after everything else, matching the real invocation:
	// the CLIs that need telling to read the prompt from stdin take that marker
	// last, after the model flag.
	TrailingArgs []string
	// RequiredArgs are passed on every invocation: the non-security flags the
	// CLI needs to run headless. A model's extra_args are appended to these.
	RequiredArgs []string
	// BypassArgs are the CLI's skip-permissions / sandbox-bypass flags. They are
	// passed only when the provider has bypass_restrictions on; otherwise they
	// are withheld, and stripped from a model's extra_args, so the CLI's own
	// permission settings decide what it may do.
	BypassArgs []string
	// Env is set for every invocation, for CLIs that need it.
	Env map[string]string
	// RequestTimeout is the per-request timeout in seconds. CLI agents run
	// whole tool loops of their own, so they need far longer than an HTTP call.
	RequestTimeout int
}

// SentinelModel is the model id meaning "let the CLI choose its own model":
// the providers recognise their own protocol name and pass no --model flag.
func (c CLIAgent) SentinelModel() string { return c.Protocol }

// CLIAgents lists every supported CLI agent, in display order.
var CLIAgents = []CLIAgent{
	{
		Protocol:       "claude-cli",
		Label:          "Claude CLI",
		Binary:         "claude",
		BaseArgs:       []string{"-p", "--output-format", "json"},
		TrailingArgs:   []string{StdinArg},
		RequiredArgs:   []string{"--no-chrome"},
		BypassArgs:     []string{"--dangerously-skip-permissions"},
		Env:            map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"},
		RequestTimeout: 3600,
	},
	{
		Protocol:       "codex-cli",
		Label:          "Codex CLI",
		Binary:         "codex",
		BaseArgs:       []string{"exec", "--json", "--color", "never"},
		TrailingArgs:   []string{StdinArg},
		RequiredArgs:   []string{"--skip-git-repo-check"},
		BypassArgs:     []string{"--dangerously-bypass-approvals-and-sandbox"},
		RequestTimeout: 3600,
	},
	{
		// Antigravity (binary "agy") replaces the deprecated Gemini CLI. The
		// prompt is piped on stdin and the provider never passes -p/--print:
		// with either, agy reads the prompt from argv and ignores stdin.
		Protocol:       "antigravity-cli",
		Label:          "Antigravity CLI",
		Binary:         "agy",
		BaseArgs:       []string{"--input-format", "text", "--output-format", "json"},
		BypassArgs:     []string{"--dangerously-skip-permissions"},
		RequestTimeout: 3600,
	},
	{
		Protocol:       "cursor-cli",
		Label:          "Cursor CLI",
		Binary:         "cursor-agent",
		BaseArgs:       []string{"-p", "--output-format", "json"},
		BypassArgs:     []string{"--yolo"},
		RequestTimeout: 3600,
	},
	{
		// OpenCode reads the prompt from stdin without a marker.
		// --standalone runs the session in the CLI process instead of
		// OpenCode's shared background service, so the run has the model's
		// environment and workspace and stops when the CLI is stopped.
		Protocol:       "opencode-cli",
		Label:          "OpenCode CLI",
		Binary:         "opencode",
		BaseArgs:       []string{"run", "--format", "json"},
		RequiredArgs:   []string{"--standalone"},
		BypassArgs:     []string{"--auto"},
		RequestTimeout: 3600,
	},
}

// CLIAgentByProtocol returns the CLI agent for a protocol, or nil if the
// protocol is not a CLI. "gemini-cli" resolves to Antigravity: Google
// deprecated the Gemini CLI, and a config still naming it must keep working
// rather than lose its required arguments.
func CLIAgentByProtocol(protocol string) *CLIAgent {
	if protocol == "gemini-cli" {
		protocol = "antigravity-cli"
	}
	for i := range CLIAgents {
		if CLIAgents[i].Protocol == protocol {
			return &CLIAgents[i]
		}
	}
	return nil
}

// CLIArgs returns the arguments to run a CLI model with: the protocol's bypass
// flags when bypass is on, its required arguments, then the model's own
// extraArgs. A non-CLI protocol gets extraArgs unchanged.
//
// The required arguments are supplied rather than copied into every model
// because omitting them is invisible until an assistant answers nothing. A
// model that has been given the same flag explicitly does not get it twice.
//
// When bypass is off, a bypass flag written into a model's extraArgs is
// stripped and returned in ignored: the provider setting is the one place the
// decision is made, and a flag left in a model must not quietly override it.
// The caller tells the operator about ignored flags.
func CLIArgs(protocol string, bypass bool, extraArgs []string) (args, ignored []string) {
	agent := CLIAgentByProtocol(protocol)
	if agent == nil {
		return extraArgs, nil
	}
	supplied := agent.RequiredArgs
	if bypass {
		supplied = append(append([]string{}, agent.BypassArgs...), agent.RequiredArgs...)
	}
	args = make([]string, 0, len(supplied)+len(extraArgs))
	args = append(args, supplied...)
	for _, a := range extraArgs {
		switch {
		case slices.Contains(supplied, a):
			continue
		case !bypass && slices.Contains(agent.BypassArgs, a):
			ignored = append(ignored, a)
		default:
			args = append(args, a)
		}
	}
	return args, ignored
}

// CLIEnv returns the environment for a CLI model: the protocol's required
// entries, overridden by anything the model sets itself.
func CLIEnv(protocol string, modelEnv map[string]string) map[string]string {
	agent := CLIAgentByProtocol(protocol)
	if agent == nil || len(agent.Env) == 0 {
		return modelEnv
	}
	out := make(map[string]string, len(agent.Env)+len(modelEnv))
	maps.Copy(out, agent.Env)
	maps.Copy(out, modelEnv)
	return out
}
