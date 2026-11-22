package models

import (
	"runtime"
	"sync"
	"testing"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// sequence records the events of one key in the order they are delivered.
type sequence struct {
	nopObserver

	mu  sync.Mutex
	evs []string
}

func (s *sequence) Parked(Parked) {
	runtime.Gosched() // widen the window a late delivery would need
	s.mu.Lock()
	s.evs = append(s.evs, "P")
	s.mu.Unlock()
}

func (s *sequence) Recovered(Recovered) {
	s.mu.Lock()
	s.evs = append(s.evs, "R")
	s.mu.Unlock()
}

// Events for one model key are delivered in the order of the transitions
// they report: a Recovered always follows the Parked it answers.
func TestObserverOrderPerKey(t *testing.T) {
	obs := &sequence{}
	h := newHealth(clock.NewFake(epoch), newPolicy(modelspec.Cooldown{}), newParkedBook(), obs)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 500 {
				h.failure("m", "p", "m", spawnllm.FailoverRateLimit, 429, 0)
			}
		})
		wg.Go(func() {
			for range 500 {
				h.success("m", "p", "m")
			}
		})
	}
	wg.Wait()
	open := false
	for i, e := range obs.evs {
		switch e {
		case "P":
			open = true
		case "R":
			if !open {
				t.Fatalf("event %d: Recovered with no Parked before it: %v", i, obs.evs[max(0, i-5):i+1])
			}
			open = false
		}
	}
	if len(h.parked.queues) != 0 {
		t.Fatal("queues were left behind")
	}
}
