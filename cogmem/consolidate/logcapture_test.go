// cogmem - Cognitive Memory
// License: MIT

package consolidate

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitTimeout bounds every wait in this package's tests. It is a failure
// deadline, never an assertion: a test that needs it has already gone wrong.
const waitTimeout = 5 * time.Second

// logEntry is one captured log record. level is the slog level in lower case
// ("debug", "info", "warn", "error"); fields holds the attributes, those added
// with Logger.With included, with integers as int.
type logEntry struct {
	level   string
	message string
	fields  map[string]any
}

// captureBackend is a slog handler target that records every record and
// streams them on a channel so a test can block until a specific line
// appears. Each test gets its own, handed to the code under test with
// WithLogger or WithManagerLogger.
type captureBackend struct {
	mu      sync.Mutex
	entries []logEntry
	ch      chan logEntry
	// onLog, when set, runs synchronously inside the logging call. It lets a
	// test react at an exact point in product code (e.g. advance a fake clock
	// the moment a job is enqueued) without a timing window.
	onLog func(logEntry)
}

// installCapture returns a captureBackend for the test; pass its logger to
// the code under test.
func installCapture(t *testing.T) *captureBackend {
	t.Helper()
	return &captureBackend{ch: make(chan logEntry, 1024)}
}

// logger returns a logger writing to c.
func (c *captureBackend) logger() *slog.Logger { return slog.New(captureHandler{c: c}) }

type captureHandler struct {
	c     *captureBackend
	attrs []slog.Attr
}

func (captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	fields := map[string]any{}
	add := func(a slog.Attr) bool {
		v := a.Value.Any()
		if n, ok := v.(int64); ok {
			v = int(n)
		}
		fields[a.Key] = v
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	h.c.log(logEntry{level: strings.ToLower(r.Level.String()), message: r.Message, fields: fields})
	return nil
}

func (h captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return captureHandler{c: h.c, attrs: append(slices.Clip(h.attrs), attrs...)}
}

func (h captureHandler) WithGroup(string) slog.Handler { return h }

func (c *captureBackend) log(e logEntry) {
	c.mu.Lock()
	c.entries = append(c.entries, e)
	hook := c.onLog
	c.mu.Unlock()
	if hook != nil {
		hook(e)
	}
	select {
	case c.ch <- e:
	default:
	}
}

// wait blocks until an entry with the given message is logged, discarding
// earlier entries from the stream (they remain in entries for count).
func (c *captureBackend) wait(t *testing.T, message string) logEntry {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case e := <-c.ch:
			if e.message == message {
				return e
			}
		case <-deadline:
			t.Fatalf("timeout waiting for log %q; logged: %v", message, c.messages())
		}
	}
}

// count reports how many entries carry the given message.
func (c *captureBackend) count(message string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.entries {
		if e.message == message {
			n++
		}
	}
	return n
}

func (c *captureBackend) messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.level+": "+e.message)
	}
	return out
}
