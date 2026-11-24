package modelspec

import (
	"fmt"
	"strings"
)

// Provider is a named endpoint a model is reached through. It owns the wire
// protocol, the base URL, the credentials, and endpoint-scoped quirks. Models
// reference a provider by Name.
type Provider struct {
	Name string `json:"name"` // Unique identifier referenced by Model.Provider
	// Protocol is the wire format: openai-chat, openai-responses, azure,
	// anthropic, anthropic-messages, claude-cli, codex-cli, antigravity-cli,
	// cursor-cli, opencode-cli, gemini, elevenlabs, or one a host registered with
	// RegisterProtocol. "anthropic" and "anthropic-messages" are the same
	// thing: Anthropic speaks one wire format. "gemini" and "elevenlabs" serve
	// task models only (speech; see TaskOnlyProtocol).
	Protocol string `json:"protocol"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Proxy    string `json:"proxy,omitempty"`
	// Endpoint-scoped openai-compat knobs.
	StrictCompat bool `json:"strict_compat,omitempty"`
	// RequireReasoningContent backfills a placeholder into reasoning_content on
	// assistant messages that carry none, for endpoints that reject history
	// missing the field. It is the exact opposite of StrictCompat, which strips
	// the field for endpoints that reject its presence: set one or the other,
	// never both.
	//
	// The case this exists for is DeepSeek V4 thinking mode: when a request
	// includes tools, DeepSeek requires reasoning_content on every assistant
	// message in history and answers 400 otherwise, even for turns that made no
	// tool call. Only ever ADDS a placeholder where the field is empty; real
	// reasoning is passed through untouched.
	RequireReasoningContent bool `json:"require_reasoning_content,omitempty"`
	NoParallelToolCalls     bool `json:"no_parallel_tool_calls,omitempty"`
	ResponseFormatJSON      bool `json:"response_format_json,omitempty"`
	// Command overrides the binary path for CLI protocols (claude-cli, etc.).
	Command string `json:"command,omitempty"`
	// BypassRestrictions passes the CLI's skip-permissions / sandbox-bypass
	// flag on every invocation, so it can run commands and edit files
	// anywhere the service user can, without asking. Off by default: the CLI
	// then runs under its own permission settings. One setting per CLI
	// provider, shared by all of its models.
	BypassRestrictions bool `json:"bypass_restrictions,omitempty"`
}

// HasCredentials reports whether this provider carries enough to
// authenticate: providers on a keyless protocol (the CLIs, which
// authenticate out-of-band, and any protocol registered as Keyless) always
// qualify; the others need an API key.
func (p *Provider) HasCredentials() bool {
	if spec, ok := LookupProtocol(p.Protocol); ok && spec.Keyless {
		return true
	}
	return p.APIKey != ""
}

// ValidateProtocol reports why the provider's protocol settings are not
// valid: an unregistered protocol, a missing base_url on a protocol that
// requires one, or a missing api_key on a task-only protocol. The error text
// is the bare reason; callers add the provider's name.
func (p *Provider) ValidateProtocol() error {
	spec, ok := LookupProtocol(p.Protocol)
	if !ok {
		return fmt.Errorf("unknown protocol %q", p.Protocol)
	}
	if spec.BaseURLRequired && p.BaseURL == "" {
		return fmt.Errorf("base_url is required for protocol %q", p.Protocol)
	}
	if spec.TaskOnly && strings.TrimSpace(p.APIKey) == "" {
		return fmt.Errorf("api_key is required for protocol %q", p.Protocol)
	}
	return nil
}
