package modelspec

import (
	"strings"
	"time"
)

// ThinkingLevel controls how the provider sends thinking parameters.
//
//   - "adaptive": adaptive thinking at the model's default effort
//   - "low"/"medium"/"high"/"xhigh"/"max": adaptive thinking with that effort
//   - "off": no thinking
type ThinkingLevel string

// Thinking levels.
const (
	ThinkingOff      ThinkingLevel = "off"
	ThinkingLow      ThinkingLevel = "low"
	ThinkingMedium   ThinkingLevel = "medium"
	ThinkingHigh     ThinkingLevel = "high"
	ThinkingXHigh    ThinkingLevel = "xhigh"
	ThinkingMax      ThinkingLevel = "max"
	ThinkingAdaptive ThinkingLevel = "adaptive"
)

// ParseThinkingLevel normalizes a config string to a ThinkingLevel. It is
// case-insensitive and whitespace-tolerant, and returns ThinkingOff for
// unknown or empty values.
func ParseThinkingLevel(level string) ThinkingLevel {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "adaptive":
		return ThinkingAdaptive
	case "low":
		return ThinkingLow
	case "medium":
		return ThinkingMedium
	case "high":
		return ThinkingHigh
	case "xhigh":
		return ThinkingXHigh
	case "max":
		return ThinkingMax
	default:
		return ThinkingOff
	}
}

// Built-in fallbacks used when neither the model nor the caller's defaults
// set a value.
const (
	DefaultMaxTokens     = 8192
	DefaultContextWindow = 128000
)

// LimitDefaults are the caller's defaults, used where a model sets nothing.
// A zero value falls back to the built-in default (DefaultMaxTokens,
// DefaultContextWindow; no request timeout).
type LimitDefaults struct {
	MaxTokens      int
	ContextWindow  int
	RequestTimeout time.Duration
}

// Limits are the per-request limits a model runs with.
type Limits struct {
	MaxTokens     int
	ContextWindow int
	ThinkingLevel ThinkingLevel
	NoTools       bool
	// Vision is VisionUserMessage, VisionToolResponse or VisionOff; any other
	// value the model carries reads as VisionOff.
	Vision string
	// RequestTimeout is 0 when neither the model nor the defaults set one.
	RequestTimeout time.Duration
}

// ResolveLimits computes the limits of model m: the model's settings over the
// caller's defaults, over the built-in fallbacks. A positive max_tokens,
// context_window or request_timeout on the model wins; otherwise the default
// applies. m may be nil (a model that is not configured): the defaults apply
// and thinking is off.
func ResolveLimits(m *Model, d LimitDefaults) Limits {
	l := Limits{
		MaxTokens:      d.MaxTokens,
		ContextWindow:  d.ContextWindow,
		ThinkingLevel:  ThinkingOff,
		Vision:         VisionOff,
		RequestTimeout: max(d.RequestTimeout, 0),
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = DefaultMaxTokens
	}
	if l.ContextWindow == 0 {
		l.ContextWindow = DefaultContextWindow
	}
	if m == nil {
		return l
	}
	l.ThinkingLevel = ParseThinkingLevel(m.ThinkingLevel)
	l.NoTools = m.NoTools
	if m.ContextWindow > 0 {
		l.ContextWindow = m.ContextWindow
	}
	if m.MaxTokens > 0 {
		l.MaxTokens = m.MaxTokens
	}
	if m.RequestTimeout > 0 {
		l.RequestTimeout = time.Duration(m.RequestTimeout) * time.Second
	}
	if m.Vision == VisionUserMessage || m.Vision == VisionToolResponse {
		l.Vision = m.Vision
	}
	return l
}
