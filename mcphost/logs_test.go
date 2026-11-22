package mcphost

import (
	"strings"
	"testing"
	"time"
)

// No log record of the store or an endpoint carries a token: issue,
// resolve, revoke, expiry, and refused credentials on both transports.
func TestLogs_NeverCarryTokens(t *testing.T) {
	var logs logBuffer
	logger := newLogger(&logs)
	f := newFixture(t, func(c *Config) { c.Logger = logger })
	f.store = NewStore(WithLogger(logger))
	// Rebuild the fixture's tokens on the logging store.
	var err error
	if f.aliceTok, err = f.store.Issue(Grant{Scope: aliceTurn}); err != nil {
		t.Fatal(err)
	}
	expiring := issue(t, f.store, Grant{Scope: aliceTurn, Expires: time.Now().Add(10 * time.Millisecond)})
	if err := f.store.Register("registered-token-value", Grant{Scope: aliceTurn}); err != nil {
		t.Fatal(err)
	}
	f.store.Resolve(f.aliceTok)
	time.Sleep(50 * time.Millisecond)
	tokens := []string{f.aliceTok, expiring, "registered-token-value", f.bobTok}
	f.store.Revoke(f.aliceTok)
	f.store.Revoke("registered-token-value")

	// Refused and accepted credentials on both endpoints (the fixture's
	// endpoints authenticate against its first store, where Bob's token
	// is valid and Alice's new ones are not).
	argC := f.mustClient("/internal", "")
	callT(t, argC, "whoami", map[string]any{TokenArgument: f.aliceTok})
	callT(t, argC, "whoami", map[string]any{TokenArgument: f.bobTok})
	for _, tok := range []string{f.aliceTok, "not-a-real-token"} {
		if _, err := f.client("/mcp", tok); err == nil {
			t.Error("a token unknown to the endpoint connected")
		}
	}
	callT(t, f.mustClient("/mcp", f.bobTok), "whoami", nil)

	out := logs.String()
	for _, want := range []string{"Session token issued", "Session token revoked", "Session token expired", "MCP authentication refused", "MCP tool call"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	for _, tok := range append(tokens, "not-a-real-token") {
		if strings.Contains(out, tok) {
			t.Errorf("log carries a token:\n%s", out)
		}
	}
}
