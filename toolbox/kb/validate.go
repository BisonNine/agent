/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// recordFields checks a frontmatter and body against the write rules that
// need no bundle: the frontmatter schema, OKF's fields (sources entries
// need a resource; timestamps are RFC 3339) and the include list. Sizes are
// checked by the caller.
func (s *Store) recordFields(id string, fm Frontmatter) []FieldError {
	out := schemaFields(compiledFrontmatterSchema(), fm, "")
	for i, src := range asList(fm["sources"]) {
		p := "frontmatter.sources[" + strconv.Itoa(i) + "]"
		m, ok := src.(map[string]any)
		if !ok {
			out = append(out, FieldError{Field: p, Reason: "must be a mapping with a resource"})
			continue
		}
		if r, isString := m["resource"].(string); !isString || r == "" {
			out = append(out, FieldError{Field: p + ".resource", Reason: "is required in a sources entry"})
		}
		out = append(out, timestampField(p+".last_modified", m["last_modified"])...)
		out = append(out, windowFields(p+".usage_window", m["usage_window"])...)
	}
	if g, ok := fm["generated"].(map[string]any); ok {
		out = append(out, timestampField("frontmatter.generated.at", g["at"])...)
	}
	for i, v := range asList(fm["verified"]) {
		if m, ok := v.(map[string]any); ok {
			out = append(out, timestampField("frontmatter.verified["+strconv.Itoa(i)+"].at", m["at"])...)
		}
	}
	out = append(out, timestampField("frontmatter.stale_after", fm["stale_after"])...)
	out = append(out, windowFields("frontmatter.usage_window", fm["usage_window"])...)
	for i, inc := range asList(fm[IncludeKey]) {
		p := "frontmatter." + IncludeKey + "[" + strconv.Itoa(i) + "]"
		ref, ok := inc.(string)
		if !ok {
			continue // the schema reports it
		}
		if reason := idProblem(ref, s.limits); reason != "" {
			out = append(out, FieldError{Field: p, Reason: "is not a concept ID: it " + reason})
		} else if ref == id {
			out = append(out, FieldError{Field: p, Reason: "names the concept itself"})
		}
	}
	return out
}

// windowFields checks a usage_window's from and to.
func windowFields(p string, v any) []FieldError {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return append(timestampField(p+".from", m["from"]), timestampField(p+".to", m["to"])...)
}

// timestampField checks one OKF timestamp (§5): an RFC 3339 date-time
// with an offset, or a full date. Null and "" count as absent.
func timestampField(p string, v any) []FieldError {
	switch v := v.(type) {
	case nil:
		return nil
	case string:
		if v == "" || validTimestamp(v) {
			return nil
		}
	}
	return []FieldError{{Field: p, Reason: `must be an RFC 3339 date-time with an offset ("2026-06-30T14:00:00Z") or a date ("2026-06-30")`}}
}

func validTimestamp(s string) bool {
	if _, err := time.Parse(time.DateOnly, s); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// textFields reports, in a frontmatter as a caller passed it, strings that
// are not valid UTF-8 (before encoding/json would replace their bytes) and
// keys with control characters, which YAML cannot carry as plain keys.
func textFields(v any, p string) []FieldError {
	switch v := v.(type) {
	case string:
		if !utf8.ValidString(v) {
			return []FieldError{{Field: p, Reason: "is not valid UTF-8"}}
		}
	case Frontmatter:
		return textFields(map[string]any(v), p)
	case map[string]any:
		var out []FieldError
		for k, item := range v {
			if !utf8.ValidString(k) {
				out = append(out, FieldError{Field: p, Reason: "has a key that is not valid UTF-8"})
				continue
			}
			if strings.IndexFunc(k, unicode.IsControl) >= 0 {
				out = append(out, FieldError{Field: p, Reason: fmt.Sprintf("has the key %q, with a control character", k)})
				continue
			}
			out = append(out, textFields(item, p+"."+k)...)
		}
		return out
	case []any:
		var out []FieldError
		for i, item := range v {
			out = append(out, textFields(item, fmt.Sprintf("%s[%d]", p, i))...)
		}
		return out
	}
	return nil
}
