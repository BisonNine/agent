package models

import (
	"sync"

	"go.bison9.com/agent/spawnllm"
)

// clientSlot holds one model's chat client: built on first use, shared by
// every call to the model, and kept across Adopt while the model's
// definition does not change. Calls in flight are counted, so a client that
// is no longer wanted (retired) is closed only once its last call has ended.
type clientSlot struct {
	build func() (spawnllm.LLMProvider, error)

	mu       sync.Mutex
	client   spawnllm.LLMProvider
	fixed    bool // registered by the host: never rebuilt
	closed   bool // a registered client has been closed
	inflight int
	retired  bool
}

func newSlot(build func() (spawnllm.LLMProvider, error)) *clientSlot {
	return &clientSlot{build: build}
}

func registeredSlot(c spawnllm.LLMProvider) *clientSlot {
	return &clientSlot{client: c, fixed: true}
}

// acquire returns the client, building it when needed, and counts a call on
// it. The returned release ends the call; it must be called exactly once.
func (s *clientSlot) acquire() (spawnllm.LLMProvider, func(), error) {
	s.mu.Lock()
	if s.client == nil && !s.fixed {
		c, err := s.build()
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		s.client = c
	}
	c := s.client
	s.inflight++
	s.mu.Unlock()
	var once sync.Once
	return c, func() { once.Do(s.release) }, nil
}

func (s *clientSlot) release() {
	s.mu.Lock()
	s.inflight--
	closing := s.takeIdleLocked()
	s.mu.Unlock()
	closeClient(closing)
}

// peek returns the client if it has been built, without counting a call.
func (s *clientSlot) peek() spawnllm.LLMProvider {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// retire marks the client as no longer wanted: it is closed now if no call is
// in flight, else when the last one ends. A call made after this (a turn
// still finishing on what it pinned) gets the client rebuilt, and closed
// again when it ends.
func (s *clientSlot) retire() {
	s.mu.Lock()
	s.retired = true
	closing := s.takeIdleLocked()
	s.mu.Unlock()
	closeClient(closing)
}

// takeIdleLocked returns the client to close, if the slot is retired and
// idle, and forgets it (a registered client is kept: it cannot be rebuilt).
func (s *clientSlot) takeIdleLocked() spawnllm.LLMProvider {
	if !s.retired || s.inflight > 0 || s.client == nil || s.closed {
		return nil
	}
	c := s.client
	if s.fixed {
		s.closed = true
	} else {
		s.client = nil
	}
	return c
}

func closeClient(c spawnllm.LLMProvider) {
	if st, ok := c.(spawnllm.StatefulProvider); ok {
		st.Close()
	}
}
