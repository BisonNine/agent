package modelspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A host configuration file's providers and models sections, read through
// these types and written back, come out byte for byte as they went in: the
// JSON tags, field order and omit rules are the host's.
func TestRoundTrip_HostSections(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "sections.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sections struct {
		Providers []Provider `json:"providers"`
		Models    []Model    `json:"models"`
	}
	if err = json.Unmarshal(want, &sections); err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(sections, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("round trip differs:\n%s\nwant\n%s", got, want)
	}
}

func TestRoundTrip_Document(t *testing.T) {
	path := filepath.Join("testdata", "document.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	got, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("round trip differs:\n%s\nwant\n%s", got, want)
	}
	bob := doc.Assignments["bob"]["chat"]
	if !bob[0].IsEnabled() || bob[1].IsEnabled() {
		t.Errorf("bob's chat assignments enabled = %v, %v; want true, false", bob[0].IsEnabled(), bob[1].IsEnabled())
	}
}

func TestDocument_MarshalEmptyListsAsArrays(t *testing.T) {
	got, err := Document{Version: 1}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"version\": 1,\n  \"providers\": [],\n  \"models\": []\n}\n"
	if string(got) != want {
		t.Errorf("Marshal = %q, want %q", got, want)
	}
	if _, err := Parse(got); err != nil {
		t.Errorf("Parse(Marshal) = %v", err)
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{name: "unknown field", json: `{"version":1,"providers":[],"models":[],"colour":"red"}`, want: `unknown field "colour"`},
		{name: "unknown nested field", json: `{"version":1,"providers":[{"name":"a","protocol":"claude-cli","colour":1}],"models":[]}`, want: `unknown field "colour"`},
		{name: "trailing data", json: `{"version":1} {}`, want: "unexpected data after the document"},
		{name: "not JSON", json: `{`, want: "parse model document"},
		{name: "invalid document", json: `{"providers":[],"models":[]}`, want: "version is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadFile_Missing(t *testing.T) {
	if _, err := LoadFile(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("LoadFile of a missing file succeeded")
	}
}

func validDocument() Document {
	return Document{
		Version: 1,
		Providers: []Provider{
			{Name: "openai", Protocol: "openai-chat", BaseURL: "https://api.example.com/v1"},
			{Name: "claude", Protocol: "claude-cli"},
		},
		Models: []Model{
			{ModelName: "gpt", Model: "gpt-5", Provider: "openai", Enabled: true},
			{ModelName: "claude", Model: "claude-cli", Provider: "claude", Enabled: true},
		},
		Assignments: map[string]map[string][]Assignment{
			"alice": {"chat": {{Model: "gpt"}, {Model: "claude"}}},
		},
	}
}

func TestDocument_Validate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Document)
		want   string // the whole error text; "" for none
	}{
		{name: "valid", change: func(*Document) {}},
		{name: "no assignments", change: func(d *Document) { d.Assignments = nil }},
		{name: "a disabled model may be assigned", change: func(d *Document) { d.Models[0].Enabled = false }},
		{name: "missing version", change: func(d *Document) { d.Version = 0 }, want: "version is required"},
		{name: "unsupported version", change: func(d *Document) { d.Version = 2 }, want: "version 2 is not supported (want 1)"},
		{name: "provider without name", change: func(d *Document) { d.Providers[1].Name = " " }, want: "providers[1]: name is required"},
		{
			name:   "duplicate provider",
			change: func(d *Document) { d.Providers[1].Name = "openai" },
			want:   `providers[1]: duplicate provider name "openai"`,
		},
		{
			name:   "bad protocol",
			change: func(d *Document) { d.Providers[0].BaseURL = "" },
			want:   `provider "openai": base_url is required for protocol "openai-chat"`,
		},
		{name: "invalid model", change: func(d *Document) { d.Models[1].Model = "" }, want: "models[1]: model is required"},
		{
			name:   "unknown provider",
			change: func(d *Document) { d.Models[1].Provider = "nope" },
			want:   `models[1] ("claude"): provider "nope" not found`,
		},
		{
			name:   "duplicate model name",
			change: func(d *Document) { d.Models[1].ModelName = "gpt" },
			want:   `models[1]: duplicate model name "gpt"`,
		},
		{
			name:   "assignment naming an unknown model",
			change: func(d *Document) { d.Assignments["alice"]["chat"][1].Model = "nope" },
			want:   `assignments["alice"]["chat"][1]: model "nope" not found`,
		},
		{
			name:   "model listed twice",
			change: func(d *Document) { d.Assignments["alice"]["chat"][1].Model = "gpt" },
			want:   `assignments["alice"]["chat"][1]: model "gpt" is listed twice`,
		},
		{
			name:   "the same model under two purposes is fine",
			change: func(d *Document) { d.Assignments["alice"]["summarize"] = []Assignment{{Model: "gpt"}} },
		},
		{
			name:   "empty agent id",
			change: func(d *Document) { d.Assignments[""] = map[string][]Assignment{} },
			want:   "assignments: agent id is required",
		},
		{
			name:   "empty purpose",
			change: func(d *Document) { d.Assignments["bob"] = map[string][]Assignment{"": nil} },
			want:   `assignments["bob"]: purpose is required`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := validDocument()
			tc.change(&d)
			err := d.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAssignment_IsEnabled(t *testing.T) {
	on, off := true, false
	if !(Assignment{}).IsEnabled() || !(Assignment{Enabled: &on}).IsEnabled() || (Assignment{Enabled: &off}).IsEnabled() {
		t.Error("IsEnabled: want unset and true on, false off")
	}
}
