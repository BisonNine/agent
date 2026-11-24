package loop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// inTask is a context of session s1 running in task id ("" for none).
func inTask(id string) context.Context {
	return hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s1", Task: id})
}

// start starts a task through reg, failing the test on an error.
func start(t *testing.T, reg hook.TaskRegistrar, ctx context.Context, s hook.TaskStart) (context.Context, hook.TaskHandle) {
	t.Helper()
	tctx, h, err := reg.StartTask(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	return tctx, h
}

func ids(infos []hook.TaskInfo) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.ID)
	}
	return out
}

// observed records what the registry's observers are told.
type observed struct {
	mu  sync.Mutex
	all []hook.TaskInfo
}

func observe(r *Tasks) *observed {
	o := &observed{}
	r.Observe(func(info hook.TaskInfo) {
		o.mu.Lock()
		o.all = append(o.all, info)
		o.mu.Unlock()
	})
	return o
}

// of is what the observers were told of task id, as "state/mode" strings.
func (o *observed) of(id string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, i := range o.all {
		if i.ID == id {
			out = append(out, string(i.State)+"/"+string(i.Mode))
		}
	}
	return out
}

// Each owner's view lists and cancels its own tasks only: another owner's
// task id is unknown to it and left running. Owners are compared exactly.
// (Ported from the host's TestTaskRegistry_RunningByOwner and the owner
// part of TestCancelTask_RecordsOnDisk.)
func TestTasks_ForOwner(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice, bob := r.For("alice"), r.For("bob")
	_, a := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "t1", Owner: "carol"}})
	_, b := start(t, bob, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "t2"}})
	if got := alice.Tasks(); len(got) != 1 || got[0].ID != "t1" || got[0].Owner != "alice" {
		t.Fatalf("alice's tasks = %+v", got)
	}
	if got := r.For("Alice").Tasks(); len(got) != 0 {
		t.Fatalf("Alice's tasks = %+v, want none (owners compare exactly)", got)
	}
	if _, err := alice.CancelTask("t2", nil); !errors.Is(err, hook.ErrUnknownTask) {
		t.Fatalf("alice cancelling bob's task = %v, want ErrUnknownTask", err)
	}
	if b.Info().State != hook.TaskRunning {
		t.Fatal("alice's cancel reached bob's task")
	}
	if got := alice.CancelAll(nil); len(got.Cancelled) != 1 || got.Cancelled[0].ID != "t1" {
		t.Fatalf("alice's CancelAll = %+v", got)
	}
	if b.Info().State != hook.TaskRunning || a.Info().State != hook.TaskCancelled {
		t.Fatalf("after alice's CancelAll: alice %+v, bob %+v", a.Info(), b.Info())
	}
	b.Finish(nil)
	if n := len(alice.Tasks()) + len(bob.Tasks()); n != 0 {
		t.Fatalf("%d tasks left", n)
	}
}

// A cancel cascades to the tasks a task started, first, whatever owns
// them; an unrelated task is left. (Ported from the host's
// TestCancelTask_Cascades.)
func TestTasks_CancelCascadesAcrossOwners(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice, bob := r.For("alice"), r.For("bob")
	pctx, _ := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "p"}})
	cctx, _ := start(t, bob, pctx, hook.TaskStart{Info: hook.TaskInfo{ID: "c"}}) // started inside p
	octx, _ := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "other"}})
	got, err := alice.CancelTask("p", nil)
	if err != nil || !slices.Equal(ids(got.Cancelled), []string{"c", "p"}) {
		t.Fatalf("CancelTask = %+v, %v; want the child then the parent", got, err)
	}
	if pctx.Err() == nil || cctx.Err() == nil || octx.Err() != nil {
		t.Fatal("the cancel reached the wrong tasks")
	}
	if got := alice.Tasks(); len(got) != 1 || got[0].ID != "other" {
		t.Fatalf("alice's tasks = %+v", got)
	}
}

// CancelAll cancels every task of the owner, including one started under
// another owner's task, and nobody else's. (Ported from the host's
// TestCancelAllTasks.)
func TestTasks_CancelAllOwnerOnly(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice, bob := r.For("alice"), r.For("bob")
	start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "a1"}})
	start(t, alice, inTask("a1"), hook.TaskStart{Info: hook.TaskInfo{ID: "a2"}})
	bctx, _ := start(t, bob, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "b1"}})
	start(t, alice, bctx, hook.TaskStart{Info: hook.TaskInfo{ID: "a3"}}) // alice's, under bob's
	got := alice.CancelAll(nil)
	if !slices.Equal(ids(got.Cancelled), []string{"a2", "a1", "a3"}) {
		t.Fatalf("CancelAll = %v", ids(got.Cancelled))
	}
	if left := bob.Tasks(); len(left) != 1 || left[0].ID != "b1" || len(alice.Tasks()) != 0 {
		t.Fatalf("left: alice %+v, bob %+v", alice.Tasks(), left)
	}
}

// CancelChildren of an id that is not a registered task (a host's record a
// restart left unrelaunched) cancels the tasks started under it, whatever
// owns them, and remembers the id: a task started under it later is
// cancelled as it starts, with the cause given. (Ported from the host's
// TestCancelTask_LeftBehindCascades and TestSupervise_OrphanRelaunchIsCancelled.)
func TestTasks_CancelChildrenOfAnUnregisteredID(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice, bob := r.For("alice"), r.For("bob")
	cctx, _ := start(t, alice, inTask("left"), hook.TaskStart{Info: hook.TaskInfo{ID: "child"}})
	gctx, _ := start(t, bob, cctx, hook.TaskStart{Info: hook.TaskInfo{ID: "grandchild"}})
	octx, _ := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "other"}})
	cause := fmt.Errorf("cancelled by Bob: %w", hook.ErrInterrupted)
	got := r.CancelChildren("left", cause)
	if !slices.Equal(ids(got), []string{"grandchild", "child"}) || cctx.Err() == nil || gctx.Err() == nil || octx.Err() != nil {
		t.Fatalf("CancelChildren = %v", ids(got))
	}
	if _, err := alice.CancelTask("left", nil); !errors.Is(err, hook.ErrUnknownTask) {
		t.Fatalf("the parent id = %v, want unknown", err)
	}

	o := observe(r)
	settled := false
	lctx, h := start(t, alice, inTask(""), hook.TaskStart{
		Info:     hook.TaskInfo{ID: "relaunched", Parent: "left"},
		OnCancel: func(error) bool { settled = true; return true },
	})
	if lctx.Err() == nil || !settled || h.Info().State != hook.TaskCancelled || !errors.Is(h.Info().Cause, cause) {
		t.Fatalf("a task started under the cancelled id: ctx %v, OnCancel %v, %+v", lctx.Err(), settled, h.Info())
	}
	if got := o.of("relaunched"); !slices.Equal(got, []string{"running/callback", "cancelled/callback"}) {
		t.Fatalf("observed %v", got)
	}
	if r.CancelChildren("nobody", nil) != nil {
		t.Fatal("CancelChildren of an id with no children cancelled something")
	}
	if r.CancelChildren("", nil) != nil || len(alice.Tasks()) != 1 {
		t.Fatalf("CancelChildren of no id cancelled the root tasks: %+v", alice.Tasks())
	}
}

// The registry remembers the last keptStopped cancelled ids: a task started
// under an older one is not cancelled.
func TestTasks_RemembersTheLastStoppedIDs(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice := r.For("alice")
	for i := range keptStopped + 1 {
		r.CancelChildren(fmt.Sprintf("gone-%d", i), nil)
	}
	r.CancelChildren("gone-1", nil) // already remembered: not moved
	oldest, _ := start(t, alice, inTask("gone-0"), hook.TaskStart{})
	kept, _ := start(t, alice, inTask("gone-1"), hook.TaskStart{})
	if oldest.Err() != nil || kept.Err() == nil {
		t.Fatalf("under the forgotten id: %v; under a kept one: %v", oldest.Err(), kept.Err())
	}
}

// A task started under a task of a cascade while the cascade runs (here
// while its sibling's OnCancel runs) is cancelled too. (Ported from the
// host's TestTaskRegistry_NothingEscapesACascade.)
func TestTasks_NothingEscapesACascade(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice := r.For("alice")
	start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "r"}})
	var late hook.TaskHandle
	start(t, alice, inTask("r"), hook.TaskStart{Info: hook.TaskInfo{ID: "c"}, OnCancel: func(error) bool {
		// While the child is being cancelled, the root starts another.
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, h, err := alice.StartTask(inTask("r"), hook.TaskStart{Info: hook.TaskInfo{ID: "late"}})
			if err != nil {
				t.Error(err)
			}
			late = h
		}()
		<-done
		return true
	}})
	if _, err := alice.CancelTask("r", nil); err != nil {
		t.Fatal(err)
	}
	if late == nil || late.Info().State != hook.TaskCancelled {
		t.Fatal("a task started during the cascade escaped it")
	}
	if n := len(alice.Tasks()); n != 0 {
		t.Fatalf("%d tasks still running", n)
	}
}

// A cancel losing to the task's own end leaves the end to it: the last
// change the observers hear is its Finish. (Ported from the host's
// TestTaskRegistry_EndEventSurvivesALostCancel.)
func TestTasks_EndSurvivesALostCancel(t *testing.T) {
	r := NewTasks(newFake(), 0)
	o := observe(r)
	_, h := start(t, r.For("alice"), inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "t"}, OnCancel: func(error) bool { return false }})
	if h.Cancel(nil) {
		t.Fatal("an ended task reported as cancelled")
	}
	if !h.Finish(toolkit.NewResult("ok")) {
		t.Fatal("Finish after a lost cancel")
	}
	if got := o.of("t"); !slices.Equal(got, []string{"running/callback", "done/callback"}) {
		t.Fatalf("observed %v", got)
	}
}

// The observers hear of each start, mode change, cancel and finish, and, for
// a cancelled task, of its worker's exit; an exit of a task that finished,
// or a second exit, tells them nothing. A task started under a cancelled
// one is told cancelled twice: as it starts, and once its worker is gone.
// (Ported from the host's TestCancelTask_ChildStartedLateIsCancelled.)
func TestTasks_Observe(t *testing.T) {
	r := NewTasks(newFake(), 0)
	r.Observe(nil) // ignored
	o := observe(r)
	alice := r.For("alice")
	_, w := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "w", Mode: hook.TaskWait}})
	w.Detach(hook.ErrInterrupted)
	w.Cancel(nil)
	w.Exited()
	w.Exited()
	_, d := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "d"}})
	d.Finish(toolkit.ErrorResult("failed"))
	d.Exited()
	_, late := start(t, alice, inTask("w"), hook.TaskStart{Info: hook.TaskInfo{ID: "late"}})
	late.Exited()
	want := map[string][]string{
		"w":    {"running/wait", "running/callback", "cancelled/callback", "cancelled/callback"},
		"d":    {"running/callback", "error/callback"},
		"late": {"running/callback", "cancelled/callback", "cancelled/callback"},
	}
	for id, w := range want {
		if got := o.of(id); !slices.Equal(got, w) {
			t.Errorf("%s: observed %v, want %v", id, got, w)
		}
	}
}

// OnCancel and OnDetach run outside the registry's lock, and so may an
// observer, which may call the registry.
func TestTasks_HostStepsRunOutsideTheLock(t *testing.T) {
	r := NewTasks(newFake(), 0)
	alice := r.For("alice")
	free := func(where string) {
		if !r.mu.TryLock() {
			t.Errorf("%s runs under the registry's lock", where)
			return
		}
		r.mu.Unlock()
	}
	r.Observe(func(hook.TaskInfo) { free("an observer"); alice.Tasks() })
	_, h := start(t, alice, inTask(""), hook.TaskStart{
		Info:     hook.TaskInfo{Mode: hook.TaskWait},
		OnCancel: func(error) bool { free("OnCancel"); return true },
		OnDetach: func() bool { free("OnDetach"); return true },
	})
	h.Detach(hook.ErrInterrupted)
	h.Cancel(nil)
}

// A cancel waits, at most the grace in all, for the workers of the tasks it
// cancelled to exit, so what they write as they unwind is written when it
// returns; a worker still running then is sealed. (Ported from the host's
// TestCancelTask_RecordFinalWhenCancelReturns.)
func TestTasks_CancelWaitsForWorkers(t *testing.T) {
	const grace = time.Minute
	t.Run("worker exits during the grace", func(t *testing.T) {
		clk := newFake()
		r := NewTasks(clk, grace)
		tctx, h := start(t, r.For("alice"), inTask(""), hook.TaskStart{})
		release := make(chan struct{})
		var wrote bool
		go func() {
			<-tctx.Done()
			<-release
			if !h.Sealed() {
				wrote = true // its partial output
			}
			h.Exited()
		}()
		done := make(chan bool, 1)
		go func() { done <- h.Cancel(nil) }()
		<-tctx.Done()
		select {
		case <-done:
			t.Fatal("the cancel returned before the worker exited")
		default:
		}
		close(release)
		if !<-done || !wrote || h.Sealed() {
			t.Fatalf("wrote %v, sealed %v", wrote, h.Sealed())
		}
	})
	t.Run("workers outlive the grace", func(t *testing.T) {
		clk := newFake()
		r := NewTasks(clk, grace)
		o := observe(r)
		alice := r.For("alice")
		_, p := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "p"}})
		_, c := start(t, alice, inTask("p"), hook.TaskStart{Info: hook.TaskInfo{ID: "c"}})
		done := make(chan hook.TaskCancel, 1)
		go func() {
			got, err := alice.CancelTask("p", nil)
			if err != nil {
				t.Error(err)
			}
			done <- got
		}()
		if err := clk.BlockUntil(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(grace - time.Second)
		select {
		case <-done:
			t.Fatal("the cancel returned before the grace ran out")
		default:
		}
		clk.Advance(time.Second) // one grace for both workers
		if got := <-done; len(got.Cancelled) != 2 || !p.Sealed() || !c.Sealed() {
			t.Fatalf("CancelTask = %+v, sealed %v %v", got, p.Sealed(), c.Sealed())
		}
		p.Exited()
		if !p.Sealed() || !slices.Equal(o.of("p"), []string{"running/callback", "cancelled/callback", "cancelled/callback"}) {
			t.Fatalf("after a late exit: sealed %v, observed %v", p.Sealed(), o.of("p"))
		}
	})
	t.Run("no grace", func(t *testing.T) {
		r := NewTasks(newFake(), -time.Second)
		alice := r.For("alice")
		tctx, gone := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "gone"}})
		_, stuck := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "stuck"}})
		gone.Exited() // a worker that returned before the cancel
		if !gone.Cancel(nil) || !stuck.Cancel(nil) {
			t.Fatal("not cancelled")
		}
		if gone.Sealed() || !stuck.Sealed() || tctx.Err() == nil {
			t.Fatalf("sealed: exited worker %v, running worker %v", gone.Sealed(), stuck.Sealed())
		}
	})
	t.Run("a task not cancelled is not sealed", func(t *testing.T) {
		r := NewTasks(newFake(), 0)
		_, h := start(t, r.For("alice"), inTask(""), hook.TaskStart{})
		h.Finish(nil)
		h.Exited()
		if h.Sealed() || h.Cancel(nil) {
			t.Fatal("a finished task was sealed or cancelled")
		}
	})
	t.Run("every cancel waits", func(t *testing.T) {
		clk := newFake()
		r := NewTasks(clk, grace)
		alice := r.For("alice")
		// Each under a parent of its own: CancelChildren remembers its id.
		cancels := map[string]func(){
			"CancelAll":      func() { alice.CancelAll(nil) },
			"CancelChildren": func() { r.CancelChildren("CancelChildren", nil) },
		}
		for name, cancel := range cancels {
			_, h := start(t, alice, inTask(name), hook.TaskStart{})
			done := make(chan struct{})
			go func() { cancel(); close(done) }()
			if err := clk.BlockUntil(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			h.Exited()
			<-done
			if h.Sealed() {
				t.Fatalf("%s sealed a worker that exited", name)
			}
		}
	})
}

// A task started under a cancelled parent is cancelled as it starts without
// waiting: its worker has not started.
func TestTasks_CancelledAtStartDoesNotWait(t *testing.T) {
	r := NewTasks(newFake(), time.Hour)
	alice := r.For("alice")
	_, p := start(t, alice, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "p"}})
	p.Exited()
	p.Cancel(nil)
	cctx, c := start(t, alice, inTask("p"), hook.TaskStart{})
	if cctx.Err() == nil || c.Info().State != hook.TaskCancelled || c.Sealed() {
		t.Fatalf("child: %+v, sealed %v", c.Info(), c.Sealed())
	}
}

// Shutdown ends every task's context with a cause wrapping ErrShutdown,
// without cancelling it or telling the observers; nothing starts after it,
// and Detach then cancels.
func TestTasks_Shutdown(t *testing.T) {
	plain := errors.New("Bob pulled the plug")
	tests := []struct {
		name  string
		cause error
		is    error
	}{
		{"nil cause", nil, hook.ErrShutdown},
		{"a cause wrapping ErrShutdown", errClosing, errClosing},
		{"another cause is wrapped", plain, plain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewTasks(newFake(), 0)
			o := observe(r)
			alice := r.For("alice")
			cancelled := false
			tctx, h := start(t, alice, inTask(""), hook.TaskStart{
				Info:     hook.TaskInfo{ID: "w", Mode: hook.TaskWait},
				OnCancel: func(error) bool { cancelled = true; return true },
			})
			r.Shutdown(tt.cause)
			cause := context.Cause(tctx)
			if !errors.Is(cause, hook.ErrShutdown) || !errors.Is(cause, tt.is) || cancelled || h.Info().State != hook.TaskRunning {
				t.Fatalf("cause %v, OnCancel %v, %+v", cause, cancelled, h.Info())
			}
			if got := o.of("w"); len(got) != 1 {
				t.Fatalf("observed %v, want the start only", got)
			}
			if _, _, err := alice.StartTask(inTask(""), hook.TaskStart{}); !errors.Is(err, hook.ErrShutdown) {
				t.Fatalf("StartTask after Shutdown = %v", err)
			}
			if h.Detach(hook.ErrInterrupted) || !cancelled {
				t.Fatal("Detach after Shutdown did not cancel")
			}
		})
	}
}

// sharedFixture is an agent fixture whose agents share one task registry,
// with a "spawn" tool that starts a task and keeps its context and handle.
type sharedFixture struct {
	*agentFixture

	tasks *Tasks

	mu      sync.Mutex
	ctxs    []context.Context
	handles []hook.TaskHandle
	regs    []hook.TaskRegistrar
}

func newSharedFixture(t *testing.T) *sharedFixture {
	t.Helper()
	f := &sharedFixture{agentFixture: newAgentFixture(t), tasks: NewTasks(newFake(), 0)}
	f.tool(t, "spawn", func(ctx context.Context, _ map[string]any) (string, error) {
		reg, ok := hook.TaskRegistrarFrom(ctx)
		if !ok {
			return "", errors.New("no registrar")
		}
		tctx, h, err := reg.StartTask(ctx, hook.TaskStart{Info: hook.TaskInfo{Kind: "subagent", Label: "Bob's research"}})
		if err != nil {
			return "", err
		}
		f.mu.Lock()
		f.ctxs, f.handles, f.regs = append(f.ctxs, tctx), append(f.handles, h), append(f.regs, reg)
		f.mu.Unlock()
		return "started", nil
	})
	return f
}

// agent builds an agent on the shared registry.
func (f *sharedFixture) agent(t *testing.T, edit func(*Spec)) *Agent {
	t.Helper()
	return f.agentFixture.agent(t, func(s *Spec) {
		s.Tasks = f.tasks
		if edit != nil {
			edit(s)
		}
	})
}

func (f *sharedFixture) task(i int) (context.Context, hook.TaskHandle, hook.TaskRegistrar) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxs[i], f.handles[i], f.regs[i]
}

// A shared registry outlives the agent instance that started a task: its
// Close and a shutdown Cancel leave the task running, and another instance
// of the agent (a reload) lists and cancels it. Only the host's Shutdown
// ends it.
func TestAgent_SharedTasksOutliveTheInstance(t *testing.T) {
	f := newSharedFixture(t)
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"), calls("spawn"), modelstest.Reply("done"))
	old := f.agent(t, nil)
	old.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	tctx, h, _ := f.task(0)
	old.Cancel(fmt.Errorf("reload: %w", hook.ErrShutdown))
	if err := old.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tctx.Err() != nil || h.Info().State != hook.TaskRunning || h.Info().Owner != "Alice" {
		t.Fatalf("the instance's Close ended its task: %v, %+v", tctx.Err(), h.Info())
	}

	reloaded := f.agent(t, nil)
	if got := reloaded.Tasks(); len(got) != 1 || got[0].ID != h.ID() {
		t.Fatalf("the reloaded instance's tasks = %+v", got)
	}
	reloaded.Run(context.Background(), "s1", hook.UserText("again")).Wait()
	if got := f.tasks.For("Alice").Tasks(); len(got) != 2 {
		t.Fatalf("the registry's tasks = %+v", got)
	}
	got, err := reloaded.CancelTask(h.ID(), nil)
	if err != nil || len(got.Cancelled) != 1 || tctx.Err() == nil {
		t.Fatalf("CancelTask from the reloaded instance = %+v, %v", got, err)
	}
	t2, _, _ := f.task(1)
	f.tasks.Shutdown(nil)
	if !errors.Is(context.Cause(t2), hook.ErrShutdown) {
		t.Fatalf("after Shutdown: %v", context.Cause(t2))
	}
}

// With a shared registry every turn's tools reach the agent's own view,
// even on a context that carries another registrar; the task the turn runs
// in is still the parent of what it starts, so a cancel of that task
// reaches across agents.
func TestAgent_SharedTasksViewOnEveryTurn(t *testing.T) {
	f := newSharedFixture(t)
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"))
	worker := f.agent(t, func(s *Spec) { s.ID = "alice-worker" })
	bob := f.tasks.For("bob")
	tctx, parent := start(t, bob, inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "bobs"}})
	if _, ok := hook.TaskRegistrarFrom(tctx); !ok {
		t.Fatal("no registrar on a task's context")
	}
	worker.Run(tctx, "s1", hook.UserText("work")).Wait()
	cctx, h, reg := f.task(0)
	if reg != hook.TaskRegistrar(worker.tasks) || h.Info().Owner != "alice-worker" || h.Info().Parent != "bobs" {
		t.Fatalf("the worker's task: %+v, registrar %T", h.Info(), reg)
	}
	if len(bob.Tasks()) != 1 || len(worker.Tasks()) != 1 {
		t.Fatalf("bob %+v, worker %+v", bob.Tasks(), worker.Tasks())
	}
	if !parent.Cancel(nil) || cctx.Err() == nil {
		t.Fatal("cancelling bob's task left the worker's task under it running")
	}
}

// An agent's Cancel (not a shutdown) on a shared registry cancels its own
// tasks and nobody else's.
func TestAgent_SharedTasksCancelOwnOnly(t *testing.T) {
	f := newSharedFixture(t)
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"))
	a := f.agent(t, nil)
	_, bobs := start(t, f.tasks.For("bob"), inTask(""), hook.TaskStart{})
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	a.Cancel(nil)
	_, h, _ := f.task(0)
	if h.Info().State != hook.TaskCancelled || bobs.Info().State != hook.TaskRunning {
		t.Fatalf("alice's %+v, bob's %+v", h.Info(), bobs.Info())
	}
}

// A task's events go to the turn that started it while it runs, then to
// the agent whose view started it; a host's view reports none.
func TestTasks_Events(t *testing.T) {
	f := newSharedFixture(t)
	var mu sync.Mutex
	var seen []string
	f.p.Script("w1", calls("spawn"), modelstest.Reply("done"))
	a := f.agent(t, func(s *Spec) {
		s.Plugins = []hook.Plugin{{OnEvent: func(_ context.Context, ev hook.Event) {
			if tf, ok := ev.(hook.TaskFinished); ok {
				mu.Lock()
				seen = append(seen, tf.Task.ID)
				mu.Unlock()
			}
		}}}
	})
	a.Run(context.Background(), "s1", hook.UserText("go")).Wait()
	_, h, _ := f.task(0)
	_, host := start(t, f.tasks.For("Alice"), inTask(""), hook.TaskStart{Info: hook.TaskInfo{ID: "hosts"}})
	h.Finish(toolkit.NewResult("found it"))
	host.Finish(nil)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{h.ID()}) {
		t.Fatalf("TaskFinished to OnEvent for %v", seen)
	}
}

// Run.Interrupt(cause) stops the turn with cause and leaves its tasks;
// Run.Cancel(cause) stops it the same way and cancels them. For the turn the
// two are the same, also when the stop lands as the model's answer returns
// (a cancel wins).
func TestRun_InterruptVersusCancel(t *testing.T) {
	cause := fmt.Errorf("Bob's /cancel: %w", hook.ErrInterrupted)
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancel), func(t *testing.T) {
			f := newSharedFixture(t)
			started, hold := make(chan struct{}), make(chan struct{})
			f.p.Script("w1", calls("spawn"), modelstest.Step{Hold: hold, Started: started, Response: &spawnllm.LLMResponse{Content: "late", FinishReason: "stop"}})
			var interrupted []hook.Interrupted
			var mu sync.Mutex
			a := f.agent(t, func(s *Spec) {
				s.Plugins = []hook.Plugin{{OnInterrupt: func(_ context.Context, ev hook.Interrupted) {
					mu.Lock()
					interrupted = append(interrupted, ev)
					mu.Unlock()
				}}}
			})
			r := a.Run(context.Background(), "s1", hook.UserText("go"))
			<-started
			if cancel {
				r.Cancel(cause)
			} else {
				r.Interrupt(cause)
			}
			close(hold) // the answer returns after the stop: discarded
			res := r.Wait()
			if res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, cause) || res.Reply != "" {
				t.Fatalf("result = %+v", res)
			}
			for _, m := range a.sessions.GetHistory("s1") {
				if m.Role == "assistant" && m.Content == "late" {
					t.Fatal("the late answer was stored")
				}
			}
			tctx, h, _ := f.task(0)
			if cancel != (tctx.Err() != nil) || cancel != (h.Info().State == hook.TaskCancelled) {
				t.Fatalf("cancel %v: task %+v", cancel, h.Info())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(interrupted) != 1 || !errors.Is(interrupted[0].Cause, cause) || cancel != (len(interrupted[0].Cancelled) == 1) {
				t.Fatalf("OnInterrupt = %+v", interrupted)
			}
		})
	}
}

// Interrupt(nil) is an interrupt with hook.ErrInterrupted.
func TestRun_InterruptNilCause(t *testing.T) {
	f := newAgentFixture(t)
	started, hold := make(chan struct{}), make(chan struct{})
	defer close(hold)
	f.p.Script("w1", modelstest.Step{Hold: hold, Started: started, Response: &spawnllm.LLMResponse{Content: "late"}})
	a := f.agent(t, nil)
	r := a.Run(context.Background(), "s1", hook.UserText("go"))
	<-started
	r.Interrupt(nil)
	if res := r.Wait(); res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
		t.Fatalf("result = %+v", res)
	}
}
