/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
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
	// MaxNameLength is the longest agent id, friendly name or billing code
	// accepted, in characters (Unicode code points). It holds a UUID.
	MaxNameLength = 64
	// DefaultBillingCode is the billing code recorded when the caller names none.
	DefaultBillingCode = "default"
)

var (
	// ErrAgentRequired reports an identity without an agent name.
	ErrAgentRequired = errors.New("agent name is required")
	// ErrNameTooLong reports an agent or billing code longer than MaxNameLength
	// characters.
	ErrNameTooLong = errors.New("name is too long")
	// ErrInvalidBillingCode reports a billing code that CheckBillingCode refuses.
	ErrInvalidBillingCode = errors.New("invalid billing code")
)

// CheckBillingCode reports whether code is a valid billing code: 1 to
// MaxNameLength characters, each an ASCII letter, digit, '.', '_' or '-'.
// Case matters ("Garden" and "garden" are two billing codes) and nothing is
// trimmed. It returns nil or an error wrapping ErrInvalidBillingCode.
// Normalize does not call it: a host that lets someone choose a billing code
// checks it here first.
func CheckBillingCode(code string) error {
	if code == "" {
		return fmt.Errorf("accounting: billing code is empty: %w", ErrInvalidBillingCode)
	}
	if len(code) > MaxNameLength {
		return fmt.Errorf("accounting: billing code has more than %d characters: %w", MaxNameLength, ErrInvalidBillingCode)
	}
	for i := 0; i < len(code); i++ {
		if !billingCodeByte(code[i]) {
			return fmt.Errorf("accounting: billing code %q has a character other than a letter, digit, '.', '_' or '-': %w",
				code, ErrInvalidBillingCode)
		}
	}
	return nil
}

// billingCodeByte reports whether b may appear in a billing code.
func billingCodeByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return b == '.' || b == '_' || b == '-'
}

// ellipsis ends a Name shortened to MaxNameLength characters.
const ellipsis = "…"

// Identity names who a launch is for: the working agent's stable id
// (required), its friendly name (optional), the billing code (optional) and
// the origin agent (optional), when that is another agent than the working
// agent (a sub-agent's launches name the agent that started the chain). Name
// is only a label for readers; records are grouped and priced by Agent or
// Origin, never by Name.
type Identity struct {
	Origin      string
	Agent       string
	Name        string
	BillingCode string
}

// Normalize returns the identity with surrounding whitespace trimmed, an
// empty BillingCode set to DefaultBillingCode, an empty Origin set to Agent
// and a Name longer than MaxNameLength characters shortened to that length,
// ending in "…". It returns an error wrapping ErrAgentRequired when Agent is
// empty, or ErrNameTooLong when Agent, Origin or BillingCode is longer than
// MaxNameLength characters; those are never shortened.
func (id Identity) Normalize() (Identity, error) {
	out := Identity{
		Origin:      strings.TrimSpace(id.Origin),
		Agent:       strings.TrimSpace(id.Agent),
		Name:        truncateName(strings.TrimSpace(id.Name)),
		BillingCode: strings.TrimSpace(id.BillingCode),
	}
	if out.Agent == "" {
		return Identity{}, fmt.Errorf("accounting: %w", ErrAgentRequired)
	}
	if n := utf8.RuneCountInString(out.Agent); n > MaxNameLength {
		return Identity{}, fmt.Errorf("accounting: agent name has %d characters, at most %d allowed: %w",
			n, MaxNameLength, ErrNameTooLong)
	}
	if n := utf8.RuneCountInString(out.Origin); n > MaxNameLength {
		return Identity{}, fmt.Errorf("accounting: origin agent name has %d characters, at most %d allowed: %w",
			n, MaxNameLength, ErrNameTooLong)
	}
	if out.Origin == "" {
		out.Origin = out.Agent
	}
	if n := utf8.RuneCountInString(out.BillingCode); n > MaxNameLength {
		return Identity{}, fmt.Errorf("accounting: billing code has %d characters, at most %d allowed: %w",
			n, MaxNameLength, ErrNameTooLong)
	}
	if out.BillingCode == "" {
		out.BillingCode = DefaultBillingCode
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
