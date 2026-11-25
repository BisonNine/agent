/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models

import (
	"fmt"

	"go.bison9.com/agent/modelspec"
)

// Build builds a catalogue from a model document: its providers, its models
// and, for each agent in its assignments, the agent's set (Set(agent)), each
// list in document order. A model or an assignment the document marks
// disabled is added disabled. The document is validated first; with
// WithSecrets its secret references are resolved into a copy, and doc is not
// changed.
func Build(doc modelspec.Document, opts ...Option) (*Catalog, error) {
	if err := doc.Validate(); err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	c := New(opts...)
	if r := c.gen.Load().cfg.secrets; r != nil {
		resolved, err := modelspec.ResolveSecrets(doc, r)
		if err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
		doc = resolved
	}
	for _, p := range doc.Providers {
		if err := c.AddProvider(p); err != nil {
			return nil, err
		}
	}
	var off []string
	for _, m := range doc.Models {
		if err := c.Add(m); err != nil {
			return nil, err
		}
		if !m.Enabled {
			off = append(off, m.ModelName)
		}
	}
	if len(off) > 0 {
		if err := c.Disable(off...); err != nil {
			return nil, err
		}
	}
	for agent, purposes := range doc.Assignments {
		set := c.Set(agent)
		for purpose, list := range purposes {
			if len(list) == 0 {
				return nil, fmt.Errorf("models: agent %q: purpose %q: %w", agent, purpose, ErrNoModelAvailable)
			}
			for i, a := range list {
				if err := set.Assign(Purpose(purpose), a.Model, i, WithEnabled(a.IsEnabled())); err != nil {
					return nil, fmt.Errorf("models: agent %q: %w", agent, err)
				}
			}
		}
	}
	return c, nil
}
