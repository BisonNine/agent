package mcphost

import (
	"slices"
	"testing"
)

func TestInjectTokenArgument_AddsRequiredField(t *testing.T) {
	in := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []string{"path"},
	}
	out := injectTokenArgument(in, "your token")

	props, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties is %T", out["properties"])
	}
	tokenProp, ok := props[TokenArgument].(map[string]any)
	if !ok || tokenProp["type"] != "string" || tokenProp["description"] != "your token" {
		t.Errorf("session_token property = %v", props[TokenArgument])
	}
	if _, ok := props["path"]; !ok {
		t.Error("existing property dropped")
	}
	if req := requiredNames(out["required"]); !slices.Equal(req, []string{"path", TokenArgument}) {
		t.Errorf("required = %v", req)
	}

	// The input is not changed.
	if req := requiredNames(in["required"]); !slices.Equal(req, []string{"path"}) {
		t.Errorf("input required changed: %v", req)
	}
	inProps, isMap := in["properties"].(map[string]any)
	if !isMap {
		t.Fatal("input properties replaced")
	}
	if _, ok := inProps[TokenArgument]; ok {
		t.Error("input properties changed")
	}
}

func TestInjectTokenArgument_NilSchema(t *testing.T) {
	out := injectTokenArgument(nil, "d")
	if out["type"] != "object" {
		t.Errorf("type = %v", out["type"])
	}
	props, isMap := out["properties"].(map[string]any)
	if !isMap {
		t.Fatalf("properties is %T", out["properties"])
	}
	if _, ok := props[TokenArgument]; !ok {
		t.Error("session_token not added")
	}
}

func TestInjectTokenArgument_AnyRequiredSlice(t *testing.T) {
	out := injectTokenArgument(map[string]any{"required": []any{"path", "content"}}, "d")
	if req := requiredNames(out["required"]); !slices.Equal(req, []string{"path", "content", TokenArgument}) {
		t.Errorf("required = %v", req)
	}
	// Already required: not added twice.
	out = injectTokenArgument(map[string]any{"required": []any{TokenArgument}}, "d")
	if req := requiredNames(out["required"]); !slices.Equal(req, []string{TokenArgument}) {
		t.Errorf("required = %v", req)
	}
}

// A tool with no schema is published with an empty object schema; a
// free-form object argument is declared open, as the engine declares it.
func TestEndToEnd_Schemas(t *testing.T) {
	f := newFixture(t)
	tools := list(t, f.mustClient("/mcp", f.aliceTok))
	fail := schemaOf(t, tools["fail"])
	if fail["type"] != "object" {
		t.Errorf("fail schema = %v", fail)
	}
	if _, ok := fail["properties"].(map[string]any); !ok {
		t.Errorf("fail schema has no properties: %v", fail)
	}
}

// A sync that finds nothing changed registers nothing.
func TestSync_NoOpWhenUnchanged(t *testing.T) {
	f := newFixture(t)
	before := f.arg.srv.ListTools()
	f.arg.sync()
	if got := f.arg.srv.ListTools(); len(got) != len(before) {
		t.Errorf("tools %d → %d", len(before), len(got))
	}
	// A changed description is replaced in place.
	if err := f.cat.Put(toolkitEntry(textToolDesc("echo", "v2"))); err != nil {
		t.Fatal(err)
	}
	f.arg.sync()
	if d := f.arg.srv.GetTool("echo").Tool.Description; d != "v2" {
		t.Errorf("description after a change = %q", d)
	}
}
