/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// A job without a directory names no memory: OnMessage and Enqueue ignore it
// rather than track or queue it.
func TestJobWithoutDirIsIgnored(t *testing.T) {
	m := newTestManager(t, func(Job, string) { t.Error("a job without a directory ran") }, WithEveryNMessages(1))
	m.OnMessage(Job{ID: "alice"})
	m.Enqueue(Job{ID: "alice"}, "manual")
	m.mu.Lock()
	sessions := len(m.sessions)
	m.mu.Unlock()
	if sessions != 0 || len(m.queue) != 0 {
		t.Fatalf("sessions=%d queued=%d, want none", sessions, len(m.queue))
	}
}

// A full queue drops the job with a warning instead of blocking the caller.
func TestEnqueueDropsWhenQueueFull(t *testing.T) {
	logs := installCapture(t)
	m := newTestManager(t, func(Job, string) {}, WithManagerLogger(logs.logger()))
	for range defaultQueueBuffer {
		m.Enqueue(Job{ID: "alice", Dir: "/tmp/alice"}, "manual")
	}
	m.Enqueue(Job{ID: "bob", Dir: "/tmp/bob"}, "nightly")
	e := logs.wait(t, "consolidation queue full; dropping job")
	if e.level != "warn" || e.fields["id"] != "bob" || e.fields["trigger"] != "nightly" {
		t.Errorf("log = %+v, want a warning naming bob's nightly job", e)
	}
	if n := logs.count("consolidation queue full; dropping job"); n != 1 {
		t.Errorf("dropped %d jobs, want 1", n)
	}
	if len(m.queue) != defaultQueueBuffer {
		t.Errorf("queued = %d, want %d", len(m.queue), defaultQueueBuffer)
	}
}

// Ending the context given to Start ends every loop, without Stop.
func TestStartContextEndsLoops(t *testing.T) {
	m := newTestManager(t, func(Job, string) {}, WithIdle(1), WithNightlyAt("03:00"))
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	recv(t, done, "the loops to exit")
	m.Stop()
}

// A job waiting for a pool slot when the manager shuts down never runs, and is
// no longer marked in flight. Shutdown is either Stop or the end of Start's
// context.
func TestShutdownDropsJobWaitingForSlot(t *testing.T) {
	for _, viaContext := range []bool{false, true} {
		name := "stop"
		if viaContext {
			name = "context"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			started := make(chan struct{}, 1)
			var mu sync.Mutex
			var ran []string
			m := newTestManager(t, func(j Job, _ string) {
				mu.Lock()
				ran = append(ran, j.ID)
				mu.Unlock()
				if j.ID == "alice" {
					started <- struct{}{}
					<-release
				}
			}, WithConcurrency(1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.Start(ctx)

			m.Enqueue(Job{ID: "alice", Dir: "/tmp/alice"}, "manual")
			recv(t, started, "alice's job to start")
			m.Enqueue(Job{ID: "bob", Dir: "/tmp/bob"}, "manual")
			inflight := func(dir string) bool {
				m.mu.Lock()
				defer m.mu.Unlock()
				return m.inflight[dir]
			}
			waitUntil(t, "bob's job to wait for a slot", func() bool { return inflight("/tmp/bob") })

			stopped := make(chan struct{})
			if viaContext {
				cancel()
			} else {
				go func() { m.Stop(); close(stopped) }()
			}
			waitUntil(t, "bob's job to be dropped", func() bool { return !inflight("/tmp/bob") })
			close(release)
			if viaContext {
				m.Stop()
			} else {
				recv(t, stopped, "Stop to return")
			}

			mu.Lock()
			defer mu.Unlock()
			if len(ran) != 1 || ran[0] != "alice" {
				t.Fatalf("ran = %v, want only alice", ran)
			}
		})
	}
}

// A job reached after shutdown has begun runs nothing.
func TestRunJobAfterShutdownRunsNothing(t *testing.T) {
	for _, viaContext := range []bool{false, true} {
		dir := t.TempDir()
		s := openDirStore(t, dir)
		seedInbox(t, s, sampleMessages())
		model := &fakeModel{raw: emptyOutput}
		m := NewManager(func(Job) (*Worker, error) { return NewWorker(s, model), nil })
		ctx, cancel := context.WithCancel(context.Background())
		if viaContext {
			cancel()
		} else {
			m.Stop()
		}
		m.runJob(ctx, Job{ID: "alice", Dir: dir}, "manual")
		cancel()
		if model.calls() != 0 {
			t.Errorf("context=%v: model called %d times after shutdown", viaContext, model.calls())
		}
		if got := watermark(t, s).ConsolidatedSeq; got != 0 {
			t.Errorf("context=%v: watermark = %d, want 0", viaContext, got)
		}
	}
}

// A failed run is logged as a warning with its error, and the job ends.
func TestRunJobLogsFailedRun(t *testing.T) {
	logs := installCapture(t)
	s := openStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	m := NewManager(func(Job) (*Worker, error) { return NewWorker(s, &fakeModel{raw: emptyOutput}), nil },
		WithManagerLogger(logs.logger()))
	m.runJob(context.Background(), Job{ID: "alice", Dir: "/tmp/alice"}, "manual")
	e := logs.wait(t, "consolidation run failed")
	if e.level != "warn" || e.fields["id"] != "alice" || e.fields["trigger"] != "manual" {
		t.Errorf("log = %+v, want a warning naming alice's manual run", e)
	}
	if msg, _ := e.fields["error"].(string); !strings.HasPrefix(msg, "consolidate: acquire lease: ") {
		t.Errorf("error = %q, want the lease error", msg)
	}
	if n := logs.count("consolidation run failed"); n != 1 {
		t.Errorf("failed runs logged = %d, want 1", n)
	}
}
