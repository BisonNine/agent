/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"
)

// Change is what one change did to the tools a set offers, by internal
// name, each list sorted. Added and Removed are tools that appeared or
// went (a tool disabled or no longer granted counts as removed, one
// enabled or newly granted as added); Changed are tools still offered
// whose definition was replaced or that moved between listed and hidden.
type Change struct {
	Added   []string
	Removed []string
	Changed []string
}

// Empty reports whether the change touched nothing.
func (ch Change) Empty() bool {
	return len(ch.Added) == 0 && len(ch.Removed) == 0 && len(ch.Changed) == 0
}

// subscriber is one Subscribe callback.
type subscriber struct {
	id uint64
	fn func(Change)
}

// setEvent is one change as seen by one set, with the subscribers the set
// had when the change was committed.
type setEvent struct {
	set  *Set
	subs []subscriber
}

// event is one committed change: the catalogue generation before and after
// and the subscribed sets it may concern.
type event struct {
	oldState, newState *catState
	sets               []setEvent
}

// notifier delivers events in commit order, one at a time, on a goroutine
// of its own: writers only queue (under the catalogue's write lock) and
// never run a subscriber's callback, so a slow subscriber holds no writer.
// The goroutine is started when the queue becomes non-empty and exits when
// it is empty again. Each callback runs under recover: a subscriber that
// panics is logged and skipped, and the others still get the event.
type notifier struct {
	logger  *slog.Logger
	mu      sync.Mutex
	pending []event
	running bool
}

// push queues ev and starts the delivery goroutine if none is running.
// The caller holds the catalogue's write lock, so events queue in commit
// order.
func (n *notifier) push(ev event) {
	n.mu.Lock()
	n.pending = append(n.pending, ev)
	start := !n.running
	n.running = true
	n.mu.Unlock()
	if start {
		go n.run()
	}
}

// run delivers queued events until the queue is empty.
func (n *notifier) run() {
	exited := false
	defer func() {
		// Clear the flag however the loop ends, so a later push starts a
		// new goroutine instead of queueing behind a dead one.
		if !exited {
			n.mu.Lock()
			n.running = false
			n.mu.Unlock()
		}
	}()
	for {
		n.mu.Lock()
		if len(n.pending) == 0 {
			n.running = false
			exited = true
			n.mu.Unlock()
			return
		}
		ev := n.pending[0]
		n.pending[0] = event{}
		n.pending = n.pending[1:]
		n.mu.Unlock()
		n.deliver(ev)
	}
}

// deliver hands each concerned subscriber its set's change, skipping sets
// the change did not touch and subscribers that have since unsubscribed.
func (n *notifier) deliver(ev event) {
	for _, se := range ev.sets {
		ch, ok := n.change(ev, se.set)
		if !ok || ch.Empty() {
			continue
		}
		for _, sub := range se.subs {
			if se.set.subscribed(sub.id) {
				n.call(sub.fn, ch)
			}
		}
	}
}

// change computes what ev did to set. It asks the set's AllowChecker, host
// code, so a panic there is logged and the set skipped.
func (n *notifier) change(ev event, s *Set) (ch Change, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			n.logger.Error("Tool grant check panicked while computing a change notice", "panic", fmt.Sprint(r))
			ok = false
		}
	}()
	before := s.offered(ev.oldState, ev.oldState.cfg(s.key))
	after := s.offered(ev.newState, ev.newState.cfg(s.key))
	return diff(before, after), true
}

// call runs one subscriber's callback, logging a panic instead of letting
// it end delivery.
func (n *notifier) call(fn func(Change), ch Change) {
	defer func() {
		if r := recover(); r != nil {
			n.logger.Error("Tool change subscriber panicked", "panic", fmt.Sprint(r))
		}
	}()
	fn(ch)
}

// diff compares two offered sets.
func diff(before, after map[string]*entry) Change {
	var ch Change
	for name, a := range after {
		b, ok := before[name]
		switch {
		case !ok:
			ch.Added = append(ch.Added, name)
		case a != b:
			ch.Changed = append(ch.Changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			ch.Removed = append(ch.Removed, name)
		}
	}
	slices.Sort(ch.Added)
	slices.Sort(ch.Removed)
	slices.Sort(ch.Changed)
	return ch
}
