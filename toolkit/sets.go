/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"maps"
	"runtime"
)

// Set returns the named set id, creating it on first use. A set created
// here grants nothing until Grant is called. The empty id is the default
// set, which grants every tool. Adopt carries named sets over by id.
func (c *Catalog) Set(id string) *Set {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setLocked(id)
}

func (c *Catalog) setLocked(id string) *Set {
	if s, ok := c.sets[id]; ok {
		return s
	}
	s := c.newSetLocked(denyAll{})
	c.sets[id] = s
	return s
}

// View returns a new unnamed set granted what allow allows (every tool
// when allow is nil): a filtered view of the catalogue, for an agent whose
// grants sit on shared tools. Adopt keeps a view's grant as it is.
func (c *Catalog) View(allow AllowChecker) *Set {
	c.mu.Lock()
	s := c.newSetLocked(allow)
	c.mu.Unlock()
	// Its settings live in the catalogue state; forget them once nobody
	// can reach the view.
	runtime.AddCleanup(s, c.dropSet, s.key)
	return s
}

// Default returns the default set: the one the catalogue's own read
// methods use. It grants every tool unless Grant narrows it.
func (c *Catalog) Default() *Set { return c.def }

// Grant sets what the set is granted: the tools allow allows, by internal
// name, or every tool when allow is nil. It takes effect at the next
// snapshot and sends subscribers one notice. allow is asked again at every
// snapshot and call, so a policy that changes its answers is honoured at
// the next one, but sends no notice: call Grant again to announce it.
func (s *Set) Grant(allow AllowChecker) {
	c := s.cat
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.state.Load()
	cfg := *cur.cfg(s.key)
	cfg.version++
	cfg.allow = allow
	next := cur.clone()
	next.cfgs[s.key] = &cfg
	c.commitLocked(cur, next)
}

// Adopt replaces the catalogue's contents with next's, in one step: "when
// you can, switch to these". The host builds next the same way it built
// the first catalogue, so every error (a duplicate name, a failed toolset)
// is returned while next is built and Adopt itself cannot fail.
//
//   - Agents keep the handles they hold: the catalogue and its sets stay
//     the same objects, and their next snapshot shows the new contents.
//   - Each named set takes the grant next's set of the same id has; a
//     named set next does not have, and every View, keeps its own.
//   - A call already running finishes on the tool it started with.
//   - Each subscriber gets one notice for the whole swap. A tool present
//     before and after counts as changed: its definition may differ.
//   - Version keeps rising.
//
// It returns the change to the catalogue as a whole, for the host to log.
// next is read, not consumed: later changes to it do not reach c, and the
// remove functions next's AddToolset and AddProvider returned act on next
// only; c's earlier ones no longer remove anything.
func (c *Catalog) Adopt(next *Catalog) Change {
	if next == nil || next == c {
		return Change{}
	}
	next.mu.Lock()
	ns := next.state.Load()
	grants := make(map[string]*setConfig, len(next.sets))
	for id, s := range next.sets {
		grants[id] = ns.cfg(s.key)
	}
	next.mu.Unlock()

	c.mu.Lock()
	targets := make(map[*Set]*setConfig, len(grants))
	for id, g := range grants {
		targets[c.setLocked(id)] = g // creates the sets c lacks
	}
	cur := c.state.Load()
	st := ns.clone()
	st.version = cur.version + 1
	// Fresh registration ids, so no remove function handed out before
	// matches what was adopted.
	for source := range st.sources {
		c.regSeq++
		st.sources[source] = c.regSeq
	}
	// Settings: c's sets keep theirs, except the named sets next has.
	st.cfgs = maps.Clone(cur.cfgs)
	for s, g := range targets {
		nc := *g
		nc.version = cur.cfg(s.key).version + 1
		st.cfgs[s.key] = &nc
	}
	st.seal()
	c.commitLocked(cur, st)
	c.mu.Unlock()

	ch := diff(cur.entries, st.entries)
	c.logger.Info("Adopted a new tool catalogue",
		"added", len(ch.Added), "removed", len(ch.Removed), "changed", len(ch.Changed))
	return ch
}
