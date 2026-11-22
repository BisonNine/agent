package toolkit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// metaTool reports the Meta it was registered with as its output, and
// counts its runs.
type metaTool struct {
	name string
	meta string
	runs atomic.Int64
}

func (m *metaTool) Name() string               { return m.name }
func (m *metaTool) Description() string        { return m.name }
func (m *metaTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (m *metaTool) Execute(context.Context, map[string]any) *Result {
	m.runs.Add(1)
	return SilentResult(m.meta)
}

func metaEntry(name, meta string) (Entry, *metaTool) {
	tool := &metaTool{name: name, meta: meta}
	return Entry{Tool: tool, Meta: meta}, tool
}

// failGate fails the test if it is ever called.
func failGate(t *testing.T) Gate {
	t.Helper()
	return func(context.Context, Info) *Result {
		t.Error("gate called")
		return nil
	}
}

func TestExecuteGated_GateSeesResolvedInfo(t *testing.T) {
	c := New()
	ext := &mockExternalTool{mockTool: *newMockTool("mcp_lookup", "lookup"), external: "lookup"}
	if err := c.AddEntries(Entry{Tool: ext, Meta: "exempt"}); err != nil {
		t.Fatal(err)
	}

	var seen []Info
	gate := func(_ context.Context, info Info) *Result {
		seen = append(seen, info)
		return nil
	}
	res := c.ExecuteGated(context.Background(), "lookup", nil, nil, gate)
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if len(seen) != 1 {
		t.Fatalf("gate called %d times, want 1", len(seen))
	}
	got := seen[0]
	if got.Name != "mcp_lookup" || got.Published != "lookup" || got.Meta != "exempt" || got.Tool != Tool(ext) || !got.Enabled {
		t.Errorf("info = %+v", got)
	}
}

func TestExecuteGated_RefusalNotRun(t *testing.T) {
	var logs bytes.Buffer
	c := New(WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	e, tool := metaEntry("refund", "m")
	mustAdd(t, c, e)

	refusal := ErrorResult("policy says no").WithError(Refusal(errors.New("denied")))
	got := c.ExecuteGated(context.Background(), "refund", nil, nil, func(context.Context, Info) *Result { return refusal })
	if got != refusal {
		t.Errorf("result = %+v, want the gate's", got)
	}
	if n := tool.runs.Load(); n != 0 {
		t.Errorf("tool ran %d times", n)
	}
	if st := c.Stats(); len(st) != 0 {
		t.Errorf("Stats = %+v, want none", st)
	}
	if !strings.Contains(logs.String(), "Tool call refused") || strings.Contains(logs.String(), "Tool execution failed") {
		t.Errorf("logs = %s", logs.String())
	}

	// A result that is not a refusal is returned as is and logs nothing extra.
	logs.Reset()
	plain := SilentResult("skipped")
	if got := c.ExecuteGated(context.Background(), "refund", nil, nil, func(context.Context, Info) *Result { return plain }); got != plain {
		t.Errorf("result = %+v, want the gate's", got)
	}
	if strings.Contains(logs.String(), "refused") || strings.Contains(logs.String(), "failed") {
		t.Errorf("logs = %s", logs.String())
	}
	if n := tool.runs.Load(); n != 0 {
		t.Errorf("tool ran %d times", n)
	}
}

func TestExecuteGated_RefusedCallsSkipGate(t *testing.T) {
	ctx := context.Background()
	c := New()
	mustAdd(t, c, listed("off", "setoff", "granted", "other")...)
	if err := c.Disable("off"); err != nil {
		t.Fatal(err)
	}
	s := c.Set("alice")
	s.Grant(allowList{"granted": true, "off": true, "setoff": true})
	if err := s.Disable("setoff"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		set  *Set
		tool string
	}{
		{"not found", c.Default(), "nope"},
		{"disabled", c.Default(), "off"},
		{"disabled in set", s, "setoff"},
		{"not granted", s, "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.set.Execute(ctx, tc.tool, nil, nil)
			got := tc.set.ExecuteGated(ctx, tc.tool, nil, nil, failGate(t))
			if !got.IsError || got.ForLLM != want.ForLLM || got.Err.Error() != want.Err.Error() ||
				IsExpectedRefusal(got.Err) != IsExpectedRefusal(want.Err) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}

	t.Run("removed since snapshot", func(t *testing.T) {
		snap := c.Snapshot()
		c.Remove("granted")
		res := snap.ExecuteGated(ctx, "granted", nil, nil, failGate(t))
		if !errors.Is(res.Err, ErrToolRemoved) {
			t.Errorf("result = %+v", res)
		}
	})
}

type allowList map[string]bool

func (a allowList) IsToolAllowed(name string) bool { return a[name] }

func TestExecuteGated_NilGateIsExecute(t *testing.T) {
	ctx := context.Background()
	c := New()
	e, tool := metaEntry("echo", "m")
	mustAdd(t, c, e)

	want := c.Execute(ctx, "echo", nil, nil)
	got := c.ExecuteGated(ctx, "echo", nil, nil, nil)
	snapGot := c.Snapshot().ExecuteGated(ctx, "echo", nil, nil, nil)
	if got.ForLLM != want.ForLLM || got.IsError || snapGot.ForLLM != want.ForLLM || snapGot.IsError || !got.Silent || !snapGot.Silent {
		t.Errorf("got %+v and %+v, want %+v", got, snapGot, want)
	}
	if n := tool.runs.Load(); n != 3 {
		t.Errorf("runs = %d, want 3", n)
	}
	if st := c.Stats(); len(st) != 1 || st[0].Calls != 3 {
		t.Errorf("Stats = %+v", st)
	}
}

func TestExecuteUnattendedGated(t *testing.T) {
	ctx := context.Background()
	c := New()
	e, tool := metaEntry("probe", "m")
	mustAdd(t, c, e)

	out, err := c.ExecuteUnattendedGated(ctx, "probe", nil, func(_ context.Context, info Info) *Result {
		if info.Meta != "m" {
			t.Errorf("Meta = %v", info.Meta)
		}
		return nil
	})
	if err != nil || out != "m" {
		t.Errorf("allowed: %q, %v", out, err)
	}

	_, err = c.ExecuteUnattendedGated(ctx, "probe", nil, func(context.Context, Info) *Result {
		return ErrorResult("policy says no").WithError(Refusal(errors.New("denied")))
	})
	if err == nil || !strings.Contains(err.Error(), "policy says no") {
		t.Errorf("refused: err = %v", err)
	}
	if n := tool.runs.Load(); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}

	if _, err := c.ExecuteUnattendedGated(ctx, "nope", nil, failGate(t)); !errors.Is(err, ErrToolNotRegistered) {
		t.Errorf("unknown: err = %v", err)
	}
}

// TestExecuteGated_RunsTheToolGateSaw replaces a tool under one name while
// calls run, and checks every run was of the tool whose Meta the gate saw.
func TestExecuteGated_RunsTheToolGateSaw(t *testing.T) {
	ctx := context.Background()
	c := New()
	a, _ := metaEntry("flip", "a")
	b, _ := metaEntry("flip", "b")
	if err := c.Put(a); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e := a
			if i%2 == 1 {
				e = b
			}
			if err := c.Put(e); err != nil {
				t.Error(err)
				return
			}
		}
	})

	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() {
			snap := c.Snapshot()
			for range 500 {
				var saw any
				gate := func(_ context.Context, info Info) *Result {
					saw = info.Meta
					return nil
				}
				res := c.ExecuteGated(ctx, "flip", nil, nil, gate)
				if res.IsError || res.ForLLM != saw {
					t.Errorf("ran %q, gate saw %v", res.ForLLM, saw)
					return
				}
				res = snap.ExecuteGated(ctx, "flip", nil, nil, gate)
				if res.IsError || res.ForLLM != saw {
					t.Errorf("snapshot ran %q, gate saw %v", res.ForLLM, saw)
					return
				}
			}
		})
	}
	callers.Wait()
	close(stop)
	writer.Wait()
}

func TestMeta_RoundTrips(t *testing.T) {
	ctx := context.Background()
	c := New()
	if err := c.AddEntries(Entry{Tool: newMockTool("added", ""), Meta: "m-add"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(Entry{Tool: newMockTool("put", ""), Meta: "m-put"}); err != nil {
		t.Fatal(err)
	}
	c.ReplaceEntries("rep_", []Entry{{Tool: newMockTool("rep_x", ""), Meta: "m-rep"}})
	if _, err := c.AddToolset(ctx, newToolset("crm", Entry{Tool: newMockTool("crm_find", ""), Meta: "m-ts"})); err != nil {
		t.Fatal(err)
	}
	ext := &mockExternalTool{mockTool: *newMockTool("mcp_ext", ""), external: "ext"}
	if err := c.AddEntries(Entry{Tool: ext, Meta: "m-ext"}, Entry{Tool: newMockTool("plain", "")}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"added": "m-add", "put": "m-put", "rep_x": "m-rep", "crm_find": "m-ts", "mcp_ext": "m-ext", "plain": nil,
	}

	check := func(t *testing.T, c *Catalog) {
		t.Helper()
		snap := c.Snapshot()
		list := snap.List()
		if len(list) != len(want) {
			t.Fatalf("List has %d tools, want %d", len(list), len(want))
		}
		for _, info := range list {
			if info.Meta != want[info.Name] {
				t.Errorf("List %s: Meta = %v, want %v", info.Name, info.Meta, want[info.Name])
			}
		}
		for name, m := range want {
			if info, ok := snap.Get(name); !ok || info.Meta != m {
				t.Errorf("Snapshot.Get %s: %v %v", name, info.Meta, ok)
			}
			if info, ok := c.Get(name); !ok || info.Meta != m {
				t.Errorf("Set.Get %s: %v %v", name, info.Meta, ok)
			}
		}
		if info, ok := snap.Lookup("ext"); !ok || info.Meta != "m-ext" {
			t.Errorf("Snapshot.Lookup ext: %v %v", info.Meta, ok)
		}
		if info, ok := c.Lookup("ext"); !ok || info.Meta != "m-ext" {
			t.Errorf("Set.Lookup ext: %v %v", info.Meta, ok)
		}
	}
	t.Run("built", func(t *testing.T) { check(t, c) })
	t.Run("adopted", func(t *testing.T) {
		live := New()
		mustAdd(t, live, Entry{Tool: newMockTool("put", ""), Meta: "stale"})
		live.Adopt(c)
		check(t, live)
	})
}
