/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package cogmem

import "time"

// Compose defaults. These are levers exposed via the New(...) functional
// options (WithTopKDomains, etc.) and driven per-agent from memory.prompt
// config; the values here are the fallback defaults.
const (
	defaultTopKDomains   = 3
	defaultMaxChars      = 4000
	defaultMinConfidence = 0.65

	// defaultRecencyWindow is how recently a domain must have been active to
	// be loaded with no trigger matching it. Long enough to cover a weekend's
	// pause in a piece of work, short enough that an idle agent's context
	// empties instead of showing last month's topics as current.
	defaultRecencyWindow = 72 * time.Hour

	// defaultCarryover is how long a previous message or tool use keeps
	// loading the domains its triggers match (see Session). It covers the
	// follow-ups of one conversation; after it, a domain comes back only by a
	// new trigger or by the recency window.
	defaultCarryover = time.Hour

	// Attachment budgets (bytes). Sized so a real reference document — a writing
	// voice guide, a playbook — is injected whole; truncation is a safety valve
	// against a runaway file, not the expected path.
	defaultFileMaxBytes      = 256 * 1024
	defaultFileTotalMaxBytes = 512 * 1024
)

// maxSignalPhrases caps how many matched keyword phrases a routing signal (and
// so the trace and the injected header) names.
const maxSignalPhrases = 3

// maxHeadlineChars caps the memory text quoted in an attached document's header.
const maxHeadlineChars = 120
