package modelspec

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestModel_Validate(t *testing.T) {
	tests := []struct {
		name  string
		model Model
		want  string // the whole error text; "" for none
	}{
		{name: "valid", model: Model{ModelName: "test", Model: "gpt-4o", Provider: "openai"}},
		{name: "missing model_name", model: Model{Model: "gpt-4o", Provider: "openai"}, want: "model_name is required"},
		{name: "missing model", model: Model{ModelName: "test", Provider: "openai"}, want: "model is required"},
		{name: "missing provider", model: Model{ModelName: "test", Model: "gpt-4o"}, want: `model "test": provider is required`},
		{name: "empty", model: Model{}, want: "model_name is required"},
		{
			name:  "bad reasoning_effort",
			model: Model{ModelName: "grok-3", Model: "grok-3", Provider: "openai", ReasoningEffort: "extreme"},
			want:  `model "grok-3": invalid reasoning_effort "extreme" (valid: none, low, medium, high, or omit)`,
		},
		{
			name:  "reserved extra_body key",
			model: Model{ModelName: "the-model", Model: "m", Provider: "openai", ExtraBody: map[string]any{"top_p": 1}},
			want:  `model "the-model": extra_body key "top_p" collides with a managed request field`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.model.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestModel_ReasoningEffortValues(t *testing.T) {
	for _, level := range []string{"", "none", "low", "medium", "high"} {
		m := Model{ModelName: "m", Model: "gpt", Provider: "openai", ReasoningEffort: level}
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%q) returned error: %v", level, err)
		}
	}
}

func TestModel_ExtraBodyCollisionRejected(t *testing.T) {
	cases := []string{
		"model", "messages", "stream", "tools", "tool_choice",
		"parallel_tool_calls", "reasoning_effort", "temperature",
		"max_tokens", "max_completion_tokens", "top_p", "n",
		"session_id", "cache_control",
	}
	if got := ReservedBodyKeys(); !slices.Equal(got, slices.Sorted(slices.Values(cases))) {
		t.Errorf("ReservedBodyKeys() = %v, want %v", got, cases)
	}
	for _, key := range cases {
		m := Model{ModelName: "the-model", Model: "whatever", Provider: "openai", ExtraBody: map[string]any{key: 1}}
		err := m.Validate()
		if err == nil {
			t.Errorf("extra_body key %q: expected validation error", key)
			continue
		}
		if !strings.Contains(err.Error(), "the-model") || !strings.Contains(err.Error(), key) {
			t.Errorf("key %q: error should name the model and the key: %v", key, err)
		}
	}
}

func TestModel_ExtraBodyAllowedKeysPass(t *testing.T) {
	m := Model{
		ModelName: "m",
		Model:     "gpt",
		Provider:  "openai",
		ExtraBody: map[string]any{
			"custom_xai_thinking": map[string]any{"budget": 1024},
			"safety_settings":     []any{"strict"},
		},
	}
	if err := m.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestModel_JSON(t *testing.T) {
	original := Model{
		ModelName:       "grok",
		Model:           "grok-3",
		Provider:        "example",
		ReasoningEffort: "high",
		ExtraBody:       map[string]any{"search_parameters": map[string]any{"mode": "auto"}},
		Enabled:         true,
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"model_name":"grok","model":"grok-3","provider":"example","enabled":true,` +
		`"reasoning_effort":"high","extra_body":{"search_parameters":{"mode":"auto"}}}`
	if string(data) != want {
		t.Errorf("Marshal = %s\nwant      %s", data, want)
	}

	// enabled is always written: false is a setting, not an absence.
	data, err = json.Marshal(Model{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"model_name":"","model":"","provider":"","enabled":false}`; string(data) != want {
		t.Errorf("zero Model = %s, want %s", data, want)
	}

	var got Model
	if err := json.Unmarshal([]byte(`{"model_name":"slow-local","model":"local","request_timeout":300}`), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.RequestTimeout != 300 {
		t.Errorf("RequestTimeout = %d, want 300", got.RequestTimeout)
	}
}

func TestModel_Validate_ReservedBodyKeyError(t *testing.T) {
	const wantText = `model "the-model": extra_body key "model" collides with a managed request field`
	m := Model{
		ModelName: "the-model", Model: "m", Provider: "openai",
		ExtraBody: map[string]any{"top_p": 1, "model": "x", "custom": true},
	}

	err := m.Validate()
	var rke *ReservedBodyKeyError
	if !errors.As(err, &rke) {
		t.Fatalf("Validate() = %v, want a *ReservedBodyKeyError", err)
	}
	if rke.Model != "the-model" || rke.Key != "model" {
		t.Errorf("error = %+v, want Model %q and Key %q (first in sorted order)", rke, "the-model", "model")
	}
	if err.Error() != wantText {
		t.Errorf("Error() = %q, want %q", err.Error(), wantText)
	}

	d := validDocument()
	d.Models[1].ExtraBody = map[string]any{"session_id": "x"}
	err = d.Validate()
	rke = nil
	if !errors.As(err, &rke) {
		t.Fatalf("Document.Validate() = %v, want a wrapped *ReservedBodyKeyError", err)
	}
	if rke.Model != d.Models[1].ModelName || rke.Key != "session_id" {
		t.Errorf("error = %+v, want Model %q and Key %q", rke, d.Models[1].ModelName, "session_id")
	}

	others := []Model{
		{Model: "m", Provider: "openai"},
		{ModelName: "a", Model: "m"},
		{ModelName: "a", Model: "m", Provider: "openai", ReasoningEffort: "extreme"},
	}
	for _, o := range others {
		if err := o.Validate(); err == nil || errors.As(err, &rke) {
			t.Errorf("Validate(%+v) = %v, want an error that is not a *ReservedBodyKeyError", o, err)
		}
	}
}

func TestModelValidateModelRequiredSentinel(t *testing.T) {
	m := Model{ModelName: "test", Provider: "openai"}
	if err := m.Validate(); !errors.Is(err, ErrModelRequired) {
		t.Fatalf("Validate() = %v, want ErrModelRequired", err)
	}
}
