/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package mcphost

import (
	"context"
	"errors"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// Transport is how an endpoint's clients carry their credential.
type Transport int

const (
	// Bearer: an "Authorization: Bearer <token>" header on every HTTP
	// request. A request without one, or with one the Authenticator refuses,
	// is answered 401 before it reaches MCP. Tool schemas are the tools' own.
	Bearer Transport = iota + 1
	// Argument: the token is the TokenArgument argument of every tool call.
	// Each published schema gains it as a required string property; it is
	// removed from the arguments before anything else sees them. tools/list
	// needs no credential and lists the endpoint's catalogue.
	Argument
)

// String returns "bearer" or "argument".
func (t Transport) String() string {
	switch t {
	case Bearer:
		return "bearer"
	case Argument:
		return "argument"
	default:
		return "unknown"
	}
}

// TokenArgument is the name of the tool argument that carries the token on
// an Argument endpoint.
const TokenArgument = "session_token"

// ErrRevoked is the cause a call's context is cancelled with when its
// credential stops working (Caller.Done) while the call runs.
var ErrRevoked = errors.New("credential revoked")

// Caller is who is calling, as the host's Authenticator decided it.
type Caller struct {
	// Set is the tools the caller is served: tools/list on a Bearer
	// endpoint lists them, and every call runs through it. Required.
	Set *toolkit.Set
	// Scope is put on every call's context with hook.WithScope.
	// Scope.AgentID names the calling agent.
	Scope hook.Scope
	// Value is the host's own data about the caller (a reply chat, an ask
	// chain, a home agent). mcphost never inspects or copies it; it is handed
	// back through CallerOf and AsyncResultFunc and must not change once
	// returned.
	Value any
	// Done, when not nil, is closed when the credential stops working
	// (revoked or expired). A call in flight then has its context cancelled
	// with ErrRevoked, and its result is not returned to the client; a Bearer
	// request in flight (a notification stream included) is ended.
	Done <-chan struct{}
}

// Authenticator decides who presents a token. It is the host's: mcphost
// implements no token format, hashing, service token or access policy. It
// is called with the bearer token of every request to a Bearer endpoint and
// with the TokenArgument of every call on an Argument endpoint; on an
// Argument endpoint the token may be empty (none was given), so the
// authenticator words that refusal too.
//
// A non-nil error refuses the caller. On an Argument endpoint its text is
// the tool error the client receives; on a Bearer endpoint the request is
// answered 401 "invalid bearer token". The error must not contain the token.
// It must be safe for concurrent use.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Caller, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(ctx context.Context, token string) (Caller, error)

// Authenticate calls f.
func (f AuthenticatorFunc) Authenticate(ctx context.Context, token string) (Caller, error) {
	return f(ctx, token)
}

type callerKey struct{}

// CallerOf returns the caller a tool call, a callback or an event of an
// endpoint is for. It is on the context of every tool call, of the
// endpoint's callbacks and of CallContext.
func CallerOf(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}
