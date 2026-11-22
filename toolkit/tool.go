package toolkit

import "context"

// Tool is the interface every tool implements.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args map[string]any) *Result
}

// AsyncCallback is how an async tool reports completion. ctx lets the
// callback be cancelled when the agent shuts down; result is the tool's
// final result.
type AsyncCallback func(ctx context.Context, result *Result)

// AsyncExecutor is an optional interface for tools that finish later and
// report through a callback. The callback is a parameter of ExecuteAsync,
// never state on the tool, so concurrent calls cannot overwrite each
// other's callbacks on a shared tool instance.
//
// Example:
//
//	func (t *exampleTool) ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *Result {
//	    go func() {
//	        result := t.runWork(ctx, args)
//	        if cb != nil { cb(ctx, result) }
//	    }()
//	    return AsyncResult("Work started, will report back")
//	}
type AsyncExecutor interface {
	Tool
	// ExecuteAsync runs the tool asynchronously. cb is invoked, possibly
	// from another goroutine, when the work completes. The registry calls
	// ExecuteAsync only with a non-nil cb.
	ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *Result
}

// SessionScoped is an optional interface for tools that need the session
// key on their context (see WithSessionKey and SessionKey). A dispatcher
// that serves tools to an external client (an MCP host) checks for it and
// injects the session key; a tool that calls SessionKey must implement it.
type SessionScoped interface {
	IsSessionScoped() bool
}

// ExternalNamer is implemented by tools that are offered to models and
// external clients under a different name from their registry name (an
// upstream MCP tool registered as "mcp_<server>_<tool>" and offered as
// "<server>_<tool>"). The registry dispatches a call by either name and
// offers the external one unless it would shadow another tool (see
// Info.Published).
type ExternalNamer interface {
	ExternalName() string
}

// DiscoveryGrouped is implemented by tools that belong to a discovery
// group: the definitions FromSpec wraps report toolspec's Group and
// RevealTogether through it.
type DiscoveryGrouped interface {
	DiscoveryGroup() (group string, revealTogether bool)
}

// ToolToSchema returns tool's definition in the OpenAI function form
// ({type: function, function: {name, description, parameters}}), with
// free-form object parameters declared open (see OpenObjectProperties).
func ToolToSchema(tool Tool) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"parameters":  OpenObjectProperties(tool.Parameters()),
		},
	}
}
