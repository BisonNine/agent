package logger

import (
	"sync"
	"testing"
)

type capture struct {
	mu      sync.Mutex
	events  []event
	logCtnt bool
}

type event struct {
	level, component, message string
	fields                    map[string]any
}

func (c *capture) Log(level, component, message string, fields map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event{level, component, message, fields})
}
func (c *capture) LogMessageContent() bool { return c.logCtnt }

func TestNoBackend_IsSilentNoOp(t *testing.T) {
	SetBackend(nil)
	// Must not panic and must report content-logging off when no backend.
	DebugCF("c", "m", nil)
	InfoCF("c", "m", nil)
	WarnCF("c", "m", nil)
	ErrorCF("c", "m", map[string]any{"k": 1})
	InfoC("c", "m")
	if GetLogMessageContent() {
		t.Error("GetLogMessageContent must be false with no backend")
	}
}

func TestBackend_RoutesEachLevel(t *testing.T) {
	c := &capture{logCtnt: true}
	SetBackend(c)
	defer SetBackend(nil)

	DebugCF("prov", "d", map[string]any{"a": 1})
	InfoCF("prov", "i", nil)
	WarnCF("prov", "w", nil)
	ErrorCF("prov", "e", nil)
	InfoC("prov", "ic")

	if len(c.events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(c.events))
	}
	if c.events[4].message != "ic" || c.events[4].fields != nil {
		t.Errorf("InfoC event = %+v, want message ic and nil fields", c.events[4])
	}
	wantLevels := []string{"debug", "info", "warn", "error", "info"}
	for i, want := range wantLevels {
		if c.events[i].level != want {
			t.Errorf("event %d level = %q, want %q", i, c.events[i].level, want)
		}
		if c.events[i].component != "prov" {
			t.Errorf("event %d component = %q, want prov", i, c.events[i].component)
		}
	}
	if c.events[0].fields["a"] != 1 {
		t.Errorf("fields not passed through: %+v", c.events[0].fields)
	}
	if !GetLogMessageContent() {
		t.Error("GetLogMessageContent should reflect the backend (true)")
	}
}

func TestSetBackendNil_RestoresNoOp(t *testing.T) {
	c := &capture{}
	SetBackend(c)
	SetBackend(nil) // restore silent
	InfoCF("c", "m", nil)
	if len(c.events) != 0 {
		t.Errorf("no events should reach a removed backend, got %d", len(c.events))
	}
}
