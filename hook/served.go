package hook

import "context"

// ServedTools is how an agent gives a CLI model its tools. A CLI model runs
// its own tool loop, so the engine never executes a tool for it: the tools are
// served to it (over MCP, by the mcphost package) and reached with a
// credential. For each turn whose model is a CLI, the engine asks ServeTurn
// for a credential scoped to the turn, puts the prompt text it returns into
// the turn's system prompt (after the summary), and calls end when the turn
// ends or is interrupted or cancelled, whichever comes first, so a stopped
// turn's credential stops working at once. end may be called more than once;
// only the first call counts.
type ServedTools interface {
	// ServeTurn issues the turn's credential for s and returns the prompt text
	// that hands it to the model ("" for none) and the function that revokes
	// it. An error is logged and the turn runs without served tools.
	ServeTurn(ctx context.Context, s Scope) (prompt string, end func(), err error)
}
