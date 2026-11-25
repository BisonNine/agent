//go:build unix

/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package shell

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/toolbox/shell/proc"
	"go.bison9.com/agent/toolspec"
)

// foregroundDefault runs a foreground command with the default timeout and
// returns its result channel once the command's wait timer is set.
func (h *harness) foregroundDefault(command string) <-chan *toolspec.Result {
	base := h.clk.Timers()
	ch := make(chan *toolspec.Result, 1)
	go func() {
		ch <- h.runCtx(context.Background(), "exec", "alice", "s1", map[string]any{"command": command})
	}()
	h.clk.BlockUntil(h.t, base+2) // the job's time limit and the foreground wait
	return ch
}

// Update changes the timeouts for calls that start after it; a foreground
// command already waiting keeps its timeout.
func TestUpdateTimeouts(t *testing.T) {
	h := newHarness(t, nil)
	running := h.foregroundDefault("sleep 100") // waits the default 60s
	if err := h.tools.Update(Settings{Timeout: 5 * time.Second, MaxTimeout: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(5 * time.Second)
	select {
	case res := <-running:
		t.Fatalf("running command took the new timeout: %s", res.ForLLM)
	case <-time.After(100 * time.Millisecond):
	}
	h.clk.Advance(55 * time.Second)
	want(t, <-running, false, "Still running after 1m0s")

	later := h.foregroundDefault("sleep 100")
	h.clk.Advance(5 * time.Second)
	want(t, <-later, false, "Still running after 5s")

	asked := h.startForeground(context.Background(), "sleep 100", 600) // capped at the new maximum
	h.clk.Advance(10 * time.Second)
	want(t, <-asked, false, "Still running after 10s")

	for _, d := range h.tools.Definitions() {
		for _, p := range d.Parameters {
			if p.Name == "timeout_seconds" && (p.Default != 5 || *p.Maximum != 10) {
				t.Errorf("timeout_seconds default %v max %v, want 5 and 10", p.Default, *p.Maximum)
			}
		}
	}
}

// Update checks everything before changing anything.
func TestUpdateRefused(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Jobs.Limits.MaxJobs = 3 })
	for _, s := range []Settings{
		{Timeout: -time.Second},
		{Timeout: 20 * time.Second, MaxTimeout: 10 * time.Second},
		{Timeout: 5 * time.Second, Limits: proc.Limits{MaxJobs: -1}},
	} {
		if err := h.tools.Update(s); err == nil {
			t.Errorf("Update(%+v) accepted", s)
		}
	}
	if timeout, maxTimeout := h.tools.currentTimeouts(); timeout != DefaultTimeout || maxTimeout != DefaultMaxTimeout {
		t.Errorf("timeouts changed to %v, %v", timeout, maxTimeout)
	}
	if got := h.tools.Manager().Limits().MaxJobs; got != 3 {
		t.Errorf("MaxJobs changed to %d", got)
	}
	if err := h.tools.Update(Settings{Limits: proc.Limits{MaxJobs: 1}}); err != nil {
		t.Fatal(err)
	}
	jobID(t, h.exec("alice", "read x", map[string]any{"background": true}))
	want(t, h.exec("alice", "echo two", nil), true, "Not started: 1 job is already running")
}

// Route is called before the process starts, so even a job that ends at once
// has its route when its notice comes; the route is on the notice and on
// the job's audit events.
func TestRouteBeforeNotice(t *testing.T) {
	const runs = 200
	var mu sync.Mutex
	routed := make(map[int]bool)
	next := 0
	notices := make(chan Notice, runs)
	h := newHarness(t, func(c *Config) {
		c.Jobs.Limits.MaxJobs = runs
		c.Route = func(*toolspec.ToolCall) any {
			mu.Lock()
			defer mu.Unlock()
			next++
			routed[next] = true
			return next
		}
		c.Notice = func(n Notice) {
			mu.Lock()
			route, isInt := n.Route.(int)
			ok := isInt && routed[route]
			mu.Unlock()
			if !ok {
				t.Errorf("notice for %s before its route: %v", n.JobID, n.Route)
			}
			notices <- n
		}
	})
	ids := make(map[string]bool)
	for range runs {
		ids[jobID(t, h.exec("alice", "true", map[string]any{"background": true}))] = true
	}
	seen := make(map[any]bool)
	for range runs {
		select {
		case n := <-notices:
			if !ids[n.JobID] || seen[n.Route] {
				t.Fatalf("notice %s route %v: unknown job or route repeated", n.JobID, n.Route)
			}
			seen[n.Route] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("%d notices of %d", len(seen), runs)
		}
	}
	mu.Lock()
	calls := next
	mu.Unlock()
	if calls != runs {
		t.Fatalf("Route called %d times for %d starts", calls, runs)
	}
	for _, e := range h.auditKinds() {
		if e.Route == nil {
			t.Errorf("audit event without its route: %+v", e)
		}
	}
}

// A foreground command gets a route too: kept at its timeout, it is a job
// whose end is announced.
func TestRouteForegroundKeptAsJob(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Route = func(call *toolspec.ToolCall) any { return "route:" + call.Session }
	})
	ch := h.startForeground(context.Background(), "sleep 100", 5)
	h.clk.Advance(5 * time.Second)
	id := jobID(t, <-ch)
	if err := h.tools.Manager().Stop(id, time.Second); err != nil { // not by the agent: announced
		t.Fatal(err)
	}
	if n := h.notice(); n.JobID != id || n.Route != "route:s1" {
		t.Fatalf("notice = %+v", n)
	}
	audits := h.auditKinds()
	if len(audits) == 0 || audits[0].Kind != AuditStart || audits[0].Route != "route:s1" {
		t.Fatalf("audits = %+v", audits)
	}
}

// Rule 8: a job stopped by a clean shutdown is announced once after the
// restart, not by Close, with a nil route; notify off and jobs already
// announced are not.
func TestShutdownAnnouncedAfterRestartTools(t *testing.T) {
	first := newHarness(t, func(c *Config) {
		c.Route = func(*toolspec.ToolCall) any { return "r" }
	})
	told := jobID(t, first.exec("alice", "echo up; read x", map[string]any{"background": true}))
	jobID(t, first.exec("alice", "read x", map[string]any{"background": true, "notify": false}))
	done := jobID(t, first.exec("alice", "echo done", map[string]any{"background": true}))
	if n := first.notice(); n.JobID != done {
		t.Fatalf("notice = %+v, want %s", n, done)
	}
	want(t, first.ctrl("alice", "wait", map[string]any{"id": told, "until": "up"}), false)
	first.close()
	select {
	case n := <-first.notices:
		t.Fatalf("Close delivered a notice: %+v", n)
	default:
	}

	restart := func() *harness {
		return newHarness(t, func(c *Config) {
			c.Jobs.Dir = first.tools.Manager().Dir()
			c.Workspace = func(*toolspec.ToolCall) string { return first.ws }
		})
	}
	second := restart()
	n := second.notice()
	if n.JobID != told || n.State != proc.StateStopped || n.StopReason != proc.StopShutdown || n.Route != nil || n.LastLines != "up" {
		t.Fatalf("notice = %+v", n)
	}
	wantText := fmt.Sprintf("Shell job %s was stopped when the service restarted, and was not started again.", told)
	if !strings.HasPrefix(n.Text, wantText) {
		t.Errorf("notice text:\n%s", n.Text)
	}
	select {
	case n := <-second.notices:
		t.Fatalf("second notice: %+v", n)
	default:
	}

	third := restart()
	select {
	case n := <-third.notices:
		t.Fatalf("announced again after a second restart: %+v", n)
	default:
	}
}
