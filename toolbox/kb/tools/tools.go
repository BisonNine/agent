/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package tools publishes a kb.Store as toolspec tools with bare names:
// "get", "search" and "put". A host mounts them under the namespace "kb"
// (kb_get, kb_search, kb_put). It uses only kb's exported API, the one a
// host's own code uses, so the two cannot disagree.
//
// One provider serves every agent: each call's knowledge bases come from
// the resolver Config.Resolver returns for that call.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"go.bison9.com/agent/toolbox/kb"
	"go.bison9.com/agent/toolspec"
)

// Namespace is the namespace the tools are published under.
const Namespace = "kb"

// Config is what the tools need from the host.
type Config struct {
	// Store serves the knowledge bases. Required.
	Store *kb.Store
	// Resolver returns the knowledge bases a call may use, typically by
	// call.AgentID. Nil, or a nil result: no knowledge bases.
	Resolver func(call *toolspec.ToolCall) kb.Resolver
}

// Tools is the knowledge-base tool provider: a toolspec.ToolProvider and
// toolspec.HostMeta.
type Tools struct {
	cfg Config
}

// New returns the tools.
func New(cfg Config) (*Tools, error) {
	if cfg.Store == nil {
		return nil, errors.New("kb tools: a Store is required")
	}
	return &Tools{cfg: cfg}, nil
}

// RegisterTools implements toolspec.ToolProvider. The tools take who the
// caller is from each call, not from deps.
func (t *Tools) RegisterTools(toolspec.Deps) []toolspec.ToolDefinition { return t.Definitions() }

// Namespace implements toolspec.HostMeta.
func (t *Tools) Namespace() string { return Namespace }

// Description implements toolspec.HostMeta.
func (t *Tools) Description() string { return "Knowledge bases" }

// Available implements toolspec.HostMeta.
func (t *Tools) Available(any) (bool, string) { return true, "" }

// Tool descriptions are read by the model on every request: short and exact.
const (
	getDescription = "Read one concept of a knowledge base by its exact ID: its frontmatter, its body, and the concepts it includes."

	searchDescription = "Search one knowledge base. Returns the best-matching sections, each with its concept ID, title, heading and a short passage. " +
		"Read a concept in full with " + Namespace + "_get."

	putDescription = "Create or replace one concept of a knowledge base. The whole record is replaced: to change a concept, " + Namespace + "_get it, " +
		"then send every key to keep. frontmatter.type is required. " + kb.IncludeKey + " lists IDs of concepts to show with this one; each must exist."
)

// Definitions returns the tool definitions with bare names.
func (t *Tools) Definitions() []toolspec.ToolDefinition {
	limits := t.cfg.Store.Limits()
	minOne, maxHits := float64(1), float64(limits.SearchMax)
	kbParam := toolspec.Parameter{Name: "kb", Type: "string", Required: true, Pattern: `\S`, Description: "Knowledge base name"}
	idParam := toolspec.Parameter{
		Name: "concept_id", Type: "string", Required: true, Pattern: `\S`,
		Description: "Concept ID: its path in the knowledge base without .md, such as guidance/crypto-review",
	}
	return []toolspec.ToolDefinition{
		{
			Name:        "get",
			Description: getDescription,
			Parameters:  []toolspec.Parameter{kbParam, idParam},
			Hints:       &toolspec.ToolHints{ReadOnly: toolspec.Allow(true)},
			Handler:     t.getHandler,
			Category:    Namespace,
		},
		{
			Name:        "search",
			Description: searchDescription,
			Parameters: []toolspec.Parameter{
				kbParam,
				{Name: "query", Type: "string", Required: true, Pattern: `\S`, Description: "Words to search for"},
				{
					Name: "limit", Type: "integer", Minimum: &minOne, Maximum: &maxHits, Default: limits.SearchDefault,
					Description: fmt.Sprintf("Most hits to return (default %d, at most %d)", limits.SearchDefault, limits.SearchMax),
				},
			},
			Hints:    &toolspec.ToolHints{ReadOnly: toolspec.Allow(true)},
			Handler:  t.searchHandler,
			Category: Namespace,
		},
		{
			Name:        "put",
			Description: putDescription,
			RawSchema:   PutSchema(),
			Hints: &toolspec.ToolHints{
				ReadOnly:   toolspec.Allow(false),
				Idempotent: toolspec.Allow(true),
				OpenWorld:  toolspec.Allow(false),
			},
			Handler:  t.putHandler,
			Category: Namespace,
		},
	}
}

// PutSchema returns the JSON Schema of put's arguments: the maintainer's
// specification, with the frontmatter schema kb checks.
func PutSchema() map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"kb", "concept_id", "frontmatter", "body"},
		"properties": map[string]any{
			"kb":          map[string]any{"type": "string", "pattern": `\S`},
			"concept_id":  map[string]any{"type": "string", "pattern": `\S`},
			"frontmatter": kb.FrontmatterSchema(),
			"body":        map[string]any{"type": "string"},
		},
	}
}

// putArgs are the only arguments put accepts.
var putArgs = map[string]bool{"kb": true, "concept_id": true, "frontmatter": true, "body": true}

type field struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

type errorResult struct {
	Error  string  `json:"error"`
	KB     string  `json:"kb"`
	Fields []field `json:"fields"`
}

type concept struct {
	ID          string         `json:"concept_id"`
	Frontmatter kb.Frontmatter `json:"frontmatter"`
	Body        string         `json:"body"`
}

type getResult struct {
	KB          string         `json:"kb"`
	ID          string         `json:"concept_id"`
	Frontmatter kb.Frontmatter `json:"frontmatter"`
	Body        string         `json:"body"`
	Includes    []concept      `json:"includes"`
	Missing     []string       `json:"missing_includes"`
	Omitted     []string       `json:"omitted_includes"`
}

type hit struct {
	ID      string `json:"concept_id"`
	Title   string `json:"title"`
	Section string `json:"section"`
	Passage string `json:"passage"`
}

type searchResult struct {
	KB   string `json:"kb"`
	Hits []hit  `json:"hits"`
}

type putResult struct {
	KB     string `json:"kb"`
	ID     string `json:"concept_id"`
	Status string `json:"status"`
}

func (t *Tools) getHandler(call *toolspec.ToolCall) (*toolspec.Result, error) {
	a := args{m: call.Args}
	name, id := a.str("kb"), a.str("concept_id")
	if a.fields != nil {
		return a.result(name)
	}
	rec, err := t.cfg.Store.Get(callContext(call), t.resolver(call), name, id)
	if err != nil {
		return failure(name, err)
	}
	res := getResult{
		KB: rec.KB, ID: rec.ID, Frontmatter: rec.Frontmatter, Body: rec.Body,
		Includes: []concept{}, Missing: nonNil(rec.Missing), Omitted: []string{},
	}
	for _, inc := range rec.Includes {
		res.Omitted = append(res.Omitted, inc.ID)
	}
	res.Omitted = append(res.Omitted, rec.Omitted...) // beyond Limits.Includes: not loaded
	base, err := encode(res)
	if err != nil {
		return nil, err
	}
	limit := t.cfg.Store.Limits().GetResult
	if len(base) > limit {
		return failure(name, fmt.Errorf("%w: concept %q is over the %d-byte result limit", kb.ErrTooLarge, rec.ID, limit))
	}
	used := len(base)
	res.Omitted = []string{}
	for _, inc := range rec.Includes {
		c := concept{ID: inc.ID, Frontmatter: inc.Frontmatter, Body: inc.Body}
		data, err := encode(c)
		if err != nil {
			return nil, err
		}
		if used+len(data)+1 > limit {
			res.Omitted = append(res.Omitted, inc.ID)
			continue
		}
		used += len(data) + 1
		res.Includes = append(res.Includes, c)
	}
	res.Omitted = append(res.Omitted, rec.Omitted...)
	return success(res)
}

func (t *Tools) searchHandler(call *toolspec.ToolCall) (*toolspec.Result, error) {
	a := args{m: call.Args}
	name, query := a.str("kb"), a.str("query")
	limit := a.integer("limit", t.cfg.Store.Limits().SearchDefault)
	if a.fields != nil {
		return a.result(name)
	}
	limit = max(1, min(limit, t.cfg.Store.Limits().SearchMax))
	hits, err := t.cfg.Store.Search(callContext(call), t.resolver(call), name, query, limit)
	if err != nil {
		return failure(name, err)
	}
	res := searchResult{KB: name, Hits: make([]hit, 0, len(hits))}
	for _, h := range hits {
		res.Hits = append(res.Hits, hit{ID: h.ID, Title: h.Title, Section: h.Section, Passage: h.Passage})
	}
	return success(res)
}

func (t *Tools) putHandler(call *toolspec.ToolCall) (*toolspec.Result, error) {
	a := args{m: call.Args}
	for k := range call.Args {
		if !putArgs[k] {
			a.fail(k, "is not an argument of put")
		}
	}
	name, id := a.str("kb"), a.str("concept_id")
	fm := a.object("frontmatter")
	body := a.str("body")
	if a.fields != nil {
		return a.result(name)
	}
	res, err := t.cfg.Store.Put(callContext(call), t.resolver(call), name, id, fm, body)
	if err != nil {
		return failure(name, err)
	}
	status := "updated"
	if res.Created {
		status = "created"
	}
	return success(putResult{KB: res.KB, ID: res.ID, Status: status})
}

// resolver returns the call's resolver, or nil.
func (t *Tools) resolver(call *toolspec.ToolCall) kb.Resolver {
	if t.cfg.Resolver == nil {
		return nil
	}
	return t.cfg.Resolver(call)
}

// args reads a call's arguments, collecting what is wrong with them.
type args struct {
	m      map[string]any
	fields []field
}

func (a *args) fail(name, reason string) {
	a.fields = append(a.fields, field{Field: name, Reason: reason})
}

// str returns a required string argument.
func (a *args) str(name string) string {
	v, ok := a.m[name]
	if !ok || v == nil {
		a.fail(name, "is required")
		return ""
	}
	s, ok := v.(string)
	if !ok {
		a.fail(name, "must be a string")
	}
	return s
}

// object returns a required object argument.
func (a *args) object(name string) kb.Frontmatter {
	v, ok := a.m[name]
	if !ok || v == nil {
		a.fail(name, "is required")
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		a.fail(name, "must be an object")
	}
	return m
}

// integer returns an optional integer argument, or def when absent.
func (a *args) integer(name string, def int) int {
	v, ok := a.m[name]
	if !ok || v == nil {
		return def
	}
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		return n
	case int64:
		f = float64(n)
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			a.fail(name, "must be an integer")
			return def
		}
		f = parsed
	default:
		a.fail(name, "must be an integer")
		return def
	}
	if f != math.Trunc(f) || math.IsInf(f, 0) {
		a.fail(name, "must be an integer")
		return def
	}
	return int(max(math.MinInt32, min(f, math.MaxInt32)))
}

// result is the error result for bad arguments.
func (a *args) result(name string) (*toolspec.Result, error) {
	reasons := make([]string, 0, len(a.fields))
	for _, f := range a.fields {
		reasons = append(reasons, f.Field+" "+f.Reason)
	}
	err := fmt.Errorf("%w: %s", kb.ErrInvalid, strings.Join(reasons, "; "))
	return errorJSON(name, err, a.fields)
}

// failure turns an error the caller can act on into an error result and
// returns any other as the Go error.
func failure(name string, err error) (*toolspec.Result, error) {
	for _, sentinel := range []error{kb.ErrUnknownKB, kb.ErrNotFound, kb.ErrReadOnly, kb.ErrInvalid, kb.ErrTooLarge} {
		if errors.Is(err, sentinel) {
			var fields []field
			if ve, ok := errors.AsType[*kb.ValidationError](err); ok {
				for _, f := range ve.Fields {
					fields = append(fields, field{Field: f.Field, Reason: f.Reason})
				}
			}
			return errorJSON(name, err, fields)
		}
	}
	return nil, err
}

func errorJSON(name string, err error, fields []field) (*toolspec.Result, error) {
	data, encErr := encode(errorResult{Error: err.Error(), KB: name, Fields: nonNil(fields)})
	if encErr != nil {
		return nil, encErr
	}
	return &toolspec.Result{ForLLM: string(data), IsError: true, Err: err}, nil
}

func success(v any) (*toolspec.Result, error) {
	data, err := encode(v)
	if err != nil {
		return nil, err
	}
	return &toolspec.Result{ForLLM: string(data)}, nil
}

// encode is compact JSON without HTML escaping.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("kb tools: encode result: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func callContext(call *toolspec.ToolCall) context.Context {
	if call.Ctx != nil {
		return call.Ctx
	}
	return context.Background()
}
