package modelspec

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

// The protocols of the vendors whose speech APIs are not OpenAI-style. A model
// on one serves task models only: it has no chat client.
const (
	GeminiProtocol     = "gemini"
	ElevenLabsProtocol = "elevenlabs"
)

// ProtocolSpec describes how providers on a protocol are validated. The zero
// value is a protocol with no requirements that needs an api_key to count as
// having credentials.
type ProtocolSpec struct {
	// BaseURLRequired marks an HTTP chat protocol: a provider on it must set
	// base_url.
	BaseURLRequired bool
	// TaskOnly marks a protocol that serves task models only. A provider on
	// it must set api_key; base_url is optional (the vendor's default).
	TaskOnly bool
	// Keyless marks a protocol that authenticates out of band, or not at all,
	// so a provider on it has credentials without an api_key.
	Keyless bool
}

var (
	protocolsMu sync.RWMutex
	protocols   = map[string]ProtocolSpec{
		"openai-chat":        {BaseURLRequired: true},
		"openai-responses":   {BaseURLRequired: true},
		"azure":              {BaseURLRequired: true},
		"anthropic":          {BaseURLRequired: true},
		"anthropic-messages": {BaseURLRequired: true},
		"claude-cli":         {Keyless: true},
		"codex-cli":          {Keyless: true},
		"antigravity-cli":    {Keyless: true},
		// Retained alias: Google deprecated the Gemini CLI in favour of
		// Antigravity, and a released config naming this must keep validating.
		"gemini-cli":       {Keyless: true},
		"cursor-cli":       {Keyless: true},
		GeminiProtocol:     {TaskOnly: true},
		ElevenLabsProtocol: {TaskOnly: true},
	}
)

// RegisterProtocol adds a protocol the SDK does not know, so providers naming
// it validate. It fails for an empty name or one already registered, built-in
// protocols included. Register at start-up, before any validation.
func RegisterProtocol(name string, spec ProtocolSpec) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("protocol name is required")
	}
	protocolsMu.Lock()
	defer protocolsMu.Unlock()
	if _, dup := protocols[name]; dup {
		return fmt.Errorf("protocol %q is already registered", name)
	}
	protocols[name] = spec
	return nil
}

// LookupProtocol returns the spec of a registered protocol.
func LookupProtocol(name string) (ProtocolSpec, bool) {
	protocolsMu.RLock()
	defer protocolsMu.RUnlock()
	spec, ok := protocols[name]
	return spec, ok
}

// Protocols returns the names of every registered protocol, sorted.
func Protocols() []string {
	protocolsMu.RLock()
	defer protocolsMu.RUnlock()
	return slices.Sorted(maps.Keys(protocols))
}

// IsCLIProtocol reports whether the protocol is a subprocess CLI provider,
// which authenticates out-of-band and needs no API key.
func IsCLIProtocol(protocol string) bool {
	switch protocol {
	// "gemini-cli" is retained as an alias for "antigravity-cli": Google
	// deprecated the Gemini CLI, and a config still naming it must keep
	// working.
	case "claude-cli", "codex-cli", "antigravity-cli", "gemini-cli", "cursor-cli":
		return true
	default:
		return false
	}
}

// TaskOnlyProtocol reports whether a provider protocol serves task models
// only (Gemini's and ElevenLabs' own APIs). Its base_url is optional (the
// vendor's default) and its api_key is required.
func TaskOnlyProtocol(protocol string) bool {
	spec, ok := LookupProtocol(protocol)
	return ok && spec.TaskOnly
}
