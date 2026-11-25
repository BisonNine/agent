/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package mcphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// Errors returned by the Store.
var (
	// ErrEmptyToken: a token to register, or one a generator made, is empty.
	ErrEmptyToken = errors.New("empty token")
	// ErrTokenInUse: the token is already held by the store.
	ErrTokenInUse = errors.New("token already in use")
	// ErrExpired: the grant's expiry has already passed.
	ErrExpired = errors.New("grant already expired")
)

// Grant is what a token stands for: who calls with it, in which scope.
type Grant struct {
	// Scope is the caller's scope (agent, session, turn, depth).
	Scope hook.Scope
	// Value is the host's own per-turn data (a reply chat, an ask chain).
	// The store never inspects or copies it; it must not change once given.
	Value any
	// Expires, when not zero, is when the token stops working on its own.
	Expires time.Time
}

// record is one token's grant and the channel closed when it stops working.
type record struct {
	grant Grant
	done  chan struct{}
	timer *time.Timer // the expiry, or nil
}

// Store holds session tokens: an engine issues one for a CLI-model turn,
// scopes it, and revokes it when the turn ends or is cancelled. A revoked or
// expired token stops resolving at once, and the Done channel Resolve gave
// for it is closed, so calls already running with it are cancelled (see
// Caller.Done). The store keeps tokens in memory only; it is safe for
// concurrent use. Create one with NewStore.
type Store struct {
	logger *slog.Logger
	gen    func() (string, error)

	mu     sync.Mutex
	tokens map[string]*record
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithLogger sets the store's logger; records carry component=mcphost. The
// default discards. Token values are never logged.
func WithLogger(l *slog.Logger) StoreOption {
	return func(s *Store) {
		if l != nil {
			s.logger = l.With("component", component)
		}
	}
}

// WithGenerator makes Issue take its tokens from gen instead of the
// default (64 lowercase hex characters from crypto/rand), for a host whose
// tokens have a shape other programs depend on. gen must return
// unguessable tokens; an error or an empty token fails Issue.
func WithGenerator(gen func() (string, error)) StoreOption {
	return func(s *Store) {
		if gen != nil {
			s.gen = gen
		}
	}
}

// NewStore returns an empty store.
func NewStore(opts ...StoreOption) *Store {
	s := &Store{
		logger: discardLogger(),
		gen:    randomToken,
		tokens: make(map[string]*record),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// randomToken returns 32 random bytes as 64 lowercase hex characters.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Issue generates a new token standing for g and returns it.
func (s *Store) Issue(g Grant) (string, error) {
	token, err := s.gen()
	if err != nil {
		return "", fmt.Errorf("mcphost: generate token: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("mcphost: generate token: %w", ErrEmptyToken)
	}
	if err := s.add(token, g); err != nil {
		return "", err
	}
	s.logger.Info("Session token issued", "agent", g.Scope.AgentID, "session", g.Scope.Session)
	return token, nil
}

// Register stores a token the host chose (a fixed token for a test
// client) standing for g. It refuses an empty token and one already held.
func (s *Store) Register(token string, g Grant) error {
	if token == "" {
		return fmt.Errorf("mcphost: register token: %w", ErrEmptyToken)
	}
	if err := s.add(token, g); err != nil {
		return err
	}
	s.logger.Info("Session token registered", "agent", g.Scope.AgentID, "session", g.Scope.Session)
	return nil
}

// add stores token for g, arming its expiry.
func (s *Store) add(token string, g Grant) error {
	if !g.Expires.IsZero() && !time.Now().Before(g.Expires) {
		return fmt.Errorf("mcphost: %w", ErrExpired)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[token]; ok {
		return fmt.Errorf("mcphost: %w", ErrTokenInUse)
	}
	rec := &record{grant: g, done: make(chan struct{})}
	if !g.Expires.IsZero() {
		rec.timer = time.AfterFunc(time.Until(g.Expires), func() { s.expire(token, rec) })
	}
	s.tokens[token] = rec
	return nil
}

// expire drops token when its expiry fires, unless it was revoked (and
// perhaps registered again) meanwhile.
func (s *Store) expire(token string, rec *record) {
	s.mu.Lock()
	if s.tokens[token] != rec {
		s.mu.Unlock()
		return
	}
	s.dropLocked(token, rec)
	s.mu.Unlock()
	s.logger.Info("Session token expired", "agent", rec.grant.Scope.AgentID, "session", rec.grant.Scope.Session)
}

// dropLocked removes token and closes its done channel. Caller holds mu.
func (s *Store) dropLocked(token string, rec *record) {
	delete(s.tokens, token)
	if rec.timer != nil {
		rec.timer.Stop()
	}
	close(rec.done)
}

// Resolve returns the grant token stands for and a channel closed when the
// token stops working, or ok false for a token the store does not hold or
// one past its expiry (which it drops then, if its timer has not yet).
func (s *Store) Resolve(token string) (g Grant, done <-chan struct{}, ok bool) {
	s.mu.Lock()
	rec, held := s.tokens[token]
	if !held {
		s.mu.Unlock()
		return Grant{}, nil, false
	}
	if !rec.grant.Expires.IsZero() && !time.Now().Before(rec.grant.Expires) {
		s.dropLocked(token, rec)
		s.mu.Unlock()
		s.logger.Info("Session token expired", "agent", rec.grant.Scope.AgentID, "session", rec.grant.Scope.Session)
		return Grant{}, nil, false
	}
	s.mu.Unlock()
	return rec.grant, rec.done, true
}

// Rescope replaces the scope and host value of token's grant (a new turn on
// a session's token), keeping its expiry. Calls already running keep the
// scope they started with. It reports whether the store holds token.
func (s *Store) Rescope(token string, scope hook.Scope, value any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tokens[token]
	if !ok {
		return false
	}
	rec.grant.Scope = scope
	rec.grant.Value = value
	return true
}

// Revoke stops token working at once and reports whether the store held
// it.
func (s *Store) Revoke(token string) bool {
	s.mu.Lock()
	rec, ok := s.tokens[token]
	if ok {
		s.dropLocked(token, rec)
	}
	s.mu.Unlock()
	if ok {
		s.logger.Info("Session token revoked", "agent", rec.grant.Scope.AgentID, "session", rec.grant.Scope.Session)
	}
	return ok
}

// RevokeFunc revokes every token whose grant match reports true (every
// token of an agent, of a session) and returns how many. match is called
// with the store locked: it must not call the store.
func (s *Store) RevokeFunc(match func(Grant) bool) int {
	if match == nil {
		return 0
	}
	s.mu.Lock()
	var dropped []Grant
	for token, rec := range s.tokens {
		if match(rec.grant) {
			s.dropLocked(token, rec)
			dropped = append(dropped, rec.grant)
		}
	}
	s.mu.Unlock()
	for _, g := range dropped {
		s.logger.Info("Session token revoked", "agent", g.Scope.AgentID, "session", g.Scope.Session)
	}
	return len(dropped)
}

// Authenticator returns an Authenticator over the store alone: a token the
// store holds is the caller its grant names, served the set sets returns
// for the grant's scope; any other token, an empty one included, is refused
// with the error text refusal. A host with other credentials as well (long
// lived service tokens) writes its own Authenticator and calls Resolve.
// A nil sets serves no one.
func (s *Store) Authenticator(sets func(hook.Scope) (*toolkit.Set, bool), refusal string) Authenticator {
	return AuthenticatorFunc(func(_ context.Context, token string) (Caller, error) {
		g, done, ok := s.Resolve(token)
		if !ok || sets == nil {
			return Caller{}, errors.New(refusal)
		}
		set, ok := sets(g.Scope)
		if !ok || set == nil {
			return Caller{}, fmt.Errorf("agent %q has no tools", g.Scope.AgentID)
		}
		return Caller{Set: set, Scope: g.Scope, Value: g.Value, Done: done}, nil
	})
}
