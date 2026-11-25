/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/fsmode"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// inner has a field the copy cannot set, which is copied as it is.
type inner struct {
	Tags    []string
	private int
}

// extraBody is an extra body holding every kind of value, and a function
// that changes each nested value through the body's own references.
func extraBody() (map[string]any, func()) {
	n := 7
	deep := map[string]any{"deep": true}
	list := []any{"a", deep}
	nums := []int{1, 2}
	arr := [2][]int{{1}, {2}}
	in := inner{Tags: []string{"x"}, private: 3}
	body := map[string]any{
		"map":     map[string]any{"k": list},
		"slice":   nums,
		"array":   arr,
		"pointer": &n,
		"struct":  in,
		"nil map": map[string]int(nil),
		"nil":     nil,
		"nil ptr": (*int)(nil),
		"nil sl":  []string(nil),
		"scalar":  "text",
	}
	return body, func() {
		deep["deep"] = false
		list[0] = "b"
		nums[0] = 9
		arr[0][0] = 9
		n = 9
		in.Tags[0] = "y"
	}
}

// A copied model shares nothing with the original, whatever its extra body
// holds, and keeps every value and type, unexported fields included.
func TestCloneModelIsDeep(t *testing.T) {
	orig, change := extraBody()
	cp := cloneModel(modelspec.Model{ExtraBody: orig})
	if !reflect.DeepEqual(cp.ExtraBody, orig) {
		t.Fatalf("copy differs:\n%#v\n%#v", cp.ExtraBody, orig)
	}
	change()
	if want, _ := extraBody(); !reflect.DeepEqual(cp.ExtraBody, want) {
		t.Fatalf("changing the original changed the copy: %#v", cp.ExtraBody)
	}
}

func TestSameSettings(t *testing.T) {
	for _, tc := range []struct {
		a, b any
		want bool
	}{
		{nil, nil, true},
		{nil, 1, false},
		{1, 1, true},
		{1, 2, false},
		{1, "1", false},
		{[]int{1}, []int{1}, false}, // not comparable: never the same
	} {
		if got := sameSettings(tc.a, tc.b); got != tc.want {
			t.Errorf("sameSettings(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// scripted is a comparable client; listed is one that cannot be compared.
type (
	scripted struct{ name string }
	listed   func()
)

func (*scripted) Chat(context.Context, accounting.Identity, []spawnllm.Message, []spawnllm.ToolDefinition, string, map[string]any) (*spawnllm.LLMResponse, error) {
	return nil, errors.New("not scripted")
}
func (*scripted) GetDefaultModel() string { return "" }

func (listed) Chat(context.Context, accounting.Identity, []spawnllm.Message, []spawnllm.ToolDefinition, string, map[string]any) (*spawnllm.LLMResponse, error) {
	return nil, errors.New("not scripted")
}
func (listed) GetDefaultModel() string { return "" }

func TestSameClient(t *testing.T) {
	a, b := &scripted{"alice"}, &scripted{"alice"}
	for _, tc := range []struct {
		name string
		x, y spawnllm.LLMProvider
		want bool
	}{
		{"same", a, a, true},
		{"equal but distinct", a, b, false},
		{"nil", a, nil, false},
		{"not comparable", listed(nil), listed(nil), false},
	} {
		if got := sameClient(tc.x, tc.y); got != tc.want {
			t.Errorf("%s: sameClient = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// funcHandler is a slog handler that cannot be compared.
type funcHandler func()

func (funcHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (funcHandler) Handle(context.Context, slog.Record) error { return nil }
func (f funcHandler) WithAttrs([]slog.Attr) slog.Handler      { return f }
func (f funcHandler) WithGroup(string) slog.Handler           { return f }

func TestSameLogger(t *testing.T) {
	h := slog.DiscardHandler
	l := slog.New(h)
	other := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for _, tc := range []struct {
		name string
		a, b *slog.Logger
		want bool
	}{
		{"same", l, l, true},
		{"over one handler", l, slog.New(h), true},
		{"other handlers", l, other, false},
		{"not comparable", slog.New(funcHandler(nil)), slog.New(funcHandler(nil)), false},
	} {
		if got := sameLogger(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameLogger = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A model parked without a last reason is parked for its most frequent one.
func TestHealthReasonMostFrequent(t *testing.T) {
	e := &healthEntry{byReason: map[spawnllm.FailoverReason]int{
		spawnllm.FailoverRateLimit: 3,
		spawnllm.FailoverTimeout:   1,
	}}
	if got := e.reason(); got != spawnllm.FailoverRateLimit {
		t.Fatalf("reason = %q", got)
	}
	e.lastReason = spawnllm.FailoverTimeout
	if got := e.reason(); got != spawnllm.FailoverTimeout {
		t.Fatalf("reason with a last reason = %q", got)
	}
}

// A CLI's environment keeps the allowlisted names and each system's own;
// the default base environment is the allowlisted part of this process's.
func TestFilterEnvPerSystem(t *testing.T) {
	environ := []string{"PATH=/bin", "XPC_FLAGS=0", "Path=C:\\", "SECRET=x", "LC_ALL=C", "SystemRoot=C:\\Windows"}
	if got := filterEnv(environ, "darwin"); !slices.Equal(got, []string{"LC_ALL=C", "PATH=/bin", "XPC_FLAGS=0"}) {
		t.Errorf("darwin: %v", got)
	}
	if got := filterEnv(environ, "windows"); !slices.Equal(got, []string{"LC_ALL=C", "PATH=/bin", "Path=C:\\", "SystemRoot=C:\\Windows"}) {
		t.Errorf("windows: %v", got)
	}
	if got := filterEnv(environ, "linux"); !slices.Equal(got, []string{"LC_ALL=C", "PATH=/bin"}) {
		t.Errorf("linux: %v", got)
	}
	base := DefaultBaseEnv()
	if !slices.IsSorted(base) {
		t.Errorf("DefaultBaseEnv is not sorted: %v", base)
	}
	for _, kv := range base {
		if !slices.Contains(os.Environ(), kv) {
			t.Errorf("DefaultBaseEnv has %q, not in the environment", kv)
		}
	}
}

func TestFileModesForAndBypassNotice(t *testing.T) {
	s := defaultSettings()
	WithFileModesFor(nil)(s)
	if got := s.modesFor("/logs/a.log"); got != (fsmode.Modes{}) {
		t.Fatalf("WithFileModesFor(nil) changed the modes: %+v", got)
	}
	WithFileModesFor(func(path string) fsmode.Modes {
		if strings.HasPrefix(path, "/shared/") {
			return fsmode.Modes{File: 0o640}
		}
		return fsmode.Modes{File: 0o600}
	})(s)
	if s.modesFor("/shared/a.log").File != 0o640 || s.modesFor("/home/a.log").File != 0o600 {
		t.Fatal("WithFileModesFor did not set the modes by path")
	}
	var got []IgnoredBypass
	WithIgnoredBypassNotice(func(ib IgnoredBypass) { got = append(got, ib) })(s)
	s.bypassNotice(IgnoredBypass{Model: "m1"})
	if len(got) != 1 || got[0].Model != "m1" {
		t.Fatalf("bypass notice = %+v", got)
	}
}

func TestCatalogRefusals(t *testing.T) {
	c := New()
	if err := c.AddProvider(modelspec.Provider{Protocol: "openai-chat"}); err == nil {
		t.Error("a provider without a name was accepted")
	}
	if err := c.RegisterClient("m1", nil); err == nil {
		t.Error("a nil client was accepted")
	}
	if _, ok := c.Get("nobody"); ok {
		t.Error("an unknown model was found")
	}
}

func TestDeclinedGuardIsCLI(t *testing.T) {
	if !(&declinedGuard{}).IsCLI() {
		t.Fatal("the guard does not mark its client as a CLI")
	}
}
