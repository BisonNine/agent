package toolkit

import "context"

// Per-call values: who a tool call is for. They travel on the context so
// concurrent calls of one shared tool instance each see their own; the
// keys are unexported pointers, reachable only through these functions.
// Tools read them through the getters, so their storage can move (to one
// scope value for the whole turn) without touching any tool.

type ctxKey struct{ name string }

var (
	ctxKeyAgentID    = &ctxKey{"agentID"}
	ctxKeyChannel    = &ctxKey{"channel"}
	ctxKeyChatID     = &ctxKey{"chatID"}
	ctxKeySessionKey = &ctxKey{"sessionKey"}
)

// WithChat returns a child context carrying the channel and chat a call
// came from. The registry's Execute does not set them: the caller does,
// before calling.
func WithChat(ctx context.Context, channel, chatID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyChannel, channel)
	ctx = context.WithValue(ctx, ctxKeyChatID, chatID)
	return ctx
}

// Channel returns the channel on ctx, or "" when unset.
func Channel(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyChannel).(string); ok {
		return v
	}
	return ""
}

// ChatID returns the chat id on ctx, or "" when unset.
func ChatID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyChatID).(string); ok {
		return v
	}
	return ""
}

// WithSessionKey returns a child context carrying the session key. A tool
// that reads it must implement SessionScoped, so a dispatcher serving tools
// to an external client knows to set it.
func WithSessionKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxKeySessionKey, key)
}

// SessionKey returns the session key on ctx, or "" when unset.
func SessionKey(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeySessionKey).(string); ok {
		return v
	}
	return ""
}

// WithAgentID returns a child context carrying the id of the agent a call
// is made for.
func WithAgentID(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, ctxKeyAgentID, agentID)
}

// AgentID returns the agent id on ctx, or "" when unset.
func AgentID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyAgentID).(string); ok {
		return v
	}
	return ""
}
