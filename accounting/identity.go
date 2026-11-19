/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package accounting

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxNameLength is the longest agent or project name accepted, in
	// characters (Unicode code points).
	MaxNameLength = 16
	// DefaultProject is the project recorded when the caller names none.
	DefaultProject = "default"
)

var (
	// ErrAgentRequired reports an identity without an agent name.
	ErrAgentRequired = errors.New("agent name is required")
	// ErrNameTooLong reports an agent or project name longer than
	// MaxNameLength characters.
	ErrNameTooLong = errors.New("name is too long")
)

// Identity names who a launch is for: the agent (required) and the project
// (optional).
type Identity struct {
	Agent   string
	Project string
}

// Normalize returns the identity with surrounding whitespace trimmed and an
// empty Project set to DefaultProject. It returns an error wrapping
// ErrAgentRequired when Agent is empty, or ErrNameTooLong when Agent or
// Project is longer than MaxNameLength characters; names are never truncated.
func (id Identity) Normalize() (Identity, error) {
	out := Identity{
		Agent:   strings.TrimSpace(id.Agent),
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
