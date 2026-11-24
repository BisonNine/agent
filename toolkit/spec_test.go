package toolkit

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.bison9.com/agent/toolspec"
)

// fakeSpecDefs returns bare-named definitions for bridge testing.
func fakeSpecDefs() []toolspec.ToolDefinition {
	return []toolspec.ToolDefinition{
		{
			Name:        "read",
			Description: "read a thing",
			Parameters: []toolspec.Parameter{
				{Name: "path", Type: "string", Required: true, Description: "the path"},
			},
			Group:          "files",
			RevealTogether: true,
			Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
				path, ok := call.Args["path"].(string)
				if !ok {
					return nil, fmt.Errorf("path is %T, want string", call.Args["path"])
				}
				return &toolspec.Result{ForLLM: "read:" + path + " sess=" + call.Session +
					" agent=" + call.AgentID + " chan=" + call.Channel + " chat=" + call.ChatID}, nil
			},
		},
		{
			Name:          "watch",
			Description:   "watch a thing",
			SessionScoped: true,
			Async:         true,
			Handler: func(call *toolspec.ToolCall) (*toolspec.Result, error) {
				if call.Notify != nil {
					call.Notify(&toolspec.Result{ForLLM: "done"})
				}
				return &toolspec.Result{ForLLM: "started", Async: true}, nil
			},
		},
	}
}

func TestFromSpec_PrefixesNames(t *testing.T) {
	defs := fakeSpecDefs()
	if got := FromSpec("file", defs[0]).Name(); got != "file_read" {
		t.Errorf("Name = %q, want file_read", got)
	}
	if got := FromSpec("", defs[0]).Name(); got != "read" {
		t.Errorf("Name with no namespace = %q, want read", got)
	}
}

func TestFromSpec_ExecuteAndSchema(t *testing.T) {
	read := FromSpec("file", fakeSpecDefs()[0])
	props, ok := read.Parameters()["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map in schema, got %v", read.Parameters())
	}
	if _, has := props["path"]; !has {
		t.Fatalf("expected 'path' property in schema, got %v", props)
	}
	// Who the call is for is threaded from ctx into the call.
	ctx := WithSessionKey(context.Background(), "sess-1")
	ctx = WithAgentID(ctx, "alice")
	ctx = WithChat(ctx, "web", "chat-7")
	res := read.Execute(ctx, map[string]any{"path": "/x"})
	if want := "read:/x sess=sess-1 agent=alice chan=web chat=chat-7"; res.IsError || res.ForLLM != want {
		t.Fatalf("result = %+v, want ForLLM %q", res, want)
	}
	// A handler error becomes an error result carrying the error.
	res = read.Execute(context.Background(), map[string]any{"path": 3})
	if !res.IsError || res.Err == nil || res.ForLLM != "path is int, want string" {
		t.Fatalf("handler error result = %+v", res)
	}
	g, ok := read.(DiscoveryGrouped)
	if !ok {
		t.Fatal("a spec tool reports its discovery group")
	}
	if group, together := g.DiscoveryGroup(); group != "files" || !together {
		t.Fatalf("DiscoveryGroup = %q, %v", group, together)
	}
}

func TestFromSpec_AsyncAndSessionInterfaces(t *testing.T) {
	defs := fakeSpecDefs()
	watch := FromSpec("file", defs[1])
	ss, ok := watch.(SessionScoped)
	if !ok || !ss.IsSessionScoped() {
		t.Fatal("file_watch should be SessionScoped")
	}
	ae, ok := watch.(AsyncExecutor)
	if !ok {
		t.Fatal("file_watch should be AsyncExecutor")
	}
	var got *Result
	res := ae.ExecuteAsync(context.Background(), map[string]any{}, func(_ context.Context, r *Result) { got = r })
	if !res.Async {
		t.Fatalf("expected async result, got %+v", res)
	}
	if got == nil || got.ForLLM != "done" {
		t.Fatalf("expected notify callback to deliver 'done', got %+v", got)
	}
	read := FromSpec("file", defs[0])
	if _, ok := read.(AsyncExecutor); ok {
		t.Fatal("file_read must not be AsyncExecutor")
	}
	if _, ok := read.(SessionScoped); ok {
		t.Fatal("file_read must not be SessionScoped")
	}
	// Each flag alone gives exactly its interface.
	asyncOnly := FromSpec("x", toolspec.ToolDefinition{Name: "a", Async: true})
	if _, ok := asyncOnly.(SessionScoped); ok {
		t.Fatal("an async-only tool must not be SessionScoped")
	}
	sessionOnly := FromSpec("x", toolspec.ToolDefinition{Name: "s", SessionScoped: true})
	if _, ok := sessionOnly.(AsyncExecutor); ok {
		t.Fatal("a session-only tool must not be AsyncExecutor")
	}
}

func TestResultFromSpec(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		in      *toolspec.Result
		err     error
		wantLLM string
		wantErr bool
		wantIs  error
	}{
		{"nil result, nil error", nil, nil, "tool returned no result", true, nil},
		{"nil result with error", nil, boom, "boom", true, boom},
		{"result with error keeps its text", &toolspec.Result{ForLLM: "partial"}, boom, "partial", true, boom},
		{"empty result with error takes its text", &toolspec.Result{}, boom, "boom", true, boom},
		{"plain result", &toolspec.Result{ForLLM: "ok", Media: []string{"m"}}, nil, "ok", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResultFromSpec(tc.in, tc.err)
			if got.ForLLM != tc.wantLLM || got.IsError != tc.wantErr {
				t.Fatalf("ResultFromSpec = %+v, want ForLLM %q IsError %v", got, tc.wantLLM, tc.wantErr)
			}
			if tc.wantIs != nil && !errors.Is(got.Err, tc.wantIs) {
				t.Fatalf("Err = %v, want %v", got.Err, tc.wantIs)
			}
		})
	}
}

func TestResultToSpec(t *testing.T) {
	if got := ResultToSpec(nil); !got.IsError || got.ForLLM != "tool returned no result" {
		t.Fatalf("ResultToSpec(nil) = %+v", got)
	}
	in := &Result{ForLLM: "a", ForUser: "b", Silent: true, Media: []string{"m"}, Images: []string{"i"}}
	got := ResultToSpec(in)
	if got.ForLLM != "a" || got.ForUser != "b" || !got.Silent || len(got.Media) != 1 || len(got.Images) != 1 {
		t.Fatalf("ResultToSpec = %+v", got)
	}
}

func TestContextValues(t *testing.T) {
	ctx := context.Background()
	if Channel(ctx) != "" || ChatID(ctx) != "" || SessionKey(ctx) != "" || AgentID(ctx) != "" {
		t.Fatal("an empty context carries no values")
	}
	ctx = WithChat(ctx, "web", "chat-42")
	ctx = WithSessionKey(ctx, "agent:alice:main")
	ctx = WithAgentID(ctx, "alice")
	if Channel(ctx) != "web" || ChatID(ctx) != "chat-42" ||
		SessionKey(ctx) != "agent:alice:main" || AgentID(ctx) != "alice" {
		t.Fatalf("values = %q %q %q %q", Channel(ctx), ChatID(ctx), SessionKey(ctx), AgentID(ctx))
	}
}
