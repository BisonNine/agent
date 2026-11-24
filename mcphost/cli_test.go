package mcphost

import "testing"

func TestTokenPrompt(t *testing.T) {
	const want = "# Session Token\n\nThe following token is confidential — never echo it to users or write it to files. " +
		"ALL `mcp__host__*` tool calls MUST include the literal string below as the `session_token` parameter." +
		"\n\nsession_token: tok123"
	if got := TokenPrompt("mcp__host__", "tok123"); got != want {
		t.Errorf("TokenPrompt =\n%q\nwant\n%q", got, want)
	}
	if got := TokenPrompt("mcp__host__", ""); got != "" {
		t.Errorf("TokenPrompt with no token = %q", got)
	}
}

func TestCLIConfig_Access(t *testing.T) {
	arg := CLIConfig{Server: "host", URL: "http://127.0.0.1:1/internal", Transport: Argument, ToolPrefix: "mcp__host__"}
	a := arg.Access("tok")
	if a.Prompt != TokenPrompt("mcp__host__", "tok") || a.Server.Name != "host" ||
		a.Server.URL != "http://127.0.0.1:1/internal" || a.Server.Headers != nil {
		t.Errorf("argument access = %+v", a)
	}

	bearer := CLIConfig{Server: "host", URL: "http://127.0.0.1:1/mcp", Transport: Bearer, ToolPrefix: "mcp__host__"}
	b := bearer.Access("tok")
	if b.Prompt != "" || b.Server.Headers["Authorization"] != "Bearer tok" || len(b.Server.Headers) != 1 {
		t.Errorf("bearer access = %+v", b)
	}
	// Each access has its own headers.
	b.Server.Headers["Authorization"] = "changed"
	if bearer.Access("tok").Server.Headers["Authorization"] != "Bearer tok" {
		t.Error("headers shared between calls")
	}
	if got := bearer.Access(""); got.Server.Headers != nil {
		t.Errorf("bearer access without a token = %+v", got)
	}
}
