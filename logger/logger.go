// Package logger is the SDK's host-injectable logging seam, shared by spawnllm,
// ctxengine and cogmem. The SDK pulls in no host logging stack. Instead a host
// application installs one Backend via SetBackend so every package's
// structured events flow into the host's own logger with consistent formatting
// and routing.
//
// The zero state is a no-op: the SDK is silent until a backend is installed.
// The CF function names mirror the host logger API the code was written
// against, so call sites read the same in both places.
package logger

import "sync/atomic"

// Backend receives the SDK's structured log events.
type Backend interface {
	// Log records one event at the given level ("debug", "info", "warn",
	// "error"), tagged with a component and structured fields.
	Log(level, component, message string, fields map[string]any)
	// LogMessageContent reports whether message content may be logged verbatim
	// (hosts may redact by default).
	LogMessageContent() bool
}

var current atomic.Pointer[Backend]

// SetBackend installs the host's logging backend. A nil backend restores the
// silent no-op. Safe to call once at startup before logging begins.
func SetBackend(b Backend) {
	if b == nil {
		current.Store(nil)
		return
	}
	current.Store(&b)
}

func emit(level, component, message string, fields map[string]any) {
	if p := current.Load(); p != nil {
		(*p).Log(level, component, message, fields)
	}
}

// DebugCF logs at debug level with a component tag and structured fields.
func DebugCF(component, message string, fields map[string]any) {
	emit("debug", component, message, fields)
}

// InfoCF logs at info level with a component tag and structured fields.
func InfoCF(component, message string, fields map[string]any) {
	emit("info", component, message, fields)
}

// WarnCF logs at warn level with a component tag and structured fields.
func WarnCF(component, message string, fields map[string]any) {
	emit("warn", component, message, fields)
}

// ErrorCF logs at error level with a component tag and structured fields.
func ErrorCF(component, message string, fields map[string]any) {
	emit("error", component, message, fields)
}

// InfoC logs at info level with a component tag and no fields.
func InfoC(component, message string) { emit("info", component, message, nil) }

// GetLogMessageContent reports whether the installed backend permits logging
// message content verbatim. Defaults to false when no backend is installed.
func GetLogMessageContent() bool {
	if p := current.Load(); p != nil {
		return (*p).LogMessageContent()
	}
	return false
}
