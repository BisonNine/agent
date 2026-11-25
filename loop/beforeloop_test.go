/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

// BeforeLoop: the set-up a turn's model loop depends on, run once the
// message is stored and the turn marked pending, and whether the turn's
// model calls ask for partial text.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
)

// beforeLoopFixture is a turn for Alice on a scripted model that answers
// "done".
func beforeLoopFixture(t *testing.T) *engineFixture {
	t.Helper()
	p := modelstest.New().Script("alice-wire", modelstest.Reply("done"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	return newEngineFixture(t, chain, clk)
}

// streamedCalls reports, for each ModelCall of the turn, whether its
// request asked for partial text.
func streamedCalls(f *engineFixture) []bool {
	var out []bool
	for _, ev := range f.rec.all() {
		if c, ok := ev.(hook.ModelCall); ok {
			_, set := c.Request.Options[spawnllm.TextDeltaOption]
			out = append(out, set)
		}
	}
	return out
}

// loopEvents counts the turn's LoopStarted, ModelCallStarted and
// TurnFinished events.
func loopEvents(f *engineFixture) (loopStarted, callsStarted int, finished []hook.TurnFinished) {
	for _, ev := range f.rec.all() {
		switch e := ev.(type) {
		case hook.LoopStarted:
			loopStarted++
		case hook.ModelCallStarted:
			callsStarted++
		case hook.TurnFinished:
			finished = append(finished, e)
		}
	}
	return loopStarted, callsStarted, finished
}

// BeforeLoop runs after the pending marker is set and before LoopStarted,
// once; whether the turn streams starts as whether an OnTextDelta is
// registered, and is what BeforeLoop leaves it.
func TestEngine_BeforeLoop(t *testing.T) {
	t.Run("order", func(t *testing.T) {
		f := beforeLoopFixture(t)
		var calls int
		var storeAt []string
		var loopStartedBefore bool
		f.spec.hooks.add(hook.Plugin{Name: "setup", BeforeLoop: func(_ context.Context, loop *hook.Loop) error {
			calls++
			storeAt = f.store.events()
			loopStartedBefore = slices.ContainsFunc(f.rec.all(), func(ev hook.Event) bool {
				_, ok := ev.(hook.LoopStarted)
				return ok
			})
			if loop.Turn.Message.Text != "hello" {
				t.Errorf("BeforeLoop sees the message %q", loop.Turn.Message.Text)
			}
			return nil
		}})
		if _, _, err := f.run(t); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !slices.Equal(storeAt, []string{"pending"}) || loopStartedBefore {
			t.Fatalf("BeforeLoop ran %d times, store at it %v, LoopStarted before it %v", calls, storeAt, loopStartedBefore)
		}
		if n, _, _ := loopEvents(f); n != 1 {
			t.Fatalf("LoopStarted %d times", n)
		}
	})
	for _, tc := range []struct {
		name     string
		delta    bool // an OnTextDelta is registered
		wantInit bool // TextDeltas as BeforeLoop is given it
		set      *bool
		want     bool
	}{
		{name: "default with OnTextDelta", delta: true, wantInit: true, want: true},
		{name: "default without OnTextDelta", wantInit: false, want: false},
		{name: "turned off", delta: true, wantInit: true, set: new(false), want: false},
		{name: "turned on without OnTextDelta", wantInit: false, set: new(true), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := beforeLoopFixture(t)
			if tc.delta {
				f.spec.hooks.add(hook.Plugin{Name: "tap", OnTextDelta: func(context.Context, hook.TextDelta) {}})
			}
			var given bool
			f.spec.hooks.add(hook.Plugin{Name: "setup", BeforeLoop: func(_ context.Context, loop *hook.Loop) error {
				given = loop.TextDeltas
				if tc.set != nil {
					loop.TextDeltas = *tc.set
				}
				return nil
			}})
			if _, _, err := f.run(t); err != nil {
				t.Fatal(err)
			}
			if given != tc.wantInit {
				t.Fatalf("BeforeLoop given TextDeltas %v, want %v", given, tc.wantInit)
			}
			if got := streamedCalls(f); !slices.Equal(got, []bool{tc.want}) {
				t.Fatalf("calls asking for partial text = %v, want [%v]", got, tc.want)
			}
		})
	}
}

// A BeforeLoop that fails or panics ends the turn as a loop that failed
// before its first model call: no LoopStarted, no model call, the later
// BeforeLoop callbacks skipped, AfterTurn run, the marker cleared, one
// TurnFinished with OutcomeError, last.
func TestEngine_BeforeLoopFailure(t *testing.T) {
	bobsErr := errors.New("Bob's set-up failed")
	for _, tc := range []struct {
		name  string
		setup hook.BeforeLoopFunc
		check func(t *testing.T, err error)
	}{
		{
			name:  "error",
			setup: func(context.Context, *hook.Loop) error { return bobsErr },
			check: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, bobsErr) {
					t.Fatalf("error = %v, want BeforeLoop's", err)
				}
			},
		},
		{
			name:  "panic",
			setup: func(context.Context, *hook.Loop) error { panic("Bob's bug") },
			check: func(t *testing.T, err error) {
				t.Helper()
				perr, ok := errors.AsType[*hook.PanicError](err)
				if !ok || perr.Callback != "BeforeLoop" || perr.Plugin != "setup" || perr.Value != "Bob's bug" {
					t.Fatalf("error = %#v, want BeforeLoop's panic", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := beforeLoopFixture(t)
			var afterTurn []hook.Outcome
			var later bool
			f.spec.hooks.add(hook.Plugin{Name: "setup", BeforeLoop: tc.setup})
			f.spec.hooks.add(hook.Plugin{Name: "later", BeforeLoop: func(context.Context, *hook.Loop) error {
				later = true
				return nil
			}})
			f.spec.hooks.add(hook.Plugin{Name: "reply", AfterTurn: func(_ context.Context, _ *hook.Turn, r *hook.TurnResult) {
				afterTurn = append(afterTurn, r.Outcome)
			}})
			_, _, err := f.run(t)
			tc.check(t, err)
			if later {
				t.Fatal("a BeforeLoop after the failed one ran")
			}
			loopStarted, callsStarted, finished := loopEvents(f)
			if loopStarted != 0 || callsStarted != 0 {
				t.Fatalf("LoopStarted %d, ModelCallStarted %d; want none", loopStarted, callsStarted)
			}
			if len(finished) != 1 || finished[0].Outcome != hook.OutcomeError || !errors.Is(finished[0].Err, err) {
				t.Fatalf("TurnFinished = %+v", finished)
			}
			if kinds := f.rec.kinds(); kinds[len(kinds)-1] != "TurnFinished" {
				t.Fatalf("events = %v, want TurnFinished last", kinds)
			}
			if !slices.Equal(afterTurn, []hook.Outcome{hook.OutcomeError}) {
				t.Fatalf("AfterTurn saw %v", afterTurn)
			}
			if got := f.store.events(); !slices.Equal(got, []string{"pending", "cleared"}) {
				t.Fatalf("store = %v, want the marker set then cleared", got)
			}
		})
	}
}

// A BeforeLoop that finds the turn stopped by shutdown ends it with the
// outcome its cause decides, and the marker is kept for the restart.
func TestEngine_BeforeLoopShutdown(t *testing.T) {
	f := beforeLoopFixture(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.spec.hooks.add(hook.Plugin{Name: "setup", BeforeLoop: func(loopCtx context.Context, _ *hook.Loop) error {
		cancel(hook.ErrShutdown)
		return loopCtx.Err()
	}})
	turn := newEngineTurn(f.spec, nil)
	if _, err := turn.run(ctx); err == nil {
		t.Fatal("the turn did not fail")
	}
	_, _, finished := loopEvents(f)
	if len(finished) != 1 || finished[0].Outcome != hook.CauseOutcome(hook.ErrShutdown) {
		t.Fatalf("TurnFinished = %+v", finished)
	}
	if got := f.store.events(); !slices.Equal(got, []string{"pending"}) {
		t.Fatalf("store = %v, want the marker kept", got)
	}
}
