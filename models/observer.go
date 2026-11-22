package models

import (
	"time"

	"go.bison9.com/agent/spawnllm"
)

// Observer receives the catalogue's events. A host raises its alerts from
// them, logs them, or ignores them. Each method is called after the state it
// reports has been recorded, with no lock held, and must not block.
//
// FellBack and Exhausted are called on the goroutine that made the call.
// Parked and Recovered are delivered per model key (provider and wire model)
// in the order the health changes they report happened: a Recovered always
// comes after the Parked it answers. To keep that order without a lock
// around the call, one goroutine at a time delivers a key's events, so one
// may arrive on the goroutine of another call to the same model.
type Observer interface {
	// Parked is called each time a failure takes a model out of rotation.
	Parked(e Parked)
	// Recovered is called when a model answers after one or more Parked
	// events for it.
	Recovered(e Recovered)
	// FellBack is called when a chain moves past one or more models to try
	// the next one.
	FellBack(e FellBack)
	// Exhausted is called when a chain call ends without an answer.
	Exhausted(e Exhausted)
}

// Parked reports a model taken out of rotation. Health is kept per provider
// and wire model (Key), so models that share both share it.
type Parked struct {
	Name     string // the model name that was called
	Provider string // its provider's name
	Model    string // its wire model id
	Key      string // spawnllm.ModelKey(Provider, Model): what health is kept by
	Reason   spawnllm.FailoverReason
	Status   int           // HTTP status of the failure
	Failures int           // consecutive failures, this one included
	Settled  bool          // past the short escalation steps: the category cooldown applies
	Cooldown time.Duration // how long it is parked
	Until    time.Time     // when it is tried again
	At       time.Time     // when the failure was recorded
}

// Recovered reports a model that answered after it had been parked.
type Recovered struct {
	Name     string
	Provider string
	Model    string
	Key      string
	Since    time.Time // the first Parked event since it last answered
	At       time.Time
}

// FellBack reports a chain moving on: Passed holds the models just passed,
// a single failed attempt or a run of consecutive skipped ones, and Next is
// the model that will be tried next.
type FellBack struct {
	Set     string
	Purpose Purpose
	Scope   Scope
	Passed  []Attempt
	Next    Candidate
}

// Exhausted reports a chain call that ended without an answer. Err is the
// error the call returned.
type Exhausted struct {
	Set      string
	Purpose  Purpose
	Scope    Scope
	Attempts []Attempt
	Err      error
}

type nopObserver struct{}

func (nopObserver) Parked(Parked)       {}
func (nopObserver) Recovered(Recovered) {}
func (nopObserver) FellBack(FellBack)   {}
func (nopObserver) Exhausted(Exhausted) {}
