package loop

import (
	"context"
	"iter"
	"sync"

	"go.bison9.com/agent/hook"
)

// run is one Run: its message, its place in the session's queue or its
// turn, and the events it has emitted, held until they are read.
type run struct {
	a       *Agent
	ctx     context.Context
	session string
	msg     hook.Message
	opts    hook.RunOptions

	// The run's state, guarded by the agent's lock: started once it left the
	// queue; cancel stops its turn; turnID is its turn's; ended once its
	// turn returned; served revokes the turn's served-tools credential.
	started bool
	ended   bool
	cancel  context.CancelCauseFunc
	turnCtx context.Context //nolint:containedctx // the turn's context, made when the run starts
	turnID  string
	served  func()

	mu       sync.Mutex
	events   []hook.Event
	next     int
	wake     chan struct{}
	finished bool
	result   hook.TurnResult
	done     chan struct{}
}

func newRun(a *Agent, ctx context.Context, session string, msg hook.Message, opts hook.RunOptions) *run {
	return &run{a: a, ctx: ctx, session: session, msg: msg, opts: opts, wake: make(chan struct{}), done: make(chan struct{})}
}

var _ hook.Run = (*run)(nil)

// add holds ev for Events; a run WithoutEvents holds only its TurnFinished.
// Nothing is added after TurnFinished, which also closes Done.
func (r *run) add(_ context.Context, ev hook.Event) {
	tf, last := ev.(hook.TurnFinished)
	if last {
		// The run is no longer running before anyone can read its
		// TurnFinished: an Interrupt from then on stops nothing of it.
		r.a.mu.Lock()
		r.ended = true
		r.a.mu.Unlock()
	}
	if r.opts.NoEvents && !last {
		return
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return
	}
	r.events = append(r.events, ev)
	if last {
		r.finished = true
		r.result = tf.TurnResult
	}
	close(r.wake)
	r.wake = make(chan struct{})
	r.mu.Unlock()
	if last {
		close(r.done)
	}
}

// Events yields the run's events not yet yielded, waiting for more until
// TurnFinished.
func (r *run) Events() iter.Seq2[hook.Event, error] {
	return func(yield func(hook.Event, error) bool) {
		for {
			r.mu.Lock()
			if r.next < len(r.events) {
				ev := r.events[r.next]
				r.events[r.next] = nil
				r.next++
				r.mu.Unlock()
				var err error
				tf, last := ev.(hook.TurnFinished)
				if last {
					err = tf.Err
				}
				if !yield(ev, err) || last {
					return
				}
				continue
			}
			if r.finished {
				r.mu.Unlock()
				return
			}
			wake := r.wake
			r.mu.Unlock()
			<-wake
		}
	}
}

// Interrupt stops this run's turn, or drops it from the queue.
func (r *run) Interrupt() { r.a.stopRun(r, hook.ErrInterrupted, false) }

// Cancel stops this run's turn and the tasks it started.
func (r *run) Cancel(cause error) {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	r.a.stopRun(r, cause, true)
}

// Done is closed once TurnFinished has been emitted.
func (r *run) Done() <-chan struct{} { return r.done }

// Wait blocks until the turn finished and returns its result.
func (r *run) Wait() hook.TurnResult {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}
