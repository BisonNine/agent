package toolkit

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
)

// capture is a slog handler that keeps every record's message.
type capture struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capture) Enabled(context.Context, slog.Level) bool { return true }
func (h *capture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *capture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capture) WithGroup(string) slog.Handler      { return h }

func (h *capture) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}

func (h *capture) count(msg string) int {
	n := 0
	for _, m := range h.all() {
		if m == msg {
			n++
		}
	}
	return n
}

func (h *capture) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = nil
}

// A gate's result is the caller's to log: the SDK logs nothing for it, and
// the only record of the call is the started line logged on entry. A
// refusal returned by a tool that ran is logged once, as before.
func TestExecuteGated_GateRefusalNotLogged(t *testing.T) {
	ctx := context.Background()
	refusal := func() *Result { return ErrorResult("policy says no").WithError(Refusal(errors.New("denied"))) }
	gate := func(context.Context, Info) *Result { return refusal() }

	tests := []struct {
		name string
		run  func(c *Catalog)
	}{
		{"ExecuteGated", func(c *Catalog) { c.ExecuteGated(ctx, "refund", nil, nil, gate) }},
		{"ExecuteUnattendedGated", func(c *Catalog) {
			if _, err := c.ExecuteUnattendedGated(ctx, "refund", nil, gate); err == nil {
				t.Error("gate refusal returned no error")
			}
		}},
		{"Snapshot.ExecuteGated", func(c *Catalog) { c.Snapshot().ExecuteGated(ctx, "refund", nil, nil, gate) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &capture{}
			c := New(WithLogger(slog.New(h)))
			tool := newMockTool("refund", "")
			tool.result = refusal()
			mustAdd(t, c, Entry{Tool: tool})

			tt.run(c)
			if got := h.all(); !slices.Equal(got, []string{"Tool execution started"}) {
				t.Errorf("gate refusal logged %q, want only the started line", got)
			}

			h.reset()
			c.Execute(ctx, "refund", nil, nil)
			if n := h.count("Tool call refused"); n != 1 {
				t.Errorf("tool refusal logged %d times, want 1 (%q)", n, h.all())
			}
		})
	}
}

// A write that changes nothing logs nothing, sends no notice and leaves
// the version where it was.
func TestNoOpWrites(t *testing.T) {
	tests := []struct {
		name  string
		write func(c *Catalog) error
	}{
		{"Remove unknown", func(c *Catalog) error { c.Remove("zzz"); return nil }},
		{"Remove nothing", func(c *Catalog) error { c.Remove(); return nil }},
		{"Replace no match", func(c *Catalog) error { c.Replace("mcp_bob_", nil); return nil }},
		{"ReplaceEntries no match", func(c *Catalog) error { c.ReplaceEntries("mcp_bob_", nil); return nil }},
		{"AddEntries nothing", func(c *Catalog) error { return c.AddEntries() }},
		{"Put nothing", func(c *Catalog) error { return c.Put() }},
		{"Enable unflagged", func(c *Catalog) error { return c.Enable("mcp_alice_search") }},
		{"EnableGroup unflagged", func(c *Catalog) error { c.EnableGroup("mcp_alice_"); return nil }},
		{"Set.Enable unflagged", func(c *Catalog) error { return c.Default().Enable("mcp_alice_search") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &capture{}
			c := New(WithLogger(slog.New(h)))
			mustAdd(t, c, listed("mcp_alice_search")...)
			var rec recorder
			c.Subscribe(rec.add)
			settle(t, c)
			h.reset()
			before, notices := c.Version(), rec.count()

			if err := tt.write(c); err != nil {
				t.Fatal(err)
			}

			settle(t, c)
			if got := h.all(); len(got) != 0 {
				t.Errorf("no-op write logged %q", got)
			}
			if c.Version() != before {
				t.Errorf("version moved %d → %d", before, c.Version())
			}
			if rec.count() != notices {
				t.Errorf("no-op write sent a notice: %+v", rec.all())
			}
		})
	}
}

// A Replace that changes something is still logged.
func TestReplace_LogsChange(t *testing.T) {
	h := &capture{}
	c := New(WithLogger(slog.New(h)))
	mustAdd(t, c, listed("mcp_alice_search")...)
	c.ReplaceEntries("mcp_alice_", nil)
	if n := h.count("Replaced tools by prefix"); n != 1 {
		t.Errorf("logged %d times, want 1 (%q)", n, h.all())
	}
}

// "Tool execution started" is the first record of every call, logged on
// entry with the name as called, before a call is refused as not found,
// not granted or disabled.
func TestExecute_StartedLoggedBeforeRefusals(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call string
		set  func(t *testing.T, c *Catalog) Registry
		want []string
	}{
		{"not found", "zzz", func(_ *testing.T, c *Catalog) Registry { return c }, []string{"Tool execution started", "Tool not found"}},
		{
			"not granted", "refund", func(_ *testing.T, c *Catalog) Registry { return c.View(grant("lookup_order")) },
			[]string{"Tool execution started", "Tool execution denied by agent allowlist"},
		},
		{"disabled", "refund", func(t *testing.T, c *Catalog) Registry {
			t.Helper()
			if err := c.Disable("refund"); err != nil {
				t.Fatal(err)
			}
			return c
		}, []string{"Tool execution started", "Tool is disabled"}},
		{"ran", "refund", func(_ *testing.T, c *Catalog) Registry { return c }, []string{"Tool execution started", "Tool execution completed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			c := New(WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
			mustAdd(t, c, Entry{Tool: newMockTool("refund", "")})
			ex := tt.set(t, c)
			buf.Reset()
			ex.Execute(ctx, tt.call, nil, nil)

			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if len(lines) != len(tt.want) {
				t.Fatalf("records = %q, want %q", lines, tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(lines[i], "msg=\""+w+"\"") {
					t.Errorf("record %d = %q, want %q", i, lines[i], w)
				}
			}
			if want := "level=INFO msg=\"Tool execution started\" component=toolkit tool=" + tt.call; !strings.Contains(lines[0], want) {
				t.Errorf("started record = %q, want %q", lines[0], want)
			}
		})
	}
}
