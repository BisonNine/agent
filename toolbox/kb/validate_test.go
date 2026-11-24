/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordFields(t *testing.T) {
	s := newStore(t, Options{})
	tests := []struct {
		name  string
		fm    Frontmatter
		field string // "" when valid
	}{
		{"minimal", Frontmatter{"type": "Note"}, ""},
		{"missing type", Frontmatter{"title": "x"}, "frontmatter"},
		{"blank type", Frontmatter{"type": "  "}, "frontmatter.type"},
		{"type not a string", Frontmatter{"type": 3}, "frontmatter.type"},
		{"title not a string", Frontmatter{"type": "a", "title": 3}, "frontmatter.title"},
		{"description not a string", Frontmatter{"type": "a", "description": true}, "frontmatter.description"},
		{"resource not a string", Frontmatter{"type": "a", "resource": []any{"x"}}, "frontmatter.resource"},
		{"tag not a string", Frontmatter{"type": "a", "tags": []any{"x", 2}}, "frontmatter.tags[1]"},
		{"extension keys pass", Frontmatter{"type": "a", "x-acme-owner": map[string]any{"deep": []any{1}}}, ""},
		{"source without resource", Frontmatter{"type": "a", "sources": []any{map[string]any{"id": "s"}}}, "frontmatter.sources[0].resource"},
		{"source with empty resource", Frontmatter{"type": "a", "sources": []any{map[string]any{"resource": ""}}}, "frontmatter.sources[0].resource"},
		{"source not a mapping", Frontmatter{"type": "a", "sources": []any{"https://x"}}, "frontmatter.sources[0]"},
		{"source ok", Frontmatter{"type": "a", "sources": []any{map[string]any{"resource": "all queries in project X"}}}, ""},
		{"generated.at date-time", Frontmatter{"type": "a", "generated": map[string]any{"by": "x", "at": "2026-06-20T14:00:00Z"}}, ""},
		{"generated.at offset", Frontmatter{"type": "a", "generated": map[string]any{"at": "2026-06-20T14:00:00.5+02:00"}}, ""},
		{"generated.at no offset", Frontmatter{"type": "a", "generated": map[string]any{"at": "2026-06-20T14:00:00"}}, "frontmatter.generated.at"},
		{"generated.at number", Frontmatter{"type": "a", "generated": map[string]any{"at": 2026}}, "frontmatter.generated.at"},
		{"generated not a mapping passes", Frontmatter{"type": "a", "generated": "agent/x"}, ""},
		{"verified.at", Frontmatter{"type": "a", "verified": []any{map[string]any{"by": "x", "at": "yesterday"}}}, "frontmatter.verified[0].at"},
		{"verified event without at passes", Frontmatter{"type": "a", "verified": []any{map[string]any{"by": "x"}, "odd"}}, ""},
		{"stale_after date", Frontmatter{"type": "a", "stale_after": "2026-09-23"}, ""},
		{"stale_after empty", Frontmatter{"type": "a", "stale_after": ""}, ""},
		{"stale_after null", Frontmatter{"type": "a", "stale_after": nil}, ""},
		{"stale_after bad", Frontmatter{"type": "a", "stale_after": "2026-13-01"}, "frontmatter.stale_after"},
		{"sources last_modified", Frontmatter{"type": "a", "sources": []any{map[string]any{"resource": "r", "last_modified": "May"}}}, "frontmatter.sources[0].last_modified"},
		{"usage_window", Frontmatter{"type": "a", "usage_window": map[string]any{"from": "2026-06-01", "to": "June"}}, "frontmatter.usage_window.to"},
		{
			"source usage_window",
			Frontmatter{"type": "a", "sources": []any{map[string]any{"resource": "r", "usage_window": map[string]any{"from": "x"}}}},
			"frontmatter.sources[0].usage_window.from",
		},
		{"include not a list item string", Frontmatter{"type": "a", IncludeKey: []any{3}}, "frontmatter.x-tenebris-include[0]"},
		{"include blank", Frontmatter{"type": "a", IncludeKey: []any{" "}}, "frontmatter.x-tenebris-include[0]"},
		{"include duplicated", Frontmatter{"type": "a", IncludeKey: []any{"b", "b"}}, "frontmatter.x-tenebris-include"},
		{"include not an ID", Frontmatter{"type": "a", IncludeKey: []any{"/SFR/x.md"}}, "frontmatter.x-tenebris-include[0]"},
		{"include itself", Frontmatter{"type": "a", IncludeKey: []any{"self"}}, "frontmatter.x-tenebris-include[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := s.recordFields("self", mustCanonical(t, tt.fm))
			if tt.field == "" {
				if fields != nil {
					t.Fatalf("fields = %+v, want none", fields)
				}
				return
			}
			if !hasField(fields, tt.field) {
				t.Fatalf("fields = %+v, want %s", fields, tt.field)
			}
		})
	}
}

func TestTextFields(t *testing.T) {
	fields := textFields(Frontmatter{
		"ok": "fine", "bad": "a\xffb", "list": []any{"x", "\xfe"}, "nested": map[string]any{"\xff": 1, "a\nb": 2},
	}, "frontmatter")
	for _, want := range []string{"frontmatter.bad", "frontmatter.list[1]", "frontmatter.nested"} {
		if !hasField(fields, want) {
			t.Errorf("fields %+v lack %s", fields, want)
		}
	}
	if len(fields) != 4 {
		t.Fatalf("fields = %+v, want 4", fields)
	}
}

func TestRuleApplies(t *testing.T) {
	tests := []struct {
		rule     Rule
		id, typ  string
		expected bool
	}{
		{Rule{}, "any", "Any", true},
		{Rule{PathPrefix: "SFR/"}, "SFR/FCS_COP.1", "Note", true},
		{Rule{PathPrefix: "SFR/"}, "SAR/x", "SFR", false},
		{Rule{PathPrefix: "SFR"}, "SFRX/a", "Note", false},
		{Rule{PathPrefix: "SFR"}, "SFR", "Note", true},
		{Rule{PathPrefix: "SFR"}, "SFR/x", "Note", true},
		{Rule{PathPrefix: "SFR/FCS"}, "SFR/FCS_COP.1", "Note", false},
		{Rule{PathPrefix: "SFR/FCS"}, "SFR/FCS/x", "Note", true},
		{Rule{Type: "SFR"}, "elsewhere/x", "SFR", true},
		{Rule{Type: "SFR"}, "SFR/x", "Note", false},
		{Rule{PathPrefix: "SFR/", Type: "SFR"}, "SFR/x", "Note", true},
		{Rule{PathPrefix: "SFR/", Type: "SFR"}, "other/x", "SFR", true},
		{Rule{PathPrefix: "SFR/", Type: "SFR"}, "other/x", "Note", false},
	}
	for _, tt := range tests {
		if got := tt.rule.applies(tt.id, tt.typ); got != tt.expected {
			t.Errorf("%+v.applies(%q, %q) = %v", tt.rule, tt.id, tt.typ, got)
		}
	}
}

func TestDatasetRules(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.notes, "rules", "sfr.json"), `{
		"type": "object",
		"required": ["sources"],
		"properties": {
			"type": {"const": "SFR"},
			"sources": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/source"}}
		},
		"$defs": {"source": {"type": "object", "required": ["element"]}}
	}`)
	f.r["notes"] = Bundle{
		Root: f.notes, Confine: filepath.Join(f.dir, "okf"), Writable: true, Create: true,
		Rules: []Rule{{PathPrefix: "SFR/", Type: "SFR", Schema: "rules/sfr.json"}},
	}
	tests := []struct {
		name, id string
		fm       Frontmatter
		fields   []string
	}{
		{"path rule, other type", "SFR/FCS_COP.1", Frontmatter{"type": "Note"}, []string{"frontmatter.type", "frontmatter"}},
		{"type rule, other path", "misc/x", Frontmatter{"type": "SFR", "sources": []any{}}, []string{"frontmatter.sources"}},
		{
			"nested via $defs", "SFR/FCS_COP.2",
			Frontmatter{"type": "SFR", "sources": []any{map[string]any{"resource": "r"}}},
			[]string{"frontmatter.sources[0]"},
		},
		{"passes", "SFR/FCS_COP.3", Frontmatter{"type": "SFR", "sources": []any{map[string]any{"resource": "r", "element": "FCS_COP.1.1"}}}, nil},
		{"rule does not apply", "misc/y", Frontmatter{"type": "Note"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.Put(f.ctx, f.r, "notes", tt.id, tt.fm, "")
			if tt.fields == nil {
				if err != nil {
					t.Fatalf("Put: %v", err)
				}
				return
			}
			fields := fieldsOf(t, err)
			for _, want := range tt.fields {
				if !hasField(fields, want) {
					t.Errorf("fields %+v lack %s", fields, want)
				}
			}
			for _, fe := range fields {
				if fe.ID != tt.id || !strings.Contains(fe.Reason, "(schema rules/sfr.json)") {
					t.Errorf("field %+v does not name the concept and the schema", fe)
				}
			}
		})
	}
}

func TestRuleSchemaProblems(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.dir, "outside.json")
	writeFile(t, outside, `{"type": "object"}`)
	writeFile(t, filepath.Join(f.notes, "rules", "remote.json"), `{"$ref": "https://example.com/schema.json"}`)
	writeFile(t, filepath.Join(f.notes, "rules", "local.json"), `{"$ref": "other.json"}`)
	writeFile(t, filepath.Join(f.notes, "rules", "other.json"), `{"type": "object"}`)
	writeFile(t, filepath.Join(f.notes, "rules", "broken.json"), `{"type": `)
	writeFile(t, filepath.Join(f.notes, "rules", "invalid.json"), `{"type": 7}`)
	if err := os.Symlink(outside, filepath.Join(f.notes, "rules", "link.json")); err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{"rules/remote.json", "rules/local.json", "rules/broken.json", "rules/invalid.json", "rules/missing.json", "../outside.json", "rules/link.json"} {
		t.Run(schema, func(t *testing.T) {
			f.r["notes"] = Bundle{Root: f.notes, Writable: true, Rules: []Rule{{Schema: schema}}}
			_, err := f.s.Put(f.ctx, f.r, "notes", "x", Frontmatter{"type": "a"}, "")
			if err == nil || errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "rule schema") {
				t.Fatalf("Put with schema %s = %v, want a rule schema failure", schema, err)
			}
		})
	}
}

func TestRuleSchemaIsReloadedWhenChanged(t *testing.T) {
	f := newFixture(t)
	schema := filepath.Join(f.notes, "rules", "s.json")
	writeFile(t, schema, `{"required": ["title"]}`)
	f.r["notes"] = Bundle{Root: f.notes, Writable: true, Rules: []Rule{{Schema: "rules/s.json"}}}
	if _, err := f.s.Put(f.ctx, f.r, "notes", "x", Frontmatter{"type": "a"}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Put = %v, want ErrInvalid", err)
	}
	f.put("y", Frontmatter{"type": "a", "title": "t"}, "") // from the cache
	writeFile(t, schema, `{"required": ["description"]}`)
	if _, err := f.s.Put(f.ctx, f.r, "notes", "z", Frontmatter{"type": "a", "title": "t"}, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Put after the schema changed = %v, want ErrInvalid", err)
	}
}

func TestFrontmatterSchemaIsTheSpecs(t *testing.T) {
	sch := FrontmatterSchema()
	props, ok := sch["properties"].(map[string]any)
	if !ok || sch["type"] != "object" || sch["additionalProperties"] != true {
		t.Fatalf("schema = %v", sch)
	}
	for _, k := range []string{"type", "title", "description", "resource", "tags", IncludeKey} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema lacks %s", k)
		}
	}
	sch["type"] = "changed"
	if FrontmatterSchema()["type"] != "object" {
		t.Fatal("FrontmatterSchema returned shared state")
	}
}
