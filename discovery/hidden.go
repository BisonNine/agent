package discovery

import (
	"context"
	"strings"

	"go.bison9.com/agent/toolkit"
)

// HideOption configures Hidden and HideEntries.
type HideOption func(*hideConfig)

type hideConfig struct {
	pins []func(t toolkit.Tool) bool
}

// AlwaysShown keeps the tools matching any of patterns listed. A pattern is
// a name prefix, compared without regard to case or to runs of underscores
// ("a__b" is "a_b"), against the tool's internal name and, when it has one,
// its ExternalName. "*" matches every tool; a trailing "*" is ignored
// ("crm_*" is "crm_"); an empty pattern matches nothing.
func AlwaysShown(patterns ...string) HideOption {
	norm := make([]string, 0, len(patterns))
	for _, p := range patterns {
		norm = append(norm, normalisePin(p))
	}
	match := func(name string) bool {
		n := collapseUnderscores(strings.ToLower(name))
		for _, p := range norm {
			if p == "*" {
				return true
			}
			if p = strings.TrimSuffix(p, "*"); p != "" && strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
	return func(c *hideConfig) {
		c.pins = append(c.pins, func(t toolkit.Tool) bool {
			if match(t.Name()) {
				return true
			}
			ext := externalName(t)
			return ext != "" && match(ext)
		})
	}
}

// AlwaysShownFunc keeps listed every tool for which pinned reports true. It
// is for a host whose pinning rule is its own. Unlike AlwaysShown, which
// also matches the ExternalName, pinned is asked exactly once per tool,
// with the tool's internal name only.
func AlwaysShownFunc(pinned func(name string) bool) HideOption {
	return func(c *hideConfig) {
		if pinned != nil {
			c.pins = append(c.pins, func(t toolkit.Tool) bool { return pinned(t.Name()) })
		}
	}
}

// pinned reports whether t is kept listed.
func (c *hideConfig) pinned(t toolkit.Tool) bool {
	for _, pin := range c.pins {
		if pin(t) {
			return true
		}
	}
	return false
}

func normalisePin(p string) string {
	return collapseUnderscores(strings.TrimSpace(strings.ToLower(p)))
}

// collapseUnderscores replaces every run of underscores with one.
func collapseUnderscores(s string) string {
	if !strings.Contains(s, "__") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prev := false
	for _, r := range s {
		if r == '_' {
			if prev {
				continue
			}
			prev = true
		} else {
			prev = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// HideEntries returns a copy of entries with every tool hidden except those
// an AlwaysShown pin keeps listed and search_tools and get_tool_details,
// which are always listed. A hidden entry with no Group takes the
// group its tool reports through toolkit.DiscoveryGrouped, if any. An entry
// with no tool is copied as is (the catalogue refuses it).
func HideEntries(entries []toolkit.Entry, opts ...HideOption) []toolkit.Entry {
	var cfg hideConfig
	for _, o := range opts {
		o(&cfg)
	}
	out := make([]toolkit.Entry, len(entries))
	for i, e := range entries {
		out[i] = e
		if e.Tool == nil {
			continue
		}
		out[i].Hidden = !isMetaTool(e.Tool) && !cfg.pinned(e.Tool)
		if out[i].Hidden && e.Group == "" {
			if g, ok := e.Tool.(toolkit.DiscoveryGrouped); ok {
				out[i].Group, out[i].RevealTogether = g.DiscoveryGroup()
			}
		}
	}
	return out
}

// Hidden returns ts with its tools hidden, as HideEntries hides them: they
// are offered only through discovery and may still be called at any time.
// It returns nil for a nil toolset.
//
//	reg.AddToolset(ctx, discovery.Hidden(crmTools, discovery.AlwaysShown("crm_start")))
func Hidden(ts toolkit.Toolset, opts ...HideOption) toolkit.Toolset {
	if ts == nil {
		return nil
	}
	return hiddenToolset{inner: ts, opts: opts}
}

type hiddenToolset struct {
	inner toolkit.Toolset
	opts  []HideOption
}

func (h hiddenToolset) Name() string { return h.inner.Name() }

func (h hiddenToolset) Tools(ctx context.Context) ([]toolkit.Entry, error) {
	entries, err := h.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	return HideEntries(entries, h.opts...), nil
}

// isMetaTool reports whether t is one of this package's meta-tools.
func isMetaTool(t toolkit.Tool) bool {
	switch t.(type) {
	case *searchTool, *detailsTool:
		return true
	default:
		return false
	}
}

// externalName is t's ExternalName, or "" when it has none.
func externalName(t toolkit.Tool) string {
	if en, ok := t.(toolkit.ExternalNamer); ok {
		return en.ExternalName()
	}
	return ""
}
