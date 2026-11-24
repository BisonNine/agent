package models

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
	anthropicmessages "go.bison9.com/agent/spawnllm/anthropic_messages"
	"go.bison9.com/agent/spawnllm/azure"
	"go.bison9.com/agent/spawnllm/openai_compat"
	"go.bison9.com/agent/spawnllm/openai_responses"
)

// ErrNoClient reports a model the catalogue cannot build a chat client for:
// a protocol it does not know, or a task-only one, with no client registered
// for the model (Catalog.RegisterClient). It is reached through the
// *ClientError that reports it.
var ErrNoClient = errors.New("models: no chat client for this model")

// Sentinels of the catalogue's other operator-facing errors, matched with
// errors.Is.
var (
	ErrUnknownModel    = errors.New("unknown model")    // no model by that name
	ErrUnknownProvider = errors.New("unknown provider") // a model names a provider not added
	ErrDuplicate       = errors.New("already exists")   // a provider or model name is taken
)

// ClientError reports that the client for a model could not be built or is
// not available. Err is the cause with no "models:" prefix, for a host that
// prints its own wording.
type ClientError struct {
	Model    string // the model's name in the catalogue
	Provider string // its provider's name
	Protocol string // the provider's protocol
	Err      error  // the cause, without any "models:" prefix; never nil
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("models: model %q: provider %q: %v", e.Model, e.Provider, e.Err)
}

func (e *ClientError) Unwrap() error { return e.Err }

// noChatError is the cause of a ClientError for a model with no chat client:
// its text is the detail, and it unwraps to ErrNoClient.
type noChatError struct{ detail string }

func (e *noChatError) Error() string { return "no chat client: " + e.detail }

func (e *noChatError) Unwrap() error { return ErrNoClient }

// compatOpts are the openai_compat options of a model reached through prov:
// the endpoint's knobs from the provider, the request's from the model.
func compatOpts(s *settings, m *modelspec.Model, prov *modelspec.Provider) []openai_compat.Option {
	return []openai_compat.Option{
		openai_compat.WithLogger(s.logger),
		openai_compat.WithMaxTokensField(m.MaxTokensField),
		openai_compat.WithRequestTimeout(time.Duration(m.RequestTimeout) * time.Second),
		openai_compat.WithStrictCompat(prov.StrictCompat),
		openai_compat.WithNoParallelToolCalls(prov.NoParallelToolCalls),
		openai_compat.WithStrictAlternation(m.StrictAlternation),
		openai_compat.WithResponseLogFile(m.ResponseLogFile),
		openai_compat.WithFileModes(s.responseLogModes(m.ResponseLogFile)),
		openai_compat.WithReasoningEffort(m.ReasoningEffort),
		openai_compat.WithExtraBody(m.ExtraBody),
		openai_compat.WithDropParams(m.DropParams),
		openai_compat.WithModelLabel(m.ModelName),
		openai_compat.WithProtocol(prov.Protocol),
		openai_compat.WithResponseFormatJSONCapable(prov.ResponseFormatJSON),
	}
}

// responsesOpts are the openai_responses options: the subset of compatOpts
// the Responses API takes.
func responsesOpts(s *settings, m *modelspec.Model, prov *modelspec.Provider) []openai_responses.Option {
	return []openai_responses.Option{
		openai_responses.WithLogger(s.logger),
		openai_responses.WithRequestTimeout(time.Duration(m.RequestTimeout) * time.Second),
		openai_responses.WithNoParallelToolCalls(prov.NoParallelToolCalls),
		openai_responses.WithReasoningEffort(m.ReasoningEffort),
		openai_responses.WithExtraBody(m.ExtraBody),
		openai_responses.WithDropParams(m.DropParams),
		openai_responses.WithModelLabel(m.ModelName),
		openai_responses.WithProtocol(prov.Protocol),
		openai_responses.WithResponseFormatJSONCapable(prov.ResponseFormatJSON),
		openai_responses.WithResponseLogFile(m.ResponseLogFile),
		openai_responses.WithFileModes(s.responseLogModes(m.ResponseLogFile)),
	}
}

// responseLogModes are the modes of a model's response log file.
func (s *settings) responseLogModes(path string) fsmode.Modes {
	if path == "" {
		return fsmode.Modes{}
	}
	return s.modesFor(path)
}

// buildClient builds the chat client of model m reached through prov. Its
// error is a *ClientError.
func buildClient(s *settings, m *modelspec.Model, prov *modelspec.Provider) (spawnllm.LLMProvider, error) {
	c, err := newClient(s, m, prov)
	if err != nil {
		return nil, &ClientError{Model: m.ModelName, Provider: prov.Name, Protocol: prov.Protocol, Err: err}
	}
	return c, nil
}

// newClient is buildClient; its error is the bare cause.
func newClient(s *settings, m *modelspec.Model, prov *modelspec.Provider) (spawnllm.LLMProvider, error) {
	if m.Model == "" {
		return nil, modelspec.ErrModelRequired
	}
	switch prov.Protocol {
	case "openai-chat":
		return spawnllm.NewHTTPProviderWithOptions(prov.APIKey, prov.BaseURL, prov.Proxy, compatOpts(s, m, prov)...), nil
	case "openai-responses":
		return openai_responses.NewProvider(prov.APIKey, prov.BaseURL, prov.Proxy, responsesOpts(s, m, prov)...), nil
	case "azure":
		return azure.NewProviderWithTimeout(prov.APIKey, prov.BaseURL, prov.Proxy, m.RequestTimeout, azure.WithLogger(s.logger)), nil
	// "anthropic" and "anthropic-messages" are the same wire format, the
	// Messages API.
	case "anthropic", "anthropic-messages":
		if prov.APIKey == "" {
			return nil, fmt.Errorf("api_key required for %s protocol", prov.Protocol)
		}
		return anthropicmessages.NewProviderWithTimeout(prov.APIKey, prov.BaseURL, m.RequestTimeout, anthropicmessages.WithLogger(s.logger)), nil
	}
	if modelspec.CLIAgentByProtocol(prov.Protocol) != nil {
		return newCLIClient(s, cliSpecFor(s, m, prov), prov.BypassRestrictions, m.ModelName), nil
	}
	if modelspec.TaskOnlyProtocol(prov.Protocol) {
		return nil, &noChatError{fmt.Sprintf("protocol %q serves task models only, not chat", prov.Protocol)}
	}
	return nil, &noChatError{fmt.Sprintf("unknown protocol %q", prov.Protocol)}
}

// cliSpec is everything a CLI client is started with: the command line
// (beyond the CLI's own base and trailing arguments) and the environment.
type cliSpec struct {
	protocol  string // the canonical protocol ("gemini-cli" is "antigravity-cli")
	command   string
	workspace string
	timeout   time.Duration
	args      []string
	env       map[string]string
	baseEnv   []string
	ignored   []string // bypass flags dropped from the model's extra args
}

// cliSpecFor is the spec of model m on CLI provider prov. The protocol's
// required arguments and the model's extra args are passed, the bypass flags
// only when the provider allows it; the protocol's environment is overlaid by
// the model's. A model with no workspace runs in ".".
func cliSpecFor(s *settings, m *modelspec.Model, prov *modelspec.Provider) cliSpec {
	agent := modelspec.CLIAgentByProtocol(prov.Protocol)
	workspace := m.Workspace
	if workspace == "" {
		workspace = "."
	}
	args, ignored := modelspec.CLIArgs(prov.Protocol, prov.BypassRestrictions, m.ExtraArgs)
	if len(ignored) > 0 {
		s.noticeIgnoredBypass(IgnoredBypass{Model: m.ModelName, Protocol: agent.Protocol, Flags: slices.Clone(ignored)})
	}
	return cliSpec{
		protocol:  agent.Protocol,
		command:   prov.Command,
		workspace: workspace,
		timeout:   time.Duration(m.RequestTimeout) * time.Second,
		args:      args,
		env:       modelspec.CLIEnv(prov.Protocol, m.Env),
		baseEnv:   s.baseEnv(),
		ignored:   ignored,
	}
}

// isolatedCLISpec is the spec a fresh, tool-less agent runs model m with: in
// workspace, with no bypass flag whatever the provider says (it has no tools
// to grant). The bypass flags in the model's extra args are left out; when
// the provider's own setting would have ignored them too, warn reports them.
func isolatedCLISpec(s *settings, m *modelspec.Model, prov *modelspec.Provider, workspace string, warn func(name string) bool) cliSpec {
	agent := modelspec.CLIAgentByProtocol(prov.Protocol)
	var kept, stripped []string
	for _, a := range m.ExtraArgs {
		if slices.Contains(agent.BypassArgs, a) {
			stripped = append(stripped, a)
			continue
		}
		kept = append(kept, a)
	}
	if len(stripped) > 0 && !prov.BypassRestrictions && warn(m.ModelName) {
		s.noticeIgnoredBypass(IgnoredBypass{Model: m.ModelName, Protocol: agent.Protocol, Flags: stripped})
	}
	isolated := *m
	isolated.ExtraArgs = kept
	isolated.Workspace = workspace
	p := *prov
	p.BypassRestrictions = false
	return cliSpecFor(s, &isolated, &p)
}

// launchCLI builds the spawnllm client a spec describes.
func launchCLI(s *settings, spec cliSpec) spawnllm.LLMProvider {
	type cliClient interface {
		spawnllm.LLMProvider
		spawnllm.BaseEnvSetter
		SetLogger(l *slog.Logger)
		SetLogContent(v bool)
	}
	var c cliClient
	switch spec.protocol {
	case "claude-cli":
		if spec.timeout > 0 {
			c = spawnllm.NewClaudeCliProviderWithTimeout(spec.command, spec.workspace, spec.timeout, spec.args, spec.env)
		} else {
			c = spawnllm.NewClaudeCliProvider(spec.command, spec.workspace, spec.args, spec.env)
		}
	case "codex-cli":
		if spec.timeout > 0 {
			c = spawnllm.NewCodexCliProviderWithTimeout(spec.command, spec.workspace, spec.timeout, spec.args, spec.env)
		} else {
			c = spawnllm.NewCodexCliProvider(spec.command, spec.workspace, spec.args, spec.env)
		}
	case "antigravity-cli":
		if spec.timeout > 0 {
			c = spawnllm.NewAntigravityCliProviderWithTimeout(spec.command, spec.workspace, spec.timeout, spec.args, spec.env)
		} else {
			c = spawnllm.NewAntigravityCliProvider(spec.command, spec.workspace, spec.args, spec.env)
		}
	case "cursor-cli":
		if spec.timeout > 0 {
			c = spawnllm.NewCursorCliProviderWithTimeout(spec.command, spec.workspace, spec.timeout, spec.args, spec.env)
		} else {
			c = spawnllm.NewCursorCliProvider(spec.command, spec.workspace, spec.args, spec.env)
		}
	case "opencode-cli":
		if spec.timeout > 0 {
			c = spawnllm.NewOpenCodeCliProviderWithTimeout(spec.command, spec.workspace, spec.timeout, spec.args, spec.env)
		} else {
			c = spawnllm.NewOpenCodeCliProvider(spec.command, spec.workspace, spec.args, spec.env)
		}
	default:
		panic("models: no CLI client for protocol " + spec.protocol)
	}
	// The CLI starts from the allowlisted environment, never the host's
	// whole one; the model's env is overlaid on it by spawnllm.
	c.SetBaseEnv(spec.baseEnv)
	c.SetLogger(s.logger)
	c.SetLogContent(s.logContent)
	return c
}

// newCLIClient builds a CLI client. With bypass off it is wrapped in the
// declined-tools guard: a CLI that cannot approve a tool call refuses it and
// reports success with an empty answer, which would reach the user as
// silence.
func newCLIClient(s *settings, spec cliSpec, bypass bool, name string) spawnllm.LLMProvider {
	c := launchCLI(s, spec)
	if bypass {
		return c
	}
	label := spec.protocol
	if agent := modelspec.CLIAgentByProtocol(spec.protocol); agent != nil {
		label = agent.Label
	}
	return &declinedGuard{LLMProvider: c, format: s.declined, model: name, label: label, protocol: spec.protocol}
}

// Declined describes a CLI that declined to use tools, for the text of its
// CLIDeclinedError (WithDeclinedMessage).
type Declined struct {
	Identity accounting.Identity // who the call was for
	Model    string              // the model name
	Label    string              // the CLI's name, e.g. "Claude CLI"
	Protocol string
	Denied   []string // the refused tools, without their MCP prefix; may be empty
}

// DefaultDeclinedMessage is the default text of a CLIDeclinedError.
func DefaultDeclinedMessage(d Declined) string {
	msg := "The " + d.Label + " declined to use tools"
	if len(d.Denied) > 0 {
		msg += " (" + strings.Join(d.Denied, ", ") + ")"
	}
	return msg + ". Allow its provider to bypass the CLI's restrictions, or allow the tools in the CLI's own settings."
}

// declinedGuard wraps a CLI client running without its permission-bypass
// flag. A CLI that refuses a tool call it cannot prompt for reports success:
// with no text, or (the Claude CLI) with prose and the refused calls listed as
// denied. The guard turns each into a CLIDeclinedError saying what to change.
type declinedGuard struct {
	spawnllm.LLMProvider

	format   func(Declined) string
	model    string
	label    string
	protocol string
}

// IsCLI marks the guard as a CLI client.
func (g *declinedGuard) IsCLI() bool { return true }

func (g *declinedGuard) Chat(ctx context.Context, id accounting.Identity, messages []spawnllm.Message, tools []spawnllm.ToolDefinition, model string, options map[string]any) (*spawnllm.LLMResponse, error) {
	resp, err := g.LLMProvider.Chat(ctx, id, messages, tools, model, options)
	var denied []string
	if resp != nil && resp.Status != nil {
		denied = resp.Status.DeniedTools
	}
	switch {
	case err != nil && strings.Contains(err.Error(), "denied"):
		return resp, &CLIDeclinedError{Message: g.message(id, nil), Cause: err}
	case err == nil && len(denied) > 0:
		return resp, &CLIDeclinedError{Message: g.message(id, denied)}
	case err == nil && resp != nil && strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0:
		return resp, &CLIDeclinedError{Message: g.message(id, nil)}
	}
	return resp, err
}

func (g *declinedGuard) message(id accounting.Identity, denied []string) string {
	return g.format(Declined{Identity: id, Model: g.model, Label: g.label, Protocol: g.protocol, Denied: shortToolNames(denied)})
}

// CLIDeclinedError is the error of a CLI that declined to use tools. Its text
// is written for the user.
type CLIDeclinedError struct {
	Message string // what to change
	Cause   error  // the CLI's own error (e.g. the denied action), when it gave one
}

func (e *CLIDeclinedError) Error() string {
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *CLIDeclinedError) Unwrap() error { return e.Cause }

// shortToolNames strips the MCP client prefix (mcp__<server>__) a CLI puts on
// the tools it reached over MCP. Other names are returned unchanged.
func shortToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if rest, ok := strings.CutPrefix(n, "mcp__"); ok {
			if _, tool, found := strings.Cut(rest, "__"); found {
				n = tool
			}
		}
		out = append(out, n)
	}
	return out
}
