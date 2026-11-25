/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateID(t *testing.T) {
	tests := []struct {
		id     string
		reason string // "" when valid
	}{
		{"guidance/crypto-review", ""},
		{"SFR/FCS_COP.1", ""},
		{"a b/c", ""},
		{"Ünïcode/ελληνικά", ""},
		{"index-of-things", ""},
		{"logbook", ""},
		{"", "is empty"},
		{"a\xffb", "is not valid UTF-8"},
		{strings.Repeat("a/", 256) + "b", "is 513 bytes"},
		{"/abs", `must not start with "/"`},
		{"dir/", `must not end with "/"`},
		{"concept.md", `must not end with ".md"`},
		{"concept.MD", `must not end with ".md"`},
		{`a\b`, `must not contain "\"`},
		{"a\x00b", "control characters"},
		{"a\nb", "control characters"},
		{"a//b", "empty segment"},
		{"./a", `starts with "."`},
		{"a/../b", `starts with "."`},
		{"a/.hidden", `starts with "."`},
		{"a/ /b", "blank segment"},
		{strings.Repeat("x", 201), "segment of 201 bytes"},
		{"index", `reserved name "index"`},
		{"dir/Index", `reserved name "Index"`},
		{"dir/LOG", `reserved name "LOG"`},
		{"a\u202eb", "U+202E"},
		{"a\u200bb", "U+200B"},
		{"a\u2028b", "U+2028"},
		{"a\u2029b", "U+2029"},
		{"x.md ", "starts or ends with a space"},
		{" lead", "starts or ends with a space"},
		{"dir /x", "starts or ends with a space"},
		{"dir/x\u00a0", "starts or ends with a space"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			err := ValidateID(tt.id)
			if tt.reason == "" {
				if err != nil {
					t.Fatalf("ValidateID(%q) = %v, want nil", tt.id, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ValidateID(%q) = %v, want ErrInvalid", tt.id, err)
			}
			fields := fieldsOf(t, err)
			if len(fields) != 1 || fields[0].Field != "concept_id" || !strings.Contains(fields[0].Reason, tt.reason) {
				t.Fatalf("ValidateID(%q) fields = %+v, want concept_id %q", tt.id, fields, tt.reason)
			}
		})
	}
}

func TestFileNameKeepsDots(t *testing.T) {
	if got := fileName("SFR/FCS_COP.1"); got != "SFR/FCS_COP.1.md" {
		t.Fatalf("fileName = %q", got)
	}
}
