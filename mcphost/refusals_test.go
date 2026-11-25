/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package mcphost

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// A caller the host's Authenticator returns without a tool set, or with a
// credential already revoked, is refused; a refusal is passed on as it is.
func TestAuthenticate_RefusesIncompleteOrRevokedCallers(t *testing.T) {
	cat := toolkit.New()
	set := cat.Default()
	revoked := make(chan struct{})
	close(revoked)
	refused := errors.New("unknown token")
	scope := hook.Scope{AgentID: "alice", Session: "s1"}
	for _, tc := range []struct {
		name   string
		caller Caller
		err    error
		want   error
	}{
		{"refused", Caller{}, refused, refused},
		{"no tool set", Caller{Scope: scope}, nil, errNoTools},
		{"revoked", Caller{Set: set, Scope: scope, Done: revoked}, nil, ErrRevoked},
		{"accepted", Caller{Set: set, Scope: scope, Done: make(chan struct{})}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := AuthenticatorFunc(func(context.Context, string) (Caller, error) { return tc.caller, tc.err })
			ep, err := New(Config{Name: "n", Version: "1.0.0", Transport: Argument, Authenticator: auth, Catalogue: FromRegistry(cat)})
			if err != nil {
				t.Fatal(err)
			}
			defer ep.Close()
			c, err := ep.authenticate(context.Background(), "token")
			if !errors.Is(err, tc.want) || (tc.want == nil && c.Scope != scope) || (tc.want != nil && c.Set != nil) {
				t.Fatalf("authenticate = %+v, %v; want %v", c, err, tc.want)
			}
		})
	}
}

// A token past its expiry does not resolve even before its timer fires, and
// is dropped then; a timer firing for a token registered again since does
// not drop the new one.
func TestStore_ExpiryBeforeTimerAndStaleTimer(t *testing.T) {
	s := NewStore()
	old := &record{grant: Grant{Scope: hook.Scope{AgentID: "alice"}, Expires: time.Now().Add(-time.Second)}, done: make(chan struct{})}
	s.tokens["t1"] = old
	if _, _, ok := s.Resolve("t1"); ok {
		t.Fatal("a token past its expiry resolved")
	}
	if _, held := s.tokens["t1"]; held || !closed(old.done) {
		t.Fatal("a token past its expiry was not dropped")
	}

	if err := s.Register("t2", Grant{Scope: hook.Scope{AgentID: "bob"}}); err != nil {
		t.Fatal(err)
	}
	s.expire("t2", old) // a timer of an earlier grant under the same token
	if _, _, ok := s.Resolve("t2"); !ok {
		t.Fatal("a stale expiry dropped the token registered since")
	}
}
