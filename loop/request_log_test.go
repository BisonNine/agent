package loop

import (
	"strings"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"
)

func TestFormatMessagesForLog(t *testing.T) {
	t.Run("empty request", func(t *testing.T) {
		for _, msgs := range [][]spawnllm.Message{nil, {}} {
			if got := formatMessagesForLog(msgs, hook.Truncate); got != "[]" {
				t.Fatalf("formatMessagesForLog(%v) = %q, want []", msgs, got)
			}
		}
	})
	t.Run("every field", func(t *testing.T) {
		msgs := []spawnllm.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", ToolCalls: []spawnllm.ToolCall{
				{ID: "c1", Type: "function", Name: "lookup", Function: &spawnllm.FunctionCall{Name: "lookup", Arguments: `{"q":"Bob"}`}},
				{ID: "c2", Type: "function", Name: "noop"},
			}},
			{Role: "tool", Content: "found", ToolCallID: "c1"},
		}
		want := "[\n" +
			"  [0] Role: user\n" +
			"  Content: hello\n" +
			"\n" +
			"  [1] Role: assistant\n" +
			"  ToolCalls:\n" +
			"    - ID: c1, Type: function, Name: lookup\n" +
			"      Arguments: {\"q\":\"Bob\"}\n" +
			"    - ID: c2, Type: function, Name: noop\n" +
			"\n" +
			"  [2] Role: tool\n" +
			"  Content: found\n" +
			"  ToolCallID: c1\n" +
			"\n" +
			"]"
		if got := formatMessagesForLog(msgs, hook.Truncate); got != want {
			t.Fatalf("formatMessagesForLog =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("content and arguments go through the truncator", func(t *testing.T) {
		var limits []int
		truncate := func(s string, limit int) string {
			limits = append(limits, limit)
			return "<cut>"
		}
		msgs := []spawnllm.Message{{
			Role:      "assistant",
			Content:   strings.Repeat("x", 500),
			ToolCalls: []spawnllm.ToolCall{{ID: "c1", Name: "lookup", Function: &spawnllm.FunctionCall{Arguments: strings.Repeat("y", 500)}}},
		}}
		got := formatMessagesForLog(msgs, truncate)
		if strings.Contains(got, "xxx") || strings.Contains(got, "yyy") || strings.Count(got, "<cut>") != 2 {
			t.Fatalf("formatMessagesForLog = %q, want content and arguments truncated", got)
		}
		if len(limits) != 2 || limits[0] != requestLogLimit || limits[1] != requestLogLimit {
			t.Fatalf("truncator limits = %v, want %d twice", limits, requestLogLimit)
		}
	})
}

func TestFormatToolsForLog(t *testing.T) {
	t.Run("no tools", func(t *testing.T) {
		for _, defs := range [][]spawnllm.ToolDefinition{nil, {}} {
			if got := formatToolsForLog(defs, hook.Truncate); got != "[]" {
				t.Fatalf("formatToolsForLog(%v) = %q, want []", defs, got)
			}
		}
	})
	t.Run("every field", func(t *testing.T) {
		defs := []spawnllm.ToolDefinition{
			{Type: "function", Function: spawnllm.ToolFunctionDefinition{Name: "lookup", Description: "Find Bob", Parameters: map[string]any{"type": "object"}}},
			{Type: "function", Function: spawnllm.ToolFunctionDefinition{Name: "noop", Description: "Nothing"}},
		}
		want := "[\n" +
			"  [0] Type: function, Name: lookup\n" +
			"      Description: Find Bob\n" +
			"      Parameters: map[type:object]\n" +
			"  [1] Type: function, Name: noop\n" +
			"      Description: Nothing\n" +
			"]"
		if got := formatToolsForLog(defs, hook.Truncate); got != want {
			t.Fatalf("formatToolsForLog =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("parameters go through the truncator", func(t *testing.T) {
		var limit int
		defs := []spawnllm.ToolDefinition{{Type: "function", Function: spawnllm.ToolFunctionDefinition{Name: "lookup", Parameters: map[string]any{"q": strings.Repeat("z", 500)}}}}
		got := formatToolsForLog(defs, func(_ string, l int) string { limit = l; return "<cut>" })
		if strings.Contains(got, "zzz") || !strings.Contains(got, "Parameters: <cut>") || limit != requestLogLimit {
			t.Fatalf("formatToolsForLog = %q (limit %d), want parameters truncated at %d", got, limit, requestLogLimit)
		}
	})
}
