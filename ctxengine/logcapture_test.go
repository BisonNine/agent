/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.bison9.com/agent/ctxengine/session"
)

// logEntry is one captured log record.
type logEntry struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// logCapture is a slog handler that keeps every record, for assertions.
type logCapture struct {
	mu      sync.Mutex
	entries []logEntry
}

func (c *logCapture) logger() *slog.Logger { return slog.New(captureHandler{c: c}) }

func (c *logCapture) all() []logEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]logEntry(nil), c.entries...)
}

func (c *logCapture) String() string {
	entries := c.all()
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("%s %v %s %v", e.level, e.attrs["component"], e.msg, e.attrs))
	}
	return strings.Join(lines, "\n")
}

type captureHandler struct {
	c     *logCapture
	attrs []slog.Attr
}

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	e := logEntry{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	for _, a := range h.attrs {
		e.attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		e.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	h.c.entries = append(h.c.entries, e)
	return nil
}

func (h captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return captureHandler{c: h.c, attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h captureHandler) WithGroup(string) slog.Handler { return h }

// misconfigured are options New warns about (the safety net is at or below
// the normal threshold), so building a Manager with them logs one record.
var misconfigured = []Option{WithNormalPercent(50), WithSafetyPercent(40)}

const safetyWarning = "compress_safety_percent <= compress_normal_percent; safety net will never fire"

func newStore(t *testing.T) session.SessionStore {
	t.Helper()
	store, err := session.NewSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() }) //nolint:errcheck // test cleanup
	return store
}

func TestLogger_DefaultIsSilent(t *testing.T) {
	// Nothing reaches the process's default logger when no logger is given.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	New("alice", newStore(t), misconfigured...)
	if buf.Len() != 0 {
		t.Errorf("default logger got output: %s", buf.String())
	}
}

func TestLogger_RecordsCarryComponent(t *testing.T) {
	logs := &logCapture{}
	New("alice", newStore(t), append([]Option{WithLogger(logs.logger())}, misconfigured...)...)

	entries := logs.all()
	if len(entries) == 0 {
		t.Fatal("no records logged")
	}
	for _, e := range entries {
		if e.attrs["component"] != "llmcontext" {
			t.Errorf("record %q has component %v, want llmcontext", e.msg, e.attrs["component"])
		}
	}
	if entries[0].msg != safetyWarning || entries[0].level != slog.LevelWarn {
		t.Errorf("first record = %s %q, want a warning %q", entries[0].level, entries[0].msg, safetyWarning)
	}
}

func TestLogger_InstancesAreIsolated(t *testing.T) {
	alice, bob := &logCapture{}, &logCapture{}
	New("alice", newStore(t), append([]Option{WithLogger(alice.logger())}, misconfigured...)...)
	New("bob", newStore(t), WithLogger(bob.logger()))

	if len(alice.all()) == 0 {
		t.Error("alice's logger got no records")
	}
	if got := bob.String(); got != "" {
		t.Errorf("bob's logger got alice's records:\n%s", got)
	}
}

// discardLog is a logger for the internal helpers tests call directly.
var discardLog = slog.New(slog.DiscardHandler)
