package hook

import "context"

type toolCallKey struct{}

// WithToolCall returns a child of ctx carrying call: the tool call a context
// runs. The engine (and mcphost) put it on the context a call's gate and tool
// run on, so a gate can name the tool the model called (call.Name), which may
// differ from the name it resolved to (toolkit.Info.Name).
func WithToolCall(ctx context.Context, call ToolCall) context.Context {
	return context.WithValue(ctx, toolCallKey{}, call)
}

// ToolCallFrom returns the tool call on ctx and whether there is one. Its
// Arguments are the call's: read them, do not change them.
func ToolCallFrom(ctx context.Context) (ToolCall, bool) {
	c, ok := ctx.Value(toolCallKey{}).(ToolCall)
	return c, ok
}
