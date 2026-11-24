package mcphost

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

var aliceTurn = hook.Scope{AgentID: "alice", Session: "s-1", TurnID: "t-1", Depth: 2}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestStore_IssueResolve(t *testing.T) {
	s := NewStore()
	seen := map[string]bool{}
	hexToken := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for range 100 {
		tok, err := s.Issue(Grant{Scope: aliceTurn})
		if err != nil {
			t.Fatal(err)
		}
		if !hexToken.MatchString(tok) {
			t.Fatalf("token %q is not 64 hex characters", tok)
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
	tok, err := s.Issue(Grant{Scope: aliceTurn, Value: "reply-chat"})
	if err != nil {
		t.Fatal(err)
	}
	g, done, ok := s.Resolve(tok)
	if !ok || g.Scope != aliceTurn || g.Value != "reply-chat" || done == nil || closed(done) {
		t.Fatalf("Resolve = %+v, %v, %v", g, done, ok)
	}
	for _, bad := range []string{"", "nope", strings.ToUpper(tok)} {
		if _, _, ok := s.Resolve(bad); ok {
			t.Errorf("Resolve(%q) succeeded", bad)
		}
	}
}

func TestStore_Revoke(t *testing.T) {
	s := NewStore()
	tok := issue(t, s, Grant{Scope: aliceTurn})
	_, done, _ := s.Resolve(tok)
	if !s.Revoke(tok) {
		t.Fatal("Revoke reported the token unknown")
	}
	if !closed(done) {
		t.Error("done not closed by Revoke")
	}
	if _, _, ok := s.Resolve(tok); ok {
		t.Error("a revoked token resolves")
	}
	if s.Revoke(tok) || s.Revoke("nope") {
		t.Error("Revoke of an unknown token reported true")
	}
}

func TestStore_RevokeFunc(t *testing.T) {
	s := NewStore()
	a1 := issue(t, s, Grant{Scope: hook.Scope{AgentID: "alice", Session: "s1"}})
	a2 := issue(t, s, Grant{Scope: hook.Scope{AgentID: "alice", Session: "s2"}})
	b1 := issue(t, s, Grant{Scope: hook.Scope{AgentID: "bob", Session: "s3"}})
	if n := s.RevokeFunc(func(g Grant) bool { return g.Scope.AgentID == "alice" }); n != 2 {
		t.Errorf("RevokeFunc revoked %d, want 2", n)
	}
	for _, tok := range []string{a1, a2} {
		if _, _, ok := s.Resolve(tok); ok {
			t.Error("Alice's token survived")
		}
	}
	if _, _, ok := s.Resolve(b1); !ok {
		t.Error("Bob's token was revoked")
	}
	if s.RevokeFunc(nil) != 0 {
		t.Error("RevokeFunc(nil) revoked something")
	}
}

func TestStore_Rescope(t *testing.T) {
	s := NewStore()
	exp := time.Now().Add(time.Hour)
	tok := issue(t, s, Grant{Scope: hook.Scope{AgentID: "alice", Depth: 3}, Expires: exp})
	next := hook.Scope{AgentID: "alice", TurnID: "t-2"}
	if !s.Rescope(tok, next, "chat-2") {
		t.Fatal("Rescope reported the token unknown")
	}
	g, _, _ := s.Resolve(tok)
	if g.Scope != next || g.Value != "chat-2" || !g.Expires.Equal(exp) {
		t.Errorf("after Rescope = %+v", g)
	}
	if s.Rescope("nope", next, nil) {
		t.Error("Rescope of an unknown token reported true")
	}
}

func TestStore_Expiry(t *testing.T) {
	s := NewStore()
	tok, err := s.Issue(Grant{Scope: aliceTurn, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Resolve(tok); !ok {
		t.Fatal("token does not resolve before its expiry")
	}
	tok = issue(t, s, Grant{Scope: aliceTurn, Expires: time.Now().Add(20 * time.Millisecond)})
	_, done, ok := s.Resolve(tok)
	if !ok {
		// A slow runner may be past the expiry already: then it is dropped.
		_, done, _ = s.Resolve(tok)
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done not closed at expiry")
	}
	if _, _, ok := s.Resolve(tok); ok {
		t.Error("an expired token resolves")
	}
	if _, err := s.Issue(Grant{Expires: time.Now().Add(-time.Second)}); !errors.Is(err, ErrExpired) {
		t.Errorf("Issue of an expired grant = %v, want ErrExpired", err)
	}
	// Revoked before its expiry and registered again: the old timer does
	// not drop the new grant.
	if err := s.Register("reused", Grant{Scope: aliceTurn, Expires: time.Now().Add(20 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	_, oldDone, _ := s.Resolve("reused")
	s.Revoke("reused")
	if err := s.Register("reused", Grant{Scope: aliceTurn}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, ok := s.Resolve("reused"); !ok {
		t.Error("an old expiry timer dropped a token registered again")
	}
	if oldDone != nil && !closed(oldDone) {
		t.Error("the revoked grant's done is open")
	}
}

func TestStore_Register(t *testing.T) {
	s := NewStore()
	if err := s.Register("fixed-test-token", Grant{Scope: aliceTurn}); err != nil {
		t.Fatal(err)
	}
	if g, _, ok := s.Resolve("fixed-test-token"); !ok || g.Scope != aliceTurn {
		t.Errorf("registered token = %+v, %v", g, ok)
	}
	if err := s.Register("fixed-test-token", Grant{}); !errors.Is(err, ErrTokenInUse) {
		t.Errorf("second Register = %v, want ErrTokenInUse", err)
	}
	if err := s.Register("", Grant{}); !errors.Is(err, ErrEmptyToken) {
		t.Errorf("Register(\"\") = %v, want ErrEmptyToken", err)
	}
}

func TestStore_Generator(t *testing.T) {
	n := 0
	s := NewStore(WithGenerator(func() (string, error) {
		n++
		return "HOST" + strings.Repeat("a", n), nil
	}))
	tok, err := s.Issue(Grant{Scope: aliceTurn})
	if err != nil || tok != "HOSTa" {
		t.Fatalf("Issue = %q, %v", tok, err)
	}
	failing := NewStore(WithGenerator(func() (string, error) { return "", errors.New("no entropy") }))
	if _, err := failing.Issue(Grant{}); err == nil {
		t.Error("a failing generator issued a token")
	}
	empty := NewStore(WithGenerator(func() (string, error) { return "", nil }))
	if _, err := empty.Issue(Grant{}); !errors.Is(err, ErrEmptyToken) {
		t.Errorf("an empty generated token = %v, want ErrEmptyToken", err)
	}
	same := NewStore(WithGenerator(func() (string, error) { return "same", nil }))
	if _, err := same.Issue(Grant{}); err != nil {
		t.Fatal(err)
	}
	if _, err := same.Issue(Grant{}); !errors.Is(err, ErrTokenInUse) {
		t.Errorf("a repeated generated token = %v, want ErrTokenInUse", err)
	}
}

func TestStore_Concurrent(t *testing.T) {
	s := NewStore()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			scope := hook.Scope{AgentID: "alice", Depth: i}
			for range 50 {
				tok, err := s.Issue(Grant{Scope: scope})
				if err != nil {
					t.Error(err)
					return
				}
				if g, _, ok := s.Resolve(tok); !ok || g.Scope != scope {
					t.Error("resolved another grant")
				}
				s.Rescope(tok, scope, i)
				s.Revoke(tok)
			}
		})
	}
	wg.Wait()
}

func TestStore_Authenticator(t *testing.T) {
	s := NewStore()
	set := toolkit.New().Set("alice")
	auth := s.Authenticator(func(sc hook.Scope) (*toolkit.Set, bool) {
		return set, sc.AgentID == "alice"
	}, "bad token")
	tok := issue(t, s, Grant{Scope: aliceTurn, Value: 7})
	c, err := auth.Authenticate(context.Background(), tok)
	if err != nil || c.Set != set || c.Scope != aliceTurn || c.Value != 7 || c.Done == nil {
		t.Fatalf("Authenticate = %+v, %v", c, err)
	}
	if _, err := auth.Authenticate(context.Background(), "nope"); err == nil || err.Error() != "bad token" {
		t.Errorf("unknown token: %v", err)
	}
	bobTok := issue(t, s, Grant{Scope: hook.Scope{AgentID: "bob"}})
	if _, err := auth.Authenticate(context.Background(), bobTok); err == nil || !strings.Contains(err.Error(), `"bob"`) {
		t.Errorf("agent without a set: %v", err)
	}
}

func issue(t *testing.T, s *Store, g Grant) string {
	t.Helper()
	tok, err := s.Issue(g)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
