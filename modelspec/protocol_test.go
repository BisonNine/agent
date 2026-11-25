/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package modelspec

import (
	"slices"
	"strings"
	"testing"
)

// The built-in protocols: every one the host configuration accepted, less
// "human", which a host registers itself.
func TestProtocols_BuiltIn(t *testing.T) {
	want := []string{
		"anthropic", "anthropic-messages", "antigravity-cli", "azure",
		"claude-cli", "codex-cli", "cursor-cli", "elevenlabs", "gemini",
		"gemini-cli", "openai-chat", "openai-responses", "opencode-cli",
	}
	got := Protocols()
	for _, p := range want {
		if !slices.Contains(got, p) {
			t.Errorf("built-in protocol %q is not registered", p)
		}
	}
	if slices.Contains(got, "human") {
		t.Error(`the SDK registers "human"; only a host may`)
	}
	if !slices.IsSorted(got) {
		t.Errorf("Protocols() = %v, not sorted", got)
	}
}

func TestRegisterProtocol(t *testing.T) {
	const name = "test-person"
	if _, ok := LookupProtocol(name); !ok {
		if err := RegisterProtocol(name, ProtocolSpec{Keyless: true}); err != nil {
			t.Fatalf("RegisterProtocol: %v", err)
		}
	}
	p := Provider{Name: "Alice", Protocol: name}
	if err := p.ValidateProtocol(); err != nil {
		t.Errorf("registered protocol refused: %v", err)
	}
	if !p.HasCredentials() {
		t.Error("a keyless registered protocol has no credentials")
	}
	if !slices.Contains(Protocols(), name) {
		t.Error("registered protocol not listed")
	}
	if IsCLIProtocol(name) || TaskOnlyProtocol(name) {
		t.Error("registered protocol reads as CLI or task-only")
	}

	if err := RegisterProtocol(name, ProtocolSpec{}); err == nil {
		t.Error("registering a protocol twice succeeded")
	}
	if err := RegisterProtocol("openai-chat", ProtocolSpec{Keyless: true}); err == nil {
		t.Error("re-registering a built-in protocol succeeded")
	}
	if spec, _ := LookupProtocol("openai-chat"); spec.Keyless || !spec.BaseURLRequired {
		t.Errorf("a refused registration changed the built-in: %+v", spec)
	}
	if err := RegisterProtocol("  ", ProtocolSpec{}); err == nil {
		t.Error("registering an empty name succeeded")
	}
}

func TestIsCLIProtocol(t *testing.T) {
	for _, p := range []string{"claude-cli", "codex-cli", "antigravity-cli", "gemini-cli", "cursor-cli", "opencode-cli"} {
		if !IsCLIProtocol(p) {
			t.Errorf("%s is not a CLI protocol", p)
		}
	}
	for _, p := range []string{"openai-chat", "anthropic", "gemini", "", "human"} {
		if IsCLIProtocol(p) {
			t.Errorf("%q is a CLI protocol", p)
		}
	}
}

// gemini and elevenlabs are HTTP protocols with an optional base URL and a
// required API key; a model on one has no chat client.
func TestTaskOnlyProtocols(t *testing.T) {
	for _, proto := range []string{GeminiProtocol, ElevenLabsProtocol} {
		if !TaskOnlyProtocol(proto) {
			t.Errorf("%s is not task-only", proto)
		}
		p := Provider{Name: "v", Protocol: proto, APIKey: "k"}
		if err := p.ValidateProtocol(); err != nil {
			t.Errorf("%s without base_url: %v", proto, err)
		}
		p.APIKey = " "
		if err := p.ValidateProtocol(); err == nil || !strings.Contains(err.Error(), "api_key is required") {
			t.Errorf("%s without api_key = %v", proto, err)
		}
	}
	if TaskOnlyProtocol("openai-chat") || TaskOnlyProtocol("no-such-protocol") {
		t.Error("a chat or unknown protocol is task-only")
	}
}

func TestProvider_ValidateProtocol(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		want     string
	}{
		{name: "http with base_url", provider: Provider{Protocol: "openai-chat", BaseURL: "https://api.example.com/v1"}},
		{name: "cli needs no base_url", provider: Provider{Protocol: "claude-cli"}},
		{name: "unknown", provider: Provider{Protocol: "smoke-signal"}, want: `unknown protocol "smoke-signal"`},
		{name: "empty", provider: Provider{}, want: `unknown protocol ""`},
		{name: "http without base_url", provider: Provider{Protocol: "anthropic"}, want: `base_url is required for protocol "anthropic"`},
		{name: "task-only without key", provider: Provider{Protocol: "elevenlabs"}, want: `api_key is required for protocol "elevenlabs"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.provider.ValidateProtocol()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestProvider_HasCredentials(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
		want     bool
	}{
		{name: "cli without key", provider: Provider{Protocol: "codex-cli"}, want: true},
		{name: "deprecated cli alias", provider: Provider{Protocol: "gemini-cli"}, want: true},
		{name: "http with key", provider: Provider{Protocol: "openai-chat", APIKey: "k"}, want: true},
		{name: "http without key", provider: Provider{Protocol: "openai-chat"}},
		{name: "task-only without key", provider: Provider{Protocol: "gemini"}},
		{name: "unknown protocol with key", provider: Provider{Protocol: "smoke-signal", APIKey: "k"}, want: true},
		{name: "unknown protocol without key", provider: Provider{Protocol: "smoke-signal"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.provider.HasCredentials(); got != tc.want {
				t.Errorf("HasCredentials() = %v, want %v", got, tc.want)
			}
		})
	}
}
