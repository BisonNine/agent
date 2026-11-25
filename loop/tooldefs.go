/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

import (
	"go.bison9.com/agent/discovery"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// Defs is the tool list of one model call in the provider type: what the
// engine sends and what it counts in the model's context.
type Defs struct {
	// Send is the list given to the provider: every tool the snapshot
	// offers, sorted by internal name, the ones hidden by progressive
	// discovery marked Deferred (spawnllm loads a deferred tool once a tool
	// result references it, natively where the provider can). It does not
	// depend on what was revealed, so it stays the same on every call while
	// the tools do (a stable prompt-cache prefix).
	Send []spawnllm.ToolDefinition
	// InContext is what reaches the model's context, and so what the tool
	// schemas cost: the tools that are not hidden, then the hidden ones
	// revealed has seen, in order of first reveal; none Deferred.
	InContext []spawnllm.ToolDefinition
}

// ToolDefs is the tool list the engine builds for a model call from snap
// (one snapshot of the agent's registry) and revealed (what the
// conversation's tool results have referenced; nil for nothing, which only
// shapes InContext). It is the function the engine itself calls, so a host
// that shows or tests the tool list gets exactly what the model is sent.
// Both lists are empty, never nil, for a snapshot with no tool, and nil for
// a nil snapshot (an agent with no tools).
//
// It is discovery.ToolDefs for a provider that loads deferred tools
// natively, copied into spawnllm's type.
func ToolDefs(snap *toolkit.Snapshot, revealed *discovery.Session) Defs {
	if snap == nil {
		return Defs{}
	}
	d := discovery.ToolDefs(snap, revealed, true)
	return Defs{Send: providerDefs(d.Send), InContext: providerDefs(d.InContext)}
}

// providerDefs copies discovery's definitions into the provider type.
func providerDefs(in []discovery.Def) []spawnllm.ToolDefinition {
	out := make([]spawnllm.ToolDefinition, 0, len(in))
	for _, d := range in {
		out = append(out, spawnllm.ToolDefinition{
			Type: "function",
			Function: spawnllm.ToolFunctionDefinition{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.Parameters,
			},
			Deferred:   d.Deferred,
			ToolSearch: d.ToolSearch,
		})
	}
	return out
}
