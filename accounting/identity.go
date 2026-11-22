/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package accounting

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxNameLength is the longest agent id, friendly name or project
	// accepted, in characters (Unicode code points). It holds a UUID.
	MaxNameLength = 64
	// DefaultProject is the project recorded when the caller names none.
	DefaultProject = "default"
)

var (
	// ErrAgentRequired reports an identity without an agent name.
	ErrAgentRequired = errors.New("agent name is required")
	// ErrNameTooLong reports an agent or project longer than MaxNameLength
	// characters.
	ErrNameTooLong = errors.New("name is too long")
)

// ellipsis ends a Name shortened to MaxNameLength characters.
const ellipsis = "…"

// Identity names who a launch is for: the agent's stable id (required), its
// friendly name (optional) and the project (optional). Name is only a label
// for readers; records are grouped and priced by Agent, never by Name.
type Identity struct {
	Agent   string
	Name    string
	Project string
}

// Normalize returns the identity with surrounding whitespace trimmed, an
// empty Project set to DefaultProject and a Name longer than MaxNameLength
// characters shortened to that length, ending in "…". It returns an error
// wrapping ErrAgentRequired when Agent is empty, or ErrNameTooLong when Agent
// or Project is longer than MaxNameLength characters; those are never
// shortened.
func (id Identity) Normalize() (Identity, error) {
	out := Identity{
		Agent:   strings.TrimSpace(id.Agent),
		Name:    truncateName(strings.TrimSpace(id.Name)),
		Project: strings.TrimSpace(id.Project),
	}
	if out.Agent == "" {
		return Identity{}, fmt.Errorf("accounting: %w", ErrAgentRequired)
	}
	if n := utf8.RuneCountInString(out.Agent); n > MaxNameLength {
		return Identity{}, fmt.Errorf("accounting: agent name has %d characters, at most %d allowed: %w",
			n, MaxNameLength, ErrNameTooLong)
	}
	if n := utf8.RuneCountInString(out.Project); n > MaxNameLength {
		return Identity{}, fmt.Errorf("accounting: project name has %d characters, at most %d allowed: %w",
			n, MaxNameLength, ErrNameTooLong)
	}
	if out.Project == "" {
		out.Project = DefaultProject
	}
	return out, nil
}

// truncateName shortens name to MaxNameLength characters, the last of them
// an ellipsis, when it is longer.
func truncateName(name string) string {
	if utf8.RuneCountInString(name) <= MaxNameLength {
		return name
	}
	runes := []rune(name)[:MaxNameLength-utf8.RuneCountInString(ellipsis)]
	return strings.TrimRightFunc(string(runes), unicode.IsSpace) + ellipsis
}
