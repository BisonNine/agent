/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc

import "time"

// Clock is the manager's source of time: job start and end times, wait
// timeouts, the stop grace period, the job time limit and log expiry all go
// through it. The default is the real clock; tests pass a fake
// (proctest.Clock) to exercise those rules without sleeping.
type Clock interface {
	Now() time.Time
	// NewTimer returns a timer that fires once, d from now. A d of zero or
	// less fires at once.
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer from a Clock.
type Timer interface {
	// C delivers the time once the timer fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether it did so.
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{t: time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }

func (r realTimer) Stop() bool { return r.t.Stop() }
