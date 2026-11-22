package mcphost

// CLIConfig is how a host's CLI models reach one of its endpoints. Every
// field is the host's own: mcphost knows no server name, URL or tool
// prefix.
type CLIConfig struct {
	// Server is the name the CLI registers the MCP server under.
	Server string
	// URL is the endpoint's full URL.
	URL string
	// Transport is the endpoint's transport.
	Transport Transport
	// ToolPrefix is the prefix the CLI shows the server's tools under, as
	// the model calls them (the CLI's own naming of a server's tools).
	ToolPrefix string
}

// CLIAccess is what a CLI model's turn needs to reach its tools.
type CLIAccess struct {
	// Prompt is the system-prompt text that gives the model its token, for
	// an Argument endpoint; empty for a Bearer endpoint or an empty token.
	Prompt string
	// Server is the MCP server entry to give the CLI for the turn.
	Server CLIServer
}

// CLIServer is one MCP server entry of a CLI's configuration, as generic
// values a host turns into the flags or file its CLI takes.
type CLIServer struct {
	Name string
	URL  string
	// Headers are the HTTP headers the CLI sends: Authorization for a
	// Bearer endpoint (a new map per Access), nil for an Argument endpoint
	// or an empty token.
	Headers map[string]string
}

// Access returns what a turn holding token needs: for an Argument endpoint
// the prompt text carrying the token and a server entry without headers,
// for a Bearer endpoint the server entry with the token in its
// Authorization header and no prompt text.
func (c CLIConfig) Access(token string) CLIAccess {
	a := CLIAccess{Server: CLIServer{Name: c.Server, URL: c.URL}}
	switch c.Transport {
	case Bearer:
		if token != "" {
			a.Server.Headers = map[string]string{"Authorization": "Bearer " + token}
		}
	case Argument:
		a.Prompt = TokenPrompt(c.ToolPrefix, token)
	}
	return a
}

// TokenPrompt is the system-prompt text that tells a model its token and
// that every call of a tool named toolPrefix* must carry it as the
// TokenArgument. It is "" for an empty token.
func TokenPrompt(toolPrefix, token string) string {
	if token == "" {
		return ""
	}
	return "# Session Token\n\nThe following token is confidential — never echo it to users or write it to files. " +
		"ALL `" + toolPrefix + "*` tool calls MUST include " +
		"the literal string below as the `" + TokenArgument + "` parameter.\n\n" + TokenArgument + ": " + token
}
