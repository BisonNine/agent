/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

import (
	"fmt"
	"unicode/utf8"
)

// A tool result is spliced into the current turn, which compaction cannot
// shrink, so an oversized one (5.7 MB has been seen in production) ends in
// max_tokens halving and a failed turn. Cap it at dispatch, keeping the head.
const (
	// toolResultCapWindowFraction is the share of the model's context window
	// one tool result may occupy.
	toolResultCapWindowFraction = 0.25
	// toolResultCharsPerToken converts that token share to characters.
	toolResultCharsPerToken = 4
	// toolResultCapFloor and toolResultCapCeiling bound the computed cap.
	toolResultCapFloor   = 16 << 10
	toolResultCapCeiling = 512 << 10
	// toolErrorMinChars is the error text a cap never cuts into: a failing
	// tool's message must reach the model intact.
	toolErrorMinChars = 4 << 10
)

// ToolResultCap returns the character cap of one tool result for a model
// whose context window is contextWindow tokens: a quarter of the window at
// four characters a token, kept between 16 KiB and 512 KiB.
func ToolResultCap(contextWindow int) int {
	limit := int(float64(contextWindow) * toolResultCapWindowFraction * toolResultCharsPerToken)
	return min(max(limit, toolResultCapFloor), toolResultCapCeiling)
}

// CapToolResult truncates content to ToolResultCap(contextWindow), keeping
// the head (never splitting a rune) and appending a marker that tells the
// model how to see the rest; content within the cap is returned unchanged.
// Error text (isError) is never cut below 4 KiB. The engine applies it to
// every tool result it stores; a host applies it to tool results it
// delivers itself, so they are capped alike.
func CapToolResult(content string, isError bool, contextWindow int) string {
	limit := ToolResultCap(contextWindow)
	if isError && limit < toolErrorMinChars {
		limit = toolErrorMinChars
	}
	if len(content) <= limit {
		return content
	}
	// Back off a continuation byte so a multi-byte rune is not split.
	cut := limit
	for cut > 0 && cut > limit-utf8.UTFMax && !utf8.RuneStart(content[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n[output truncated: kept %d of %d characters. "+
		"Use the tool's paging/range options or a narrower query to see more.]",
		content[:cut], cut, len(content))
}
