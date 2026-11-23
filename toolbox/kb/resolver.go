/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"context"
	"strings"

	"go.bison9.com/agent/fsmode"
)

// Resolver maps knowledge-base names to bundles. The host implements one
// per agent (or per caller): it decides which names exist, where each
// bundle lives and whether it may be written. The library knows no naming
// scheme of its own.
type Resolver interface {
	// Resolve returns the bundle named name, or an error matching
	// ErrUnknownKB when there is none.
	Resolve(ctx context.Context, name string) (Bundle, error)
	// Bundles lists the knowledge bases the caller can use, for the
	// system prompt.
	Bundles(ctx context.Context) ([]Info, error)
}

// Info describes one knowledge base for a listing.
type Info struct {
	Name     string
	Writable bool
}

// Bundle is where a knowledge base lives and what may be done with it.
type Bundle struct {
	// Name is the knowledge base's name, as results report it. Empty: the
	// name that was resolved.
	Name string
	// Root is the bundle root, an absolute path.
	Root string
	// Confine, when set, is an absolute ancestor of Root. Root is then
	// opened (and created) through an os.Root on Confine, so a symbolic
	// link on the way cannot lead out of Confine.
	Confine string
	// Writable allows Put.
	Writable bool
	// Create lets a Put create a missing Root (and the missing directories
	// between Confine and Root). A missing Root reads as an empty bundle
	// when Create is set, and as ErrUnknownKB when it is not.
	Create bool
	// Rules are the bundle's dataset rules (see Rule).
	Rules []Rule
	// Modes are the modes of the directories and files a Put creates; a
	// zero field takes the Store's (Options.Modes).
	Modes fsmode.Modes
}

// Rule applies a JSON Schema to the frontmatter of some concepts. A rule
// applies to a concept whose ID starts with PathPrefix (when PathPrefix is
// set) or whose type is Type (when Type is set), so a concept cannot avoid
// a path's schema by declaring another type. A rule with neither applies
// to every concept. Schema is the bundle-relative path of the schema file,
// a JSON Schema (draft 2020-12 unless it says otherwise) that may not
// reference any other file or URL.
type Rule struct {
	PathPrefix string
	Type       string
	Schema     string
}

// applies reports whether the rule covers a concept with this ID and type.
func (r Rule) applies(id, typ string) bool {
	if r.PathPrefix == "" && r.Type == "" {
		return true
	}
	if r.PathPrefix != "" && strings.HasPrefix(id, r.PathPrefix) {
		return true
	}
	return r.Type != "" && typ == r.Type
}
