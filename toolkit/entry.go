/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"context"

	"go.bison9.com/agent/toolspec"
)

// Entry is one tool to put in a catalogue, and how it is offered. It is an
// input value: the catalogue copies it and keeps no reference to it.
type Entry struct {
	// Tool is the tool; its Name is the name it is registered under.
	Tool Tool
	// Hidden marks a tool kept out of the listed tools: it is offered only
	// through progressive discovery, and may still be called at any time.
	Hidden bool
	// Group names the discovery set the tool belongs to; RevealTogether
	// asks discovery to reveal the whole group when one member is revealed.
	Group          string
	RevealTogether bool
	// Meta is the host's own data about the tool (a policy exemption, a
	// label), carried unchanged into Info.Meta and published in the same
	// generation as the tool. toolkit never inspects or copies it: treat it
	// as immutable once registered. nil by default.
	Meta any
}

// EntryFromSpec returns def as a listed Entry published under ns (see
// FromSpec), carrying def's Group and RevealTogether.
func EntryFromSpec(ns string, def toolspec.ToolDefinition) Entry {
	return Entry{
		Tool:           FromSpec(ns, def),
		Group:          def.Group,
		RevealTogether: def.RevealTogether,
	}
}

// Toolset is a named group of tools mounted and removed as a unit: an
// upstream server's tools, a service's tools, or any group whose contents
// change. Adding a toolset again under the same name replaces its tools in
// one step.
type Toolset interface {
	// Name identifies the toolset in the catalogue (see Info.Source).
	Name() string
	// Tools returns the toolset's current tools.
	Tools(ctx context.Context) ([]Entry, error)
}
