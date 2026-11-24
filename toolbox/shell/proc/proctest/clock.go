/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package proctest provides a fake proc.Clock for tests of the process
// manager and of the code built on it.
package proctest

import (
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
)

// Clock is a fake proc.Clock. Time stands still until Advance moves it;
// timers fire when Advance reaches them.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*timer
	changed chan struct{} // closed and replaced whenever the timer set changes
}

// NewClock returns a fake clock reading start.
func NewClock(start time.Time) *Clock {
	return &Clock{now: start, changed: make(chan struct{})}
}

type timer struct {
	c     *Clock
	at    time.Time
	ch    chan time.Time
	fired bool
}

// Now returns the fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer returns a timer that fires when the clock reaches now+d.
func (c *Clock) NewTimer(d time.Duration) proc.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{c: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		t.fired = true
		t.ch <- c.now
		return t
	}
	c.timers = append(c.timers, t)
	c.notifyLocked()
	return t
}

func (t *timer) C() <-chan time.Time { return t.ch }

func (t *timer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.fired {
		return false
	}
	t.fired = true
	t.c.removeLocked(t)
	return true
}

// Advance moves the clock forward by d and fires every timer now due.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.at.After(c.now) {
			kept = append(kept, t)
			continue
		}
		t.fired = true
		t.ch <- c.now
	}
	clear(c.timers[len(kept):])
	c.timers = kept
	c.notifyLocked()
}

// Timers returns the number of timers waiting to fire.
func (c *Clock) Timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// BlockTimeout is how long BlockUntil waits, in real time, before it fails
// the test.
const BlockTimeout = 10 * time.Second

// BlockUntil waits until at least n timers are waiting to fire, so a test
// can Advance only once the code under test has set its timer. It fails tb
// if that does not happen within BlockTimeout. Call it from the test's own
// goroutine.
func (c *Clock) BlockUntil(tb testing.TB, n int) {
	tb.Helper()
	deadline := time.NewTimer(BlockTimeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		have := len(c.timers)
		ch := c.changed
		c.mu.Unlock()
		if have >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline.C:
			tb.Fatalf("fake clock: %d timers waiting after %v, want %d", have, BlockTimeout, n)
			return
		}
	}
}

func (c *Clock) removeLocked(t *timer) {
	for i, x := range c.timers {
		if x == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			break
		}
	}
	c.notifyLocked()
}

func (c *Clock) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}
