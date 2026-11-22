package toolkit

import (
	"fmt"
	"maps"
)

// Switching tools on and off. A tool is usable by an agent when it is
// enabled in the catalogue and in that agent's set. A disabled tool stays
// registered (List and Get return it, with Info.Enabled false), but it is
// not offered: no snapshot, Listed or Hidden includes it, so neither the
// model, discovery nor an MCP host sees it, and a call to it returns the
// tool error `tool "x" is disabled`. A change takes effect at the next
// snapshot or call; a call already running is not interrupted.
//
// A flag belongs to the name: it survives the tool being replaced (a
// toolset refreshed, Put, Replace) or removed and added again, until the
// name is enabled. Adopt takes the flags next was built with.
//
// Switching a listed tool on or off changes the tool list, so it costs the
// agent its prompt cache, like adding or removing one.

// Disable switches the named tools off for every agent. It refuses the
// whole call, changing nothing, when a name is not registered
// (ErrToolNotFound).
func (c *Catalog) Disable(names ...string) error { return c.setFlags(names, true) }

// Enable switches the named tools back on for every agent. A name with no
// flag is ignored, registered or not, so a flag left on a removed name can
// always be cleared.
func (c *Catalog) Enable(names ...string) error { return c.setFlags(names, false) }

// Enabled reports whether name is registered and not disabled in the
// catalogue. A set's own flag is reported by Set.Enabled.
func (c *Catalog) Enabled(name string) bool {
	st := c.state.Load()
	_, ok := st.entries[name]
	return ok && !st.disabled[name]
}

// DisableGroup switches off, for every agent, the tools of a toolset or
// provider mounted under group, or, when no toolset or provider has that
// name, every tool whose internal name starts with group (include the
// separator: "mcp_alice_"). It returns how many tools the group holds.
func (c *Catalog) DisableGroup(group string) int { return c.setGroup(group, true) }

// EnableGroup is DisableGroup switching the tools back on.
func (c *Catalog) EnableGroup(group string) int { return c.setGroup(group, false) }

func (c *Catalog) setFlags(names []string, off bool) error {
	return c.update(func(next *catState) (bool, error) {
		if off {
			if err := registered(next, names); err != nil {
				return false, err
			}
		}
		return flip(next.disabled, names, off), nil
	})
}

func (c *Catalog) setGroup(group string, off bool) int {
	n := 0
	c.change(func(next *catState) bool {
		names := next.group(group)
		n = len(names)
		return flip(next.disabled, names, off)
	})
	return n
}

// Disable switches the named tools off for this set only. It refuses the
// whole call, changing nothing, when a name is not registered in the
// catalogue (ErrToolNotFound).
func (s *Set) Disable(names ...string) error {
	return s.setFlags(true, func(st *catState) ([]string, error) {
		return names, registered(st, names)
	})
}

// Enable switches the named tools back on for this set. A name with no
// flag is ignored, registered or not.
func (s *Set) Enable(names ...string) error {
	return s.setFlags(false, func(*catState) ([]string, error) { return names, nil })
}

// Enabled reports whether name is usable by this set: registered, granted,
// and enabled in both the catalogue and the set.
func (s *Set) Enabled(name string) bool {
	info, ok := s.Get(name)
	return ok && info.Enabled
}

// DisableGroup is Catalog.DisableGroup for this set only.
func (s *Set) DisableGroup(group string) int { return s.setGroup(group, true) }

// EnableGroup is Catalog.EnableGroup for this set only.
func (s *Set) EnableGroup(group string) int { return s.setGroup(group, false) }

func (s *Set) setGroup(group string, off bool) int {
	n := 0
	err := s.setFlags(off, func(st *catState) ([]string, error) {
		names := st.group(group)
		n = len(names)
		return names, nil
	})
	if err != nil {
		s.cat.logger.Error("Switching a tool group failed", "group", group, "error", err)
	}
	return n
}

// setFlags sets (off) or clears the set's flags for the names pick returns
// from the current catalogue state, under the write lock.
func (s *Set) setFlags(off bool, pick func(st *catState) ([]string, error)) error {
	c := s.cat
	c.mu.Lock()
	st := c.state.Load()
	names, err := pick(st)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	cur := s.cfg.Load()
	disabled := maps.Clone(cur.disabled)
	if disabled == nil {
		disabled = map[string]bool{}
	}
	if !flip(disabled, names, off) {
		c.mu.Unlock()
		return nil
	}
	next := *cur
	next.version = cur.version + 1
	next.disabled = disabled
	ev := c.commitLocked(st, nil, map[*Set]*setConfig{s: &next})
	c.mu.Unlock()
	c.publish(ev)
	return nil
}

// registered refuses a name st does not hold.
func registered(st *catState, names []string) error {
	for _, name := range names {
		if _, ok := st.entries[name]; !ok {
			return fmt.Errorf("toolkit: %q: %w", name, ErrToolNotFound)
		}
	}
	return nil
}

// flip sets (off) or clears the flags for names, reporting whether any
// changed.
func flip(flags map[string]bool, names []string, off bool) bool {
	changed := false
	for _, name := range names {
		if flags[name] == off {
			continue
		}
		if off {
			flags[name] = true
		} else {
			delete(flags, name)
		}
		changed = true
	}
	return changed
}
