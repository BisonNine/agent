package toolkit

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// A disabled tool is not offered, is left out of what discovery sees, and
// a call to it is an ordinary tool error; it stays registered.
func TestDisable_Catalogue(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("lookup_order", "refund")...)
	mustAdd(t, c, hiddenEntry("crm_find", "crm", false))
	snap := c.Snapshot()

	if err := c.Disable("refund", "crm_find"); err != nil {
		t.Fatal(err)
	}
	if c.Enabled("refund") || !c.Enabled("lookup_order") || c.Enabled("missing") {
		t.Fatal("Enabled does not reflect the flags")
	}
	if got := c.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order"}) {
		t.Fatalf("offered = %v", got)
	}
	if len(c.Hidden()) != 0 {
		t.Fatal("a disabled hidden tool must be left out of the discovery corpus")
	}
	// Still registered.
	if got := c.Names(); len(got) != 3 {
		t.Fatalf("Names = %v", got)
	}
	info, ok := c.Get("refund")
	if !ok || info.Enabled {
		t.Fatalf("Get(refund) = %+v, %v; want registered and not enabled", info, ok)
	}
	if got := len(c.List()); got != 3 {
		t.Fatalf("List has %d tools, want 3", got)
	}
	// A call, from an old snapshot or the current contents.
	for _, res := range []*Result{
		snap.Execute(context.Background(), "refund", nil, nil),
		c.Execute(context.Background(), "refund", nil, nil),
	} {
		if !res.IsError || res.ForLLM != `tool "refund" is disabled` ||
			!errors.Is(res.Err, ErrToolDisabled) || !IsExpectedRefusal(res.Err) {
			t.Fatalf("disabled call = %+v", res)
		}
	}
	if _, err := c.ExecuteUnattended(context.Background(), "refund", nil); err == nil {
		t.Fatal("an unattended call to a disabled tool must fail")
	}

	if err := c.Enable("refund", "crm_find"); err != nil {
		t.Fatal(err)
	}
	if res := c.Execute(context.Background(), "refund", nil, nil); res.IsError {
		t.Fatalf("enabled again = %+v", res)
	}
	if len(c.Hidden()) != 1 {
		t.Fatal("an enabled hidden tool is offered again")
	}
}

func TestDisable_RefusesUnknownName(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a")...)
	v := c.Version()
	if err := c.Disable("a", "typo"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Disable = %v, want ErrToolNotFound", err)
	}
	if !c.Enabled("a") || c.Version() != v {
		t.Fatal("a refused Disable changed something")
	}
	set := c.Set("alice")
	if err := set.Disable("typo"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("Set.Disable = %v, want ErrToolNotFound", err)
	}
	// Enable of an unknown name is not an error.
	if err := c.Enable("typo"); err != nil {
		t.Fatal(err)
	}
	if err := set.Enable("typo"); err != nil {
		t.Fatal(err)
	}
}

// Both levels must be on: the catalogue's flag is for everyone, a set's
// for that agent.
func TestDisable_TwoLevels(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("lookup_order", "refund")...)
	alice := c.View(nil)
	bob := c.View(nil)

	if err := alice.Disable("refund"); err != nil {
		t.Fatal(err)
	}
	if alice.Enabled("refund") || !bob.Enabled("refund") || !c.Enabled("refund") {
		t.Fatal("a set's flag is for that set only")
	}
	if got := alice.Snapshot().Names(); !slices.Equal(got, []string{"lookup_order"}) {
		t.Fatalf("alice = %v", got)
	}
	if res := alice.Execute(context.Background(), "refund", nil, nil); res.ForLLM != `tool "refund" is disabled` {
		t.Fatalf("alice call = %+v", res)
	}
	if res := bob.Execute(context.Background(), "refund", nil, nil); res.IsError {
		t.Fatalf("bob call = %+v", res)
	}

	if err := c.Disable("lookup_order"); err != nil {
		t.Fatal(err)
	}
	if len(alice.Snapshot().Names()) != 0 || !slices.Equal(bob.Snapshot().Names(), []string{"refund"}) {
		t.Fatal("the catalogue's flag is for everyone")
	}
	// Enabling in the set does not override the catalogue.
	if err := alice.Enable("lookup_order", "refund"); err != nil {
		t.Fatal(err)
	}
	if alice.Enabled("lookup_order") || !alice.Enabled("refund") {
		t.Fatal("a tool is usable only when on at both levels")
	}
	if info, _ := alice.Get("lookup_order"); info.Enabled {
		t.Fatal("Info.Enabled reports both levels")
	}
}

func TestDisableGroup(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("mcp_alice_search", "mcp_alice_fetch", "mcp_bob_search", "files_index")...)
	if _, err := c.AddToolset(context.Background(), newToolset("crm", listed("crm_find", "crm_note")...)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddProvider("file", specProvider(fakeSpecDefs()...), toolSpecDeps()); err != nil {
		t.Fatal(err)
	}

	// By toolset.
	if n := c.DisableGroup("crm"); n != 2 {
		t.Fatalf("DisableGroup(crm) = %d, want 2", n)
	}
	// By provider namespace: its tools only, not every name starting "file".
	if n := c.DisableGroup("file"); n != 2 || !c.Enabled("files_index") || c.Enabled("file_read") {
		t.Fatalf("DisableGroup(file) = %d; files_index enabled %v", n, c.Enabled("files_index"))
	}
	// By prefix.
	alice := c.View(nil)
	if n := alice.DisableGroup("mcp_alice_"); n != 2 {
		t.Fatalf("Set.DisableGroup = %d, want 2", n)
	}
	want := []string{"files_index", "mcp_bob_search"}
	if got := alice.Snapshot().Names(); !slices.Equal(got, want) {
		t.Fatalf("alice = %v, want %v", got, want)
	}
	if n := alice.EnableGroup("mcp_alice_"); n != 2 {
		t.Fatalf("EnableGroup = %d", n)
	}
	if n := c.EnableGroup("crm"); n != 2 || !c.Enabled("crm_find") {
		t.Fatal("EnableGroup switches the toolset back on")
	}
	if n := c.DisableGroup("nothing_"); n != 0 {
		t.Fatalf("an empty group = %d", n)
	}
}

// Switching on and off is a change: one notice each, none for a no-op.
func TestDisable_Notices(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a", "b")...)
	alice := c.View(nil)
	var catRec, aliceRec recorder
	defer c.Subscribe(catRec.add)()
	defer alice.Subscribe(aliceRec.add)()

	v := alice.Version()
	if err := c.Disable("a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Disable("a"); err != nil { // no-op
		t.Fatal(err)
	}
	if err := alice.Disable("b"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Disable("b"); err != nil { // no-op
		t.Fatal(err)
	}
	if alice.Version() <= v {
		t.Fatal("switching moves the version")
	}
	if err := c.Enable("a"); err != nil {
		t.Fatal(err)
	}

	settle(t, c)
	cat := catRec.all()
	if len(cat) != 2 || !slices.Equal(cat[0].Removed, []string{"a"}) || !slices.Equal(cat[1].Added, []string{"a"}) {
		t.Fatalf("catalogue notices = %+v", cat)
	}
	got := aliceRec.all()
	if len(got) != 3 || !slices.Equal(got[1].Removed, []string{"b"}) {
		t.Fatalf("alice notices = %+v", got)
	}
}

// A flag belongs to the name: it survives a refresh of the tool.
func TestDisable_SurvivesReplacement(t *testing.T) {
	c := New()
	ts := newToolset("crm", listed("crm_find")...)
	if _, err := c.AddToolset(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	alice := c.View(nil)
	if err := c.Disable("crm_find"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddToolset(context.Background(), ts.with(listed("crm_find")...)); err != nil {
		t.Fatal(err)
	}
	c.ReplaceEntries("crm_", listed("crm_find"))
	if c.Enabled("crm_find") || alice.Enabled("crm_find") {
		t.Fatal("a refreshed tool must stay disabled")
	}
}

// Adopt takes the flags next was built with, at both levels.
func TestDisable_AcrossAdopt(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a", "b")...)
	if err := c.Disable("a"); err != nil {
		t.Fatal(err)
	}
	c.Set("alice").Grant(nil)

	next := New()
	mustAdd(t, next, listed("a", "b")...)
	if err := next.Disable("b"); err != nil {
		t.Fatal(err)
	}
	na := next.Set("alice")
	na.Grant(nil)
	if err := na.Disable("a"); err != nil {
		t.Fatal(err)
	}
	c.Adopt(next)

	if !c.Enabled("a") || c.Enabled("b") {
		t.Fatal("the catalogue takes next's flags")
	}
	alice := c.Set("alice")
	if alice.Enabled("a") || alice.Enabled("b") {
		t.Fatal("alice's set takes next's flags as well as the catalogue's")
	}
}

// Switching from many goroutines while calls run: run under -race.
func TestDisable_Concurrent(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a", "b", "c")...)
	alice := c.View(nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for _, toggle := range []func() error{
		func() error { return c.Disable("a") },
		func() error { return c.Enable("a") },
		func() error { return alice.Disable("b") },
		func() error { return alice.Enable("b") },
		func() error { c.DisableGroup("c"); c.EnableGroup("c"); return nil },
	} {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := toggle(); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := alice.Snapshot()
			for _, name := range []string{"a", "b", "c"} {
				if res := snap.Execute(context.Background(), name, nil, nil); res.IsError &&
					!errors.Is(res.Err, ErrToolDisabled) {
					t.Errorf("%s: %s", name, res.ForLLM)
				}
			}
			_ = alice.List()
		}
	})
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
