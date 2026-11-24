//go:build unix

/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package proc_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
)

func setLimits(t *testing.T, m *proc.Manager, l proc.Limits) {
	t.Helper()
	if err := m.SetLimits(l); err != nil {
		t.Fatalf("SetLimits(%+v): %v", l, err)
	}
}

func TestSetLimitsValidated(t *testing.T) {
	m, _ := newManager(t, proc.Options{Limits: proc.Limits{MaxJobs: 3}})
	for _, l := range []proc.Limits{
		{MaxJobs: -1},
		{MaxJobTime: -time.Second},
		{MaxOutputBytes: -1},
		{LogRetention: -time.Second},
	} {
		if err := m.SetLimits(l); err == nil {
			t.Errorf("SetLimits(%+v) accepted", l)
		}
	}
	if got := m.Limits(); got.MaxJobs != 3 {
		t.Fatalf("a refused SetLimits changed the limits: %+v", got)
	}
	setLimits(t, m, proc.Limits{MaxJobs: 5})
	want := proc.Limits{
		MaxJobs:        5,
		MaxJobTime:     proc.DefaultMaxJobTime,
		MaxOutputBytes: proc.DefaultMaxOutputBytes,
		LogRetention:   proc.DefaultLogRetention,
	}
	if got := m.Limits(); got != want {
		t.Fatalf("Limits = %+v, want %+v", got, want)
	}
}

// Lowering MaxJobs stops nothing; starts are refused until below it.
func TestSetLimitsMaxJobsLowered(t *testing.T) {
	m, _ := newManager(t, proc.Options{Limits: proc.Limits{MaxJobs: 4}})
	jobs := make([]proc.Info, 0, 3)
	for range 3 {
		jobs = append(jobs, start(t, m, proc.Spec{Command: "read x", Input: true}))
	}
	setLimits(t, m, proc.Limits{MaxJobs: 2})
	if running := m.Running("alice"); len(running) != 3 {
		t.Fatalf("running = %d, want 3: lowering the limit stopped a job", len(running))
	}
	for i := range 2 {
		_, err := m.Start(context.Background(), proc.Spec{Owner: "alice", Command: "true"})
		var limit *proc.LimitError
		if !errors.As(err, &limit) || limit.Max != 2 {
			t.Fatalf("start with %d running: err = %v, want a LimitError at 2", 3-i, err)
		}
		if err := m.Stop(jobs[i].ID, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	waitEnd(t, m, start(t, m, proc.Spec{Command: "true"}).ID)
}

// Shortening MaxJobTime applies to running jobs from their start: one
// already older than the new limit is stopped at once; a younger one when
// it reaches it.
func TestSetLimitsMaxJobTimeShortened(t *testing.T) {
	m, clk := newManager(t, proc.Options{Limits: proc.Limits{MaxJobTime: time.Hour}})
	old := start(t, m, proc.Spec{Command: "read x", Input: true})
	clk.BlockUntil(t, 1)
	clk.Advance(30 * time.Minute)
	young := start(t, m, proc.Spec{Command: "read x", Input: true})
	clk.BlockUntil(t, 2)

	setLimits(t, m, proc.Limits{MaxJobTime: 10 * time.Minute})
	r := waitEnd(t, m, old.ID)
	if r.Info.State != proc.StateStopped || r.Info.StopReason != proc.StopTimeLimit || r.Info.Ended.Sub(r.Info.Started) != 30*time.Minute {
		t.Fatalf("old job: %s %q after %v", r.Info.State, r.Info.StopReason, r.Info.Ended.Sub(r.Info.Started))
	}
	if got, _ := m.Get(young.ID); !got.Running() {
		t.Fatalf("young job stopped early: %+v", got)
	}
	clk.BlockUntil(t, 1)
	clk.Advance(10 * time.Minute)
	r = waitEnd(t, m, young.ID)
	if r.Info.StopReason != proc.StopTimeLimit || r.Info.Ended.Sub(r.Info.Started) != 10*time.Minute {
		t.Fatalf("young job: %q after %v", r.Info.StopReason, r.Info.Ended.Sub(r.Info.Started))
	}
}

// Raising MaxJobTime lets a running job pass its old limit.
func TestSetLimitsMaxJobTimeRaised(t *testing.T) {
	m, clk := newManager(t, proc.Options{Limits: proc.Limits{MaxJobTime: time.Hour}})
	info := start(t, m, proc.Spec{Command: "read x", Input: true})
	clk.BlockUntil(t, 1)
	setLimits(t, m, proc.Limits{MaxJobTime: 2 * time.Hour})
	clk.Advance(time.Hour)
	clk.BlockUntil(t, 1) // re-armed for the new limit
	if got, _ := m.Get(info.ID); !got.Running() {
		t.Fatalf("job stopped at its old limit: %+v", got)
	}
	clk.Advance(time.Hour)
	r := waitEnd(t, m, info.ID)
	if r.Info.StopReason != proc.StopTimeLimit || r.Info.Ended.Sub(r.Info.Started) != 2*time.Hour {
		t.Fatalf("job: %q after %v", r.Info.StopReason, r.Info.Ended.Sub(r.Info.Started))
	}
}

// A new MaxOutputBytes applies to output a running job writes from then on.
func TestSetLimitsMaxOutputBytes(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	info := start(t, m, proc.Spec{Command: "echo ready; read x; seq 1 5000", Input: true})
	waitMatch(t, m, info.ID, "ready")
	setLimits(t, m, proc.Limits{MaxOutputBytes: 1000})
	if err := m.Write(info.ID, []byte("go\n")); err != nil {
		t.Fatal(err)
	}
	r := waitEnd(t, m, info.ID)
	if r.Info.DroppedBytes == 0 {
		t.Fatalf("nothing dropped from %d bytes under a 1000-byte limit", r.Info.OutputBytes)
	}
	st, err := os.Stat(info.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 1000+250+64 {
		t.Fatalf("log is %d bytes, over the new limit and slack", st.Size())
	}

	// Raising it again: a later job keeps everything.
	setLimits(t, m, proc.Limits{})
	r = waitEnd(t, m, start(t, m, proc.Spec{Command: "seq 1 5000"}).ID)
	if r.Info.DroppedBytes != 0 {
		t.Fatalf("dropped %d bytes under the default limit", r.Info.DroppedBytes)
	}
}

// A new LogRetention applies at the next sweep.
func TestSetLimitsLogRetention(t *testing.T) {
	m, clk := newManager(t, proc.Options{})
	old := start(t, m, proc.Spec{Command: "echo old"})
	waitEnd(t, m, old.ID)
	clk.Advance(2 * time.Hour)
	setLimits(t, m, proc.Limits{LogRetention: time.Hour})
	if _, ok := m.Get(old.ID); !ok {
		t.Fatal("job removed before the next sweep")
	}
	start(t, m, proc.Spec{Command: "true"})
	if _, ok := m.Get(old.ID); ok {
		t.Fatal("expired job still listed after the next sweep")
	}
}

// SetLimits is safe alongside starts; run under -race.
func TestSetLimitsConcurrentStarts(t *testing.T) {
	m, _ := newManager(t, proc.Options{})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := m.SetLimits(proc.Limits{MaxJobs: 2 + i%4, MaxOutputBytes: int64(1000 + i%3*1000), MaxJobTime: time.Duration(1+i%3) * time.Hour}); err != nil {
				t.Error(err)
				return
			}
			_ = m.Limits()
		}
	})
	var starts sync.WaitGroup
	for g := range 4 {
		starts.Go(func() {
			owner := fmt.Sprintf("owner%d", g)
			for range 10 {
				info, err := m.Start(context.Background(), proc.Spec{Owner: owner, Command: "seq 1 2000"})
				if _, ok := errors.AsType[*proc.LimitError](err); ok {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := m.Wait(testCtx(t), info.ID, proc.Until{}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	starts.Wait()
	close(stop)
	wg.Wait()
}

// endRecorder collects OnEnd calls.
type endRecorder struct {
	mu    sync.Mutex
	infos []proc.Info
}

func (r *endRecorder) onEnd(i proc.Info) {
	r.mu.Lock()
	r.infos = append(r.infos, i)
	r.mu.Unlock()
}

// waitFor waits until n calls have been recorded: a job's end is visible to
// Wait before OnEnd runs.
func (r *endRecorder) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		have := len(r.infos)
		r.mu.Unlock()
		if have >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d OnEnd calls, want %d", have, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *endRecorder) take() []proc.Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.infos
	r.infos = nil
	return out
}

// Rule 8: a job stopped by a clean shutdown with notify on is announced once
// by the next manager, not by Close; one with notify off, or one that had
// already ended and been announced, is not.
func TestShutdownAnnouncedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	var ends endRecorder
	first, clk := newManager(t, proc.Options{Dir: dir, OnEnd: ends.onEnd})
	told := start(t, first, proc.Spec{Command: "echo up; read x", Input: true, Notify: true})
	quiet := start(t, first, proc.Spec{Command: "read x", Input: true})
	done := start(t, first, proc.Spec{Command: "echo done", Notify: true})
	waitEnd(t, first, done.ID)
	waitMatch(t, first, told.ID, "up")
	ends.waitFor(t, 1)
	if got := ends.take(); len(got) != 1 || got[0].ID != done.ID {
		t.Fatalf("OnEnd before Close = %+v, want %s", got, done.ID)
	}

	closeManager(t, first, clk)
	if got := ends.take(); len(got) != 0 {
		t.Fatalf("Close announced %+v", got)
	}
	if got, _ := first.Get(quiet.ID); got.State != proc.StateStopped || got.StopReason != proc.StopShutdown {
		t.Fatalf("quiet job after Close: %+v", got)
	}

	newManager(t, proc.Options{Dir: dir, OnEnd: ends.onEnd})
	got := ends.take()
	if len(got) != 1 || got[0].ID != told.ID || got[0].State != proc.StateStopped ||
		got[0].StopReason != proc.StopShutdown || !got[0].Notify || got[0].Route != nil {
		t.Fatalf("announced after restart = %+v, want %s once", got, told.ID)
	}

	newManager(t, proc.Options{Dir: dir, OnEnd: ends.onEnd})
	if got := ends.take(); len(got) != 0 {
		t.Fatalf("announced again after a second restart: %+v", got)
	}
}

// Spec.Route comes back in Info and in OnEnd.
func TestRouteKeptWithJob(t *testing.T) {
	var ends endRecorder
	m, clk := newManager(t, proc.Options{OnEnd: ends.onEnd})
	info := start(t, m, proc.Spec{Command: "true", Route: "conversation-1"})
	if info.Route != "conversation-1" {
		t.Fatalf("Start Info.Route = %v", info.Route)
	}
	waitEnd(t, m, info.ID)
	ends.waitFor(t, 1)
	closeManager(t, m, clk)
	if got := ends.take(); len(got) != 1 || got[0].Route != "conversation-1" {
		t.Fatalf("OnEnd = %+v", got)
	}
}
