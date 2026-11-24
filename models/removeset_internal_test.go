package models

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// countStore is a SelectionStore that counts its writes.
type countStore struct {
	mu     sync.Mutex
	writes int
}

func (s *countStore) LoadSelection(string, Purpose, Scope) (string, error) { return "", nil }

func (s *countStore) SaveSelection(string, Purpose, Scope, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	return nil
}

func (s *countStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// removeCatalog has models "a" and "b", answered by a scripted client, and a
// set "alice" with both for Chat.
func removeCatalog(t *testing.T, opts ...Option) (*Catalog, *Set, *modelstest.Provider) {
	t.Helper()
	c := New(opts...)
	if err := c.AddProvider(modelspec.Provider{Name: "p", Protocol: "openai-chat", BaseURL: "https://example.invalid/v1"}); err != nil {
		t.Fatal(err)
	}
	fake := modelstest.New()
	for _, n := range []string{"a", "b"} {
		if err := c.Add(modelspec.Model{ModelName: n, Model: n, Provider: "p"}); err != nil {
			t.Fatal(err)
		}
		if err := c.RegisterClient(n, fake); err != nil {
			t.Fatal(err)
		}
	}
	alice := c.Set("alice")
	for i, n := range []string{"a", "b"} {
		if err := alice.Assign(Chat, n, i); err != nil {
			t.Fatal(err)
		}
	}
	return c, alice, fake
}

// heldFor reports whether the run state holds anything for set id.
func heldFor(c *Catalog, id string) bool {
	r := c.state
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.selected {
		if k.set == id {
			return true
		}
	}
	for k := range r.loaded {
		if k.set == id {
			return true
		}
	}
	for k := range r.serve {
		if k.set == id {
			return true
		}
	}
	return false
}

func TestRemoveSet(t *testing.T) {
	store := &countStore{}
	c, alice, _ := removeCatalog(t, WithSelectionStore(store))
	if err := alice.Disable("a"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Select(Chat, "b", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Begin(Chat, "s1").Chat(context.Background(), closeID, Request{}); err != nil {
		t.Fatal(err)
	}
	if !heldFor(c, "alice") {
		t.Fatal("setup: nothing held for alice")
	}
	writes := store.count()

	if !c.RemoveSet("alice") {
		t.Fatal("RemoveSet(alice) = false")
	}
	if _, ok := c.gen.Load().sets["alice"]; ok {
		t.Error("the set is still in the generation")
	}
	if _, ok := c.handles["alice"]; ok {
		t.Error("the handle is still kept")
	}
	if heldFor(c, "alice") {
		t.Error("selections or serving records are still held")
	}
	if store.count() != writes {
		t.Errorf("the store was written %d times", store.count()-writes)
	}
	if err := alice.Assign(Summarize, "a", 0); err == nil {
		t.Error("a write on the removed handle succeeded")
	}
	fresh := c.Set("alice")
	if fresh == alice {
		t.Fatal("Set returned the removed handle")
	}
	if len(fresh.List(Chat)) != 0 || !fresh.Enabled("a") || fresh.Selected(Chat, "s1").Name != "" {
		t.Errorf("the new set is not empty: %+v", fresh.List(Chat))
	}
	if !c.RemoveSet("alice") {
		t.Error("removing the new, empty set reported none")
	}
	for _, id := range []string{"alice", "", "nobody"} {
		if c.RemoveSet(id) {
			t.Errorf("RemoveSet(%q) = true", id)
		}
	}
}

// A chain begun before RemoveSet finishes on what it pinned and records
// nothing for the removed set.
func TestRemoveSetPinnedChainCompletes(t *testing.T) {
	c, alice, fake := removeCatalog(t)
	hold, started := make(chan struct{}), make(chan struct{})
	fake.Script("a", modelstest.Step{Hold: hold, Started: started, Response: modelstest.Reply("ok").Response})
	ch := alice.Begin(Chat, "s1")
	done := make(chan error, 1)
	go func() {
		_, err := ch.Chat(context.Background(), closeID, Request{})
		done <- err
	}()
	<-started
	c.RemoveSet("alice")
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("pinned chain: %v", err)
	}
	if heldFor(c, "alice") {
		t.Error("the pinned chain recorded a serving record for the removed set")
	}
}

// Begin, calls and RemoveSet on one set at once. Run with -race.
func TestRemoveSetRacesBegin(t *testing.T) {
	c, _, _ := removeCatalog(t)
	var refused atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := c.Set("alice")
				if s.Assign(Chat, "a", 0) != nil {
					refused.Add(1) // already assigned, or the set was just removed
				}
				if _, err := s.Begin(Chat, "s1").Chat(context.Background(), closeID, Request{}); err != nil {
					if nm := (*NoModelError)(nil); !errors.As(err, &nm) {
						t.Errorf("Chat = %v", err)
						return
					}
				}
			}
		})
	}
	for range 200 {
		c.RemoveSet("alice")
	}
	close(stop)
	wg.Wait()
	c.RemoveSet("alice")
	if heldFor(c, "alice") {
		t.Error("something is held for the removed set")
	}
}
