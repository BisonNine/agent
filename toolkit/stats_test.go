/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func statNames(st []Stat) []string {
	out := make([]string, 0, len(st))
	for _, s := range st {
		out = append(out, s.Name)
	}
	return out
}

// A tool's counts go when it leaves the catalogue, however it leaves; a
// tool replaced under the same name keeps them.
func TestStats_PrunedOnRemoval(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		remove func(c *Catalog, removeTS func()) error
		kept   []string
	}{
		{"Remove", func(c *Catalog, _ func()) error { c.Remove("a"); return nil }, []string{"b", "ts_x"}},
		{"ReplaceEntries drops", func(c *Catalog, _ func()) error {
			c.ReplaceEntries("a", listed("a2"))
			return nil
		}, []string{"b", "ts_x"}},
		{"Replace same name keeps", func(c *Catalog, _ func()) error {
			c.ReplaceEntries("a", listed("a"))
			return nil
		}, []string{"a", "b", "ts_x"}},
		{"Put same name keeps", func(c *Catalog, _ func()) error { return c.Put(listed("a")...) }, []string{"a", "b", "ts_x"}},
		{"toolset removed", func(_ *Catalog, removeTS func()) error { removeTS(); return nil }, []string{"a", "b"}},
		{"Adopt drops", func(c *Catalog, _ func()) error {
			next := New()
			if err := next.AddEntries(listed("b")...); err != nil {
				return err
			}
			c.Adopt(next)
			return nil
		}, []string{"b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New()
			view := c.View(nil)
			mustAdd(t, c, listed("a", "b")...)
			removeTS, err := c.AddToolset(ctx, newToolset("ts", listed("ts_x")...))
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []Registry{c, view} {
				for _, name := range []string{"a", "b", "ts_x"} {
					r.Execute(ctx, name, nil, nil)
				}
			}

			if err := tt.remove(c, removeTS); err != nil {
				t.Fatal(err)
			}

			for _, s := range []*Set{c.Default(), view} {
				got := statNames(s.Stats())
				if !slices.Equal(got, tt.kept) {
					t.Errorf("Stats names = %v, want %v", got, tt.kept)
				}
			}
		})
	}
}

// A name removed and added again starts its count over.
func TestStats_ReaddedNameStartsOver(t *testing.T) {
	ctx := context.Background()
	c := New()
	mustAdd(t, c, listed("a")...)
	c.Execute(ctx, "a", nil, nil)
	c.Execute(ctx, "a", nil, nil)
	c.Remove("a")
	mustAdd(t, c, listed("a")...)
	c.Execute(ctx, "a", nil, nil)
	want := []Stat{{Name: "a", Calls: 1}}
	if got := c.Stats(); !slices.Equal(got, want) {
		t.Fatalf("Stats = %+v, want %+v", got, want)
	}
}

// A call that finishes after its tool was removed leaves no count behind.
func TestStats_CallFinishingAfterRemoval(t *testing.T) {
	c := New()
	started, release := make(chan struct{}), make(chan struct{})
	mustAdd(t, c, Entry{Tool: &blockingTool{mockTool: *newMockTool("slow", ""), started: started, release: release}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Execute(context.Background(), "slow", nil, nil)
	}()
	<-started
	c.Remove("slow")
	close(release)
	<-done
	if got := c.Stats(); len(got) != 0 {
		t.Fatalf("Stats = %+v, want none", got)
	}
}

// Concurrent calls and removals: no race, and a name removed for good is
// absent from Stats once the calls are done.
func TestStats_ConcurrentExecuteAndRemove(t *testing.T) {
	ctx := context.Background()
	c := New()
	view := c.View(nil)
	const rounds = 50
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range rounds {
				name := fmt.Sprintf("t%d_%d", w, i)
				if err := c.AddEntries(listed(name)...); err != nil {
					t.Error(err)
					return
				}
				for range 3 {
					c.Execute(ctx, name, nil, nil)
					view.Execute(ctx, name, nil, nil)
					_ = c.Stats()
				}
				c.Remove(name)
			}
		})
		wg.Go(func() {
			for i := range rounds {
				c.Execute(ctx, fmt.Sprintf("t%d_%d", w, i), nil, nil)
				_ = view.Stats()
			}
		})
	}
	wg.Wait()
	for _, s := range []*Set{c.Default(), view} {
		if got := s.Stats(); len(got) != 0 {
			t.Fatalf("Stats = %+v, want none", got)
		}
	}
}
