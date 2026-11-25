/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.bison9.com/agent/toolbox/kb"
	"go.bison9.com/agent/toolspec"
)

// resolver serves one writable bundle "notes" and one read-only
// "shared/ref", the same for every agent but Bob, who has none.
type resolver struct {
	notes, ref string
	err        error
}

func (r resolver) Resolve(_ context.Context, name string) (kb.Bundle, error) {
	if r.err != nil {
		return kb.Bundle{}, r.err
	}
	switch name {
	case "notes":
		return kb.Bundle{Root: r.notes, Writable: true, Create: true}, nil
	case "shared/ref":
		return kb.Bundle{Root: r.ref}, nil
	}
	return kb.Bundle{}, kb.ErrUnknownKB
}

func (r resolver) Bundles(context.Context) ([]kb.Info, error) {
	return []kb.Info{{Name: "notes", Writable: true}, {Name: "shared/ref"}}, nil
}

type harness struct {
	t     *testing.T
	store *kb.Store
	tools *Tools
	r     resolver
	defs  map[string]toolspec.ToolDefinition
}

func newHarness(t *testing.T, limits kb.Limits) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := kb.New(kb.Options{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	h := &harness{t: t, store: store, r: resolver{notes: filepath.Join(dir, "notes"), ref: filepath.Join(dir, "ref")}}
	if mkErr := os.MkdirAll(h.r.ref, 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	h.tools, err = New(Config{Store: store, Resolver: func(c *toolspec.ToolCall) kb.Resolver {
		if c.AgentID == "bob" {
			return nil
		}
		return h.r
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.defs = map[string]toolspec.ToolDefinition{}
	for _, d := range h.tools.RegisterTools(toolspec.Deps{AgentID: "ignored"}) {
		h.defs[d.Name] = d
	}
	return h
}

// call runs a tool as Alice.
func (h *harness) call(tool string, args map[string]any) (*toolspec.Result, error) {
	h.t.Helper()
	return h.defs[tool].Handler(&toolspec.ToolCall{Ctx: context.Background(), Args: args, AgentID: "alice"})
}

// ok runs a tool that must succeed and returns its JSON.
func (h *harness) ok(tool string, args map[string]any) string {
	h.t.Helper()
	res, err := h.call(tool, args)
	if err != nil {
		h.t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		h.t.Fatalf("%s: error result %s", tool, res.ForLLM)
	}
	return res.ForLLM
}

// fails runs a tool that must give an error result and returns its JSON.
func (h *harness) fails(tool string, args map[string]any, want error) errorResult {
	h.t.Helper()
	res, err := h.call(tool, args)
	if err != nil {
		h.t.Fatalf("%s: Go error %v, want an error result", tool, err)
	}
	if !res.IsError || !errors.Is(res.Err, want) {
		h.t.Fatalf("%s: result %+v, want an error result matching %v", tool, res, want)
	}
	var out errorResult
	if err := json.Unmarshal([]byte(res.ForLLM), &out); err != nil {
		h.t.Fatal(err)
	}
	if out.Fields == nil || !strings.Contains(res.ForLLM, `"fields":[`) {
		h.t.Fatalf("%s: fields missing in %s", tool, res.ForLLM)
	}
	return out
}

func TestNew(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted no Store")
	}
	h := newHarness(t, kb.Limits{})
	if h.tools.Namespace() != "kb" || h.tools.Description() == "" {
		t.Fatalf("HostMeta = %q, %q", h.tools.Namespace(), h.tools.Description())
	}
	if ok, reason := h.tools.Available(nil); !ok || reason != "" {
		t.Fatalf("Available = %v, %q", ok, reason)
	}
}

func TestDefinitions(t *testing.T) {
	h := newHarness(t, kb.Limits{})
	if len(h.defs) != 3 {
		t.Fatalf("definitions = %v", h.defs)
	}
	for name, d := range h.defs {
		if d.Category != "kb" || d.SessionScoped || d.Async || d.DefaultAllow != nil || d.PrimaryOnly || d.Handler == nil || d.Description == "" {
			t.Errorf("%s: %+v", name, d)
		}
	}
	for _, name := range []string{"get", "search"} {
		hints := h.defs[name].Hints
		if hints == nil || hints.ReadOnly == nil || !*hints.ReadOnly || hints.Idempotent != nil || hints.OpenWorld != nil || hints.Destructive != nil {
			t.Errorf("%s hints = %+v", name, hints)
		}
		if h.defs[name].RawSchema != nil {
			t.Errorf("%s has a raw schema", name)
		}
	}
	hints := h.defs["put"].Hints
	if hints == nil || hints.ReadOnly == nil || *hints.ReadOnly || hints.Idempotent == nil || !*hints.Idempotent ||
		hints.OpenWorld == nil || *hints.OpenWorld || hints.Destructive != nil {
		t.Errorf("put hints = %+v", hints)
	}
	schema := h.defs["put"].Schema()
	if !reflect.DeepEqual(schema, PutSchema()) {
		t.Fatalf("put schema = %v", schema)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || schema["additionalProperties"] != false ||
		!reflect.DeepEqual(schema["required"], []any{"kb", "concept_id", "frontmatter", "body"}) {
		t.Fatalf("put schema envelope = %v", schema)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || !reflect.DeepEqual(props["frontmatter"], kb.FrontmatterSchema()) {
		t.Fatalf("put frontmatter schema = %v", props["frontmatter"])
	}
	search := h.defs["search"].Schema()
	limit, ok := search["properties"].(map[string]any)["limit"].(map[string]any)
	if !ok || limit["type"] != "integer" || limit["minimum"] != 1.0 || limit["maximum"] != 20.0 || limit["default"] != 5 {
		t.Fatalf("limit = %v", limit)
	}
	if !reflect.DeepEqual(search["required"], []string{"kb", "query"}) {
		t.Fatalf("search required = %v", search["required"])
	}
	if !reflect.DeepEqual(h.defs["get"].Schema()["required"], []string{"kb", "concept_id"}) {
		t.Fatalf("get required = %v", h.defs["get"].Schema()["required"])
	}
}

func TestPutGetSearch(t *testing.T) {
	h := newHarness(t, kb.Limits{})
	put := map[string]any{
		"kb": "notes", "concept_id": "guidance/crypto-review",
		"frontmatter": map[string]any{"type": "Evaluation Guidance", "title": "Review <crypto> & claims", "tags": []any{"cryptography"}},
		"body":        "Record the evidence for each claimed algorithm and key size.\n",
	}
	if got := h.ok("put", put); got != `{"kb":"notes","concept_id":"guidance/crypto-review","status":"created"}` {
		t.Fatalf("put = %s", got)
	}
	if got := h.ok("put", put); got != `{"kb":"notes","concept_id":"guidance/crypto-review","status":"updated"}` {
		t.Fatalf("second put = %s", got)
	}
	h.ok("put", map[string]any{
		"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note", kb.IncludeKey: []any{"guidance/crypto-review"}}, "body": "A\n",
	})
	got := h.ok("get", map[string]any{"kb": "notes", "concept_id": "a"})
	want := `{"kb":"notes","concept_id":"a","frontmatter":{"type":"Note","x-tenebris-include":["guidance/crypto-review"]},"body":"A\n",` +
		`"includes":[{"concept_id":"guidance/crypto-review","frontmatter":{"tags":["cryptography"],"title":"Review <crypto> & claims","type":"Evaluation Guidance"},` +
		`"body":"Record the evidence for each claimed algorithm and key size.\n"}],"missing_includes":[],"omitted_includes":[]}`
	if got != want {
		t.Fatalf("get =\n%s\nwant\n%s", got, want)
	}
	got = h.ok("search", map[string]any{"kb": "notes", "query": "key size"})
	if !strings.HasPrefix(got, `{"kb":"notes","hits":[{"concept_id":"guidance/crypto-review","title":"Review <crypto> & claims","section":"","passage":"Record the evidence`) {
		t.Fatalf("search = %s", got)
	}
	if got := h.ok("search", map[string]any{"kb": "notes", "query": "zebra"}); got != `{"kb":"notes","hits":[]}` {
		t.Fatalf("empty search = %s", got)
	}
}

func TestGetResultLimit(t *testing.T) {
	h := newHarness(t, kb.Limits{GetResult: 400})
	h.ok("put", map[string]any{"kb": "notes", "concept_id": "big", "frontmatter": map[string]any{"type": "Note"}, "body": strings.Repeat("b", 300)})
	h.ok("put", map[string]any{"kb": "notes", "concept_id": "small", "frontmatter": map[string]any{"type": "Note"}, "body": "s"})
	h.ok("put", map[string]any{
		"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note", kb.IncludeKey: []any{"big", "small"}}, "body": "",
	})
	if err := os.Remove(filepath.Join(h.r.notes, "small.md")); err != nil {
		t.Fatal(err)
	}
	var res getResult
	if err := json.Unmarshal([]byte(h.ok("get", map[string]any{"kb": "notes", "concept_id": "a"})), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Includes) != 0 || !reflect.DeepEqual(res.Omitted, []string{"big"}) || !reflect.DeepEqual(res.Missing, []string{"small"}) {
		t.Fatalf("get = %+v", res)
	}
	h.ok("put", map[string]any{"kb": "notes", "concept_id": "small", "frontmatter": map[string]any{"type": "Note"}, "body": "s"})
	if err := json.Unmarshal([]byte(h.ok("get", map[string]any{"kb": "notes", "concept_id": "a"})), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Includes) != 1 || res.Includes[0].ID != "small" || !reflect.DeepEqual(res.Omitted, []string{"big"}) {
		t.Fatalf("get = %+v", res)
	}
	out := h.fails("get", map[string]any{"kb": "notes", "concept_id": "big"}, kb.ErrTooLarge)
	if !strings.Contains(out.Error, "big") {
		t.Fatalf("too large = %+v", out)
	}
}

func TestSearchLimitArgument(t *testing.T) {
	h := newHarness(t, kb.Limits{})
	for i := range 25 {
		h.ok("put", map[string]any{"kb": "notes", "concept_id": "n" + string(rune('a'+i)), "frontmatter": map[string]any{"type": "Note"}, "body": "common"})
	}
	for _, tt := range []struct {
		limit any
		want  int
	}{{nil, 5}, {0.0, 1}, {-3.0, 1}, {3.0, 3}, {100.0, 20}, {1e300, 20}, {7, 7}, {int64(2), 2}, {json.Number("4"), 4}} {
		args := map[string]any{"kb": "notes", "query": "common"}
		if tt.limit != nil {
			args["limit"] = tt.limit
		}
		var res searchResult
		if err := json.Unmarshal([]byte(h.ok("search", args)), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) != tt.want {
			t.Errorf("limit %v: %d hits, want %d", tt.limit, len(res.Hits), tt.want)
		}
	}
	for _, bad := range []any{2.5, "3", json.Number("x"), true} {
		out := h.fails("search", map[string]any{"kb": "notes", "query": "common", "limit": bad}, kb.ErrInvalid)
		if len(out.Fields) != 1 || out.Fields[0].Field != "limit" {
			t.Errorf("limit %v: %+v", bad, out)
		}
	}
}

func TestErrorResults(t *testing.T) {
	h := newHarness(t, kb.Limits{Body: 4})
	h.ok("put", map[string]any{"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": ""})
	tests := []struct {
		name   string
		tool   string
		args   map[string]any
		want   error
		fields []string
		text   string
	}{
		{"unknown kb", "get", map[string]any{"kb": "nope", "concept_id": "a"}, kb.ErrUnknownKB, nil, `"nope"`},
		{"not found", "get", map[string]any{"kb": "notes", "concept_id": "b"}, kb.ErrNotFound, nil, `"b"`},
		{"bad id", "get", map[string]any{"kb": "notes", "concept_id": "../a"}, kb.ErrInvalid, []string{"concept_id"}, ""},
		{"missing args", "get", map[string]any{}, kb.ErrInvalid, []string{"kb", "concept_id"}, ""},
		{"wrong types", "get", map[string]any{"kb": 1, "concept_id": nil}, kb.ErrInvalid, []string{"kb", "concept_id"}, ""},
		{"read-only", "put", map[string]any{"kb": "shared/ref", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": ""}, kb.ErrReadOnly, nil, `"shared/ref"`},
		{
			"invalid frontmatter", "put",
			map[string]any{"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"tags": "x", "sources": []any{map[string]any{}}}, "body": ""},
			kb.ErrInvalid,
			[]string{"frontmatter", "frontmatter.sources[0].resource"},
			"",
		},
		{"too large", "put", map[string]any{"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": "12345"}, kb.ErrTooLarge, nil, "body"},
		{
			"put arguments", "put",
			map[string]any{"kb": "notes", "concept_id": "a", "frontmatter": "x", "extra": 1},
			kb.ErrInvalid,
			[]string{"extra", "frontmatter", "body"},
			"",
		},
		{"empty query", "search", map[string]any{"kb": "notes", "query": " "}, kb.ErrInvalid, []string{"query"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := h.fails(tt.tool, tt.args, tt.want)
			var names []string
			for _, f := range out.Fields {
				names = append(names, f.Field)
			}
			for _, want := range tt.fields {
				if !strings.Contains(strings.Join(names, ","), want) {
					t.Errorf("fields %v lack %s", names, want)
				}
			}
			if tt.fields == nil && len(out.Fields) != 0 {
				t.Errorf("fields = %v, want none", names)
			}
			if !strings.Contains(out.Error, tt.text) {
				t.Errorf("error %q does not contain %q", out.Error, tt.text)
			}
			if kbArg, isString := tt.args["kb"].(string); isString && out.KB != kbArg {
				t.Errorf("kb = %q, want %q", out.KB, kbArg)
			}
		})
	}
}

func TestNoResolver(t *testing.T) {
	h := newHarness(t, kb.Limits{})
	res, err := h.defs["get"].Handler(&toolspec.ToolCall{Args: map[string]any{"kb": "notes", "concept_id": "a"}, AgentID: "bob"})
	if err != nil || !res.IsError || !errors.Is(res.Err, kb.ErrUnknownKB) {
		t.Fatalf("get for an agent without knowledge bases = %+v, %v", res, err)
	}
	store, err := kb.New(kb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	tools, err := New(Config{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	res, err = tools.Definitions()[1].Handler(&toolspec.ToolCall{Args: map[string]any{"kb": "notes", "query": "x"}})
	if err != nil || !res.IsError || !errors.Is(res.Err, kb.ErrUnknownKB) {
		t.Fatalf("search with no resolver = %+v, %v", res, err)
	}
}

func TestUnexpectedFailuresAreGoErrors(t *testing.T) {
	h := newHarness(t, kb.Limits{})
	h.r.err = errors.New("resolver down")
	for tool, args := range map[string]map[string]any{
		"get":    {"kb": "notes", "concept_id": "a"},
		"search": {"kb": "notes", "query": "x"},
		"put":    {"kb": "notes", "concept_id": "a", "frontmatter": map[string]any{"type": "Note"}, "body": ""},
	} {
		res, err := h.call(tool, args)
		if err == nil || res != nil || !strings.Contains(err.Error(), "resolver down") {
			t.Errorf("%s = %+v, %v; want the Go error", tool, res, err)
		}
	}
}

func TestIncludesBeyondTheLimitAreOmitted(t *testing.T) {
	h := newHarness(t, kb.Limits{Includes: 1})
	for _, id := range []string{"b", "c"} {
		h.ok("put", map[string]any{"kb": "notes", "concept_id": id, "frontmatter": map[string]any{"type": "Note"}, "body": ""})
	}
	if err := os.WriteFile(filepath.Join(h.r.notes, "a.md"), []byte("---\ntype: Note\nx-tenebris-include: [b, c]\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var res getResult
	if err := json.Unmarshal([]byte(h.ok("get", map[string]any{"kb": "notes", "concept_id": "a"})), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Includes) != 1 || res.Includes[0].ID != "b" || !reflect.DeepEqual(res.Omitted, []string{"c"}) {
		t.Fatalf("get = %+v", res)
	}
}
