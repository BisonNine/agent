package loop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// keptEnded is how many ended tasks the registry remembers, so a cancel of
// a task that ended recently reports AlreadyEnded rather than ErrUnknownTask.
const keptEnded = 256

// keptStopped is how many cancelled task ids (with the parent ids given to
// CancelChildren) the registry remembers, so a task started under one of
// them later is cancelled as it starts.
const keptStopped = 1024

// Tasks is a task registry: tasks (sub-agents, task-model jobs, the late
// parts of async tools), each on a context of its own, registered under an
// id with the agent that owns them. Every agent has one: its own, private
// one, or, with Spec.Tasks, one the host shares between its agents and
// keeps across their instances (a config reload, temporary clones), so a
// task outlives the agent instance that started it.
//
// Each agent sees the registry through For(owner), with owner its
// identity's agent: it lists and cancels that owner's tasks only, and a
// cancel cascades to the tasks they started, whatever agent owns those.
// Every method is safe for concurrent use.
type Tasks struct {
	clock clock.Clock
	// grace is how long a cancel waits for the workers of the tasks it
	// cancelled to report they exited (TaskHandle.Exited).
	grace time.Duration

	mu    sync.Mutex
	tasks map[string]*task
	// order is the ids of the running tasks and the ended ones still kept,
	// oldest first.
	order []string
	ended []string
	seq   uint64
	// shutdown: the host is shutting down; no task starts, and Detach
	// cancels.
	shutdown bool
	// cancellingAll counts the CancelAll calls in progress per owner: a task
	// of that owner started meanwhile is cancelled at once.
	cancellingAll map[string]int
	// stopped holds the cause of each task id a cancel reached (and of each
	// parent id given to CancelChildren), the last keptStopped of them in
	// stoppedOrder: a task started under one is cancelled at once.
	stopped      map[string]error
	stoppedOrder []string
	// settled is signalled whenever a task's settle (claim) ends.
	settled *sync.Cond
	// observers are told of every change of a task (Observe).
	observers []func(hook.TaskInfo)
}

// NewTasks returns an empty registry for a host to share between its agents
// (Spec.Tasks). clk is its clock (nil means the real one): task times, and
// the cancel grace. grace is how long a cancel waits, in all, for the
// workers of the tasks it cancelled to report they exited
// (TaskHandle.Exited) before it seals those still running
// (TaskHandle.Sealed) and returns; 0 (or less) means a cancel does not wait,
// and seals at once the workers that have not exited. A host that reports
// its workers' exit passes hook.DefaultCancelGrace or its own.
func NewTasks(clk clock.Clock, grace time.Duration) *Tasks {
	r := &Tasks{
		clock:         clock.Or(clk),
		grace:         max(grace, 0),
		tasks:         map[string]*task{},
		cancellingAll: map[string]int{},
		stopped:       map[string]error{},
	}
	r.settled = sync.NewCond(&r.mu)
	return r
}

// For is the registry as owner sees it: a hook.TaskRegistrar whose
// StartTask registers tasks owned by owner, and whose Tasks, CancelTask and
// CancelAll list and cancel owner's tasks only (an id of another owner's
// task is ErrUnknownTask). Cancelling a task cascades to the tasks it
// started whatever owns them. Owners are compared exactly: a host that
// treats two spellings of an agent id as one passes one of them.
//
// A view from For emits no events of its own: a task started on a turn's
// context reports TaskStarted and TaskFinished to that turn while it runs.
// The agent's own view (Spec.Tasks) also reports them to its OnEvent. The
// observers (Observe) are told of every task whichever view started it.
func (r *Tasks) For(owner string) hook.TaskRegistrar { return r.view(owner, nil) }

// view is the registry as owner sees it, reporting events to a (nil for
// none) outside a turn.
func (r *Tasks) view(owner string, a *Agent) *taskView {
	return &taskView{r: r, owner: owner, a: a}
}

// Observe registers fn, told of each task as it starts, changes mode
// (Detach), is cancelled and finishes (TaskInfo.State says how), and, for a
// cancelled task, once more when its worker reports it exited
// (TaskHandle.Exited). It runs on the goroutine of the change, never under
// the registry's lock, and must not block; it may call the registry.
// Shutdown tells it nothing: the tasks' records stay as they are.
func (r *Tasks) Observe(fn func(hook.TaskInfo)) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	r.observers = append(r.observers, fn)
	r.mu.Unlock()
}

// notify tells the observers of info.
func (r *Tasks) notify(info hook.TaskInfo) {
	r.mu.Lock()
	obs := slices.Clone(r.observers)
	r.mu.Unlock()
	for _, fn := range obs {
		fn(info)
	}
}

// Shutdown tells the registry the host is shutting down: no task starts from
// now on (StartTask fails with hook.ErrShutdown), Detach cancels, and every
// running task's context ends with cause, without OnCancel, TaskFinished or
// an observer call, so a host keeping records relaunches them after the
// restart. A nil cause means hook.ErrShutdown; one that does not wrap it is
// wrapped. An agent never shuts down a registry it was given (Spec.Tasks):
// the host calls this once, as it shuts down.
func (r *Tasks) Shutdown(cause error) {
	switch {
	case cause == nil:
		cause = hook.ErrShutdown
	case !errors.Is(cause, hook.ErrShutdown):
		cause = fmt.Errorf("%w: %w", cause, hook.ErrShutdown)
	}
	r.mu.Lock()
	r.shutdown = true
	var live []*task
	for _, id := range r.order {
		if t := r.tasks[id]; t != nil && t.info.State == hook.TaskRunning {
			live = append(live, t)
		}
	}
	r.mu.Unlock()
	for _, t := range live {
		t.cancel(cause)
	}
}

// CancelChildren cancels every running task started under parentID (with
// theirs), whatever owns them, children first, and returns those it
// cancelled; parentID itself is not cancelled and need not be a registered
// task (a host's record of a task a restart left unrelaunched). The id is
// remembered: a task started under it later is cancelled as it starts, with
// cause (nil means hook.ErrInterrupted). Like every cancel it waits up to
// the grace for the cancelled workers to exit. An empty id names no task:
// nothing is cancelled.
func (r *Tasks) CancelChildren(parentID string, cause error) []hook.TaskInfo {
	if parentID == "" {
		return nil
	}
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	r.mu.Lock()
	r.markStoppedLocked(parentID, cause)
	r.mu.Unlock()
	var c cancelRun
	for _, t := range r.running(func(t *task) bool { return t.info.Parent == parentID }) {
		r.cancelTree(t, cause, &c)
	}
	r.waitExited(c.stopped)
	return c.out.Cancelled
}

// markStoppedLocked remembers that id was cancelled with cause. r.mu is
// held.
func (r *Tasks) markStoppedLocked(id string, cause error) {
	if _, ok := r.stopped[id]; ok {
		return
	}
	r.stopped[id] = cause
	r.stoppedOrder = append(r.stoppedOrder, id)
	if len(r.stoppedOrder) > keptStopped {
		delete(r.stopped, r.stoppedOrder[0])
		r.stoppedOrder = r.stoppedOrder[1:]
	}
}

// task is one registered task. Its info and flags are guarded by the
// registry's lock.
type task struct {
	info   hook.TaskInfo
	cancel context.CancelCauseFunc
	ctx    context.Context

	onCancel func(cause error) bool
	onDetach func() bool

	// settling: a cancel or detach is running its host step (OnCancel,
	// OnDetach) outside the lock; Finish and other settles wait for it.
	settling bool
	// declined: OnCancel said the task had already ended; no cancel asks
	// again, and its Finish delivers the result.
	declined bool

	// exited is closed once the worker reported it returned (Exited);
	// sealed tells a worker still running after a cancel stopped waiting
	// for it that it may write nothing more.
	exited     chan struct{}
	exitedOnce sync.Once
	sealed     atomic.Bool

	// turn is the turn that started the task, for its events while that turn
	// runs; agent is the agent whose view started it, for its events
	// otherwise (nil: none); scope is the starter's scope, for the events'
	// headers.
	turn  *turnOnContext
	agent *Agent
	scope hook.Scope
}

// cancelRun is what one cancel did: its result, and the tasks it cancelled,
// whose workers it waits for.
type cancelRun struct {
	out     hook.TaskCancel
	stopped []*task
}

// emit sends a task event to the turn that started the task while it runs,
// else to the agent that started it, if any.
func (r *Tasks) emit(t *task, ev hook.Event) {
	ctx := hook.WithScope(context.Background(), t.scope)
	if t.turn != nil && !t.turn.finished.Load() {
		t.turn.hooks.emit(ctx, ev)
		return
	}
	if t.agent != nil {
		t.agent.hooks.emit(ctx, ev)
	}
}

func (r *Tasks) header(t *task) hook.EventHeader {
	return hook.EventHeader{Scope: t.scope, At: r.clock.Now()}
}

// taskView is the registry as one owner sees it: the hook.TaskRegistrar on
// that owner's tool calls' and tasks' contexts.
type taskView struct {
	r     *Tasks
	owner string
	// a is the agent the view reports events to outside a turn; nil for a
	// host's view (For).
	a *Agent
}

var _ hook.TaskRegistrar = (*taskView)(nil)

// StartTask registers a task on a context of its own (see hook.TaskRegistrar).
func (v *taskView) StartTask(ctx context.Context, start hook.TaskStart) (context.Context, hook.TaskHandle, error) {
	r := v.r
	from, _ := hook.ScopeOf(ctx)
	info := start.Info
	info.Owner = v.owner
	info.State = hook.TaskRunning
	info.Ended = time.Time{}
	info.Cause = nil
	if info.Session == "" {
		info.Session = from.Session
	}
	if info.Parent == "" {
		info.Parent = from.Task
	}
	if info.Turn == "" {
		info.Turn = from.TurnID
	}
	if info.Started.IsZero() {
		info.Started = r.clock.Now()
	}
	if info.Mode == "" {
		info.Mode = hook.TaskCallback
	}

	r.mu.Lock()
	if r.shutdown {
		r.mu.Unlock()
		return nil, nil, fmt.Errorf("start task: %w", hook.ErrShutdown)
	}
	if info.ID == "" {
		r.seq++
		info.ID = fmt.Sprintf("task-%d", r.seq)
	}
	if old := r.tasks[info.ID]; old != nil {
		if old.info.State == hook.TaskRunning {
			r.mu.Unlock()
			return nil, nil, fmt.Errorf("start task %q: duplicate id", info.ID)
		}
		// An ended task's id started again (a relaunch): the new task
		// replaces what is kept of the old one.
		r.forgetLocked(info.ID)
	}
	tctx, cancel := context.WithCancelCause(context.Background())
	tctx = hook.WithScope(tctx, hook.TaskScope(from, info, start.Depth))
	tctx = hook.WithTaskRegistrar(tctx, v)
	t := &task{
		info: info, ctx: tctx, cancel: cancel, onCancel: start.OnCancel, onDetach: start.OnDetach,
		exited: make(chan struct{}), agent: v.a, scope: from,
	}
	if turn, ok := turnOf(ctx); ok {
		t.turn = turn
	}
	r.tasks[info.ID] = t
	r.order = append(r.order, info.ID)
	var cause error
	if c, ok := r.stopped[info.Parent]; ok && info.Parent != "" {
		cause = c
	} else if r.cancellingAll[v.owner] > 0 {
		cause = hook.ErrInterrupted
	}
	snapshot := t.info
	r.mu.Unlock()

	r.emit(t, hook.TaskStarted{EventHeader: r.header(t), Task: snapshot})
	r.notify(snapshot)
	h := &taskHandle{r: r, t: t}
	if cause != nil {
		// Its worker has not started: there is nothing to wait for.
		r.cancelOne(t, cause, &cancelRun{})
	}
	return tctx, h, nil
}

// forgetLocked drops what is kept of the ended task id. r.mu is held.
func (r *Tasks) forgetLocked(id string) {
	delete(r.tasks, id)
	r.order = slices.DeleteFunc(r.order, func(o string) bool { return o == id })
	r.ended = slices.DeleteFunc(r.ended, func(o string) bool { return o == id })
}

// Tasks lists the owner's running tasks, oldest first.
func (v *taskView) Tasks() []hook.TaskInfo {
	r := v.r
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hook.TaskInfo
	for _, id := range r.order {
		if t := r.tasks[id]; t != nil && t.info.State == hook.TaskRunning && v.owns(t) {
			out = append(out, t.info)
		}
	}
	return out
}

// owns reports whether t is the view's owner's. It reads t.info.Owner,
// which never changes.
func (v *taskView) owns(t *task) bool { return t.info.Owner == v.owner }

// CancelTask cancels the owner's task id and, first, the tasks it started.
func (v *taskView) CancelTask(id string, cause error) (hook.TaskCancel, error) {
	r := v.r
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	r.mu.Lock()
	t := r.tasks[id]
	r.mu.Unlock()
	if t == nil || !v.owns(t) {
		return hook.TaskCancel{}, fmt.Errorf("cancel task %q: %w", id, hook.ErrUnknownTask)
	}
	var c cancelRun
	r.cancelTree(t, cause, &c)
	r.waitExited(c.stopped)
	return c.out, nil
}

// CancelAll cancels every running task of the owner, none escaping.
func (v *taskView) CancelAll(cause error) hook.TaskCancel {
	r := v.r
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	r.mu.Lock()
	r.cancellingAll[v.owner]++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.cancellingAll[v.owner]--; r.cancellingAll[v.owner] == 0 {
			delete(r.cancellingAll, v.owner)
		}
		r.mu.Unlock()
	}()
	var c cancelRun
	visited := map[*task]bool{}
	for {
		// The roots: the owner's tasks not started by another of its tasks
		// this call is still to cancel (that one's cascade takes them).
		roots := r.running(func(t *task) bool {
			if visited[t] || t.declined || !v.owns(t) {
				return false
			}
			p := r.tasks[t.info.Parent]
			return p == nil || p.info.State != hook.TaskRunning || visited[p] || p.declined || !v.owns(p)
		})
		if len(roots) == 0 {
			break
		}
		for _, t := range roots {
			visited[t] = true
			r.cancelTree(t, cause, &c)
		}
	}
	r.waitExited(c.stopped)
	return c.out
}

// cancelTurn cancels the owner's tasks turn started (with theirs).
func (v *taskView) cancelTurn(turnID string, cause error) []hook.TaskInfo {
	if turnID == "" {
		return nil
	}
	r := v.r
	var c cancelRun
	for _, t := range r.running(func(t *task) bool { return v.owns(t) && t.info.Turn == turnID }) {
		r.cancelTree(t, cause, &c)
	}
	r.waitExited(c.stopped)
	return c.out.Cancelled
}

// running returns the running tasks match accepts (called under the lock),
// oldest first.
func (r *Tasks) running(match func(*task) bool) []*task {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*task
	for _, id := range r.order {
		if t := r.tasks[id]; t != nil && t.info.State == hook.TaskRunning && match(t) {
			out = append(out, t)
		}
	}
	return out
}

// cancelTree cancels t's children (with theirs, whatever owns them), then
// t, recording each in c. From the start t counts as stopped: a task started
// under it meanwhile is cancelled as it starts.
func (r *Tasks) cancelTree(t *task, cause error, c *cancelRun) {
	r.mu.Lock()
	if t.info.State == hook.TaskRunning && !t.declined {
		r.markStoppedLocked(t.info.ID, cause)
	}
	var children []*task
	for _, id := range r.order {
		if ch := r.tasks[id]; ch != nil && ch.info.Parent == t.info.ID && ch.info.State == hook.TaskRunning {
			children = append(children, ch)
		}
	}
	r.mu.Unlock()
	for _, ch := range children {
		r.cancelTree(ch, cause, c)
	}
	r.cancelOne(t, cause, c)
}

// claim marks t as being settled by the caller, once any other settle has
// ended, when it is still running and ok (called under the lock) accepts it.
func (r *Tasks) claim(t *task, ok func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for t.settling {
		r.settled.Wait()
	}
	if t.info.State != hook.TaskRunning || !ok() {
		return false
	}
	t.settling = true
	return true
}

// unsettleLocked ends a claim. r.mu is held.
func (r *Tasks) unsettleLocked(t *task) {
	t.settling = false
	r.settled.Broadcast()
}

// cancelOne cancels t alone: OnCancel (outside the lock), then its state,
// its context, TaskFinished and the observers.
func (r *Tasks) cancelOne(t *task, cause error, c *cancelRun) bool {
	if !r.claim(t, func() bool { return !t.declined }) {
		r.mu.Lock()
		info := t.info
		r.mu.Unlock()
		c.out.AlreadyEnded = append(c.out.AlreadyEnded, info)
		return false
	}
	if t.onCancel != nil && !t.onCancel(cause) {
		r.mu.Lock()
		t.declined = true
		r.unsettleLocked(t)
		info := t.info
		r.mu.Unlock()
		c.out.AlreadyEnded = append(c.out.AlreadyEnded, info)
		return false
	}
	r.mu.Lock()
	r.unsettleLocked(t)
	r.markStoppedLocked(t.info.ID, cause)
	t.info.State = hook.TaskCancelled
	t.info.Cause = cause
	t.info.Ended = r.clock.Now()
	info := t.info
	r.endedLocked(t.info.ID)
	r.mu.Unlock()
	t.cancel(cause)
	c.out.Cancelled = append(c.out.Cancelled, info)
	c.stopped = append(c.stopped, t)
	r.emit(t, hook.TaskFinished{EventHeader: r.header(t), Task: info})
	r.notify(info)
	return true
}

// waitExited waits, at most the grace in all, for the workers of the
// cancelled tasks to report they exited, so what they write as they unwind
// (partial output) is written before the cancel returns. A worker still
// running then is sealed: it must write nothing more.
func (r *Tasks) waitExited(list []*task) {
	if len(list) == 0 {
		return
	}
	var timeout <-chan time.Time
	expired := r.grace <= 0
	if !expired {
		timer := r.clock.NewTimer(r.grace)
		defer timer.Stop()
		timeout = timer.C()
	}
	for _, t := range list {
		if !expired {
			select {
			case <-t.exited:
				continue
			case <-timeout:
				expired = true
			}
		}
		select {
		case <-t.exited:
		default:
			t.sealed.Store(true)
		}
	}
}

// endedLocked moves id from the running order to the ended ones, forgetting
// the oldest ended task beyond keptEnded.
func (r *Tasks) endedLocked(id string) {
	r.ended = append(r.ended, id)
	if len(r.ended) > keptEnded {
		r.forgetLocked(r.ended[0])
	}
}

// taskHandle is a starter's hold on one task.
type taskHandle struct {
	r *Tasks
	t *task
}

func (h *taskHandle) ID() string { return h.t.info.ID }

func (h *taskHandle) Info() hook.TaskInfo {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	return h.t.info
}

// Finish ends the task with result (see hook.TaskHandle).
func (h *taskHandle) Finish(result *toolkit.Result) bool {
	r, t := h.r, h.t
	r.mu.Lock()
	for t.settling {
		r.settled.Wait()
	}
	if t.info.State != hook.TaskRunning {
		r.mu.Unlock()
		return false
	}
	t.info.State = hook.TaskDone
	if result != nil && (result.IsError || result.Err != nil) {
		t.info.State = hook.TaskError
	}
	t.info.Ended = r.clock.Now()
	info := t.info
	r.endedLocked(t.info.ID)
	r.mu.Unlock()
	t.cancel(context.Canceled)
	r.emit(t, hook.TaskFinished{EventHeader: r.header(t), Task: info, Result: result})
	r.notify(info)
	return true
}

// Exited records that the task's worker has returned (see hook.TaskHandle).
func (h *taskHandle) Exited() {
	r, t := h.r, h.t
	first := false
	t.exitedOnce.Do(func() {
		close(t.exited)
		first = true
	})
	if !first {
		return
	}
	r.mu.Lock()
	info := t.info
	r.mu.Unlock()
	if info.State == hook.TaskCancelled {
		r.notify(info)
	}
}

// Sealed reports that a cancel stopped waiting for the worker.
func (h *taskHandle) Sealed() bool { return h.t.sealed.Load() }

// Detach stops the turn waiting for a TaskWait task (see hook.TaskHandle).
func (h *taskHandle) Detach(cause error) bool {
	r, t := h.r, h.t
	cancel := errors.Is(cause, hook.ErrShutdown)
	claimed := r.claim(t, func() bool {
		if r.shutdown {
			cancel = true
		}
		return !cancel && t.info.Mode == hook.TaskWait
	})
	if cancel {
		var c cancelRun
		r.cancelTree(t, cause, &c)
		r.waitExited(c.stopped)
		return false
	}
	if !claimed {
		return false
	}
	ok := t.onDetach == nil || t.onDetach()
	r.mu.Lock()
	if ok {
		t.info.Mode = hook.TaskCallback
	}
	r.unsettleLocked(t)
	info := t.info
	r.mu.Unlock()
	if ok {
		r.notify(info)
	}
	return ok
}

// Cancel cancels the task and its own tasks.
func (h *taskHandle) Cancel(cause error) bool {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	var c cancelRun
	h.r.cancelTree(h.t, cause, &c)
	h.r.waitExited(c.stopped)
	return slices.Contains(c.stopped, h.t)
}
