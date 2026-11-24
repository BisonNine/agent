/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateID reports whether id is a canonical concept ID under the default
// limits: the bundle-relative path of the concept's file without its ".md",
// with "/" between segments. The error is a *ValidationError (ErrInvalid)
// naming the rule broken. A Store applies its own Limits.
func ValidateID(id string) error {
	if reason := idProblem(id, DefaultLimits); reason != "" {
		return invalid("", id, FieldError{Field: "concept_id", Reason: reason})
	}
	return nil
}

// idProblem returns why id is not a canonical concept ID, or "".
func idProblem(id string, l Limits) string {
	switch {
	case id == "":
		return "is empty"
	case len(id) > l.IDBytes:
		return fmt.Sprintf("is %d bytes; the limit is %d", len(id), l.IDBytes)
	case !utf8.ValidString(id):
		return "is not valid UTF-8"
	case strings.HasPrefix(id, "/"):
		return `must not start with "/"`
	case strings.HasSuffix(id, "/"):
		return `must not end with "/"`
	case strings.HasSuffix(strings.ToLower(id), ".md"):
		return `must not end with ".md"`
	}
	for _, r := range id {
		if r == '\\' {
			return `must not contain "\"`
		}
		if unicode.IsControl(r) {
			return "must not contain control characters"
		}
		if unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Sprintf("must not contain the invisible or separator character %U", r)
		}
	}
	segments := strings.Split(id, "/")
	for _, seg := range segments {
		switch {
		case seg == "":
			return "must not contain an empty segment"
		case strings.HasPrefix(seg, "."):
			return `must not contain a segment that starts with "."`
		case strings.TrimSpace(seg) == "":
			return "must not contain a blank segment"
		case strings.TrimSpace(seg) != seg:
			return "must not have a segment that starts or ends with a space"
		case len(seg) > l.SegmentBytes:
			return fmt.Sprintf("has a segment of %d bytes; the limit is %d", len(seg), l.SegmentBytes)
		}
	}
	if last := strings.ToLower(segments[len(segments)-1]); last == "index" || last == "log" {
		return fmt.Sprintf("uses the reserved name %q", segments[len(segments)-1])
	}
	return ""
}

// fileName returns the bundle-relative file path of a concept ID. The ".md"
// is appended, never substituted: "SFR/FCS_COP.1" is "SFR/FCS_COP.1.md".
func fileName(id string) string { return id + ".md" }
