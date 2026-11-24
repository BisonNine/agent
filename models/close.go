package models

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed reports a call on a catalogue after Close.
var ErrClosed = errors.New("models: this catalogue is closed")

// lifecycle counts the calls running on a catalogue (chain calls and client
// handles not yet released), so Close can wait for them.
type lifecycle struct {
	mu     sync.Mutex
	closed bool
	active int
	idle   chan struct{} // made by shut; closed once active reaches 0
}

// enter counts a call starting, or returns ErrClosed after Close.
func (l *lifecycle) enter() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	l.active++
	return nil
}

// leave ends a call enter counted.
func (l *lifecycle) leave() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.closed && l.active == 0 {
		close(l.idle)
	}
}

func (l *lifecycle) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// shut marks the catalogue closed. It returns a channel closed once no call
// is running, and false when it was already closed.
func (l *lifecycle) shut() (<-chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, false
	}
	l.closed = true
	l.idle = make(chan struct{})
	if l.active == 0 {
		close(l.idle)
	}
	return l.idle, true
}

// counted wraps a client's release so it also ends the call counted by enter.
func (c *Catalog) counted(release func(), err error) func() {
	if err != nil {
		c.life.leave()
		return nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			release()
			c.life.leave()
		})
	}
}

// Close shuts the catalogue down. Calls that start after it (Begin's chain
// calls, Client, Caller) and writes return ErrClosed. It waits until the
// calls in flight have ended and every client handle has been released, or
// until ctx ends, then closes every client the catalogue built, including
// clients replaced by an earlier Adopt that were still waiting for their last
// call. A client the host registered is never closed. When ctx ends first,
// Close returns ctx.Err(): idle clients are closed at once and those still in
// use once their last call ends.
//
// A second Close returns nil, as does closing a catalogue that was adopted
// into another.
func (c *Catalog) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.spent.Load() {
		c.mu.Unlock()
		return nil
	}
	idle, first := c.life.shut()
	c.mu.Unlock()
	if !first {
		return nil
	}

	var err error
	select {
	case <-idle:
	default:
		select {
		case <-idle:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}

	// Writes are refused now, so the contents no longer change. A client
	// retired by an earlier Adopt or write is closed by its last release.
	for _, e := range c.gen.Load().models {
		e.slot.retire()
	}
	return err
}
