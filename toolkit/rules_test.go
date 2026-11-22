package toolkit

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bison9.com/agent/toolspec"
)

// recorder collects change notices.
type recorder struct {
	mu      sync.Mutex
	changes []Change
}

func (r *recorder) add(ch Change) {
	r.mu.Lock()
	r.changes = append(r.changes, ch)
	r.mu.Unlock()
}

func (r *recorder) all() []Change {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.changes)
}

func (r *recorder) count() int { return len(r.all()) }

// fakeToolset is a toolset whose contents the test sets.
type fakeToolset struct {
	name    string
	entries []Entry
	err     error
}

func (f *fakeToolset) Name() string                           { return f.name }
func (f *fakeToolset) Tools(context.Context) ([]Entry, error) { return f.entries, f.err }
func (f *fakeToolset) with(entries ...Entry) *fakeToolset     { f.entries = entries; return f }
func hiddenEntry(name, group string, together bool) Entry {
	return Entry{Tool: newMockTool(name, name), Hidden: true, Group: group, RevealTogether: together}
}

func newToolset(name string, entries ...Entry) *fakeToolset {
	return &fakeToolset{name: name, entries: entries}
}
func specProvider(defs ...toolspec.ToolDefinition) providerFunc { return providerFunc(defs) }

type providerFunc []toolspec.ToolDefinition

func (p providerFunc) RegisterTools(toolspec.Deps) []toolspec.ToolDefinition { return p }

// --- toolsets and providers ---

func TestAddToolset(t *testing.T) {
	c := New()
	var rec recorder
	defer c.Subscribe(rec.add)()

	crm := newToolset("crm", hiddenEntry("crm_find", "crm", true), hiddenEntry("crm_note", "crm", true))
	remove, err := c.AddToolset(context.Background(), crm)
	if err != nil {
		t.Fatal(err)
	}
	if got := infoNames(c.Hidden()); !slices.Equal(got, []string{"crm_find", "crm_note"}) {
		t.Fatalf("hidden = %v", got)
	}
	if info, _ := c.Get("crm_find"); info.Source != "crm" || info.Group != "crm" || !info.RevealTogether {
		t.Fatalf("crm_find = %+v", info)
	}

	// Adding it again swaps its tools in one step, with one notice.
	crm.with(hiddenEntry("crm_find", "crm", true), hiddenEntry("crm_call", "crm", true))
	remove2, err := c.AddToolset(context.Background(), crm)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Names(); !slices.Equal(got, []string{"crm_call", "crm_find"}) {
		t.Fatalf("after re-add = %v", got)
	}
	settle(t, c)
	changes := rec.all()
	if len(changes) != 2 {
		t.Fatalf("notices = %+v, want one per AddToolset", changes)
	}
	want := Change{Added: []string{"crm_call"}, Removed: []string{"crm_note"}, Changed: []string{"crm_find"}}
	if !slices.Equal(changes[1].Added, want.Added) || !slices.Equal(changes[1].Removed, want.Removed) ||
		!slices.Equal(changes[1].Changed, want.Changed) {
		t.Fatalf("re-add notice = %+v, want %+v", changes[1], want)
	}

	// The first remove func is stale; the current one unmounts.
	remove()
	if len(c.Names()) != 2 {
		t.Fatal("a stale remove func must do nothing")
	}
	remove2()
	remove2()
	if len(c.Names()) != 0 {
		t.Fatalf("after remove = %v", c.Names())
	}
	settle(t, c)
	if rec.count() != 3 {
		t.Fatalf("notices = %d, want 3", rec.count())
	}
}

func TestAddToolset_Refusals(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("crm_find")...)
	v := c.Version()
	if _, err := c.AddToolset(context.Background(), newToolset("crm", hiddenEntry("crm_find", "", false))); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("clash = %v, want ErrDuplicate", err)
	}
	boom := errors.New("server down")
	if _, err := c.AddToolset(context.Background(), &fakeToolset{name: "crm", err: boom}); !errors.Is(err, boom) {
		t.Fatalf("Tools error = %v", err)
	}
	if _, err := c.AddToolset(context.Background(), newToolset("")); err == nil {
		t.Fatal("a toolset with no name must be refused")
	}
	if _, err := c.AddToolset(context.Background(), newToolset("x", listed("x_a", "x_a")...)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a name twice = %v", err)
	}
	if c.Version() != v {
		t.Fatal("a refused toolset must change nothing")
	}
}

func TestAddProvider(t *testing.T) {
	c := New()
	remove, err := c.AddProvider("file", specProvider(fakeSpecDefs()...), toolspec.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Names(); !slices.Equal(got, []string{"file_read", "file_watch"}) {
		t.Fatalf("Names = %v", got)
	}
	info, _ := c.Get("file_read")
	if info.Source != "file" || info.Group != "files" {
		t.Fatalf("file_read = %+v", info)
	}
	ctx := WithSessionKey(context.Background(), "s1")
	if res := c.Execute(ctx, "file_read", map[string]any{"path": "/x"}, nil); res.IsError {
		t.Fatalf("file_read = %+v", res)
	}
	if _, err := c.AddProvider("", specProvider(), toolspec.Deps{}); err == nil {
		t.Fatal("a provider needs a namespace")
	}
	remove()
	if len(c.Names()) != 0 {
		t.Fatalf("after remove = %v", c.Names())
	}
}

// --- a change takes effect at the next snapshot ---

func TestSnapshot_IsFixed(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a", "b")...)
	mustAdd(t, c, hiddenEntry("h", "", false))
	snap := c.Snapshot()
	c.Remove("a")
	mustAdd(t, c, listed("z")...)

	if got := snap.Names(); !slices.Equal(got, []string{"a", "b", "h"}) {
		t.Fatalf("old snapshot changed: %v", got)
	}
	if got := infoNames(snap.Listed()); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("old snapshot listed = %v", got)
	}
	if got := infoNames(snap.Hidden()); !slices.Equal(got, []string{"h"}) {
		t.Fatalf("old snapshot hidden = %v", got)
	}
	next := c.Snapshot()
	if got := next.Names(); !slices.Equal(got, []string{"b", "h", "z"}) {
		t.Fatalf("next snapshot = %v", got)
	}
	if next.Version() == snap.Version() {
		t.Fatal("the version must move with the contents")
	}
	if _, ok := snap.Get("a"); !ok {
		t.Fatal("a snapshot keeps what it had")
	}
}

// A call to a tool removed since the snapshot is an ordinary tool error.
func TestSnapshot_RemovedToolIsAToolError(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("refund", "lookup_order")...)
	snap := c.Snapshot()
	c.Remove("refund")

	res := snap.Execute(context.Background(), "refund", nil, nil)
	if !res.IsError || res.ForLLM != `tool "refund" is no longer available` || !errors.Is(res.Err, ErrToolRemoved) {
		t.Fatalf("removed tool = %+v", res)
	}
	// One that was never there is "not found".
	if res := snap.Execute(context.Background(), "nope", nil, nil); res.ForLLM != `tool "nope" not found` {
		t.Fatalf("unknown tool = %+v", res)
	}
	// One added since the snapshot runs.
	mustAdd(t, c, listed("late")...)
	if res := snap.Execute(context.Background(), "late", nil, nil); res.IsError {
		t.Fatalf("a tool added since the snapshot = %+v", res)
	}
	// A replaced tool runs in its new form.
	newer := newMockTool("lookup_order", "v2")
	newer.result = SilentResult("v2")
	if err := c.Put(Entry{Tool: newer}); err != nil {
		t.Fatal(err)
	}
	if res := snap.Execute(context.Background(), "lookup_order", nil, nil); res.ForLLM != "v2" {
		t.Fatalf("replaced tool = %+v", res)
	}
}

// blockingTool runs until released, to hold a call in flight.
type blockingTool struct {
	mockTool

	started chan struct{}
	release chan struct{}
}

func (b *blockingTool) Execute(context.Context, map[string]any) *Result {
	close(b.started)
	<-b.release
	return SilentResult("finished on " + b.desc)
}

// A call already running finishes on the tool it started with.
func TestRunningCallFinishesOnOldTool(t *testing.T) {
	c := New()
	old := &blockingTool{mockTool: *newMockTool("slow", "old"), started: make(chan struct{}), release: make(chan struct{})}
	mustAdd(t, c, Entry{Tool: old})
	snap := c.Snapshot()
	done := make(chan *Result)
	go func() { done <- snap.Execute(context.Background(), "slow", nil, nil) }()
	<-old.started
	c.Remove("slow")
	mustAdd(t, c, Entry{Tool: newMockTool("slow", "new")})
	close(old.release)
	if res := <-done; res.ForLLM != "finished on old" {
		t.Fatalf("running call = %+v", res)
	}
}

// --- subscribers ---

func TestSubscribe_OnePerChangeNoneForNoOp(t *testing.T) {
	c := New()
	var rec recorder
	unsubscribe := c.Subscribe(rec.add)

	mustAdd(t, c, listed("a", "b")...)
	c.Remove("zzz") // no-op
	if err := c.AddEntries(listed("a")...); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate = %v", err)
	}
	c.ReplaceEntries("q_", nil) // no-op
	c.Replace("a", []toolspec.ToolDefinition{{Name: "a2"}})
	c.Remove("b")

	settle(t, c)
	changes := rec.all()
	if len(changes) != 3 {
		t.Fatalf("notices = %+v, want 3", changes)
	}
	if !slices.Equal(changes[0].Added, []string{"a", "b"}) {
		t.Fatalf("first = %+v", changes[0])
	}
	if !slices.Equal(changes[1].Added, []string{"a2"}) || !slices.Equal(changes[1].Removed, []string{"a"}) {
		t.Fatalf("replace = %+v", changes[1])
	}
	if !slices.Equal(changes[2].Removed, []string{"b"}) {
		t.Fatalf("remove = %+v", changes[2])
	}

	unsubscribe()
	unsubscribe()
	mustAdd(t, c, listed("c")...)
	settle(t, c)
	if rec.count() != 3 {
		t.Fatal("no notice after unsubscribe")
	}
}

// Redefining a tool under the same name is a change.
func TestSubscribe_RedefinitionIsChanged(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a")...)
	var rec recorder
	defer c.Subscribe(rec.add)()
	if err := c.Put(listed("a")...); err != nil {
		t.Fatal(err)
	}
	settle(t, c)
	changes := rec.all()
	if len(changes) != 1 || !slices.Equal(changes[0].Changed, []string{"a"}) || len(changes[0].Added) != 0 {
		t.Fatalf("notices = %+v", changes)
	}
}

// A subscriber may read and write the registry from its callback; the
// change it makes is delivered after it returns, in order.
func TestSubscribe_CallbackMayCallBack(t *testing.T) {
	c := New()
	var mu sync.Mutex
	var seen []string
	c.Subscribe(func(ch Change) {
		mu.Lock()
		seen = append(seen, fmt.Sprint(ch.Added, ch.Removed))
		mu.Unlock()
		_ = c.Snapshot().Names()
		_ = c.Version()
		if slices.Contains(ch.Added, "trigger") {
			c.Remove("trigger")
		}
	})
	done := make(chan struct{})
	go func() {
		mustAdd(t, c, listed("trigger")...)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: a subscriber writing back into the registry")
	}
	settle(t, c)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"[trigger] []", "[] [trigger]"}) {
		t.Fatalf("notices = %v", seen)
	}
}

// Notices are delivered with no registry lock held: a subscriber blocked
// in its callback does not block writers or readers.
func TestSubscribe_NoLockHeld(t *testing.T) {
	c := New()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c.Subscribe(func(Change) {
		once.Do(func() {
			close(entered)
			<-release
		})
	})
	go func() { mustAdd(t, c, listed("a")...) }()
	<-entered
	done := make(chan struct{})
	go func() {
		mustAdd(t, c, listed("b")...)
		_ = c.Snapshot()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a writer was blocked by a subscriber's callback")
	}
	close(release)
}

// --- concurrency ---

// Every operation from many goroutines while calls run: run under -race.
func TestConcurrentChangesDuringCalls(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("stable")...)
	var notices atomic.Int64
	defer c.Subscribe(func(Change) { notices.Add(1) })()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				name := fmt.Sprintf("w%d_%d", w, i%5)
				if err := c.AddEntries(listed(name)...); err != nil && !errors.Is(err, ErrDuplicate) {
					t.Error(err)
				}
				if err := c.Put(listed(name)...); err != nil {
					t.Error(err)
				}
				c.ReplaceEntries(fmt.Sprintf("w%d_", w), listed(name+"_r"))
				c.Remove(name + "_r")
				remove, err := c.AddToolset(context.Background(), newToolset(fmt.Sprintf("ts%d", w), listed(fmt.Sprintf("ts%d_x", w))...))
				if err != nil {
					t.Error(err)
					return
				}
				remove()
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := c.Snapshot()
				for _, name := range snap.Names() {
					res := snap.Execute(context.Background(), name, nil, nil)
					if res == nil {
						t.Error("nil result")
						return
					}
				}
				if res := snap.Execute(context.Background(), "stable", nil, nil); res.IsError {
					t.Errorf("stable tool failed: %s", res.ForLLM)
					return
				}
				_ = c.List()
				_ = c.Stats()
				_ = c.Version()
			}
		})
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if notices.Load() == 0 {
		t.Fatal("expected notices")
	}
}

// --- version and stats ---

func TestVersion_Monotonic(t *testing.T) {
	c := New()
	last := c.Version()
	step := func(what string) {
		t.Helper()
		if v := c.Version(); v <= last {
			t.Fatalf("%s: version %d not after %d", what, v, last)
		} else {
			last = v
		}
	}
	mustAdd(t, c, listed("a")...)
	step("add")
	if err := c.Put(listed("a")...); err != nil {
		t.Fatal(err)
	}
	step("put")
	c.Remove("a")
	step("remove")
}

func TestStats(t *testing.T) {
	c := New()
	bad := newMockTool("bad", "")
	bad.result = ErrorResult("boom")
	mustAdd(t, c, Entry{Tool: newMockTool("good", "")}, Entry{Tool: bad})
	for range 3 {
		c.Execute(context.Background(), "good", nil, nil)
	}
	c.Execute(context.Background(), "bad", nil, nil)
	c.Execute(context.Background(), "missing", nil, nil)
	want := []Stat{{Name: "bad", Calls: 1, Errors: 1}, {Name: "good", Calls: 3}}
	if got := c.Stats(); !slices.Equal(got, want) {
		t.Fatalf("Stats = %+v, want %+v", got, want)
	}
}

// Info is a copy: changing it changes nothing.
func TestInfoIsACopy(t *testing.T) {
	c := New()
	mustAdd(t, c, listed("a")...)
	list := c.List()
	list[0].Name = "changed"
	list[0].Hidden = true
	names := c.Snapshot().Names()
	names[0] = "changed"
	if info, _ := c.Get("a"); info.Name != "a" || info.Hidden || c.Snapshot().Names()[0] != "a" {
		t.Fatal("a returned copy altered the registry")
	}
}

func toolSpecDeps() toolspec.Deps { return toolspec.Deps{} }

// Notices from concurrent writers arrive in commit order: replaying them
// gives the catalogue's contents, and none adds a tool already added or
// removes one not there.
func TestSubscribe_OrderedUnderConcurrentWriters(t *testing.T) {
	c := New()
	var mu sync.Mutex
	seen := map[string]bool{}
	defer c.Subscribe(func(ch Change) {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range ch.Added {
			if seen[n] {
				t.Errorf("%s added twice", n)
			}
			seen[n] = true
		}
		for _, n := range ch.Removed {
			if !seen[n] {
				t.Errorf("%s removed before it was added", n)
			}
			delete(seen, n)
		}
	})()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				name := fmt.Sprintf("w%d_%d", w, i)
				if err := c.AddEntries(listed(name)...); err != nil {
					t.Error(err)
					return
				}
				if i%2 == 0 {
					c.Remove(name)
				}
			}
		})
	}
	wg.Wait()
	settle(t, c)
	mu.Lock()
	defer mu.Unlock()
	got := slices.Sorted(maps.Keys(seen))
	if !slices.Equal(got, c.Names()) {
		t.Fatalf("replayed notices give %d tools, catalogue has %d", len(got), len(c.Names()))
	}
}
