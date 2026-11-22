package toolkit

import (
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

// setEvent is one change as seen by one set: the configuration before and
// after, and the subscribers it had when the change was made.
type setEvent struct {
	set            *Set
	oldCfg, newCfg *setConfig
	subs           []subscriber
}

// event is one committed change: the catalogue state before and after and
// the subscribed sets it may concern.
type event struct {
	oldState, newState *catState
	sets               []setEvent
}

// notifier delivers events in the order they were committed, one at a
// time, with no registry lock held. Whoever finds it idle delivers;
// a writer that finds it busy (another writer, or a subscriber writing
// back into the registry from its callback) leaves its event to the
// goroutine already delivering, so a callback may call the registry
// without deadlock.
type notifier struct {
	mu       sync.Mutex
	pending  []event
	draining bool
}

func (n *notifier) push(ev event) {
	n.mu.Lock()
	n.pending = append(n.pending, ev)
	n.mu.Unlock()
}

func (n *notifier) drain() {
	n.mu.Lock()
	if n.draining {
		n.mu.Unlock()
		return
	}
	n.draining = true
	for len(n.pending) > 0 {
		ev := n.pending[0]
		n.pending = n.pending[1:]
		n.mu.Unlock()
		deliver(ev)
		n.mu.Lock()
	}
	n.draining = false
	n.mu.Unlock()
}

// deliver hands each concerned subscriber its set's change, skipping sets
// the change did not touch and subscribers that have since unsubscribed.
func deliver(ev event) {
	for _, se := range ev.sets {
		ch := diff(se.set.offered(ev.oldState, se.oldCfg), se.set.offered(ev.newState, se.newCfg))
		if ch.Empty() {
			continue
		}
		for _, sub := range se.subs {
			if se.set.subscribed(sub.id) {
				sub.fn(ch)
			}
		}
	}
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
