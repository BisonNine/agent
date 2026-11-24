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
// Results and errors name a knowledge base as the caller asked for it.
type Bundle struct {
	// Root is the bundle root, an absolute path.
	Root string
	// Confine, when set, is an absolute ancestor of Root, typically the
	// folder holding an agent's own bundles. It guarantees that the bundle
	// is a real directory inside Confine: Root and every directory between
	// Confine and Root are opened one at a time from an os.Root on Confine,
	// each checked to be a directory and not a symbolic link, so no link
	// (absolute or relative) can make the bundle another bundle under
	// Confine or anything outside it. Confine itself is the host's and is
	// opened as given. Without Confine, Root is opened as given, links
	// included. Inside the bundle, links are never followed either way.
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

	name string // the name resolved, set by the Store
}

// Rule applies a JSON Schema to the frontmatter of some concepts. A rule
// applies to a concept whose ID is under PathPrefix (when PathPrefix is
// set) or whose type is Type (when Type is set), so a concept cannot avoid
// a path's schema by declaring another type. PathPrefix matches whole
// segments: "SFR" (or "SFR/") covers "SFR" and "SFR/FCS_COP.1", not
// "SFRX/a". A rule with neither applies
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
	if p := strings.TrimSuffix(r.PathPrefix, "/"); p != "" && (id == p || strings.HasPrefix(id, p+"/")) {
		return true
	}
	return r.Type != "" && typ == r.Type
}
