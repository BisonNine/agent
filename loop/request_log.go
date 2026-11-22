package loop

// This file is part of the turn engine (the engine_*.go files): it imports no
// host package and uses no host identifier, so it can live in a package of
// its own. engine_guard_test.go enforces that.

import (
	"fmt"
	"strings"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/spawnllm"
)

// requestLogLimit is how many runes of a message's content, a tool call's
// arguments or a tool's parameters the "Full LLM request" line keeps.
const requestLogLimit = 200

// formatMessagesForLog renders messages for the "Full LLM request" line,
// shortening long text with truncate.
func formatMessagesForLog(messages []spawnllm.Message, truncate hook.Truncator) string {
	if len(messages) == 0 {
		return "[]"
	}

	var sb strings.Builder
	sb.WriteString("[\n")
	for i, msg := range messages {
		fmt.Fprintf(&sb, "  [%d] Role: %s\n", i, msg.Role)
		if len(msg.ToolCalls) > 0 {
			sb.WriteString("  ToolCalls:\n")
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&sb, "    - ID: %s, Type: %s, Name: %s\n", tc.ID, tc.Type, tc.Name)
				if tc.Function != nil {
					fmt.Fprintf(
						&sb,
						"      Arguments: %s\n",
						truncate(tc.Function.Arguments, requestLogLimit),
					)
				}
			}
		}
		if msg.Content != "" {
			content := truncate(msg.Content, requestLogLimit)
			fmt.Fprintf(&sb, "  Content: %s\n", content)
		}
		if msg.ToolCallID != "" {
			fmt.Fprintf(&sb, "  ToolCallID: %s\n", msg.ToolCallID)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("]")
	return sb.String()
}

// formatToolsForLog renders tool definitions for the "Full LLM request"
// line, shortening long parameters with truncate.
func formatToolsForLog(toolDefs []spawnllm.ToolDefinition, truncate hook.Truncator) string {
	if len(toolDefs) == 0 {
		return "[]"
	}

	var sb strings.Builder
	sb.WriteString("[\n")
	for i, tool := range toolDefs {
		fmt.Fprintf(&sb, "  [%d] Type: %s, Name: %s\n", i, tool.Type, tool.Function.Name)
		fmt.Fprintf(&sb, "      Description: %s\n", tool.Function.Description)
		if len(tool.Function.Parameters) > 0 {
			fmt.Fprintf(
				&sb,
				"      Parameters: %s\n",
				truncate(fmt.Sprintf("%v", tool.Function.Parameters), requestLogLimit),
			)
		}
	}
	sb.WriteString("]")
	return sb.String()
}
