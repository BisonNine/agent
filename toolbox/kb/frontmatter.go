/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// IncludeKey is the frontmatter key that lists the concepts a get includes:
// canonical concept IDs in the same bundle. It is a Tenebris extension to
// OKF.
const IncludeKey = "x-tenebris-include"

// listKeys are the fields OKF defines as lists; one item given alone is a
// list of one (OKF §11.2).
var listKeys = []string{"tags", "sources", "verified", "parameters", IncludeKey}

// leadingKeys are written first, in this order, when a key is new.
var leadingKeys = []string{"type", "title", "description", "resource", "tags"}

// maxNodes bounds the YAML values one frontmatter may expand to, against
// alias bombs.
const maxNodes = 1 << 16

// Frontmatter is a concept's metadata as JSON-shaped values: map[string]any,
// []any, string, bool, nil, int64 and float64. Timestamps are strings, in
// their source text.
type Frontmatter map[string]any

// Type returns the concept's type, or "".
func (f Frontmatter) Type() string { return f.str("type") }

// Title returns the concept's title, or "".
func (f Frontmatter) Title() string { return f.str("title") }

// Tags returns the concept's tags that are strings.
func (f Frontmatter) Tags() []string { return f.strings("tags") }

// Includes returns the IDs listed under IncludeKey that are strings.
func (f Frontmatter) Includes() []string { return f.strings(IncludeKey) }

// Sources returns the concept's sources entries that are mappings.
func (f Frontmatter) Sources() []map[string]any {
	var out []map[string]any
	for _, v := range asList(f["sources"]) {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (f Frontmatter) str(key string) string {
	if s, ok := f[key].(string); ok {
		return s
	}
	return ""
}

func (f Frontmatter) strings(key string) []string {
	var out []string
	for _, v := range asList(f[key]) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// asList returns v as a list: a list as it is, nil as none, anything else
// as a list of one.
func asList(v any) []any {
	switch v := v.(type) {
	case nil:
		return nil
	case []any:
		return v
	default:
		return []any{v}
	}
}

// normalizeLists turns each list field given as one item into a list of
// one, in place. Nil stays nil (the same as absent).
func normalizeLists(fm Frontmatter) {
	for _, k := range listKeys {
		v, ok := fm[k]
		if !ok || v == nil {
			continue
		}
		if _, isList := v.([]any); !isList {
			fm[k] = []any{v}
		}
	}
}

// concept is a parsed concept file.
type concept struct {
	fm   Frontmatter
	root *yaml.Node // the frontmatter mapping
	text string     // the frontmatter's YAML text
	body string
}

// splitDocument splits a file into its frontmatter text and body. ok is
// false when the file does not start with a "---" line or the block is not
// closed by another.
func splitDocument(data []byte) (text, body string, ok bool) {
	s := string(data)
	rest, found := cutDelimiter(s)
	if !found {
		return "", "", false
	}
	pos := 0
	for pos <= len(rest) {
		line := rest[pos:]
		if after, found := cutDelimiter(line); found {
			return rest[:pos], after, true
		}
		if line == "---" {
			return rest[:pos], "", true
		}
		nl := strings.IndexByte(line, '\n')
		if nl < 0 {
			break
		}
		pos += nl + 1
	}
	return "", "", false
}

// cutDelimiter returns what follows a leading "---" line in s.
func cutDelimiter(s string) (string, bool) {
	if after, ok := strings.CutPrefix(s, "---\n"); ok {
		return after, true
	}
	if after, ok := strings.CutPrefix(s, "---\r\n"); ok {
		return after, true
	}
	return "", false
}

// parseConcept parses a concept file. It returns (nil, nil) for a file
// that is not a concept (no frontmatter, or no type: OKF §3.2), and the
// problems for one whose frontmatter is not a valid mapping.
func parseConcept(data []byte) (*concept, []FieldError) {
	text, body, ok := splitDocument(data)
	if !ok {
		return nil, nil
	}
	root, fm, problems := parseFrontmatter(text)
	if problems != nil {
		return nil, problems
	}
	if root == nil {
		return nil, nil
	}
	switch t := fm["type"].(type) {
	case nil:
		return nil, nil
	case string:
		if t == "" {
			return nil, nil
		}
	default:
		return nil, []FieldError{{Field: "frontmatter.type", Reason: "must be a string"}}
	}
	return &concept{fm: fm, root: root, text: text, body: body}, nil
}

// parseFrontmatter parses frontmatter text. root is nil for empty text.
func parseFrontmatter(text string) (*yaml.Node, Frontmatter, []FieldError) {
	var doc yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, nil
		}
		return nil, nil, yamlProblem(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, nil, yamlProblem(err)
		}
		return nil, nil, []FieldError{{Field: "frontmatter", Reason: "holds more than one YAML document"}}
	}
	root := doc.Content[0] // a decoded document holds one node
	if root.Kind != yaml.MappingNode {
		return nil, nil, []FieldError{{Field: "frontmatter", Reason: "is not a mapping"}}
	}
	c := converter{budget: maxNodes}
	c.enter(root)
	m := c.mapping(root, &fieldRef{key: "frontmatter"})
	if c.problems != nil {
		return nil, nil, c.problems
	}
	fm := Frontmatter(m)
	normalizeLists(fm)
	return root, fm, nil
}

// yamlProblem reports a YAML syntax error.
func yamlProblem(err error) []FieldError {
	return []FieldError{{Field: "frontmatter", Reason: "is not valid YAML: " + strings.TrimPrefix(err.Error(), "yaml: ")}}
}

// Bounds on what reading one frontmatter may cost, whatever its text.
const (
	maxProblems  = 20  // problems reported per concept
	maxFieldPath = 200 // bytes of a field path in a problem
)

// fieldRef is a field path, built only when a problem names it, so deep or
// aliased values cost nothing extra.
type fieldRef struct {
	parent *fieldRef
	key    string
	index  int // used when key is ""
}

func (f *fieldRef) child(key string) *fieldRef { return &fieldRef{parent: f, key: key} }
func (f *fieldRef) item(i int) *fieldRef       { return &fieldRef{parent: f, index: i} }

// String renders the path, its head kept when it is long.
func (f *fieldRef) String() string {
	var parts []*fieldRef
	for p := f; p != nil; p = p.parent {
		parts = append(parts, p)
	}
	var b strings.Builder
	for _, p := range slices.Backward(parts) {
		switch {
		case p.parent == nil:
			b.WriteString(p.key)
		case p.key != "":
			b.WriteString("." + p.key)
		default:
			b.WriteString("[" + strconv.Itoa(p.index) + "]")
		}
		if b.Len() > maxFieldPath {
			break
		}
	}
	return clip(b.String(), maxFieldPath)
}

// clip shortens s to at most n bytes on a rune boundary, ending with "…"
// when cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// converter turns YAML nodes into JSON-shaped values, collecting problems
// with their paths. Its work is bounded: at most maxNodes values are
// converted (aliases count each time they are expanded), an alias to a
// value being converted is refused, and conversion stops at maxProblems.
type converter struct {
	budget   int
	problems []FieldError
	open     map[*yaml.Node]bool // collections being converted
}

func (c *converter) fail(path *fieldRef, reason string) {
	c.problems = append(c.problems, FieldError{Field: path.String(), Reason: reason})
	if len(c.problems) >= maxProblems {
		c.budget = 0 // stop: enough is known
	}
}

func (c *converter) value(n *yaml.Node, path *fieldRef) any {
	if c.spent(path) {
		return nil
	}
	c.budget--
	switch n.Kind {
	case yaml.AliasNode:
		if c.open[n.Alias] {
			c.fail(path, "is an alias to a value that contains it")
			return nil
		}
		return c.value(n.Alias, path)
	case yaml.MappingNode:
		c.enter(n)
		defer delete(c.open, n)
		return c.mapping(n, path)
	case yaml.SequenceNode:
		c.enter(n)
		defer delete(c.open, n)
		out := make([]any, 0, min(len(n.Content), c.budget))
		for i, item := range n.Content {
			if c.spent(path) {
				break
			}
			out = append(out, c.value(item, path.item(i)))
		}
		return out
	default: // a scalar: documents occur only at the top
		return c.scalar(n, path)
	}
}

// spent reports whether the budget is used up, as a problem the first
// time when no other problem stopped the work.
func (c *converter) spent(path *fieldRef) bool {
	if c.budget > 0 {
		return false
	}
	if len(c.problems) == 0 {
		c.fail(path, "expands to too many values")
	}
	return true
}

func (c *converter) enter(n *yaml.Node) {
	if c.open == nil {
		c.open = make(map[*yaml.Node]bool)
	}
	c.open[n] = true
}

func (c *converter) mapping(n *yaml.Node, path *fieldRef) map[string]any {
	out := make(map[string]any, min(len(n.Content)/2, c.budget))
	for i := 0; i+1 < len(n.Content) && !c.spent(path); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
			if k.ShortTag() == "!!merge" {
				c.fail(path, "uses a merge key (<<), which is not supported")
			} else {
				c.fail(path, "has a key that is not a string")
			}
			continue
		}
		kp := path.child(k.Value)
		if _, dup := out[k.Value]; dup {
			c.fail(kp, "is a duplicate key")
			continue
		}
		out[k.Value] = c.value(v, kp)
	}
	return out
}

func (c *converter) scalar(n *yaml.Node, path *fieldRef) any {
	switch tag := n.ShortTag(); tag {
	case "!!str", "!!timestamp":
		// A timestamp keeps its source text: OKF gives its meaning.
		return n.Value
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			c.fail(path, "is not a valid boolean")
		}
		return b
	case "!!int", "!!float":
		var v any
		if err := n.Decode(&v); err != nil {
			c.fail(path, "is not a valid number")
			return nil
		}
		num, ok := jsonNumber(v)
		if !ok {
			c.fail(path, "is not a finite number")
			return nil
		}
		return num
	default:
		c.fail(path, "uses the YAML tag "+tag+", which is not supported")
		return nil
	}
}

// maxExactInt is the largest integer a float64 holds exactly.
const maxExactInt = 1 << 53

// jsonNumber returns a number as int64 when it is a whole number a float64
// holds exactly, else as float64. ok is false for a value that is not a
// finite number.
func jsonNumber(v any) (any, bool) {
	var f float64
	switch v := v.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		if v <= math.MaxInt64 {
			return int64(v), true
		}
		f = float64(v)
	case float64:
		f = v
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
		parsed, err := v.Float64()
		if err != nil {
			return nil, false
		}
		f = parsed
	default:
		return nil, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	if f == math.Trunc(f) && math.Abs(f) <= maxExactInt {
		return int64(f), true
	}
	return f, true
}

// canonicalize returns fm as JSON-shaped values: whatever Go values a
// caller passed are converted as encoding/json would, numbers as by
// jsonNumber. List shorthand is normalised. A number that is not finite
// as a float64 (a json.Number such as 1e400) is an error naming its field.
func canonicalize(fm Frontmatter) (Frontmatter, error) {
	data, err := json.Marshal(map[string]any(fm))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v map[string]any
	if decErr := dec.Decode(&v); decErr != nil {
		return nil, decErr
	}
	var bad []FieldError
	numbers(v, &fieldRef{key: "frontmatter"}, &bad) // in place
	if bad != nil {
		return nil, invalid("", "", bad...)
	}
	out := Frontmatter(v)
	if out == nil {
		out = Frontmatter{}
	}
	normalizeLists(out)
	return out, nil
}

// numbers replaces each json.Number in v, adding to bad each one that is
// out of range.
func numbers(v any, path *fieldRef, bad *[]FieldError) any {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			v[k] = numbers(item, path.child(k), bad)
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = numbers(item, path.item(i), bad)
		}
		return v
	case json.Number:
		if n, ok := jsonNumber(v); ok {
			return n
		}
		*bad = append(*bad, FieldError{Field: path.String(), Reason: "is a number out of range: " + clip(v.String(), 40)})
		return nil
	default:
		return v
	}
}

// canonicalJSON is the comparison form of a value.
func canonicalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "\x00" + err.Error()
	}
	return string(data)
}

// sameValues reports whether two frontmatters hold the same values.
func sameValues(a, b Frontmatter) bool {
	return canonicalJSON(map[string]any(a)) == canonicalJSON(map[string]any(b))
}

// keyOrder returns the order fm's keys are written in: the keys old has,
// in its order, then the new ones, leading keys first and the rest sorted.
func keyOrder(fm Frontmatter, old *concept) []string {
	order := make([]string, 0, len(fm))
	seen := make(map[string]bool, len(fm))
	if old != nil {
		for i := 0; i+1 < len(old.root.Content); i += 2 {
			k := old.root.Content[i].Value
			if _, ok := fm[k]; ok && !seen[k] {
				order = append(order, k)
				seen[k] = true
			}
		}
	}
	for _, k := range leadingKeys {
		if _, ok := fm[k]; ok && !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range fm {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	return append(order, rest...)
}

// renderFrontmatter returns the YAML text for fm. When old is given (an
// update), the text of every key whose value is unchanged is kept as it
// was (OKF §4: do not re-serialise unchanged values), changed keys are
// written in place and new keys appended; if that cannot be done exactly,
// the whole mapping is written afresh in the same key order. Each attempt
// is made first with readable string styles, then with every string that
// needs it double-quoted, and is accepted only if it parses back to fm.
func renderFrontmatter(fm Frontmatter, old *concept) (string, error) {
	order := keyOrder(fm, old)
	var attempts []func(safe bool) (string, error)
	if old != nil && spliceable(old) {
		attempts = append(attempts, func(safe bool) (string, error) { return splice(fm, old, order, safe) })
	}
	attempts = append(attempts, func(safe bool) (string, error) { return freshRender(fm, order, safe) })
	var lastErr error
	for _, attempt := range attempts {
		for _, safe := range []bool{false, true} {
			text, err := attempt(safe)
			if err != nil {
				lastErr = err
				continue
			}
			if parsesTo(text, fm) {
				return text, nil
			}
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", errors.New("the YAML encoder cannot write it so that it reads back the same")
}

// parsesTo reports whether text parses to the values of fm.
func parsesTo(text string, fm Frontmatter) bool {
	root, got, problems := parseFrontmatter(text)
	return root != nil && problems == nil && sameValues(got, fm)
}

// freshRender writes fm as one block mapping in the given key order.
func freshRender(fm Frontmatter, order []string, safe bool) (string, error) {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range order {
		m.Content = append(m.Content, stringNode(k, safe), valueNode(fm[k], safe))
	}
	return encodeNode(m)
}

// valueNode builds the YAML node of a JSON-shaped value. Lists of plain
// scalars are written in flow style ([a, b]), as OKF's examples are; maps
// with their keys sorted.
func valueNode(v any, safe bool) *yaml.Node {
	switch v := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(v, 10)}
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(v, 'g', -1, 64)}
	case string:
		return stringNode(v, safe)
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		flow := len(v) > 0
		for _, item := range v {
			c := valueNode(item, safe)
			if c.Kind != yaml.ScalarNode || strings.Contains(c.Value, "\n") {
				flow = false
			}
			n.Content = append(n.Content, c)
		}
		if flow {
			n.Style = yaml.FlowStyle
		}
		return n
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			n.Content = append(n.Content, stringNode(k, safe), valueNode(v[k], safe))
		}
		return n
	default:
		// Not reached: values are canonicalised first. Written as text,
		// the read-back check refuses it.
		return stringNode(fmt.Sprint(v), true)
	}
}

// stringNode builds a string scalar. The encoder quotes a string that
// would read as another type. A multi-line string is a literal block,
// unless safe is set or it holds what a block cannot carry, when it is
// double-quoted with escapes.
func stringNode(s string, safe bool) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	switch {
	case s == "<<": // the encoder writes it plain, and it reads back as a merge key
		n.Style = yaml.DoubleQuotedStyle
	case strings.IndexFunc(s, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) >= 0,
		s != strings.TrimSpace(s) && strings.Contains(s, "\n"):
		n.Style = yaml.DoubleQuotedStyle
	case strings.Contains(s, "\n"):
		if safe {
			n.Style = yaml.DoubleQuotedStyle
		} else {
			n.Style = yaml.LiteralStyle
		}
	}
	return n
}

// encodeNode encodes a node as YAML with two-space indents.
func encodeNode(n *yaml.Node) (string, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", fmt.Errorf("encode frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("encode frontmatter: %w", err)
	}
	return b.String(), nil
}

// spliceable reports whether old's text can be edited key by key: a block
// mapping whose keys each start their own line, at one column, with no
// anchors or aliases anywhere.
func spliceable(old *concept) bool {
	r := old.root
	if r.Style&yaml.FlowStyle != 0 || len(r.Content) == 0 {
		return false
	}
	return !hasAnchors(r)
}

func hasAnchors(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	return slices.ContainsFunc(n.Content, hasAnchors)
}

// splice edits old's text key by key. Each top-level key owns the lines
// from its own to the next key's; trailing blank and column-0 comment
// lines (comments about the next key) are kept when the entry is replaced
// or removed.
func splice(fm Frontmatter, old *concept, order []string, safe bool) (string, error) {
	lines := strings.SplitAfter(old.text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	type entry struct{ content, gap []string }
	entries := make(map[string]entry)
	keys := old.root.Content
	var out strings.Builder
	for i := 0; i+1 < len(keys); i += 2 {
		start := keys[i].Line - 1
		next := len(lines)
		if i+2 < len(keys) {
			next = keys[i+2].Line - 1
		}
		end := next
		for end > start+1 && isGapLine(lines[end-1]) {
			end--
		}
		if i == 0 {
			out.WriteString(strings.Join(lines[:start], ""))
		}
		entries[keys[i].Value] = entry{content: lines[start:end], gap: lines[end:next]}
	}
	oldValues := old.fm
	written := make(map[string]bool)
	writeComments := func(gap []string) {
		for _, l := range gap {
			if strings.HasPrefix(l, "#") {
				out.WriteString(l)
			}
		}
	}
	for i := 0; i+1 < len(keys); i += 2 {
		k := keys[i].Value
		e := entries[k]
		v, keep := fm[k]
		switch {
		case !keep:
			writeComments(e.gap)
		case canonicalJSON(v) == canonicalJSON(oldValues[k]):
			out.WriteString(strings.Join(e.content, ""))
			out.WriteString(strings.Join(e.gap, ""))
		default:
			text, err := pairText(k, v, safe)
			if err != nil {
				return "", err
			}
			out.WriteString(text)
			writeComments(e.gap)
		}
		written[k] = true
	}
	for _, k := range order {
		if written[k] {
			continue
		}
		text, err := pairText(k, fm[k], safe)
		if err != nil {
			return "", err
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

// isGapLine reports whether a line is blank or a comment at column 0.
func isGapLine(l string) bool {
	return strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#")
}

// pairText encodes one key and its value as a block-mapping entry.
func pairText(key string, v any, safe bool) (string, error) {
	return encodeNode(&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{stringNode(key, safe), valueNode(v, safe)}})
}

// renderDocument returns a concept file's bytes.
func renderDocument(fmText, body string) []byte {
	var b bytes.Buffer
	b.Grow(len(fmText) + len(body) + 8)
	b.WriteString("---\n")
	b.WriteString(fmText)
	b.WriteString("---\n")
	b.WriteString(body)
	return b.Bytes()
}
