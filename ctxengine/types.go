/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine

import (
	"time"
)

// ModelChain records which LLM chain is configured for compression (for stats and logging).
type ModelChain struct {
	Primary   string   `json:"primary,omitempty"`
	Fallbacks []string `json:"fallbacks,omitempty"`
}

// ContextStats holds observable state for a session's context.
type ContextStats struct {
	TotalMessages       int
	MeaningfulMessages  int
	EstimatedTokens     int
	ContextWindowPct    float64
	LastCompressedAt    time.Time
	LastCompressionGain float64
	CompressionCooling  bool
	CoolingSinceCount   int
	// SummaryTokens is the estimated token count of the stored summary (runes/4).
	// Zero when no summary has been generated.
	SummaryTokens int
}

// CompactionReason says what started a compaction pass.
type CompactionReason string

const (
	// CompactionReasonSafetyNet: the request or stored history reached the
	// safety line (an add, Assemble, PreDispatchCheck or CheckAndCompress).
	CompactionReasonSafetyNet CompactionReason = "safety_net"
	// CompactionReasonThreshold: history reached the normal percentage trigger.
	CompactionReasonThreshold CompactionReason = "threshold"
	// CompactionReasonCount: enough messages arrived since the last pass.
	CompactionReasonCount CompactionReason = "count"
	// CompactionReasonAge: the oldest live message passed the trigger age.
	CompactionReasonAge CompactionReason = "age"
	// CompactionReasonManual: the host called Compact.
	CompactionReasonManual CompactionReason = "manual"
	// CompactionReasonOverflow: the host called ForceCompress after a provider
	// rejected the request as too large.
	CompactionReasonOverflow CompactionReason = "overflow"
)

// CompactionStart describes a compaction pass that is about to call the
// summarization model. See WithCompactionStartFunc.
type CompactionStart struct {
	SessionKey string
	Reason     CompactionReason
	// Blocking is true when the pass runs inside a call a turn waits on: an
	// add, Assemble, PreDispatchCheck, CheckAndCompress or ForceCompress. It is
	// false for Compact and CompactIfDue.
	Blocking bool
}
