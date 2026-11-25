/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
)

func list(names ...string) []models.Assignment {
	out := make([]models.Assignment, 0, len(names))
	for _, n := range names {
		out = append(out, models.Assignment{Name: n})
	}
	return out
}

func TestReplace(t *testing.T) {
	st := &memStore{data: map[string]string{}}
	f := newFixture(t, three, models.WithSelectionStore(st))
	bob := f.cat.Set("bob")
	if _, err := bob.ReplaceAll(map[models.Purpose][]models.Assignment{
		models.Chat:      list("sonnet", "gpt", "haiku"),
		models.Summarize: {{Name: "haiku", Options: []models.AssignOption{models.WithSettings("s")}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := names(bob.List(models.Chat)); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("chat = %q", got)
	}
	if bob.List(models.Summarize)[0].Settings != "s" {
		t.Fatal("options were not applied")
	}
	if err := bob.Select(models.Chat, "gpt", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := bob.Select(models.Chat, "haiku", "s2"); err != nil {
		t.Fatal(err)
	}
	if got := answeredBy(t, bob.Begin(models.Chat, "s1")); got != "gpt" {
		t.Fatalf("answered by %q", got)
	}
	st.saves = nil

	for _, bad := range [][]models.Assignment{list("sonnet", "nope"), list("gpt", "gpt")} {
		if _, err := bob.Replace(models.Chat, bad...); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	if got := names(bob.List(models.Chat)); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("a refused replace changed the list: %q", got)
	}

	reverted, err := bob.Replace(models.Chat, list("haiku", "gpt")...)
	if err != nil {
		t.Fatal(err)
	}
	want := []models.Reverted{{Set: "bob", Purpose: models.Chat, Scope: "s2", Model: "haiku"}}
	if len(reverted) != 0 {
		t.Fatalf("reverted = %+v, want none (both selections kept)", reverted)
	}
	if got := bob.Selected(models.Chat, "s1").Name; got != "gpt" {
		t.Fatalf("a still-valid selection was lost: %q", got)
	}
	if cur := bob.Current(models.Chat, "s1"); cur.Serving != "gpt" {
		t.Fatalf("serving was lost: %+v", cur)
	}
	// Both selections kept by name in a reordered list: written again.
	if w := []string{"bob|chat|s1=gpt", "bob|chat|s2=haiku"}; !slices.Equal(st.saves, w) {
		t.Fatalf("saves = %q, want %q", st.saves, w)
	}

	st.saves = nil
	reverted, err = bob.Replace(models.Chat, list("sonnet", "gpt")...)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reverted, want) {
		t.Fatalf("reverted = %+v, want %+v", reverted, want)
	}
	if w := []string{"bob|chat|s1=gpt", "bob|chat|s2="}; !slices.Equal(st.saves, w) {
		t.Fatalf("saves = %q, want %q", st.saves, w)
	}

	st.saves = nil
	f.obs.reset()
	before := names(bob.List(models.Chat))
	if r, err := bob.Replace(models.Chat, list("sonnet", "gpt")...); err != nil || len(r) != 0 || len(st.saves) != 0 {
		t.Fatalf("an identical replace: %v %+v, saves %q", err, r, st.saves)
	}
	if !slices.Equal(names(bob.List(models.Chat)), before) {
		t.Fatal("an identical replace changed the list")
	}
	if _, err := bob.Replace(models.Summarize); err != nil || len(bob.List(models.Summarize)) != 0 {
		t.Fatal("an empty list did not remove the purpose")
	}
}

// Unassign and Remove write a reverted selection to the store; Reorder
// writes a kept one again.
func TestSelectionStoreFollowsListChanges(t *testing.T) {
	st := &memStore{data: map[string]string{}}
	f := newFixture(t, three, models.WithSelectionStore(st))
	if err := f.cat.Select(models.Chat, "gpt", "s"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.Select(models.Chat, "haiku", "t"); err != nil {
		t.Fatal(err)
	}
	st.saves = nil
	if err := f.cat.Default().Reorder(models.Chat, []string{"haiku", "gpt", "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if w := []string{"|chat|s=gpt", "|chat|t=haiku"}; !slices.Equal(st.saves, w) {
		t.Fatalf("after reorder saves = %q, want %q", st.saves, w)
	}
	st.saves = nil
	f.cat.Default().Unassign(models.Chat, "gpt")
	if w := []string{"|chat|s=", "|chat|t=haiku"}; !slices.Equal(st.saves, w) {
		t.Fatalf("after unassign saves = %q, want %q", st.saves, w)
	}
	st.saves = nil
	if err := f.cat.Remove("haiku"); err != nil {
		t.Fatal(err)
	}
	if w := []string{"|chat|t="}; !slices.Equal(st.saves, w) {
		t.Fatalf("after remove saves = %q, want %q", st.saves, w)
	}
	st.saves = nil
	next := build(t, f.fake, []spec{three[0]}, models.WithSelectionStore(st))
	if err := f.cat.Select(models.Chat, "sonnet", "u"); err != nil {
		t.Fatal(err)
	}
	st.saves = nil
	f.cat.Adopt(next)
	if len(st.saves) != 0 {
		t.Fatalf("an adopt that kept the list wrote %q", st.saves)
	}
}

type flakyStore struct {
	memStore

	fail bool
}

func (s *flakyStore) LoadSelection(set string, p models.Purpose, scope models.Scope) (string, error) {
	if s.fail {
		return "", errors.New("not now")
	}
	return s.memStore.LoadSelection(set, p, scope)
}

// A load error leaves the selection unloaded: it is read again next time.
func TestSelectionLoadErrorRetried(t *testing.T) {
	st := &flakyStore{fail: true}
	st.data = map[string]string{"|chat|s": "haiku"}
	c := build(t, modelstest.New(), three, models.WithSelectionStore(st))
	if got := c.Selected(models.Chat, "s").Name; got != "sonnet" {
		t.Fatalf("during the error: %q, want the default", got)
	}
	st.fail = false
	if got := c.Selected(models.Chat, "s").Name; got != "haiku" {
		t.Fatalf("after the error: %q, want the stored selection", got)
	}
}

// Turns begun while lists are replaced see the old list or the new one,
// never an empty or mixed one, and a still-valid selection is never lost.
func TestReplaceIsAtomic(t *testing.T) {
	f := newFixture(t, three)
	bob := f.cat.Set("bob")
	a, b := list("sonnet", "gpt", "haiku"), list("haiku", "gpt")
	if _, err := bob.Replace(models.Chat, a...); err != nil {
		t.Fatal(err)
	}
	if err := bob.Select(models.Chat, "gpt", "s"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			next := a
			if i%2 == 0 {
				next = b
			}
			if _, err := bob.Replace(models.Chat, next...); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 2000 {
		got := candNames(bob.Begin(models.Chat, "s"))
		if !slices.Equal(got, []string{"gpt", "sonnet", "haiku"}) && !slices.Equal(got, []string{"gpt", "haiku"}) {
			close(stop)
			wg.Wait()
			t.Fatalf("a turn saw %q", got)
		}
	}
	close(stop)
	wg.Wait()
}

// blockingStore holds its first write of "" until released.
type blockingStore struct {
	memStore

	held, release chan struct{}
	once          sync.Once
}

func (s *blockingStore) SaveSelection(set string, p models.Purpose, scope models.Scope, name string) error {
	if name == "" {
		hold := false
		s.once.Do(func() { hold = true })
		if hold {
			close(s.held)
			<-s.release
		}
	}
	return s.memStore.SaveSelection(set, p, scope, name)
}

// A store write delayed behind a newer selection never overwrites it: the
// store ends with what memory holds.
func TestStoreWritesFollowMemory(t *testing.T) {
	st := &blockingStore{held: make(chan struct{}), release: make(chan struct{})}
	st.data = map[string]string{}
	f := newFixture(t, three, models.WithSelectionStore(st))
	if err := f.cat.Select(models.Chat, "gpt", "s"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.cat.Default().Unassign(models.Chat, "gpt") // reverts s: writes "", held
	}()
	<-st.held
	if err := f.cat.Select(models.Chat, "haiku", "s"); err != nil {
		t.Fatal(err)
	}
	close(st.release)
	<-done
	st.mu.Lock()
	got := st.data["|chat|s"]
	st.mu.Unlock()
	if got != "haiku" {
		t.Fatalf("store = %q, memory = %q", got, f.cat.Selected(models.Chat, "s").Name)
	}
}
