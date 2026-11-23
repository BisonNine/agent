/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// frontmatterSchema is the shape every written frontmatter must have: the
// "frontmatter" property of the kb_put argument schema in the maintainer's
// specification.
const frontmatterSchema = `{
  "type": "object",
  "required": ["type"],
  "properties": {
    "type": { "type": "string", "pattern": "\\S" },
    "title": { "type": "string" },
    "description": { "type": "string" },
    "resource": { "type": "string" },
    "tags": { "type": "array", "items": { "type": "string" } },
    "x-tenebris-include": {
      "type": "array", "uniqueItems": true,
      "items": { "type": "string", "pattern": "\\S" }
    }
  },
  "additionalProperties": true
}`

// FrontmatterSchema returns the JSON Schema (draft 2020-12) every written
// frontmatter must satisfy, for embedding in a tool's argument schema. Put
// checks it whatever the caller checked.
func FrontmatterSchema() map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(frontmatterSchema), &m); err != nil {
		panic("kb: frontmatter schema: " + err.Error()) // a constant: cannot fail
	}
	return m
}

// compiledFrontmatterSchema is FrontmatterSchema, compiled once.
var compiledFrontmatterSchema = sync.OnceValue(func() *jsonschema.Schema {
	sch, err := compileSchema("frontmatter.json", []byte(frontmatterSchema))
	if err != nil {
		panic("kb: frontmatter schema: " + err.Error()) // a constant: cannot fail
	}
	return sch
})

// noLoader refuses every external reference: a rule schema stands alone.
type noLoader struct{}

func (noLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("references to other files or URLs are not allowed (%s)", u)
}

// compileSchema compiles a JSON Schema document named name.
func compileSchema(name string, data []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	loc := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(name)}).String()
	if err := c.AddResource(loc, doc); err != nil {
		return nil, err
	}
	return c.Compile(loc)
}

// schemaFields runs a schema over fm and returns what it rejects. suffix is
// added to each reason.
func schemaFields(sch *jsonschema.Schema, fm Frontmatter, suffix string) []FieldError {
	err := sch.Validate(map[string]any(fm))
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []FieldError{{Field: "frontmatter", Reason: err.Error() + suffix}}
	}
	var out []FieldError
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		leaf := &jsonschema.ValidationError{SchemaURL: e.SchemaURL, InstanceLocation: e.InstanceLocation, ErrorKind: e.ErrorKind}
		out = append(out, FieldError{Field: fieldPath(e.InstanceLocation), Reason: leaf.BasicOutput().Error.String() + suffix})
	}
	walk(ve)
	return out
}

// fieldPath turns a JSON instance location into a field path such as
// "frontmatter.sources[1].resource".
func fieldPath(tokens []string) string {
	var b strings.Builder
	b.WriteString("frontmatter")
	for _, t := range tokens {
		if _, err := strconv.Atoi(t); err == nil {
			b.WriteString("[" + t + "]")
		} else {
			b.WriteString("." + t)
		}
	}
	return b.String()
}

// schemaCache holds compiled rule schemas, recompiled when the file
// changes.
type schemaCache struct {
	mu sync.Mutex
	m  map[string]cachedSchema
}

type cachedSchema struct {
	size int64
	mod  time.Time
	sch  *jsonschema.Schema
}

// get returns the compiled schema at the bundle-relative path name. key
// identifies the bundle.
func (c *schemaCache) get(r *os.Root, key, name string) (*jsonschema.Schema, error) {
	p := filepath.FromSlash(name)
	info, err := r.Stat(p)
	if err != nil {
		return nil, err
	}
	ck := key + "\x00" + name
	c.mu.Lock()
	cached, ok := c.m[ck]
	c.mu.Unlock()
	if ok && cached.size == info.Size() && cached.mod.Equal(info.ModTime()) {
		return cached.sch, nil
	}
	data, err := r.ReadFile(p)
	if err != nil {
		return nil, err
	}
	sch, err := compileSchema(name, data)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]cachedSchema)
	}
	c.m[ck] = cachedSchema{size: info.Size(), mod: info.ModTime(), sch: sch}
	c.mu.Unlock()
	return sch, nil
}

// ruleFields applies the bundle's rules to a concept.
func (s *Store) ruleFields(r *os.Root, key string, b Bundle, id string, fm Frontmatter) ([]FieldError, error) {
	var out []FieldError
	for _, rule := range b.Rules {
		if !rule.applies(id, fm.Type()) {
			continue
		}
		if r == nil {
			return nil, fmt.Errorf("kb: bundle %q: rule schema %q: the bundle has no directory yet", b.name, rule.Schema)
		}
		sch, err := s.schemas.get(r, key, rule.Schema)
		if err != nil {
			return nil, fmt.Errorf("kb: bundle %q: rule schema %q: %w", b.name, rule.Schema, err)
		}
		out = append(out, schemaFields(sch, fm, " (schema "+rule.Schema+")")...)
	}
	return out, nil
}
