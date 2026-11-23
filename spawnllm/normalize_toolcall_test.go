package spawnllm

import (
	"encoding/json"
	"testing"

	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

func TestNormalizeToolCall_NameFromFunction(t *testing.T) {
	tc := ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: &FunctionCall{
			Name:      "get_weather",
			Arguments: `{"location":"NYC"}`,
		},
	}
	result := NormalizeToolCall(tc, nil)
	if result.Name != "get_weather" {
		t.Errorf("Name = %q, want %q", result.Name, "get_weather")
	}
	if result.Function.Name != "get_weather" {
		t.Errorf("Function.Name = %q, want %q", result.Function.Name, "get_weather")
	}
	if result.Arguments["location"] != "NYC" {
		t.Errorf("Arguments[location] = %v, want NYC", result.Arguments["location"])
	}
}

func TestNormalizeToolCall_ArgumentsFromFunctionString(t *testing.T) {
	tc := ToolCall{
		ID:   "call_2",
		Type: "function",
		Name: "list_files",
		Function: &FunctionCall{
			Name:      "list_files",
			Arguments: `{"path":"/tmp"}`,
		},
	}
	result := NormalizeToolCall(tc, nil)
	if result.Arguments["path"] != "/tmp" {
		t.Errorf("Arguments[path] = %v, want /tmp", result.Arguments["path"])
	}
}

func TestNormalizeToolCall_NilFunctionCreated(t *testing.T) {
	tc := ToolCall{
		ID:        "call_3",
		Type:      "function",
		Name:      "read_file",
		Arguments: map[string]any{"path": "foo.txt"},
	}
	result := NormalizeToolCall(tc, nil)
	if result.Function == nil {
		t.Fatal("Function should not be nil after normalization")
	}
	if result.Function.Name != "read_file" {
		t.Errorf("Function.Name = %q, want %q", result.Function.Name, "read_file")
	}
	// Arguments should be encoded back into Function.Arguments
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result.Function.Arguments), &parsed); err != nil {
		t.Fatalf("Function.Arguments parse error: %v", err)
	}
	if parsed["path"] != "foo.txt" {
		t.Errorf("Function.Arguments path = %v, want foo.txt", parsed["path"])
	}
}

func TestNormalizeToolCall_NilArgumentsInitialized(t *testing.T) {
	tc := ToolCall{
		ID:   "call_4",
		Name: "no_args_tool",
	}
	result := NormalizeToolCall(tc, nil)
	if result.Arguments == nil {
		t.Error("Arguments should not be nil after normalization")
	}
}

func TestNormalizeToolCall_ExistingArgumentsPreserved(t *testing.T) {
	tc := ToolCall{
		ID:        "call_5",
		Name:      "search",
		Arguments: map[string]any{"query": "golang"},
		Function: &FunctionCall{
			Name:      "search",
			Arguments: `{"query":"old"}`, // should not override existing Arguments
		},
	}
	result := NormalizeToolCall(tc, nil)
	// Since Arguments is already set, it should not be overwritten from Function.Arguments
	if result.Arguments["query"] != "golang" {
		t.Errorf("Arguments[query] = %v, want golang (pre-set arguments should be preserved)", result.Arguments["query"])
	}
}

func TestNormalizeToolCall_FunctionNameFallback(t *testing.T) {
	// Name is empty, Function.Name should be copied to top-level Name
	tc := ToolCall{
		ID:   "call_6",
		Type: "function",
		Function: &FunctionCall{
			Name:      "calculate",
			Arguments: `{}`,
		},
	}
	result := NormalizeToolCall(tc, nil)
	if result.Name != "calculate" {
		t.Errorf("Name = %q, want calculate", result.Name)
	}
}

func TestNormalizeToolCall_InvalidFunctionArguments(t *testing.T) {
	// Invalid JSON in Function.Arguments should not crash
	tc := ToolCall{
		ID:   "call_7",
		Name: "tool",
		Function: &FunctionCall{
			Name:      "tool",
			Arguments: `not-valid-json`,
		},
	}
	result := NormalizeToolCall(tc, nil)
	// Arguments should be initialized to empty map, not panic
	if result.Arguments == nil {
		t.Error("Arguments should not be nil")
	}
}

func TestNormalizeToolCall_UnencodableArgumentsAreLogged(t *testing.T) {
	logs := &launchtest.Logs{}
	tc := ToolCall{ID: "call_8", Name: "send", Arguments: map[string]any{"ch": make(chan int)}}

	result := NormalizeToolCall(tc, logs.Logger())
	if result.Function == nil || result.Function.Name != "send" || result.Function.Arguments != "" {
		t.Errorf("Function = %+v, want name send and no encoded arguments", result.Function)
	}
	e, ok := logs.Find("tool call arguments are not JSON-encodable")
	if !ok || e.Attrs["tool"] != "send" || e.Attrs["component"] != "provider" {
		t.Errorf("record = %+v (found %v), want one naming tool send", e, ok)
	}

	// A nil logger discards the record instead of panicking.
	if got := NormalizeToolCall(tc, nil); got.Name != "send" {
		t.Errorf("Name = %q, want send", got.Name)
	}
}

func TestNormalizeToolCall_FillsEmptyFunctionFields(t *testing.T) {
	tc := ToolCall{
		ID:        "call_9",
		Name:      "lookup",
		Arguments: map[string]any{"q": "alice"},
		Function:  &FunctionCall{},
	}
	result := NormalizeToolCall(tc, nil)
	if result.Function.Name != "lookup" || result.Function.Arguments != `{"q":"alice"}` {
		t.Errorf("Function = %+v, want name lookup and the encoded arguments", result.Function)
	}
}
