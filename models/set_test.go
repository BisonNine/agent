package models_test

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

func TestSetAssignOrder(t *testing.T) {
	f := newFixture(t, three)
	bob := f.cat.Set("bob")
	for _, a := range []struct {
		name     string
		priority int
	}{{"haiku", 2}, {"gpt", 1}, {"sonnet", 2}} {
		if err := bob.Assign(models.Summarize, a.name, a.priority, models.WithSettings(a.name+"-settings")); err != nil {
			t.Fatal(err)
		}
	}
	list := bob.List(models.Summarize)
	if got := names(list); !slices.Equal(got, []string{"gpt", "haiku", "sonnet"}) {
		t.Fatalf("order = %q: lower priority first, ties in insertion order", got)
	}
	if list[0].Settings != "gpt-settings" || !list[0].Enabled || list[0].Purpose != models.Summarize {
		t.Fatalf("entry = %+v", list[0])
	}
	if err := bob.Assign(models.Summarize, "gpt", 5); err == nil {
		t.Error("a model was assigned twice to one purpose")
	}
	if err := bob.Assign(models.Summarize, "nope", 5); err == nil {
		t.Error("an unknown model was assigned")
	}
	if err := bob.Assign("", "gpt", 5); err == nil {
		t.Error("an empty purpose was accepted")
	}
	if err := bob.Reorder(models.Summarize, []string{"sonnet", "gpt", "haiku"}); err != nil {
		t.Fatal(err)
	}
	if got := names(bob.List(models.Summarize)); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("after reorder = %q", got)
	}
	for _, bad := range [][]string{{"sonnet", "gpt"}, {"sonnet", "gpt", "gpt"}, {"sonnet", "gpt", "nope"}} {
		if err := bob.Reorder(models.Summarize, bad); err == nil {
			t.Errorf("reorder %q accepted", bad)
		}
	}
	if !bob.Unassign(models.Summarize, "gpt") || bob.Unassign(models.Summarize, "gpt") {
		t.Fatal("unassign did not report once")
	}
	if got := bob.Purposes(); !slices.Equal(got, []models.Purpose{models.Summarize}) {
		t.Fatalf("purposes = %q", got)
	}
	if f.cat.Set("bob") != bob || bob.ID() != "bob" {
		t.Fatal("Set did not return the same handle")
	}
	if got := names(f.cat.List(models.Chat)); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("the default set changed: %q", got)
	}
}

// The selected model is tried first, then the rest in priority order; a
// scoped selection wins over the set's default one.
func TestSetSelection(t *testing.T) {
	f := newFixture(t, three)
	if got := candNames(f.cat.Begin(models.Chat)); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("no selection: %q", got)
	}
	if err := f.cat.Select(models.Chat, "haiku"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.Select(models.Chat, "gpt", "session-1"); err != nil {
		t.Fatal(err)
	}
	for scope, want := range map[models.Scope][]string{
		"":          {"haiku", "sonnet", "gpt"},
		"session-1": {"gpt", "sonnet", "haiku"},
		"session-2": {"haiku", "sonnet", "gpt"},
	} {
		if got := candNames(f.cat.Begin(models.Chat, scope)); !slices.Equal(got, want) {
			t.Errorf("scope %q: %q, want %q", scope, got, want)
		}
		if got := f.cat.Selected(models.Chat, scope).Name; got != want[0] {
			t.Errorf("scope %q: selected %q", scope, got)
		}
	}
	f.cat.Reset(models.Chat, "session-1")
	if got := f.cat.Selected(models.Chat, "session-1").Name; got != "haiku" {
		t.Errorf("after reset the scope follows the default: %q", got)
	}
	f.cat.Reset(models.Chat)
	if got := f.cat.Selected(models.Chat, "session-1").Name; got != "sonnet" {
		t.Errorf("after both resets: %q", got)
	}
	if err := f.cat.Select(models.Chat, "nope"); err == nil {
		t.Error("a model not in the list was selected")
	}
	if got := f.cat.Selected(models.Memory); got.Name != "" {
		t.Errorf("a purpose without models has a selection: %+v", got)
	}
}

// A turn is pinned: a chain does not change when the set does.
func TestChainIsPinned(t *testing.T) {
	f := newFixture(t, three)
	ch := f.cat.Begin(models.Chat, "s")
	if err := f.cat.Select(models.Chat, "haiku", "s"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.Default().Reorder(models.Chat, []string{"gpt", "haiku", "sonnet"}); err != nil {
		t.Fatal(err)
	}
	f.cat.Default().Unassign(models.Chat, "sonnet")
	if got := candNames(ch); !slices.Equal(got, []string{"sonnet", "gpt", "haiku"}) {
		t.Fatalf("pinned chain changed: %q", got)
	}
	for range 2 {
		res, err := ch.Chat(t.Context(), alice, models.Request{})
		if err != nil || res.Candidate.Name != "sonnet" {
			t.Fatalf("pinned call: %v, %+v", err, res)
		}
	}
	if got := candNames(f.cat.Begin(models.Chat, "s")); !slices.Equal(got, []string{"haiku", "gpt"}) {
		t.Fatalf("next turn: %q", got)
	}
}

func TestPurposeFallback(t *testing.T) {
	f := newFixture(t, three, models.WithFallback("vision", models.Summarize))
	ch := f.cat.Begin(models.Worker)
	if ch.Purpose() != models.Chat || len(ch.Candidates()) != 3 {
		t.Fatalf("worker without models: purpose %q, %d models", ch.Purpose(), len(ch.Candidates()))
	}
	if err := f.cat.Assign(models.Worker, "haiku", 0); err != nil {
		t.Fatal(err)
	}
	if ch := f.cat.Begin(models.Worker); ch.Purpose() != models.Worker || !slices.Equal(candNames(ch), []string{"haiku"}) {
		t.Fatalf("worker with its own: %q %q", ch.Purpose(), candNames(ch))
	}
	if ch := f.cat.Begin("vision"); len(ch.Candidates()) != 0 {
		t.Fatal("vision fell back to an empty summarize list and found models")
	}
	if err := f.cat.Assign(models.Summarize, "gpt", 0); err != nil {
		t.Fatal(err)
	}
	if ch := f.cat.Begin("vision"); ch.Purpose() != models.Summarize {
		t.Fatalf("vision: %q", ch.Purpose())
	}
	g := newFixture(t, three, models.WithFallback(models.Worker, ""))
	if _, err := g.cat.Begin(models.Worker).Chat(t.Context(), alice, models.Request{}); !errors.Is(err, models.ErrNoModelAvailable) {
		t.Fatalf("worker without a rule: %v", err)
	}
}

type memStore struct {
	mu     sync.Mutex
	data   map[string]string
	loads  int
	saves  []string
	broken bool
}

func (s *memStore) key(set string, p models.Purpose, scope models.Scope) string {
	return set + "|" + string(p) + "|" + string(scope)
}

func (s *memStore) LoadSelection(set string, p models.Purpose, scope models.Scope) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return s.data[s.key(set, p, scope)], nil
}

func (s *memStore) SaveSelection(set string, p models.Purpose, scope models.Scope, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, s.key(set, p, scope)+"="+name)
	if s.broken {
		return errors.New("disk full")
	}
	s.data[s.key(set, p, scope)] = name
	return nil
}

// Selections survive a restart through the store: read once when first
// needed, written on each change.
func TestSelectionStore(t *testing.T) {
	st := &memStore{data: map[string]string{"alice|chat|s1": "haiku", "alice|chat|s2": "gone"}}
	c := build(t, nil, three, models.WithSelectionStore(st))
	set := c.Set("alice")
	for _, n := range []string{"sonnet", "gpt", "haiku"} {
		if err := set.Assign(models.Chat, n, len(set.List(models.Chat))); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if got := set.Selected(models.Chat, "s1").Name; got != "haiku" {
			t.Fatalf("restored selection = %q", got)
		}
	}
	if got := set.Selected(models.Chat, "s2").Name; got != "sonnet" {
		t.Fatalf("a stored model not in the list = %q, want the default", got)
	}
	loads := st.loads
	set.Selected(models.Chat, "s1")
	if st.loads != loads {
		t.Fatal("the store was read again")
	}
	if err := set.Select(models.Chat, "gpt", "s1"); err != nil {
		t.Fatal(err)
	}
	set.Reset(models.Chat, "s1")
	if want := []string{"alice|chat|s1=gpt", "alice|chat|s1="}; !slices.Equal(st.saves, want) {
		t.Fatalf("saves = %q, want %q", st.saves, want)
	}

	st.broken = true
	if err := set.Select(models.Chat, "haiku", "s3"); err != nil {
		t.Fatalf("a store error failed the selection: %v", err)
	}
	if got := set.Selected(models.Chat, "s3").Name; got != "haiku" {
		t.Fatalf("the selection was not kept in memory: %q", got)
	}
}

// Current says what is serving and why it is not the selected model.
func TestCurrent(t *testing.T) {
	f := newFixture(t, three)
	if cur := f.cat.Current(models.Chat, "s"); cur.Selected != "sonnet" || cur.Serving != "" {
		t.Fatalf("before any call: %+v", cur)
	}
	f.park(t, "sonnet", 402)
	f.clock.Advance(time.Second)
	if _, err := f.cat.Begin(models.Chat, "s").Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	cur := f.cat.Current(models.Chat, "s")
	if cur.Selected != "sonnet" || cur.Serving != "gpt" || cur.Reason != string(spawnllm.FailoverBilling) ||
		!cur.Since.Equal(epoch.Add(time.Second)) || len(cur.Attempts) != 1 {
		t.Fatalf("after a fallback: %+v", cur)
	}
	f.clock.Advance(time.Minute)
	if _, err := f.cat.Begin(models.Chat, "s").Chat(t.Context(), alice, models.Request{}); err != nil {
		t.Fatal(err)
	}
	if cur := f.cat.Current(models.Chat, "s"); !cur.Since.Equal(epoch.Add(time.Second)) {
		t.Fatalf("since moved while the same model served: %+v", cur)
	}
	if cur := f.cat.Current(models.Chat); cur.Serving != "" {
		t.Fatalf("another scope saw it: %+v", cur)
	}
}

func TestRemoveRevertsSelection(t *testing.T) {
	f := newFixture(t, three)
	if err := f.cat.Select(models.Chat, "gpt", "s"); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.Remove("gpt"); err != nil {
		t.Fatal(err)
	}
	if got := f.cat.Selected(models.Chat, "s").Name; got != "sonnet" {
		t.Fatalf("selection of a removed model = %q", got)
	}
	if err := f.cat.Add(modelspec.Model{ModelName: "gpt", Model: "gpt-5", Provider: "openai"}, models.For(models.Chat, 1)); err != nil {
		t.Fatal(err)
	}
	if got := f.cat.Selected(models.Chat, "s").Name; got != "sonnet" {
		t.Fatalf("the selection came back with the model: %q", got)
	}
	if strings.Join(names(f.cat.List(models.Chat)), ",") != "sonnet,gpt,haiku" {
		t.Fatalf("list = %q", names(f.cat.List(models.Chat)))
	}
}
