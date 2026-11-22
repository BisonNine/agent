package modelspec

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Vision passthrough modes for Model.Vision.
const (
	VisionOff          = "off"           // default: tool images are not sent to the model
	VisionUserMessage  = "user_message"  // inject images as a follow-up user turn (any chat endpoint)
	VisionToolResponse = "tool_response" // attach images to the tool result (OpenAI Responses API, xAI)
)

// Model is one configured model: an alias, the id the endpoint expects, the
// provider it is reached through, and its per-model settings.
type Model struct {
	// Required fields
	ModelName string `json:"model_name"` // User-facing alias for the model
	Model     string `json:"model"`      // Raw model id the endpoint expects
	Provider  string `json:"provider"`   // Name of the Provider this model is reached through

	// Special providers (CLI-based, OAuth, etc.)
	Workspace string `json:"workspace,omitempty"` // Workspace path for CLI-based providers

	// Optional optimizations
	MaxTokens      int    `json:"max_tokens,omitempty"`       // Maximum tokens per response; overrides the caller's default
	ContextWindow  int    `json:"context_window,omitempty"`   // Actual model context window size in tokens
	MaxTokensField string `json:"max_tokens_field,omitempty"` // Field name for max tokens (e.g., "max_completion_tokens")
	RequestTimeout int    `json:"request_timeout,omitempty"`  // Seconds
	ThinkingLevel  string `json:"thinking_level,omitempty"`   // Extended thinking: off|adaptive|low|medium|high|xhigh|max
	NoTools        bool   `json:"no_tools,omitempty"`         // When true, tools are not passed to this model
	// Vision controls how images returned by tools (e.g. MCP screenshots)
	// reach this model: "off"/"" (default) drops them; "user_message" injects
	// them as a follow-up user turn (works on any chat endpoint);
	// "tool_response" attaches them to the tool result itself (the OpenAI
	// Responses API and xAI accept it; plain OpenAI chat, whose tool messages
	// are text-only, does not).
	Vision    string            `json:"vision,omitempty"`
	ExtraArgs []string          `json:"extra_args,omitempty"` // Additional CLI arguments appended after required flags
	Env       map[string]string `json:"env,omitempty"`        // Environment variables for CLI-based providers
	Enabled   bool              `json:"enabled"`              // If false, model is skipped in all operations

	// ResponseLogFile, when non-empty, causes every raw HTTP response body from
	// the openai_compat provider to be appended to the given path. Diagnostic
	// feature only; no rotation, no expansion of ~ or env vars. Ignored by
	// providers other than openai_compat.
	ResponseLogFile string `json:"response_log_file,omitempty"`

	// ReasoningEffort sets the OpenAI-style reasoning_effort request field for
	// models that natively accept it (notably Grok). Valid values are "none",
	// "low", "medium", "high", or empty. Empty omits the field entirely; "none"
	// is sent explicitly (e.g. to disable reasoning on models that support it).
	// Providers that don't understand the field will silently ignore it.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// ExtraBody is a free-form passthrough map merged into the JSON request
	// body for OpenAI-compatible providers. Use it for per-provider knobs that
	// are not modelled natively. Keys colliding with managed fields (see
	// ReservedBodyKeys) are refused by Validate.
	ExtraBody map[string]any `json:"extra_body,omitempty"`

	// DropParams lists top-level request-body fields to strip before sending to
	// OpenAI-compatible providers. Use it to suppress a parameter a model or
	// upstream rejects, e.g. "temperature" for OpenRouter reasoning models that
	// don't advertise it and would 404 under provider.require_parameters.
	// Stripping is applied last (after extra_body), so it always wins. It is a
	// literal filter: listing structural fields like "messages" or "model" will
	// break the request. Ignored by providers other than openai_compat.
	DropParams []string `json:"drop_params,omitempty"`

	// StrictAlternation rewrites the outbound message list for chat-only models
	// that require strict user/assistant alternation and reject system/tool
	// roles (e.g. Gemma on some gateways): the system prompt is folded into the
	// first user turn, tool results become user turns, and consecutive
	// same-role messages are merged. It is model-scoped because models on the
	// same endpoint differ. Pair with no_tools, since tool_calls are dropped.
	// Ignored by providers other than openai_compat.
	StrictAlternation bool `json:"strict_alternation,omitempty"`
}

// reservedBodyKeys lists the JSON request fields owned by the request
// builder, and the ones spawnllm sets itself on OpenRouter requests
// (session_id from the agent id, cache_control for Claude models). ExtraBody
// entries colliding with these keys are refused by Validate, which is what
// guarantees the merge step in the request builder cannot overwrite a managed
// field.
var reservedBodyKeys = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"stream":                {},
	"tools":                 {},
	"tool_choice":           {},
	"parallel_tool_calls":   {},
	"reasoning_effort":      {},
	"temperature":           {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"top_p":                 {},
	"n":                     {},
	"session_id":            {},
	"cache_control":         {},
}

// ReservedBodyKeys returns the request-body fields extra_body may not set,
// sorted.
func ReservedBodyKeys() []string {
	return slices.Sorted(maps.Keys(reservedBodyKeys))
}

// Validate checks that the model has its required fields, a valid
// reasoning_effort, and no extra_body key that collides with a managed
// request field. It does not check that the provider exists; Document.Validate
// does.
func (m *Model) Validate() error {
	if m.ModelName == "" {
		return errors.New("model_name is required")
	}
	if m.Model == "" {
		return errors.New("model is required")
	}
	if m.Provider == "" {
		return fmt.Errorf("model %q: provider is required", m.ModelName)
	}
	switch m.ReasoningEffort {
	case "", "none", "low", "medium", "high":
		// ok
	default:
		return fmt.Errorf(
			"model %q: invalid reasoning_effort %q (valid: none, low, medium, high, or omit)",
			m.ModelName, m.ReasoningEffort,
		)
	}
	for _, k := range slices.Sorted(maps.Keys(m.ExtraBody)) {
		if _, reserved := reservedBodyKeys[k]; reserved {
			return fmt.Errorf(
				"model %q: extra_body key %q collides with a managed request field",
				m.ModelName, k,
			)
		}
	}
	return nil
}
