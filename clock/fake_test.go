package clock

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestOrDefaultsToReal(t *testing.T) {
	if Or(nil) != Real {
		t.Fatal("Or(nil) is not Real")
	}
	f := NewFake(epoch)
	if Or(f) != f {
		t.Fatal("Or(f) is not f")
	}
}

func TestFakeTimerFiresOnlyWhenDue(t *testing.T) {
	f := NewFake(epoch)
	tm := f.NewTimer(time.Second)
	f.Advance(999 * time.Millisecond)
	select {
	case <-tm.C():
		t.Fatal("timer fired early")
	default:
	}
	f.Advance(time.Millisecond)
	select {
	case at := <-tm.C():
		if !at.Equal(epoch.Add(time.Second)) {
			t.Fatalf("fired at %v, want %v", at, epoch.Add(time.Second))
		}
	default:
		t.Fatal("timer did not fire when due")
	}
	if f.Waiters() != 0 {
		t.Fatalf("waiters = %d after the timer fired", f.Waiters())
	}
}

func TestFakeTimerStopAndReset(t *testing.T) {
	f := NewFake(epoch)
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop of an armed timer reported false")
	}
	f.Advance(time.Hour)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tm.Reset(time.Minute) {
		t.Fatal("Reset of a stopped timer reported it active")
	}
	f.Advance(time.Minute)
	select {
	case <-tm.C():
	default:
		t.Fatal("reset timer did not fire")
	}
}

func TestFakeStopDropsUndeliveredValue(t *testing.T) {
	f := NewFake(epoch)
	tm := f.NewTimer(time.Second)
	f.Advance(time.Second)
	tm.Stop()
	select {
	case <-tm.C():
		t.Fatal("value delivered after Stop")
	default:
	}
}

func TestFakeFiresInTimeOrder(t *testing.T) {
	f := NewFake(epoch)
	var order []int
	for i, d := range []time.Duration{3 * time.Second, time.Second, 2 * time.Second} {
		f.AfterFunc(d, func() {
			order = append(order, i)
			if got, want := f.Now(), epoch.Add(d); !got.Equal(want) {
				t.Errorf("func %d ran at %v, want %v", i, got, want)
			}
		})
	}
	f.Advance(time.Hour)
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 0 {
		t.Fatalf("order = %v, want [1 2 0]", order)
	}
	if got := f.Now(); !got.Equal(epoch.Add(time.Hour)) {
		t.Fatalf("now = %v after Advance", got)
	}
}

func TestFakeTickerKeepsOneTick(t *testing.T) {
	f := NewFake(epoch)
	tk := f.NewTicker(time.Second)
	defer tk.Stop()
	f.Advance(5 * time.Second)
	select {
	case at := <-tk.C():
		if !at.Equal(epoch.Add(time.Second)) {
			t.Fatalf("first kept tick at %v", at)
		}
	default:
		t.Fatal("no tick")
	}
	select {
	case <-tk.C():
		t.Fatal("ticker kept more than one tick")
	default:
	}
	f.Advance(time.Second)
	select {
	case at := <-tk.C():
		if !at.Equal(epoch.Add(6 * time.Second)) {
			t.Fatalf("tick at %v, want %v", at, epoch.Add(6*time.Second))
		}
	default:
		t.Fatal("ticker stopped ticking")
	}
	tk.Reset(time.Minute)
	f.Advance(59 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("reset ticker ticked early")
	default:
	}
}

func TestFakeSleepAndBlockUntil(t *testing.T) {
	f := NewFake(epoch)
	var woke atomic.Bool
	done := make(chan struct{})
	go func() {
		f.Sleep(time.Minute)
		woke.Store(true)
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("sleeper never waited: %v", err)
	}
	if woke.Load() {
		t.Fatal("woke before the clock moved")
	}
	f.Advance(time.Minute)
	<-done
}

func TestFakeBlockUntilGivesUp(t *testing.T) {
	f := NewFake(epoch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.BlockUntil(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestFakeZeroTimerFiresAtOnce(t *testing.T) {
	f := NewFake(epoch)
	select {
	case <-f.After(0):
	default:
		t.Fatal("After(0) did not fire at once")
	}
	ran := make(chan struct{})
	f.AfterFunc(-time.Second, func() { close(ran) })
	<-ran
}

func TestRealClockBasics(t *testing.T) {
	before := time.Now()
	if Real.Now().Before(before) {
		t.Fatal("Real.Now went back")
	}
	tm := Real.NewTimer(time.Hour)
	if !tm.Stop() {
		t.Fatal("Real timer Stop reported false")
	}
	tk := Real.NewTicker(time.Hour)
	tk.Reset(2 * time.Hour)
	tk.Stop()
	ctx, cancel := Real.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("Real.WithTimeout has no deadline")
	}
}

func TestFakeSinceAndUntil(t *testing.T) {
	f := NewFake(epoch)
	f.Advance(90 * time.Second)
	if got := f.Since(epoch); got != 90*time.Second {
		t.Fatalf("Since = %v, want 90s", got)
	}
	if got := f.Until(epoch.Add(2 * time.Minute)); got != 30*time.Second {
		t.Fatalf("Until = %v, want 30s", got)
	}
}

func TestFakeSetNeverMovesBack(t *testing.T) {
	f := NewFake(epoch)
	f.Set(epoch.Add(-time.Hour))
	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("now = %v after Set to the past, want %v", got, epoch)
	}
}

func TestFakeTickerRejectsNonPositiveInterval(t *testing.T) {
	f := NewFake(epoch)
	mustPanic(t, "NewTicker(0)", func() { f.NewTicker(0) })
	tk := f.NewTicker(time.Second)
	defer tk.Stop()
	mustPanic(t, "Reset(-1s)", func() { tk.Reset(-time.Second) })
	// The rejected Reset leaves the ticker on its old period.
	f.Advance(time.Second)
	select {
	case <-tk.C():
	default:
		t.Fatal("ticker stopped ticking after a rejected Reset")
	}
}

func TestFakeTickerStop(t *testing.T) {
	f := NewFake(epoch)
	tk := f.NewTicker(time.Second)
	tk.Stop()
	if f.Waiters() != 0 {
		t.Fatalf("waiters = %d after Stop", f.Waiters())
	}
	f.Advance(time.Minute)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker ticked")
	default:
	}
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s did not panic", what)
		}
	}()
	fn()
}

func TestRealClockTimers(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	if Real.Since(past) < time.Hour {
		t.Fatal("Real.Since is short")
	}
	if Real.Until(past) > -time.Hour {
		t.Fatal("Real.Until is not negative for the past")
	}
	Real.Sleep(0)
	<-Real.After(0)

	ran := make(chan struct{})
	Real.AfterFunc(0, func() { close(ran) })
	<-ran

	tm := Real.NewTimer(0)
	<-tm.C()
	if tm.Reset(time.Hour) {
		t.Fatal("Reset of a fired timer reported it active")
	}
	if !tm.Stop() {
		t.Fatal("Stop of a re-armed timer reported false")
	}

	tk := Real.NewTicker(time.Hour)
	defer tk.Stop()
	select {
	case <-tk.C():
		t.Fatal("hourly ticker ticked at once")
	default:
	}

	deadline := time.Now().Add(time.Hour)
	ctx, cancel := Real.WithDeadline(context.Background(), deadline)
	defer cancel()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("Real.WithDeadline deadline = %v, %v", got, ok)
	}
}
