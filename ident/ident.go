// Package ident is the agent id rule: which ids are valid, an id's identity
// and when two ids name the same agent. It imports only the standard library,
// so a host's configuration, a forum, a classifier and the agent loop can all
// apply one rule.
//
// An agent id is 1 to MaxIDLength characters of ASCII letters (either case),
// digits, '-' and '_', and starts with a letter or digit. Ids are
// case-insensitive: an agent's identity is its id in lower case
// (NormalizeID), and SameID compares identities. A host keeps an id as it was
// written and uses NormalizeID wherever the id names something (a folder, a
// session, a record).
package ident

import (
	"regexp"
	"strings"
)

// MaxIDLength is the longest agent id, in characters (ASCII, so bytes).
const MaxIDLength = 64

// invalidRuns matches each run of characters an id may not hold.
var invalidRuns = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// ValidID reports whether id satisfies the agent id rule.
func ValidID(id string) bool {
	if id == "" || len(id) > MaxIDLength {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// NormalizeID is an id's identity: a valid id in lower case. A valid id
// becomes its lower-case form; any other input is repaired first (RepairID)
// and then lower-cased. It returns "" when nothing of the input survives the
// repair; a host that routes such input to a default agent substitutes that
// agent's id itself.
func NormalizeID(id string) string {
	if ValidID(id) {
		return asciiLower(id)
	}
	return asciiLower(RepairID(id))
}

// SameID reports whether a and b name the same agent: both have an identity
// (NormalizeID is not "") and the identities are equal, so "Bob" and "bob"
// are the same agent and two unusable ids are never the same.
func SameID(a, b string) bool {
	na := NormalizeID(a)
	return na != "" && na == NormalizeID(b)
}

// RepairID is the valid id suggested for id, keeping its case: every run of
// characters outside letters, digits, '-' and '_' becomes one '-', leading
// and trailing '-' and '_' are removed, and the result is cut to MaxIDLength
// characters without a trailing '-' or '_'. It is "" when nothing is left.
// A host uses it to suggest a fix when it refuses an id.
func RepairID(id string) string {
	result := strings.Trim(invalidRuns.ReplaceAllString(id, "-"), "-_")
	if len(result) > MaxIDLength {
		result = strings.TrimRight(result[:MaxIDLength], "-_")
	}
	return result
}

// asciiLower lower-cases A-Z only. Unicode case mapping differs between
// languages (U+0130 is one rune in Go and two in JavaScript), so the rule
// leaves every other character to the repair, which a web front end can
// reproduce exactly.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}
