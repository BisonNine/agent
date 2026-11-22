package toolkit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// allowNames grants exactly the names it holds.
type allowNames map[string]bool

func (a allowNames) IsToolAllowed(name string) bool { return a[name] }

func grant(names ...string) allowNames {
	a := allowNames{}
	for _, n := range names {
		a[n] = true
	}
	return a
}

// Both a catalogue and a set are what an agent is given.
var (
	_ Registry = (*Catalog)(nil)
	_ Registry = (*Set)(nil)
)

func TestSet_GrantsNothingUntilGranted(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("lookup_order", "refund")...)
	alice := c.Set("alice")
	if c.Set("alice") != alice {
		t.Fatal("Set returns the same set for the same id")
	}
	if len(alice.Snapshot().Names()) != 0 || len(alice.List()) != 0 {
		t.Fatal("a new named set grants nothing")
	}
	alice.Grant(grant("lookup_order"))
	if got := alice.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order"}) {
		t.Fatalf("alice = %v", got)
	}
	alice.Grant(nil)
	if got := alice.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order", "refund"}) {
		t.Fatalf("alice with every tool = %v", got)
	}
	if c.Set("") != c.Default() || len(c.Default().Snapshot().Names()) != 2 {
		t.Fatal("the empty id is the default set, which grants every tool")
	}
}

// A call to a tool the set is not granted is an expected refusal, with the
// text models are told to read as configuration.
func TestSet_UngrantedCallIsRefusal(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("lookup_order", "refund")...)
	bob := c.View(grant("lookup_order"))
	res := bob.Execute(context.Background(), "refund", nil, nil)
	if !res.IsError || res.ForLLM != NotEnabledMessage("refund") || !IsExpectedRefusal(res.Err) {
		t.Fatalf("ungranted call = %+v", res)
	}
	if res := bob.Execute(context.Background(), "lookup_order", nil, nil); res.IsError {
		t.Fatalf("granted call = %+v", res)
	}
	if _, ok := bob.Get("refund"); ok {
		t.Fatal("Get must not return an ungranted tool")
	}
	if _, err := bob.ExecuteUnattended(context.Background(), "refund", nil); !errors.Is(err, ErrToolNotRegistered) {
		t.Fatalf("unattended ungranted = %v", err)
	}
}

// A call by an ExternalName is granted on the internal name.
func TestSet_ExternalNameGrantedOnInternalName(t *testing.T) {
	c := New()
	mustAdd(t, c, Entry{Tool: &mockExternalTool{
		mockTool: *newMockTool("mcp_playwright_browser_navigate", "navigate"),
		external: "playwright_browser_navigate",
	}})
	allowed := c.View(grant("mcp_playwright_browser_navigate"))
	if res := allowed.Execute(context.Background(), "playwright_browser_navigate", nil, nil); res.IsError {
		t.Fatalf("bare-name call should pass the grant, got %s", res.ForLLM)
	}
	denied := c.View(grant())
	if res := denied.Execute(context.Background(), "playwright_browser_navigate", nil, nil); !res.IsError || !IsExpectedRefusal(res.Err) {
		t.Fatalf("a call whose internal name is not granted = %+v", res)
	}
}

// One catalogue behind several agents with different views: a change
// reaches each at its next snapshot, and each subscriber hears only about
// the tools its set offers.
func TestSharedCatalogDifferentViews(t *testing.T) {
	c := New()
	alice := c.Set("alice")
	alice.Grant(grant("lookup_order", "refund", "crm_find"))
	bob := c.View(grant("lookup_order"))
	var aliceRec, bobRec recorder
	defer alice.Subscribe(aliceRec.add)()
	defer bob.Subscribe(bobRec.add)()

	mustAdd(t, c, listed("lookup_order", "refund", "weather")...)
	if got := alice.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order", "refund"}) {
		t.Fatalf("alice = %v", got)
	}
	if got := bob.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order"}) {
		t.Fatalf("bob = %v", got)
	}

	c.Remove("refund")  // alice only
	c.Remove("weather") // nobody
	settle(t, c)
	if aliceRec.count() != 2 || bobRec.count() != 1 {
		t.Fatalf("notices: alice %d, bob %d; want 2 and 1", aliceRec.count(), bobRec.count())
	}
	if got := aliceRec.all()[1].Removed; !slices.Equal(got, []string{"refund"}) {
		t.Fatalf("alice removal = %v", got)
	}

	// A change of grant is a change for that set only.
	bob.Grant(grant("lookup_order", "crm_find"))
	mustAdd(t, c, listed("crm_find")...)
	settle(t, c)
	if bobRec.count() != 2 || aliceRec.count() != 3 {
		t.Fatalf("notices: alice %d, bob %d; want 3 and 2", aliceRec.count(), bobRec.count())
	}
	v := bob.Version()
	bob.Grant(grant("lookup_order"))
	if bob.Version() <= v {
		t.Fatal("a grant change moves the set's version")
	}
	settle(t, c)
	if got := bobRec.all()[2].Removed; !slices.Equal(got, []string{"crm_find"}) {
		t.Fatalf("bob regrant = %+v", bobRec.all()[2])
	}
}

// buildNext builds a replacement catalogue as a host does on a reload.
func buildNext(t *testing.T) *Catalog {
	t.Helper()
	next := New()
	mustAdd(t, next, listed("lookup_order", "refund_v2")...)
	if _, err := next.AddToolset(context.Background(), newToolset("crm", hiddenEntry("crm_find", "crm", true))); err != nil {
		t.Fatal(err)
	}
	next.Set("alice").Grant(grant("lookup_order", "refund_v2", "crm_find"))
	return next
}

func TestAdopt(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("lookup_order", "refund")...)
	alice := c.Set("alice")
	alice.Grant(grant("lookup_order", "refund"))
	bob := c.Set("bob")
	bob.Grant(grant("lookup_order"))
	view := c.View(grant("refund_v2"))
	removeOld, err := c.AddToolset(context.Background(), newToolset("crm", listed("crm_old")...))
	if err != nil {
		t.Fatal(err)
	}

	var catRec, aliceRec, bobRec recorder
	defer c.Subscribe(catRec.add)()
	defer alice.Subscribe(aliceRec.add)()
	defer bob.Subscribe(bobRec.add)()
	before := alice.Version()

	ch := c.Adopt(buildNext(t))

	if !slices.Equal(ch.Added, []string{"crm_find", "refund_v2"}) ||
		!slices.Equal(ch.Removed, []string{"crm_old", "refund"}) ||
		!slices.Equal(ch.Changed, []string{"lookup_order"}) {
		t.Fatalf("Adopt change = %+v", ch)
	}
	// The handles are the same objects and show the new contents.
	if c.Set("alice") != alice {
		t.Fatal("the agent keeps its handle")
	}
	if got := alice.Snapshot().Names(); !slices.Equal(got, []string{"crm_find", "lookup_order", "refund_v2"}) {
		t.Fatalf("alice after adopt = %v", got)
	}
	if got := infoNames(alice.Hidden()); !slices.Equal(got, []string{"crm_find"}) {
		t.Fatalf("alice hidden = %v", got)
	}
	// bob is not in next: he keeps his own grant.
	if got := bob.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order"}) {
		t.Fatalf("bob after adopt = %v", got)
	}
	// A view keeps its grant and sees the new tools.
	if got := view.Snapshot().Names(); !slices.Equal(got, []string{"refund_v2"}) {
		t.Fatalf("view after adopt = %v", got)
	}
	// One notice per subscriber for the whole swap.
	settle(t, c)
	if catRec.count() != 1 || aliceRec.count() != 1 || bobRec.count() != 1 {
		t.Fatalf("notices: catalogue %d, alice %d, bob %d; want 1 each", catRec.count(), aliceRec.count(), bobRec.count())
	}
	if got := bobRec.all()[0]; !slices.Equal(got.Changed, []string{"lookup_order"}) || len(got.Added)+len(got.Removed) != 0 {
		t.Fatalf("bob notice = %+v", got)
	}
	if alice.Version() <= before {
		t.Fatal("the version keeps rising across Adopt")
	}
	// A remove function from before the swap no longer removes anything.
	removeOld()
	if _, ok := c.Get("crm_find"); !ok {
		t.Fatal("a stale remove function removed an adopted toolset")
	}
	// Adopting itself or nil changes nothing.
	v := c.Version()
	if ch := c.Adopt(c); !ch.Empty() || c.Version() != v {
		t.Fatal("Adopt(c) must change nothing")
	}
	if ch := c.Adopt(nil); !ch.Empty() || c.Version() != v {
		t.Fatal("Adopt(nil) must change nothing")
	}
}

// Later changes to the adopted catalogue do not reach the adopter.
func TestAdopt_NextIsNotConsumed(t *testing.T) {
	c := New()
	next := buildNext(t)
	c.Adopt(next)
	mustAdd(t, next, listed("only_in_next")...)
	if _, ok := c.Get("only_in_next"); ok {
		t.Fatal("a change to next reached the adopter")
	}
	mustAdd(t, c, listed("only_in_c")...)
	if _, ok := next.Get("only_in_c"); ok {
		t.Fatal("a change to the adopter reached next")
	}
}

// A call already running finishes on the old tool; the next call runs the
// new one.
func TestAdopt_RunningCallFinishesOnOldTool(t *testing.T) {
	c := New()
	old := &blockingTool{mockTool: *newMockTool("slow", "old"), started: make(chan struct{}), release: make(chan struct{})}
	mustAdd(t, c, Entry{Tool: old})
	done := make(chan *Result)
	go func() { done <- c.Snapshot().Execute(context.Background(), "slow", nil, nil) }()
	<-old.started

	next := New()
	newer := newMockTool("slow", "new")
	newer.result = SilentResult("new")
	mustAdd(t, next, Entry{Tool: newer})
	c.Adopt(next)

	close(old.release)
	if res := <-done; res.ForLLM != "finished on old" {
		t.Fatalf("running call = %+v", res)
	}
	if res := c.Execute(context.Background(), "slow", nil, nil); res.ForLLM != "new" {
		t.Fatalf("next call = %+v", res)
	}
}

// Adopt, grants and calls from many goroutines: run under -race.
func TestAdopt_Concurrent(t *testing.T) {
	c := New()
	alice := c.Set("alice")
	alice.Grant(nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	defer alice.Subscribe(func(Change) { _ = alice.Version() })()
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			next := New()
			if err := next.AddEntries(listed("stable", fmt.Sprintf("t%d", i%3))...); err != nil {
				t.Error(err)
				return
			}
			next.Set("alice").Grant(nil)
			c.Adopt(next)
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			alice.Grant(nil)
		}
	})
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := alice.Snapshot()
				for _, name := range snap.Names() {
					if res := snap.Execute(context.Background(), name, nil, nil); res == nil {
						t.Error("nil result")
					}
				}
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
