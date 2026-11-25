/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package modelspec

import (
	"testing"
	"time"
)

func TestResolveLimits(t *testing.T) {
	builtIn := Limits{MaxTokens: 8192, ContextWindow: 128000, ThinkingLevel: ThinkingOff, Vision: VisionOff}
	tests := []struct {
		name     string
		model    *Model
		defaults LimitDefaults
		want     Limits
	}{
		{name: "no model, no defaults: built-in fallbacks", want: builtIn},
		{name: "model with nothing set: built-in fallbacks", model: &Model{}, want: builtIn},
		{
			name:     "defaults apply when the model sets nothing",
			model:    &Model{},
			defaults: LimitDefaults{MaxTokens: 4096, ContextWindow: 64000, RequestTimeout: time.Minute},
			want:     Limits{MaxTokens: 4096, ContextWindow: 64000, ThinkingLevel: ThinkingOff, Vision: VisionOff, RequestTimeout: time.Minute},
		},
		{
			name:     "no model: defaults apply",
			defaults: LimitDefaults{MaxTokens: 4096, ContextWindow: 64000},
			want:     Limits{MaxTokens: 4096, ContextWindow: 64000, ThinkingLevel: ThinkingOff, Vision: VisionOff},
		},
		{
			name: "the model's settings win",
			model: &Model{
				MaxTokens: 16384, ContextWindow: 200000, ThinkingLevel: "high",
				NoTools: true, Vision: VisionToolResponse, RequestTimeout: 300,
			},
			defaults: LimitDefaults{MaxTokens: 4096, ContextWindow: 64000, RequestTimeout: time.Minute},
			want: Limits{
				MaxTokens: 16384, ContextWindow: 200000, ThinkingLevel: ThinkingHigh,
				NoTools: true, Vision: VisionToolResponse, RequestTimeout: 300 * time.Second,
			},
		},
		{
			name:  "negative model values are ignored",
			model: &Model{MaxTokens: -1, ContextWindow: -1, RequestTimeout: -1},
			want:  builtIn,
		},
		{
			name:     "a negative default timeout is none",
			defaults: LimitDefaults{RequestTimeout: -time.Second},
			want:     builtIn,
		},
		{
			name:  "thinking level is normalized",
			model: &Model{ThinkingLevel: "  XHigh "},
			want:  Limits{MaxTokens: 8192, ContextWindow: 128000, ThinkingLevel: ThinkingXHigh, Vision: VisionOff},
		},
		{
			name:  "an unknown thinking level is off",
			model: &Model{ThinkingLevel: "turbo"},
			want:  builtIn,
		},
		{
			name:  "vision user_message",
			model: &Model{Vision: VisionUserMessage},
			want:  Limits{MaxTokens: 8192, ContextWindow: 128000, ThinkingLevel: ThinkingOff, Vision: VisionUserMessage},
		},
		{
			name:  "an unknown vision mode is off",
			model: &Model{Vision: "sometimes"},
			want:  builtIn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveLimits(tc.model, tc.defaults); got != tc.want {
				t.Errorf("ResolveLimits = %+v\nwant            %+v", got, tc.want)
			}
		})
	}
}

func TestParseThinkingLevel(t *testing.T) {
	tests := map[string]ThinkingLevel{
		"":          ThinkingOff,
		"off":       ThinkingOff,
		"adaptive":  ThinkingAdaptive,
		"low":       ThinkingLow,
		"Medium":    ThinkingMedium,
		"High":      ThinkingHigh,
		"xhigh":     ThinkingXHigh,
		"MAX":       ThinkingMax,
		"something": ThinkingOff,
	}
	for in, want := range tests {
		if got := ParseThinkingLevel(in); got != want {
			t.Errorf("ParseThinkingLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
