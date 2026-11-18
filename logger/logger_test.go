package logger

import (
	"reflect"
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

	fields := map[string]any{"a": 1, "ok": true}
	DebugCF("prov", "d", fields)
	InfoCF("engine", "i", fields)
	WarnCF("store", "w", fields)
	ErrorCF("tools", "e", fields)
	InfoC("composer", "ic")

	want := []event{
		{"debug", "prov", "d", fields},
		{"info", "engine", "i", fields},
		{"warn", "store", "w", fields},
		{"error", "tools", "e", fields},
		{"info", "composer", "ic", nil},
	}
	if !reflect.DeepEqual(c.events, want) {
		t.Errorf("events = %+v, want %+v", c.events, want)
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
