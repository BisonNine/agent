/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models

import (
	"sort"
	"sync"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

const (
	// failureWindow resets a model's failure counts when it has not failed
	// for this long.
	failureWindow = 24 * time.Hour

	// maxRetryAfterCooldown caps a server-supplied Retry-After, so a
	// misbehaving upstream cannot park a model for hours.
	maxRetryAfterCooldown = 5 * time.Minute

	// maxSameModelRetryWait bounds how long a call waits on a Retry-After
	// hint before retrying the same model once. A longer hint moves on to
	// the next model at once (the cooldown still records it).
	maxSameModelRetryWait = 3 * time.Second
)

// escalation is the cooldown of a model's first consecutive failures, before
// the category cooldown of its HTTP status applies: failure 1 → 1m, 2 → 3m,
// 3 → 5m, 4 and later → the category's. A transient blip costs a short wait;
// a model that keeps failing escalates into the full category cooldown.
var escalation = []time.Duration{1 * time.Minute, 3 * time.Minute, 5 * time.Minute}

// policy is a modelspec.Cooldown in durations.
type policy struct {
	billingAuth, rateLimit, badRequest, clientError, serverError time.Duration
}

func newPolicy(c modelspec.Cooldown) policy {
	return policy{
		billingAuth: c.BillingAuth(),
		rateLimit:   c.RateLimit(),
		badRequest:  c.BadRequest(),
		clientError: c.ClientError(),
		serverError: c.ServerError(),
	}
}

// category returns the settled cooldown for an HTTP status, or 0 when the
// status never cools: 413 (fixed by compaction, not by waiting) and anything
// that is not an HTTP error (e.g. a network error).
func (p policy) category(status int) time.Duration {
	switch {
	case status == 413:
		return 0
	case status == 401 || status == 402 || status == 403:
		return p.billingAuth
	case status == 429:
		return p.rateLimit
	case status == 400:
		return p.badRequest
	case status >= 400 && status < 500:
		return p.clientError
	case status >= 500 && status < 600:
		return p.serverError
	default:
		return 0
	}
}

// forFailure returns the cooldown of the n-th consecutive failure with the
// given status. Billing and auth failures do not recover in minutes, so they
// skip the escalation and take the category cooldown at once.
func (p policy) forFailure(n, status int) time.Duration {
	cat := p.category(status)
	if cat == 0 {
		return 0
	}
	if status == 401 || status == 402 || status == 403 {
		return cat
	}
	if n >= 1 && n <= len(escalation) {
		return escalation[n-1]
	}
	return cat
}

// Health is one model's state.
type Health struct {
	Name      string // model name
	Provider  string // provider name
	Model     string // wire model id
	Key       string // what health is kept by: models sharing it share their health
	Available bool   // not parked
	Until     time.Time
	Since     time.Time // the last failure
	Reason    spawnllm.FailoverReason
	Failures  int // consecutive failures
}

// healthEntry is one key's cooldown state.
type healthEntry struct {
	failures   int
	byReason   map[spawnllm.FailoverReason]int
	until      time.Time
	lastReason spawnllm.FailoverReason
	last       time.Time
}

// health tracks cooldowns per spawnllm.ModelKey, so a rate limit on one model
// does not block a sibling on the same provider. It belongs to one generation
// of a catalogue: Adopt starts the next with none. The parked book outlives
// it, so a model reported parked is reported recovered after an Adopt too.
// It sends Parked and Recovered to obs, through the book (see parkedBook).
type health struct {
	clock  clock.Clock
	policy policy
	parked *parkedBook
	obs    Observer

	mu      sync.RWMutex
	entries map[string]*healthEntry
}

func newHealth(c clock.Clock, p policy, book *parkedBook, obs Observer) *health {
	if obs == nil {
		obs = nopObserver{}
	}
	return &health{clock: clock.Or(c), policy: p, parked: book, obs: obs, entries: make(map[string]*healthEntry)}
}

// failure records a failure of key and, if it parked the model, sends the
// Parked event (and returns it).
func (h *health) failure(name, provider, model string, reason spawnllm.FailoverReason, status int, retryAfter time.Duration) (Parked, bool) {
	key := spawnllm.ModelKey(provider, model)
	ev, ok := h.recordFailure(name, provider, model, key, reason, status, retryAfter)
	h.parked.deliver(key)
	return ev, ok
}

func (h *health) recordFailure(name, provider, model, key string, reason spawnllm.FailoverReason, status int, retryAfter time.Duration) (Parked, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.clock.Now()
	e := h.entries[key]
	if e == nil {
		e = &healthEntry{byReason: make(map[spawnllm.FailoverReason]int)}
		h.entries[key] = e
	}
	if !e.last.IsZero() && now.Sub(e.last) > failureWindow {
		e.failures = 0
		e.byReason = make(map[spawnllm.FailoverReason]int)
	}
	if h.policy.category(status) == 0 {
		return Parked{}, false
	}
	e.failures++
	e.byReason[reason]++
	e.lastReason = reason
	e.last = now
	cooldown := h.policy.forFailure(e.failures, status)
	if retryAfter > 0 {
		cooldown = min(retryAfter, maxRetryAfterCooldown)
	}
	e.until = now.Add(cooldown)
	ev := Parked{
		Name: name, Provider: provider, Model: model, Key: key,
		Reason: reason, Status: status, Failures: e.failures,
		Settled:  e.failures > len(escalation),
		Cooldown: cooldown, Until: e.until, At: now,
	}
	obs := h.obs
	h.parked.park(key, now, func() { obs.Parked(ev) })
	return ev, true
}

// success clears key's failures and, if it had been reported parked, sends
// the Recovered event (and returns it).
func (h *health) success(name, provider, model string) (Recovered, bool) {
	key := spawnllm.ModelKey(provider, model)
	h.mu.Lock()
	if e := h.entries[key]; e != nil {
		e.failures = 0
		e.byReason = make(map[spawnllm.FailoverReason]int)
		e.until = time.Time{}
		e.lastReason = ""
	}
	now := h.clock.Now()
	var ev Recovered
	obs := h.obs
	// Queued under h.mu, like a failure's Parked, so the two are queued in
	// the order the health changed.
	_, ok := h.parked.recover(key, func(since time.Time) func() {
		ev = Recovered{Name: name, Provider: provider, Model: model, Key: key, Since: since, At: now}
		return func() { obs.Recovered(ev) }
	})
	h.mu.Unlock()
	h.parked.deliver(key)
	return ev, ok
}

// parkedFor returns how long key stays parked (0 when available) and why.
func (h *health) parkedFor(provider, model string) (time.Duration, spawnllm.FailoverReason) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e := h.entries[spawnllm.ModelKey(provider, model)]
	if e == nil {
		return 0, ""
	}
	now := h.clock.Now()
	if e.until.IsZero() || !now.Before(e.until) {
		return 0, ""
	}
	return e.until.Sub(now), e.reason()
}

func (e *healthEntry) reason() spawnllm.FailoverReason {
	if e.lastReason != "" {
		return e.lastReason
	}
	var top spawnllm.FailoverReason
	best := 0
	for r, n := range e.byReason {
		if n > best {
			best, top = n, r
		}
	}
	return top
}

// state returns the Health of one model.
func (h *health) state(name, provider, model string) Health {
	key := spawnllm.ModelKey(provider, model)
	out := Health{Name: name, Provider: provider, Model: model, Key: key, Available: true}
	h.mu.RLock()
	defer h.mu.RUnlock()
	e := h.entries[key]
	if e == nil {
		return out
	}
	out.Failures = e.failures
	out.Since = e.last
	if !e.until.IsZero() && h.clock.Now().Before(e.until) {
		out.Available = false
		out.Until = e.until
		out.Reason = e.reason()
	}
	return out
}

// clear removes the state of the given keys, or of every key when none is
// given, and returns how many entries it removed. The parked book is left as
// it is, so a model reported parked is still reported when it answers.
func (h *health) clear(keys []string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(keys) == 0 {
		n := len(h.entries)
		h.entries = make(map[string]*healthEntry)
		return n
	}
	n := 0
	for _, k := range keys {
		if _, ok := h.entries[k]; ok {
			delete(h.entries, k)
			n++
		}
	}
	return n
}

// parkedBook remembers, per key, when a model was first reported parked
// since it last answered, so its next answer is reported as Recovered. It is
// shared by every generation of a catalogue.
//
// It also orders the Parked and Recovered events of each key. An event is
// queued under the book's lock in the same step as the transition it
// reports, so the queue is in transition order; deliver then hands the
// queue to the observer with no lock held. One goroutine at a time drains a
// key's queue, so a key's events arrive in order, possibly on another
// goroutine than the call that caused them.
type parkedBook struct {
	mu     sync.Mutex
	since  map[string]time.Time
	queues map[string]*eventQueue
}

type eventQueue struct {
	events   []func()
	draining bool
}

func newParkedBook() *parkedBook {
	return &parkedBook{since: make(map[string]time.Time), queues: make(map[string]*eventQueue)}
}

// park records that key was parked at at (the first time since it last
// answered) and queues ev.
func (b *parkedBook) park(key string, at time.Time, ev func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.since[key]; !ok {
		b.since[key] = at
	}
	b.queueLocked(key, ev)
}

// recover clears key's parked record; when there was one, it queues the event
// mk makes from the time it was first parked.
func (b *parkedBook) recover(key string, mk func(since time.Time) func()) (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	since, ok := b.since[key]
	if !ok {
		return time.Time{}, false
	}
	delete(b.since, key)
	if mk != nil {
		b.queueLocked(key, mk(since))
	}
	return since, true
}

func (b *parkedBook) queueLocked(key string, ev func()) {
	q := b.queues[key]
	if q == nil {
		q = &eventQueue{}
		b.queues[key] = q
	}
	q.events = append(q.events, ev)
}

// deliver sends key's queued events in order, unless another goroutine is
// already doing so (it will send them too). No lock is held while an event
// is sent.
func (b *parkedBook) deliver(key string) {
	b.mu.Lock()
	q := b.queues[key]
	if q == nil || q.draining {
		b.mu.Unlock()
		return
	}
	q.draining = true
	for len(q.events) > 0 {
		ev := q.events[0]
		q.events = q.events[1:]
		b.mu.Unlock()
		b.send(key, q, ev)
		b.mu.Lock()
	}
	q.draining = false
	delete(b.queues, key)
	b.mu.Unlock()
}

// send calls ev; if it panics, the queue is released, and any events left
// are handed to another goroutine, before the panic goes on.
func (b *parkedBook) send(key string, q *eventQueue, ev func()) {
	defer func() {
		if r := recover(); r != nil {
			b.mu.Lock()
			q.draining = false
			left := len(q.events) > 0
			if !left {
				delete(b.queues, key)
			}
			b.mu.Unlock()
			if left {
				go b.deliver(key)
			}
			panic(r)
		}
	}()
	ev()
}

// sortHealth orders a Health list by model name.
func sortHealth(list []Health) {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
}
