package mcphost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

type hostKey struct{}

// recorder keeps the events and callback calls of one endpoint's calls.
type recorder struct {
	mu     sync.Mutex
	events []hook.Event
	notes  []string
}

func (r *recorder) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, s)
}

func (r *recorder) plugin() hook.Plugin {
	return hook.Plugin{
		Name: "recorder",
		OnEvent: func(ctx context.Context, ev hook.Event) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, ev)
		},
	}
}

func (r *recorder) finished() []hook.ToolCallFinished {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hook.ToolCallFinished
	for _, ev := range r.events {
		if f, ok := ev.(hook.ToolCallFinished); ok {
			out = append(out, f)
		}
	}
	return out
}

func TestDispatch_CallbacksAndEvents(t *testing.T) {
	rec := &recorder{}
	f := newFixture(t, func(c *Config) {
		c.CallContext = func(ctx context.Context, caller Caller) context.Context {
			return context.WithValue(ctx, hostKey{}, "chat-of-"+caller.Scope.AgentID)
		}
		c.Plugins = []hook.Plugin{rec.plugin(), {
			Name: "host",
			BeforeTool: func(ctx context.Context, call *hook.ToolCall) (*toolkit.Result, error) {
				caller, ok := CallerOf(ctx)
				s, _ := hook.ScopeOf(ctx)
				chat, isString := ctx.Value(hostKey{}).(string)
				if !isString {
					chat = "none"
				}
				rec.note("before " + call.Name + " caller=" + caller.Scope.AgentID + " scope=" + s.AgentID +
					" host=" + chat + " ok=" + map[bool]string{true: "y", false: "n"}[ok])
				switch call.Name {
				case "echo":
					call.Arguments["text"] = "rewritten"
				case "secret":
					return nil, toolkit.Refusal(errors.New("refused by policy"))
				case "gated":
					return toolkit.NewResult("answered by the host"), nil
				}
				return nil, nil //nolint:nilnil // (nil, nil): run the tool
			},
			OnToolError: func(_ context.Context, call *hook.ToolCall, res *toolkit.Result) *toolkit.Result {
				rec.note("onerror " + call.Name + ": " + res.ForLLM)
				if call.Name == "fail" {
					return toolkit.ErrorResult("boom, explained")
				}
				return nil
			},
			AfterTool: func(_ context.Context, call *hook.ToolCall, res *toolkit.Result) {
				rec.note("after " + call.Name)
				res.ForLLM = strings.ReplaceAll(res.ForLLM, "hidden", "[redacted]")
			},
		}}
	})
	c := f.mustClient("/mcp", f.aliceTok)

	if r := callT(t, c, "echo", map[string]any{"text": "hi"}); r.text != "keys=text text=rewritten" {
		t.Errorf("echo = %+v", r)
	}
	if r := callT(t, c, "secret", nil); !r.isError || r.text != "refused by policy" {
		t.Errorf("secret = %+v", r)
	}
	if r := callT(t, c, "gated", nil); r.isError || r.text != "answered by the host" {
		t.Errorf("gated = %+v", r)
	}
	if r := callT(t, c, "fail", nil); !r.isError || r.text != "boom, explained" {
		t.Errorf("fail = %+v", r)
	}
	if r := callT(t, c, "deep", nil); r.text != "a [redacted] tool" {
		t.Errorf("deep = %+v", r)
	}

	rec.mu.Lock()
	notes := strings.Join(rec.notes, "\n")
	rec.mu.Unlock()
	for _, want := range []string{
		"before echo caller=alice scope=alice host=chat-of-alice ok=y",
		"onerror secret: refused by policy",
		"onerror fail: boom",
		"after fail",
		"after gated",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("missing %q in:\n%s", want, notes)
		}
	}

	fin := rec.finished()
	if len(fin) != 5 {
		t.Fatalf("got %d ToolCallFinished, want 5", len(fin))
	}
	echo := fin[0]
	if echo.Call.Name != "echo" || echo.Call.Arguments["text"] != "rewritten" || echo.Answered {
		t.Errorf("echo finished = %+v", echo)
	}
	if echo.Result.ForUser != "keys=text text=rewritten" {
		t.Errorf("echo ForUser = %q: the host needs the tool's output for people", echo.Result.ForUser)
	}
	if echo.Scope.AgentID != "alice" || echo.Scope.TurnID != "t1" {
		t.Errorf("echo scope = %+v", echo.Scope)
	}
	if !fin[1].Answered || !toolkit.IsExpectedRefusal(fin[1].Result.Err) {
		t.Errorf("secret finished = %+v", fin[1])
	}
	if !fin[2].Answered {
		t.Errorf("gated finished = %+v", fin[2])
	}

	// Every ToolCallStarted precedes its ToolCallFinished, with the model's
	// arguments.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	started, ok := rec.events[0].(hook.ToolCallStarted)
	if !ok || started.Call.Name != "echo" || started.Call.Arguments["text"] != "hi" {
		t.Errorf("first event = %#v", rec.events[0])
	}
	if started.Call.ID == "" || started.Call.ID != echo.Call.ID {
		t.Errorf("call ids: started %q, finished %q", started.Call.ID, echo.Call.ID)
	}
}

// A panic in a tool callback fails only that call, skips its later
// callbacks, and is reported as the engine reports it.
func TestDispatch_CallbackPanic(t *testing.T) {
	rec := &recorder{}
	var afterRan bool
	f := newFixture(t, func(c *Config) {
		c.Plugins = []hook.Plugin{rec.plugin(), {
			Name:       "broken",
			BeforeTool: func(context.Context, *hook.ToolCall) (*toolkit.Result, error) { panic("callback exploded") },
			AfterTool:  func(context.Context, *hook.ToolCall, *toolkit.Result) { afterRan = true },
		}, {
			Name:    "observer",
			OnEvent: func(context.Context, hook.Event) { panic("observer exploded") },
		}}
	})
	c := f.mustClient("/mcp", f.aliceTok)
	r := callT(t, c, "whoami", nil)
	if !r.isError || r.text != "Tool whoami failed with an internal error." {
		t.Errorf("result = %+v", r)
	}
	if afterRan {
		t.Error("AfterTool ran after a BeforeTool panic")
	}
	fin := rec.finished()
	if len(fin) != 1 {
		t.Fatalf("finished events = %d", len(fin))
	}
	var pe *hook.PanicError
	if !errors.As(fin[0].Result.Err, &pe) || pe.Callback != "BeforeTool" || pe.Plugin != "broken" {
		t.Errorf("Err = %v, want a BeforeTool PanicError of broken", fin[0].Result.Err)
	}
}

// A background tool's late result reaches the host through AsyncResult,
// with the caller and the call.
func TestDispatch_AsyncResult(t *testing.T) {
	type late struct {
		caller Caller
		call   hook.ToolCall
		result *toolkit.Result
	}
	got := make(chan late, 1)
	f := newFixture(t, func(c *Config) {
		c.DetachBackground = true
		c.AsyncResult = func(_ context.Context, caller Caller, call hook.ToolCall, _ toolkit.Info, result *toolkit.Result) {
			got <- late{caller, call, result}
		}
	})
	c := f.mustClient("/internal", "")
	r := callT(t, c, "job", map[string]any{TokenArgument: f.aliceTok, "job": "j1"})
	if r.isError || r.text != "started" {
		t.Fatalf("job = %+v", r)
	}
	select {
	case l := <-got:
		if l.caller.Scope.AgentID != "alice" || l.call.Name != "job" || l.call.Arguments["job"] != "j1" ||
			l.result.ForLLM != "finished j1" {
			t.Errorf("late result = %+v", l)
		}
		if _, leaked := l.call.Arguments[TokenArgument]; leaked {
			t.Error("the late call carries the token")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no late result")
	}

	// Without AsyncResult the tool runs synchronously.
	plain := newFixture(t)
	r = callT(t, plain.mustClient("/mcp", plain.aliceTok), "job", nil)
	if r.isError || r.text != "sync job" {
		t.Errorf("job without AsyncResult = %+v", r)
	}
}

// Listing runs no call and logs nothing of one.
func TestList_IsNotACall(t *testing.T) {
	var logs logBuffer
	rec := &recorder{}
	f := newFixture(t, func(c *Config) {
		c.Logger = newLogger(&logs)
		c.Plugins = []hook.Plugin{rec.plugin()}
	})
	_ = list(t, f.mustClient("/internal", ""))
	_ = list(t, f.mustClient("/mcp", f.aliceTok))
	rec.mu.Lock()
	if len(rec.events) != 0 {
		t.Errorf("tools/list emitted %d events", len(rec.events))
	}
	rec.mu.Unlock()
	if out := logs.String(); strings.Contains(out, "MCP tool call") || strings.Contains(out, "refused") {
		t.Errorf("tools/list logged a call:\n%s", out)
	}
}

func TestDispatch_InFlight(t *testing.T) {
	f := newFixture(t)
	c := f.mustClient("/mcp", f.aliceTok)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(ctx, c, "slow", nil)
	}()
	<-f.started
	if n := f.bearer.InFlight(); n != 1 {
		t.Errorf("InFlight = %d, want 1", n)
	}
	cancel()
	<-done
	if cause := <-f.slowDone; cause == nil {
		t.Error("slow tool was not cancelled")
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.bearer.InFlight() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.bearer.InFlight(); n != 0 {
		t.Errorf("InFlight after the call = %d", n)
	}
}

// A panic in any host callback a call runs is answered as a panicking
// tool is in each mode: the internal-error result, or with
// PanicAsProtocolError MCP's protocol error. It ends with the call's
// ToolCallFinished carrying the panic, as a tool's panic does; only an
// Authenticator panic, before there is a caller, has no events.
func TestDispatch_HostCallbackPanic(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tool     string // "fail" reaches OnToolError
		path     string
		set      func(*Config)
		callback string // the *hook.PanicError's Callback; "" for none
		toolErr  bool   // a *hook.ToolPanicError (the Gate runs in the tool's step)
		noEvents bool
	}{
		{name: "Authenticator", tool: "whoami", path: "/internal", noEvents: true, set: func(c *Config) {
			c.Authenticator = AuthenticatorFunc(func(context.Context, string) (Caller, error) { panic("host exploded") })
		}},
		{name: "CallContext", tool: "whoami", path: "/mcp", callback: "CallContext", set: func(c *Config) {
			c.CallContext = func(context.Context, Caller) context.Context { panic("host exploded") }
		}},
		{name: "ToolContext", tool: "whoami", path: "/mcp", callback: "ToolContext", set: func(c *Config) {
			c.ToolContext = func(context.Context, Caller, toolkit.Info) context.Context { panic("host exploded") }
		}},
		{name: "BeforeTool", tool: "whoami", path: "/mcp", callback: "BeforeTool", set: func(c *Config) {
			c.Plugins = append(c.Plugins, hook.Plugin{Name: "host", BeforeTool: func(context.Context, *hook.ToolCall) (*toolkit.Result, error) {
				panic("host exploded")
			}})
		}},
		{name: "OnToolError", tool: "fail", path: "/mcp", callback: "OnToolError", set: func(c *Config) {
			c.Plugins = append(c.Plugins, hook.Plugin{Name: "host", OnToolError: func(context.Context, *hook.ToolCall, *toolkit.Result) *toolkit.Result {
				panic("host exploded")
			}})
		}},
		{name: "AfterTool", tool: "whoami", path: "/mcp", callback: "AfterTool", set: func(c *Config) {
			c.Plugins = append(c.Plugins, hook.Plugin{Name: "host", AfterTool: func(context.Context, *hook.ToolCall, *toolkit.Result) { panic("host exploded") }})
		}},
		{name: "Gate", tool: "whoami", path: "/mcp", toolErr: true, set: func(c *Config) {
			c.Gate = func(context.Context, toolkit.Info) *toolkit.Result { panic("host exploded") }
		}},
	} {
		for _, protocol := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protocol=%v", tc.name, protocol), func(t *testing.T) {
				rec := &recorder{}
				f := newFixture(t, func(c *Config) {
					c.PanicAsProtocolError = protocol
					c.Plugins = []hook.Plugin{rec.plugin()}
					tc.set(c)
				})
				ep, token, args := f.bearer, f.aliceTok, map[string]any(nil)
				if tc.path == "/internal" {
					ep, token, args = f.arg, "", map[string]any{TokenArgument: f.aliceTok}
				}
				r := callT(t, f.mustClient(tc.path, token), tc.tool, args)
				internalError := fmt.Sprintf("Tool %s failed with an internal error.", tc.tool)
				if protocol {
					want := fmt.Sprintf("panic recovered in %s tool handler: host exploded", tc.tool)
					if r.err == nil || !strings.Contains(r.err.Error(), want) {
						t.Errorf("result = %+v, want the protocol error %q", r, want)
					}
				} else if r.err != nil || !r.isError || r.text != internalError {
					t.Errorf("result = %+v, want the error result %q", r, internalError)
				}
				if n := ep.InFlight(); n != 0 {
					t.Errorf("InFlight after the call = %d", n)
				}

				fin := rec.finished()
				if tc.noEvents {
					if len(rec.events) != 0 {
						t.Errorf("events = %+v, want none", rec.events)
					}
					return
				}
				if len(rec.events) != 2 || len(fin) != 1 {
					t.Fatalf("events = %+v, want ToolCallStarted and ToolCallFinished", rec.events)
				}
				if fin[0].Result.ForLLM != internalError || !fin[0].Result.IsError {
					t.Errorf("finished result = %+v", fin[0].Result)
				}
				var pe *hook.PanicError
				var tpe *hook.ToolPanicError
				switch {
				case tc.toolErr:
					if !errors.As(fin[0].Result.Err, &tpe) || tpe.Tool != tc.tool {
						t.Errorf("Err = %v, want a *hook.ToolPanicError", fin[0].Result.Err)
					}
				case !errors.As(fin[0].Result.Err, &pe) || pe.Callback != tc.callback || pe.Value != "host exploded":
					t.Errorf("Err = %v, want a %s *hook.PanicError", fin[0].Result.Err, tc.callback)
				}
			})
		}
	}
}
