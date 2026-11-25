/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/toolspec"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A panicking subscriber is logged and skipped: the others still get every
// notice, later notices still arrive, and the writer does not panic.
func TestSubscribe_PanickingSubscriber(t *testing.T) {
	var buf syncBuffer
	c := New(WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	c.Subscribe(func(Change) { panic("bad subscriber") })
	var rec recorder
	c.Subscribe(rec.add)

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("a subscriber's panic reached the writer: %v", r)
			}
		}()
		mustAdd(t, c, listed("a")...)
		mustAdd(t, c, listed("b")...)
	}()
	waitFor(t, "both notices", func() bool { return rec.count() == 2 })
	mustAdd(t, c, listed("c")...)
	waitFor(t, "a later notice", func() bool { return rec.count() == 3 })
	waitFor(t, "the panic to be logged", func() bool {
		return strings.Contains(buf.String(), "bad subscriber")
	})
}

// A slow subscriber never holds a writer, and the delivery goroutine exits
// once the queue is empty.
func TestSubscribe_SlowSubscriberDoesNotHoldWriters(t *testing.T) {
	before := runtime.NumGoroutine()
	c := New()
	release := make(chan struct{})
	delivered := make(chan Change, 8)
	c.Subscribe(func(ch Change) {
		<-release
		delivered <- ch
	})
	done := make(chan struct{})
	go func() {
		mustAdd(t, c, listed("a")...)
		if err := c.Disable("a"); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a writer waited for a slow subscriber")
	}
	close(release)
	for range 2 {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatal("a notice was lost")
		}
	}
	waitFor(t, "the delivery goroutine to exit", func() bool { return runtime.NumGoroutine() <= before })
}

func TestSubscribe_Nil(t *testing.T) {
	c := New()
	unsubscribe := c.Subscribe(nil)
	mustAdd(t, c, listed("a")...)
	unsubscribe()
	unsubscribe()
	// Nothing was registered, so nothing is pending.
	waitFor(t, "an idle notifier", func() bool {
		c.notes.mu.Lock()
		defer c.notes.mu.Unlock()
		return !c.notes.running && len(c.notes.pending) == 0
	})
}

// Readers see one generation: a set that alternates, through Adopt,
// between (tool a, granted a) and (tool b, granted b) is never seen with
// the catalogue of one and the grant of the other.
func TestAdopt_ReadersSeeOneGeneration(t *testing.T) {
	build := func(name string) *Catalog {
		next := New()
		if err := next.AddEntries(listed(name)...); err != nil {
			t.Fatal(err)
		}
		next.Set("alice").Grant(grant(name))
		return next
	}
	nexts := []*Catalog{build("a"), build("b")}
	c := New()
	c.Adopt(nexts[0])
	alice := c.Set("alice")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.Adopt(nexts[i%2])
		}
	})
	var bad atomic.Int64
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if len(alice.Snapshot().Names()) != 1 || len(alice.List()) != 1 {
					bad.Add(1)
				}
				res := alice.Execute(context.Background(), "a", nil, nil)
				if res.IsError && !strings.Contains(res.ForLLM, "not found") {
					bad.Add(1) // granted-b paired with tool a
				}
				if v := alice.Version(); v == 0 {
					bad.Add(1)
				}
				if _, err := alice.ExecuteUnattended(context.Background(), "a", nil); err != nil &&
					!errors.Is(err, ErrToolNotRegistered) {
					bad.Add(1) // checked against one generation, run against another
				}
			}
		})
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d reads mixed two generations", n)
	}
}

// A tool returning nil: the host's texts.
func TestNilResultTexts(t *testing.T) {
	c := New()
	mustAdd(t, c, Entry{Tool: &mockTool{name: "nil_tool"}})
	if _, err := c.ExecuteUnattended(context.Background(), "nil_tool", nil); err == nil ||
		err.Error() != `tool "nil_tool" returned no result` {
		t.Fatalf("ExecuteUnattended = %v", err)
	}
	if res := c.Execute(context.Background(), "nil_tool", nil, nil); res.ForLLM != "tool returned no result" {
		t.Fatalf("Execute = %+v", res)
	}
}

func TestMount_NilRefused(t *testing.T) {
	c := New()
	if _, err := c.AddProvider("x", nil, toolspec.Deps{}); err == nil {
		t.Fatal("a nil provider must be refused")
	}
	if _, err := c.AddToolset(context.Background(), nil); err == nil {
		t.Fatal("a nil toolset must be refused")
	}
	if len(c.Names()) != 0 {
		t.Fatal("nothing is mounted")
	}
}

// settle waits until every queued change notice has been delivered.
func settle(t *testing.T, c *Catalog) {
	t.Helper()
	waitFor(t, "notices to be delivered", func() bool {
		c.notes.mu.Lock()
		defer c.notes.mu.Unlock()
		return !c.notes.running && len(c.notes.pending) == 0
	})
}
