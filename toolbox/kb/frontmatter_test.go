/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func TestSplitDocument(t *testing.T) {
	tests := []struct {
		name, in, text, body string
		ok                   bool
	}{
		{"plain", "---\ntype: a\n---\nbody\n", "type: a\n", "body\n", true},
		{"crlf delimiters", "---\r\ntype: a\r\n---\r\nbody\r\n", "type: a\r\n", "body\r\n", true},
		{"closed at end of file", "---\ntype: a\n---", "type: a\n", "", true},
		{"empty block", "---\n---\nx", "", "x", true},
		{"body with a --- line", "---\ntype: a\n---\none\n---\ntwo\n", "type: a\n", "one\n---\ntwo\n", true},
		{"not closed", "---\ntype: a\n", "", "", false},
		{"no frontmatter", "# Title\n", "", "", false},
		{"indented delimiter", " ---\ntype: a\n---\n", "", "", false},
		{"delimiter with trailing space does not close", "---\ntype: a\n--- \n", "", "", false},
		{"byte order mark", "\ufeff---\ntype: a\n---\n", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, body, ok := splitDocument([]byte(tt.in))
			if ok != tt.ok || text != tt.text || body != tt.body {
				t.Fatalf("splitDocument = %q, %q, %v; want %q, %q, %v", text, body, ok, tt.text, tt.body, tt.ok)
			}
		})
	}
}

func TestParseConcept(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Frontmatter // nil with no problem: not a concept
		problem string      // a field path expected among the problems
	}{
		{name: "minimal", in: "---\ntype: Note\n---\n", want: Frontmatter{"type": "Note"}},
		{name: "no frontmatter", in: "# README\n"},
		{name: "empty frontmatter", in: "---\n---\n"},
		{name: "no type", in: "---\ntitle: x\n---\n"},
		{name: "empty type", in: "---\ntype: \"\"\n---\n"},
		{name: "null type", in: "---\ntype: null\n---\n"},
		{name: "type not a string", in: "---\ntype: [a]\n---\n", problem: "frontmatter.type"},
		{name: "not a mapping", in: "---\n- a\n---\n", problem: "frontmatter"},
		{name: "bad yaml", in: "---\ntype: [a\n---\n", problem: "frontmatter"},
		{name: "two documents", in: "---\ntype: a\n...\n--- x\n---\n", problem: "frontmatter"},
		{name: "bad second document", in: "---\ntype: a\n...\n[\n---\n", problem: "frontmatter"},
		{name: "duplicate key", in: "---\ntype: a\ntype: b\n---\n", problem: "frontmatter.type"},
		{name: "duplicate key at depth", in: "---\ntype: a\ngenerated:\n  by: x\n  by: y\n---\n", problem: "frontmatter.generated.by"},
		{
			name: "duplicate key in a list", in: "---\ntype: a\nsources:\n  - resource: r\n    resource: s\n---\n",
			problem: "frontmatter.sources[0].resource",
		},
		{name: "custom tag", in: "---\ntype: a\nx: !foo bar\n---\n", problem: "frontmatter.x"},
		{name: "binary tag", in: "---\ntype: a\nx: !!binary aGk=\n---\n", problem: "frontmatter.x"},
		{name: "merge key", in: "---\ntype: a\nbase: &b {x: 1}\nc:\n  <<: *b\n---\n", problem: "frontmatter.c"},
		{name: "integer key", in: "---\ntype: a\n1: x\n---\n", problem: "frontmatter"},
		{name: "not a number", in: "---\ntype: a\nx: .nan\n---\n", problem: "frontmatter.x"},
		{name: "infinite", in: "---\ntype: a\nx: -.inf\n---\n", problem: "frontmatter.x"},
		{
			name: "scalars",
			in:   "---\ntype: a\ns: text\ni: 42\nh: 0x1F\nf: 1.5\nw: 5.0\nb: true\nn: null\nq: \"0.3\"\nbig: 18446744073709551615\n---\n",
			want: Frontmatter{
				"type": "a", "s": "text", "i": int64(42), "h": int64(31), "f": 1.5, "w": int64(5), "b": true, "n": nil, "q": "0.3",
				"big": float64(18446744073709551615),
			},
		},
		{
			name: "timestamps keep their text",
			in:   "---\ntype: a\nstale_after: 2026-06-30\ngenerated: {by: x, at: 2026-06-20T14:00:00Z}\n---\n",
			want: Frontmatter{"type": "a", "stale_after": "2026-06-30", "generated": map[string]any{"by": "x", "at": "2026-06-20T14:00:00Z"}},
		},
		{
			name: "aliases resolve",
			in:   "---\ntype: a\nx: &v [1, 2]\ny: *v\n---\n",
			want: Frontmatter{"type": "a", "x": []any{int64(1), int64(2)}, "y": []any{int64(1), int64(2)}},
		},
		{
			name: "list shorthand",
			in:   "---\ntype: a\ntags: one, two\nsources: {resource: r}\nverified: {by: x, at: \"2026-06-25\"}\nparameters: {name: y, type: integer}\nx-tenebris-include: b\n---\n",
			want: Frontmatter{
				"type": "a", "tags": []any{"one, two"},
				"sources":            []any{map[string]any{"resource": "r"}},
				"verified":           []any{map[string]any{"by": "x", "at": "2026-06-25"}},
				"parameters":         []any{map[string]any{"name": "y", "type": "integer"}},
				"x-tenebris-include": []any{"b"},
			},
		},
		{name: "null list stays null", in: "---\ntype: a\ntags:\n---\n", want: Frontmatter{"type": "a", "tags": nil}},
		{name: "mapping through an alias", in: "---\n*a\n---\n", problem: "frontmatter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, problems := parseConcept([]byte(tt.in))
			if tt.problem != "" {
				if !hasField(problems, tt.problem) {
					t.Fatalf("problems = %+v, want %s", problems, tt.problem)
				}
				return
			}
			if problems != nil {
				t.Fatalf("problems = %+v", problems)
			}
			if tt.want == nil {
				if c != nil {
					t.Fatalf("parsed a concept %+v, want none", c.fm)
				}
				return
			}
			if c == nil {
				t.Fatal("not a concept")
			}
			if !reflect.DeepEqual(c.fm, tt.want) {
				t.Fatalf("fm = %#v\nwant %#v", c.fm, tt.want)
			}
		})
	}
}

func TestAliasBombIsRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("---\ntype: a\nl0: &l0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 8; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [*l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d, *l%d]\n", i, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	b.WriteString("---\n")
	_, problems := parseConcept([]byte(b.String()))
	if len(problems) != 1 || !strings.Contains(problems[0].Reason, "too many values") {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestFrontmatterAccessors(t *testing.T) {
	fm := Frontmatter{
		"type": "Note", "title": "T", "tags": []any{"a", 1, "b"},
		"sources":            []any{map[string]any{"resource": "r"}, "loose"},
		"x-tenebris-include": []any{"x", 2},
	}
	if fm.Type() != "Note" || fm.Title() != "T" {
		t.Fatalf("Type, Title = %q, %q", fm.Type(), fm.Title())
	}
	if got := fm.Tags(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Tags = %v", got)
	}
	if got := fm.Includes(); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("Includes = %v", got)
	}
	if got := fm.Sources(); len(got) != 1 || got[0]["resource"] != "r" {
		t.Fatalf("Sources = %v", got)
	}
	empty := Frontmatter{"title": 3, "tags": "solo"}
	if empty.Type() != "" || empty.Title() != "" || !reflect.DeepEqual(empty.Tags(), []string{"solo"}) {
		t.Fatalf("accessors on odd values: %q %q %v", empty.Type(), empty.Title(), empty.Tags())
	}
}

func TestCanonicalize(t *testing.T) {
	type owner struct {
		Name string `json:"name"`
	}
	got, err := canonicalize(Frontmatter{
		"type": "a", "n": 5000.0, "f": 1.25, "i": 7, "tags": "solo", "owner": owner{Name: "Alice"},
		"list": []string{"x"}, "huge": 1e300,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Frontmatter{
		"type": "a", "n": int64(5000), "f": 1.25, "i": int64(7), "tags": []any{"solo"},
		"owner": map[string]any{"name": "Alice"}, "list": []any{"x"}, "huge": 1e300,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalize = %#v\nwant %#v", got, want)
	}
	if _, err := canonicalize(Frontmatter{"f": func() {}}); err == nil {
		t.Fatal("canonicalize accepted a func")
	}
	if got, err := canonicalize(nil); err != nil || got == nil {
		t.Fatalf("canonicalize(nil) = %v, %v", got, err)
	}
}

func TestJSONNumber(t *testing.T) {
	tests := []struct {
		in   any
		want any
		ok   bool
	}{
		{42, int64(42), true},
		{int64(-3), int64(-3), true},
		{uint64(9), int64(9), true},
		{uint64(1 << 63), float64(1 << 63), true},
		{2.5, 2.5, true},
		{float64(1 << 60), float64(1 << 60), true},
		{"x", nil, false},
	}
	for _, tt := range tests {
		got, ok := jsonNumber(tt.in)
		if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("jsonNumber(%v) = %#v, %v; want %#v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRenderNewFrontmatter(t *testing.T) {
	fm := mustCanonical(t, Frontmatter{
		"x-acme-owner": "team:alice", "tags": []any{"cryptography", "true", "1.0"}, "title": "Review cryptographic claims",
		"type": "Evaluation Guidance", "okf_version": "0.3", "stale_after": "2026-06-30", "count": 5000,
		"generated": map[string]any{"by": "agent/x", "at": "2026-06-20T14:00:00Z"},
	})
	text, err := renderFrontmatter(fm, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `type: Evaluation Guidance
title: Review cryptographic claims
tags: [cryptography, "true", "1.0"]
count: 5000
generated:
  at: "2026-06-20T14:00:00Z"
  by: agent/x
okf_version: "0.3"
stale_after: "2026-06-30"
x-acme-owner: team:alice
`
	if text != want {
		t.Fatalf("render =\n%s\nwant\n%s", text, want)
	}
}

// TestRoundTripProperty renders generated records and reads them back.
func TestRoundTripProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	atoms := []string{
		"", "plain", "0.3", "1e5", "007", "true", "False", "yes", "null", "~", "---", "...", "- item", "a: b", "#x", "x #y",
		"line\nline", "trailing\n", "\nleading", "crlf\r\nline", "tab\there", "ünïcødé ελληνικά", "'quote'", `"dq"`, "[x]", "{y}",
		"*alias", "&anchor", "!tag", "%dir", "@at", "`tick`", "  spaced  ", "2026-06-30", "2026-06-20T14:00:00Z", "0x1F",
		strings.Repeat("long words ", 20), "---\nbody-like\n---",
		"<<", "=", "!", "&", "*", "|", ">", "%", "@", "?", "-", ":", ",", "~", "Null", "y", "off", ".inf", "-.Inf", "0o17", "0b101",
		"+1", "1_000", "1:20", "? x", ": x", "<<x", "!!str", "\\", "\u00a0",
	}
	pick := func() string { return atoms[rng.IntN(len(atoms))] }
	// Keys with control characters are refused by Put (textFields).
	pickKey := func() string {
		for {
			if k := pick(); !strings.ContainsAny(k, "\r\n\t") {
				return k
			}
		}
	}
	var value func(depth int) any
	value = func(depth int) any {
		switch rng.IntN(7) {
		case 0:
			return int64(rng.IntN(100000) - 50000)
		case 1:
			return rng.Float64()*1000 - 500
		case 2:
			return rng.IntN(2) == 0
		case 3:
			if depth < 3 {
				n := rng.IntN(4)
				l := make([]any, n)
				for i := range l {
					l[i] = value(depth + 1)
				}
				return l
			}
		case 4:
			if depth < 3 {
				m := map[string]any{}
				for range rng.IntN(4) {
					m[pickKey()] = value(depth + 1)
				}
				return m
			}
		case 5:
			return nil
		}
		return pick()
	}
	for i := range 2000 {
		fm := Frontmatter{"type": "T" + pick()}
		for range rng.IntN(6) {
			fm[pickKey()] = value(0)
		}
		fm = mustCanonical(t, fm)
		var b strings.Builder
		for range rng.IntN(4) {
			b.WriteString(pick() + "\n")
		}
		body := b.String()
		text, err := renderFrontmatter(fm, nil)
		if err != nil {
			t.Fatalf("case %d: render %#v: %v", i, fm, err)
		}
		c, problems := parseConcept(renderDocument(text, body))
		if problems != nil || c == nil {
			t.Fatalf("case %d: parse back: %v (%q)", i, problems, text)
		}
		if !sameValues(c.fm, fm) || c.body != body {
			t.Fatalf("case %d: round trip differs\nfm   %#v\ngot  %#v\nbody %q got %q", i, fm, c.fm, body, c.body)
		}
	}
}

func TestUpdateKeepsUnchangedText(t *testing.T) {
	oldText := `# Leading comment
type: Playbook
title: "Incident response: data freshness alert"   # kept as written
tags: oncall
generated: { by: "human:alice", at: "2026-04-12T09:00:00Z" }
# About the owner
x-acme-owner:
  team: data
  nested: {deep: [1, 2]}
description: old
stale_after: 2026-09-23
`
	old := mustParse(t, "---\n"+oldText+"---\nbody\n")
	fm := cloneFM(old.fm)
	fm["description"] = "new"
	delete(fm, "stale_after")
	fm["resource"] = "https://example.com/x"
	text, err := renderFrontmatter(mustCanonical(t, fm), old)
	if err != nil {
		t.Fatal(err)
	}
	want := `# Leading comment
type: Playbook
title: "Incident response: data freshness alert"   # kept as written
tags: oncall
generated: { by: "human:alice", at: "2026-04-12T09:00:00Z" }
# About the owner
x-acme-owner:
  team: data
  nested: {deep: [1, 2]}
description: new
resource: https://example.com/x
`
	if text != want {
		t.Fatalf("update =\n%s\nwant\n%s", text, want)
	}
}

func TestUpdateChangedKeyKeepsNextKeysComment(t *testing.T) {
	old := mustParse(t, "---\ntype: a\ntitle: one\n\n# about tags\ntags: [x]\n---\n")
	fm := cloneFM(old.fm)
	fm["title"] = "two"
	text, err := renderFrontmatter(fm, old)
	if err != nil {
		t.Fatal(err)
	}
	if want := "type: a\ntitle: two\n# about tags\ntags: [x]\n"; text != want {
		t.Fatalf("update = %q, want %q", text, want)
	}
	delete(fm, "title")
	text, err = renderFrontmatter(fm, old)
	if err != nil {
		t.Fatal(err)
	}
	if want := "type: a\n# about tags\ntags: [x]\n"; text != want {
		t.Fatalf("removal = %q, want %q", text, want)
	}
}

func TestUpdateFallsBackToFreshRender(t *testing.T) {
	tests := []struct{ name, old string }{
		{"flow mapping", "{type: a, title: one, x: 1}\n"},
		{"anchors", "type: a\ntitle: &t one\nx: *t\n"},
		{"two keys on a line", "type: a\ntitle: {k: v}\nx: 1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := mustParse(t, "---\n"+tt.old+"---\n")
			fm := cloneFM(old.fm)
			fm["title"] = "two"
			text, err := renderFrontmatter(fm, old)
			if err != nil {
				t.Fatal(err)
			}
			root, got, problems := parseFrontmatter(text)
			if root == nil || problems != nil || !sameValues(got, fm) {
				t.Fatalf("fallback %q does not read back as %v", text, fm)
			}
			if !strings.HasPrefix(text, "type: a\ntitle: two\n") {
				t.Fatalf("fallback lost the key order: %q", text)
			}
		})
	}
}

func TestSpliceThatMisreadsFallsBack(t *testing.T) {
	// A block scalar kept with its trailing blank lines (keep chomping)
	// followed by a replaced key: the splice must still read back, or the
	// whole mapping is written afresh.
	old := mustParse(t, "---\ntype: a\nnote: |+\n  text\n\ntitle: one\n---\n")
	fm := cloneFM(old.fm)
	fm["title"] = "two"
	text, err := renderFrontmatter(fm, old)
	if err != nil {
		t.Fatal(err)
	}
	if !parsesTo(text, fm) {
		t.Fatalf("%q does not read back", text)
	}
}

func TestSplitSections(t *testing.T) {
	body := "intro text\n\n# One\nfirst\n## Two ##\nsecond\n```\n# not a heading\n```\n~~~~\n# nor this\n~~~\n~~~~\n#hashtag\n####### seven\n    # indented code\n# Three\n"
	got := splitSections(body)
	want := []section{
		{heading: "", text: "intro text"},
		{heading: "One", text: "first"},
		{heading: "Two", text: "second\n```\n# not a heading\n```\n~~~~\n# nor this\n~~~\n~~~~\n#hashtag\n####### seven\n    # indented code"},
		{heading: "Three", text: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sections = %#v\nwant %#v", got, want)
	}
	if got := splitSections(""); !reflect.DeepEqual(got, []section{{}}) {
		t.Fatalf("empty body sections = %#v", got)
	}
	if got := splitSections("```js`x\n# Real\n"); len(got) != 2 || got[1].heading != "Real" {
		t.Fatalf("a backtick info string is not a fence: %#v", got)
	}
	if h, ok := atxHeading("# C# ##"); !ok || h != "C#" {
		t.Fatalf("atxHeading = %q, %v", h, ok)
	}
	if h, ok := atxHeading("#"); !ok || h != "" {
		t.Fatalf("atxHeading(#) = %q, %v", h, ok)
	}
	if isFenceClose("    ```", "```") {
		t.Fatal("an indented fence closed")
	}
}

func TestMatchExpression(t *testing.T) {
	tests := map[string]string{
		"aes keys":         `"aes" OR "keys"`,
		`say "hi" -- NEAR`: `"say" OR """hi""" OR "NEAR"`,
		"title:x":          `"title:x"`,
		"-- ** ()":         "",
	}
	for in, want := range tests {
		if got := matchExpression(in); got != want {
			t.Errorf("matchExpression(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCutRunes(t *testing.T) {
	if got := cutRunes("ünïcødé", 4); got != "ünï…" {
		t.Fatalf("cutRunes = %q", got)
	}
	if got := cutRunes("abc", 3); got != "abc" {
		t.Fatalf("cutRunes = %q", got)
	}
}

func mustCanonical(t *testing.T, fm Frontmatter) Frontmatter {
	t.Helper()
	out, err := canonicalize(fm)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustParse(t *testing.T, doc string) *concept {
	t.Helper()
	c, problems := parseConcept([]byte(doc))
	if problems != nil || c == nil {
		t.Fatalf("parse %q: %v", doc, problems)
	}
	return c
}

func cloneFM(fm Frontmatter) Frontmatter {
	out := make(Frontmatter, len(fm))
	maps.Copy(out, fm)
	return out
}

// TestYAMLSpecialTokensRoundTrip writes each YAML-special token as a key,
// a value and a list item.
func TestYAMLSpecialTokensRoundTrip(t *testing.T) {
	toks := []string{"<<", "=", "!", "&", "*", "|", ">", "%", "@", "`", "?", "-", ":", "#", ",", "[", "]", "{", "}", "'", "\"", "~", "null", "Null", "NULL", "true", "y", "n", "on", "off", ".inf", ".nan", "-.Inf", "0o17", "0b101", "+1", "1_000", "1:20", "---", "...", "- ", "? x", ": x", "<<x", "*a", "&a", "!!str", "x: y", "a #b", " ", "", "\\", "\u0085", "\u00a0", "\ufeff"}
	for _, k := range toks {
		for _, fm := range []Frontmatter{{"type": "a", k: "v"}, {"type": "a", "v": k}, {"type": "a", "l": []any{k}}} {
			if _, err := renderFrontmatter(mustCanonical(t, fm), nil); err != nil {
				t.Errorf("%q: %v", k, err)
			}
		}
	}
}
