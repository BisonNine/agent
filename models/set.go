package models

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Purpose is what a model is used for. It is an open set: a host adds its
// own simply by using them.
type Purpose string

// The purposes the SDK itself uses.
const (
	Chat      Purpose = "chat"      // the agent's turns
	Summarize Purpose = "summarize" // compaction summaries
	Memory    Purpose = "memory"    // cognitive-memory consolidation
	Worker    Purpose = "worker"    // sub-agents
)

func checkPurpose(p Purpose) error {
	if strings.TrimSpace(string(p)) == "" {
		return errors.New("models: purpose is required")
	}
	return nil
}

// assignment is one model in a purpose's list.
type assignment struct {
	name     string
	priority int
	seq      int // insertion order, for equal priorities
}

// setData is the models assigned to each purpose. Immutable once published.
type setData struct {
	lists map[Purpose][]assignment // each sorted by priority, then seq
	seq   int
}

func (sd *setData) clone() *setData {
	if sd == nil {
		return &setData{lists: map[Purpose][]assignment{}}
	}
	n := &setData{lists: make(map[Purpose][]assignment, len(sd.lists)), seq: sd.seq}
	for p, l := range sd.lists {
		n.lists[p] = slices.Clone(l)
	}
	return n
}

func (sd *setData) has(name string) bool {
	if sd == nil {
		return false
	}
	for l := range maps.Values(sd.lists) {
		if slices.ContainsFunc(l, func(a assignment) bool { return a.name == name }) {
			return true
		}
	}
	return false
}

func (sd *setData) without(name string) *setData {
	n := sd.clone()
	for p, l := range n.lists {
		l = slices.DeleteFunc(l, func(a assignment) bool { return a.name == name })
		if len(l) == 0 {
			delete(n.lists, p)
		} else {
			n.lists[p] = l
		}
	}
	return n
}

func sortAssignments(l []assignment) {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].priority != l[j].priority {
			return l[i].priority < l[j].priority
		}
		return l[i].seq < l[j].seq
	})
}

// assign adds an assignment to set id in g.
func (g *generation) assign(id string, p Purpose, name string, priority int) error {
	if err := checkPurpose(p); err != nil {
		return err
	}
	if _, ok := g.models[name]; !ok {
		return fmt.Errorf("models: unknown model %q", name)
	}
	sd := g.sets[id].clone()
	if slices.ContainsFunc(sd.lists[p], func(a assignment) bool { return a.name == name }) {
		return fmt.Errorf("models: model %q is already assigned to %s", name, p)
	}
	sd.lists[p] = append(sd.lists[p], assignment{name: name, priority: priority, seq: sd.seq})
	sd.seq++
	sortAssignments(sd.lists[p])
	g.sets[id] = sd
	return nil
}

// usable reports whether model name may be used, and whether g has it.
func (g *generation) usable(_ string, name string) (bool, bool) {
	e, ok := g.models[name]
	if !ok {
		return false, false
	}
	return !e.disabled, true
}

// Begin pins the models of purpose p for one turn, in priority order. The
// chain does not change when the catalogue does, except that each call
// re-reads whether a model is enabled.
func (c *Catalog) Begin(p Purpose) *Chain {
	g := c.gen.Load()
	ch := &Chain{cat: c, gen: g, purpose: p}
	if sd := g.sets[""]; sd != nil {
		for _, a := range sd.lists[p] {
			ch.cands = append(ch.cands, candidateOf(g, a))
		}
	}
	return ch
}
