package modelspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
)

// DocumentVersion is the Document format this package reads and writes.
const DocumentVersion = 1

// Assignment is one model in an agent's list for a purpose. Lists are in
// priority order, first is preferred.
type Assignment struct {
	Model string `json:"model"`
	// Enabled is nil when the document does not say, which means enabled.
	// Use IsEnabled to read it.
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled reports whether the assignment is on: unset or true.
func (a Assignment) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// Document is a whole model configuration: the providers, the models, and
// optionally which models each agent uses for which purpose. It is plain data;
// the application decides where it is stored.
//
// Its providers and models sections have the same JSON form as the host
// configuration files the types were taken from, so such a section can be
// used as is.
type Document struct {
	Version   int        `json:"version"`
	Providers []Provider `json:"providers"`
	Models    []Model    `json:"models"`
	// Assignments maps agent id → purpose → models in priority order.
	Assignments map[string]map[string][]Assignment `json:"assignments,omitempty"`
}

// Parse decodes and validates a document. Unknown fields and trailing data
// are errors, so a misspelt key is reported rather than ignored.
func Parse(data []byte) (Document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Document
	if err := dec.Decode(&d); err != nil {
		return Document{}, fmt.Errorf("parse model document: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Document{}, errors.New("parse model document: unexpected data after the document")
	}
	if err := d.Validate(); err != nil {
		return Document{}, err
	}
	return d, nil
}

// LoadFile reads and parses the document at path.
func LoadFile(path string) (Document, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller names the file
	if err != nil {
		return Document{}, fmt.Errorf("load model document: %w", err)
	}
	d, err := Parse(data)
	if err != nil {
		return Document{}, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// Marshal encodes the document as indented JSON ending in a newline. Empty
// provider and model lists are written as [], never null.
func (d Document) Marshal() ([]byte, error) {
	if d.Providers == nil {
		d.Providers = []Provider{}
	}
	if d.Models == nil {
		d.Models = []Model{}
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal model document: %w", err)
	}
	return append(data, '\n'), nil
}

// Validate checks the document: a supported version; every provider named,
// uniquely, with valid protocol settings; every model valid, uniquely named
// and on a known provider; every assignment naming a known model, at most
// once per list.
func (d Document) Validate() error {
	switch {
	case d.Version == 0:
		return errors.New("version is required")
	case d.Version != DocumentVersion:
		return fmt.Errorf("version %d is not supported (want %d)", d.Version, DocumentVersion)
	}
	providers := make(map[string]struct{}, len(d.Providers))
	for i := range d.Providers {
		p := &d.Providers[i]
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("providers[%d]: name is required", i)
		}
		if _, dup := providers[p.Name]; dup {
			return fmt.Errorf("providers[%d]: duplicate provider name %q", i, p.Name)
		}
		providers[p.Name] = struct{}{}
		if err := p.ValidateProtocol(); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
	}
	models := make(map[string]struct{}, len(d.Models))
	for i := range d.Models {
		m := &d.Models[i]
		if err := m.Validate(); err != nil {
			return fmt.Errorf("models[%d]: %w", i, err)
		}
		if _, ok := providers[m.Provider]; !ok {
			return fmt.Errorf("models[%d] (%q): provider %q not found", i, m.ModelName, m.Provider)
		}
		if _, dup := models[m.ModelName]; dup {
			return fmt.Errorf("models[%d]: duplicate model name %q", i, m.ModelName)
		}
		models[m.ModelName] = struct{}{}
	}
	return d.validateAssignments(models)
}

func (d Document) validateAssignments(models map[string]struct{}) error {
	for _, agent := range slices.Sorted(maps.Keys(d.Assignments)) {
		if strings.TrimSpace(agent) == "" {
			return errors.New("assignments: agent id is required")
		}
		purposes := d.Assignments[agent]
		for _, purpose := range slices.Sorted(maps.Keys(purposes)) {
			if strings.TrimSpace(purpose) == "" {
				return fmt.Errorf("assignments[%q]: purpose is required", agent)
			}
			seen := make(map[string]struct{}, len(purposes[purpose]))
			for i, a := range purposes[purpose] {
				if _, ok := models[a.Model]; !ok {
					return fmt.Errorf("assignments[%q][%q][%d]: model %q not found", agent, purpose, i, a.Model)
				}
				if _, dup := seen[a.Model]; dup {
					return fmt.Errorf("assignments[%q][%q][%d]: model %q is listed twice", agent, purpose, i, a.Model)
				}
				seen[a.Model] = struct{}{}
			}
		}
	}
	return nil
}

// clone returns a deep copy of d: no slice, map or pointer is shared with it.
// Values inside a model's extra_body are copied when they are JSON values
// (objects, arrays and scalars); any other type is carried over as is.
func (d Document) clone() Document {
	out := Document{Version: d.Version}
	out.Providers = slices.Clone(d.Providers)
	if d.Models != nil {
		out.Models = make([]Model, len(d.Models))
		for i, m := range d.Models {
			out.Models[i] = cloneModel(m)
		}
	}
	if d.Assignments != nil {
		out.Assignments = make(map[string]map[string][]Assignment, len(d.Assignments))
		for agent, purposes := range d.Assignments {
			if purposes == nil {
				out.Assignments[agent] = nil
				continue
			}
			cp := make(map[string][]Assignment, len(purposes))
			for purpose, list := range purposes {
				cp[purpose] = cloneAssignments(list)
			}
			out.Assignments[agent] = cp
		}
	}
	return out
}

func cloneModel(m Model) Model {
	m.ExtraArgs = slices.Clone(m.ExtraArgs)
	m.Env = maps.Clone(m.Env)
	m.DropParams = slices.Clone(m.DropParams)
	if m.ExtraBody != nil {
		if body, ok := cloneJSON(m.ExtraBody).(map[string]any); ok {
			m.ExtraBody = body
		}
	}
	return m
}

func cloneAssignments(list []Assignment) []Assignment {
	if list == nil {
		return nil
	}
	out := make([]Assignment, len(list))
	for i, a := range list {
		if a.Enabled != nil {
			v := *a.Enabled
			a.Enabled = &v
		}
		out[i] = a
	}
	return out
}

// cloneJSON deep-copies a decoded JSON value.
func cloneJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return t
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = cloneJSON(val)
		}
		return out
	case []any:
		if t == nil {
			return t
		}
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = cloneJSON(val)
		}
		return out
	default:
		return v
	}
}
