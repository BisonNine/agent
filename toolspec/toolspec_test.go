package toolspec

import (
	"reflect"
	"testing"
)

func TestParametersToSchema(t *testing.T) {
	minimum, maximum := 1.0, 10.0
	minLen, maxLen := 2, 20
	tests := []struct {
		name   string
		params []Parameter
		want   map[string]any
	}{
		{
			name:   "no parameters",
			params: nil,
			want:   map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			name:   "a bare parameter is an optional string",
			params: []Parameter{{Name: "q"}},
			want: map[string]any{"type": "object", "properties": map[string]any{
				"q": map[string]any{"type": "string"},
			}},
		},
		{
			name: "every constraint is carried",
			params: []Parameter{{
				Name: "count", Description: "How many.", Required: true, Type: "integer",
				Default: 3, Enum: []any{1, 3, 5}, Pattern: "^[0-9]+$", Format: "int32",
				Minimum: &minimum, Maximum: &maximum, MinLength: &minLen, MaxLength: &maxLen,
				Examples: []any{3},
			}},
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{"count": map[string]any{
					"type": "integer", "description": "How many.", "default": 3,
					"enum": []any{1, 3, 5}, "pattern": "^[0-9]+$", "format": "int32",
					"minimum": 1.0, "maximum": 10.0, "minLength": 2, "maxLength": 20,
					"examples": []any{3},
				}},
				"required": []string{"count"},
			},
		},
		{
			name: "arrays get items, string by default",
			params: []Parameter{
				{Name: "tags", Type: "array", Required: true},
				{Name: "ids", Type: "array", Items: "integer", Required: true},
			},
			want: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				},
				"required": []string{"tags", "ids"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParametersToSchema(tt.params); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParametersToSchema =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

func TestSchema(t *testing.T) {
	raw := map[string]any{"type": "object", "additionalProperties": true}
	d := ToolDefinition{RawSchema: raw, Parameters: []Parameter{{Name: "ignored"}}}
	if got := d.Schema(); !reflect.DeepEqual(got, raw) {
		t.Fatalf("Schema with RawSchema = %v, want it verbatim", got)
	}
	d = ToolDefinition{Parameters: []Parameter{{Name: "q", Required: true}}}
	if got, want := d.Schema(), ParametersToSchema(d.Parameters); !reflect.DeepEqual(got, want) {
		t.Fatalf("Schema = %v, want %v", got, want)
	}
}

func TestDefaultAllowed(t *testing.T) {
	tests := []struct {
		name  string
		allow *bool
		want  bool
	}{
		{"unset denies", nil, false},
		{"false denies", Allow(false), false},
		{"true allows", Allow(true), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (ToolDefinition{DefaultAllow: tt.allow}).DefaultAllowed(); got != tt.want {
				t.Fatalf("DefaultAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}
