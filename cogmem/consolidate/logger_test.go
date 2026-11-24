// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
)

// failingManager starts a Manager whose factory always fails, so each job
// logs one warning, and stops it when the test ends.
func failingManager(t *testing.T, opts ...ManagerOption) *Manager {
	t.Helper()
	m := NewManager(func(Job) (*Worker, error) {
		return nil, errors.New("no model configured")
	}, opts...)
	m.Start(context.Background())
	t.Cleanup(m.Stop)
	return m
}

func TestLoggerDefaultIsSilent(t *testing.T) {
	// Nothing reaches the process's default logger when no logger is given.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	called := make(chan struct{}, 1)
	m := NewManager(func(Job) (*Worker, error) {
		called <- struct{}{}
		return nil, errors.New("no model configured")
	})
	m.Start(context.Background())
	m.Enqueue(Job{ID: "alice", Dir: "/tmp/alice"}, "manual")
	<-called
	m.Stop()

	if buf.Len() != 0 {
		t.Errorf("default logger got output: %s", buf.String())
	}
}

func TestLoggerRecordsCarryComponent(t *testing.T) {
	logs := installCapture(t)
	m := failingManager(t, WithManagerLogger(logs.logger()))
	m.Enqueue(Job{ID: "alice", Dir: "/tmp/alice"}, "manual")

	e := logs.wait(t, "consolidation worker factory failed")
	if e.fields["component"] != "cogmem" {
		t.Errorf("component = %v, want cogmem", e.fields["component"])
	}
}

func TestLoggerInstancesAreIsolated(t *testing.T) {
	alice, bob := installCapture(t), installCapture(t)
	ma := failingManager(t, WithManagerLogger(alice.logger()))
	mb := failingManager(t, WithManagerLogger(bob.logger()))
	ma.Enqueue(Job{ID: "alice", Dir: "/tmp/alice"}, "manual")
	mb.Enqueue(Job{ID: "bob", Dir: "/tmp/bob"}, "manual")

	ea := alice.wait(t, "consolidation worker factory failed")
	eb := bob.wait(t, "consolidation worker factory failed")
	ma.Stop()
	mb.Stop()
	if ea.fields["id"] != "alice" || eb.fields["id"] != "bob" {
		t.Fatalf("alice got %v, bob got %v", ea.fields["id"], eb.fields["id"])
	}
	for name, c := range map[string]*captureBackend{"alice": alice, "bob": bob} {
		c.mu.Lock()
		for _, e := range c.entries {
			if id, ok := e.fields["id"]; ok && id != name {
				t.Errorf("%s's logger got a record for %v: %s", name, id, e.message)
			}
		}
		c.mu.Unlock()
	}
}
