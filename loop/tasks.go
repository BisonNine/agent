package loop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// keptEnded is how many ended tasks the registry remembers, so a cancel of
// a task that ended recently reports AlreadyEnded rather than ErrUnknownTask.
const keptEnded = 256

// taskRegistry is the agent's hook.TaskRegistrar: its tasks, each on a
// context of its own, by id. It is also what the agent's TaskController
// methods list and cancel.
type taskRegistry struct {
	a *Agent

	mu    sync.Mutex
	tasks map[string]*task
	// order is the ids of the running tasks and the ended ones still kept,
	// oldest first.
	order []string
	ended []string
	seq   uint64
	// shutdown: the agent is shutting down; no task starts, and Detach
	// cancels.
	shutdown bool
	// cancellingAll counts CancelAll calls in progress: a task started
	// meanwhile is cancelled at once.
	cancellingAll int
	// settled is signalled whenever a task's settle (claim) ends.
	settled *sync.Cond
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
	// cancelCause is set while a cancel of this task is under way: a child
	// started now is cancelled at once, with it.
	cancelCause error
	// declined: OnCancel said the task had already ended; no cancel asks
	// again, and its Finish delivers the result.
	declined bool

	// turn is the turn that started the task, for its events while that turn
	// runs; scope is the starter's scope, for the events' headers.
	turn  *turnOnContext
	scope hook.Scope
}

func newTaskRegistry(a *Agent) *taskRegistry {
	r := &taskRegistry{a: a, tasks: map[string]*task{}}
	r.settled = sync.NewCond(&r.mu)
	return r
}

var _ hook.TaskRegistrar = (*taskRegistry)(nil)

// emit sends a task event to the turn that started the task while it runs,
// else to the agent's consumers only.
func (r *taskRegistry) emit(t *task, ev hook.Event) {
	ctx := hook.WithScope(context.Background(), t.scope)
	if t.turn != nil && !t.turn.finished.Load() {
		t.turn.hooks.emit(ctx, ev)
		return
	}
	r.a.hooks.emit(ctx, ev)
}

func (r *taskRegistry) header(t *task) hook.EventHeader {
	return hook.EventHeader{Scope: t.scope, At: r.a.clock.Now()}
}

// StartTask registers a task on a context of its own (see hook.TaskRegistrar).
func (r *taskRegistry) StartTask(ctx context.Context, start hook.TaskStart) (context.Context, hook.TaskHandle, error) {
	from, _ := hook.ScopeOf(ctx)
	info := start.Info
	info.Owner = r.a.identity.Agent
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
		info.Started = r.a.clock.Now()
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
	if _, dup := r.tasks[info.ID]; dup {
		r.mu.Unlock()
		return nil, nil, fmt.Errorf("start task %q: duplicate id", info.ID)
	}
	tctx, cancel := context.WithCancelCause(context.Background())
	tctx = hook.WithScope(tctx, hook.TaskScope(from, info, start.Depth))
	tctx = hook.WithTaskRegistrar(tctx, r)
	t := &task{info: info, ctx: tctx, cancel: cancel, onCancel: start.OnCancel, onDetach: start.OnDetach, scope: from}
	if turn, ok := turnOf(ctx); ok {
		t.turn = turn
	}
	r.tasks[info.ID] = t
	r.order = append(r.order, info.ID)
	parent := r.tasks[info.Parent]
	var cause error
	switch {
	case parent != nil && parent.cancelCause != nil:
		cause = parent.cancelCause
	case parent != nil && parent.info.State == hook.TaskCancelled:
		cause = parent.info.Cause
	case r.cancellingAll > 0:
		cause = hook.ErrInterrupted
	}
	cancelNow := cause != nil
	snapshot := t.info
	r.mu.Unlock()

	r.emit(t, hook.TaskStarted{EventHeader: r.header(t), Task: snapshot})
	h := &taskHandle{r: r, t: t}
	if cancelNow {
		r.cancelOne(t, cause, &hook.TaskCancel{})
	}
	return tctx, h, nil
}

// Tasks lists the running tasks, oldest first.
func (r *taskRegistry) Tasks() []hook.TaskInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hook.TaskInfo
	for _, id := range r.order {
		if t := r.tasks[id]; t != nil && t.info.State == hook.TaskRunning {
			out = append(out, t.info)
		}
	}
	return out
}

// CancelTask cancels task id and, first, the tasks it started.
func (r *taskRegistry) CancelTask(id string, cause error) (hook.TaskCancel, error) {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	r.mu.Lock()
	t := r.tasks[id]
	r.mu.Unlock()
	if t == nil {
		return hook.TaskCancel{}, fmt.Errorf("cancel task %q: %w", id, hook.ErrUnknownTask)
	}
	var out hook.TaskCancel
	r.cancelTree(t, cause, &out)
	return out, nil
}

// CancelAll cancels every running task, none escaping.
func (r *taskRegistry) CancelAll(cause error) hook.TaskCancel {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	r.mu.Lock()
	r.cancellingAll++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cancellingAll--
		r.mu.Unlock()
	}()
	var out hook.TaskCancel
	visited := map[*task]bool{}
	for {
		roots := r.running(func(t *task) bool {
			p := r.tasks[t.info.Parent]
			return !visited[t] && !t.declined && (p == nil || p.info.State != hook.TaskRunning || visited[p] || p.declined)
		})
		if len(roots) == 0 {
			return out
		}
		for _, t := range roots {
			visited[t] = true
			r.cancelTree(t, cause, &out)
		}
	}
}

// cancelTurn cancels the tasks turn started (with theirs).
func (r *taskRegistry) cancelTurn(turnID string, cause error) []hook.TaskInfo {
	if turnID == "" {
		return nil
	}
	var out hook.TaskCancel
	for _, t := range r.running(func(t *task) bool { return t.info.Turn == turnID }) {
		r.cancelTree(t, cause, &out)
	}
	return out.Cancelled
}

// running returns the running tasks match accepts, oldest first.
func (r *taskRegistry) running(match func(*task) bool) []*task {
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

// cancelTree cancels t's children (with theirs), then t, recording each in
// out.
func (r *taskRegistry) cancelTree(t *task, cause error, out *hook.TaskCancel) {
	r.mu.Lock()
	if t.info.State == hook.TaskRunning && !t.declined {
		t.cancelCause = cause
	}
	var children []*task
	for _, id := range r.order {
		if c := r.tasks[id]; c != nil && c.info.Parent == t.info.ID && c.info.State == hook.TaskRunning {
			children = append(children, c)
		}
	}
	r.mu.Unlock()
	for _, c := range children {
		r.cancelTree(c, cause, out)
	}
	r.cancelOne(t, cause, out)
}

// claim marks t as being settled by the caller, once any other settle has
// ended, when it is still running and ok (called under the lock) accepts it.
func (r *taskRegistry) claim(t *task, ok func() bool) bool {
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
func (r *taskRegistry) unsettleLocked(t *task) {
	t.settling = false
	r.settled.Broadcast()
}

// cancelOne cancels t alone: OnCancel, then its state, its context and
// TaskFinished.
func (r *taskRegistry) cancelOne(t *task, cause error, out *hook.TaskCancel) bool {
	if !r.claim(t, func() bool { return !t.declined }) {
		r.mu.Lock()
		info := t.info
		r.mu.Unlock()
		out.AlreadyEnded = append(out.AlreadyEnded, info)
		return false
	}
	if t.onCancel != nil && !t.onCancel(cause) {
		r.mu.Lock()
		t.declined = true
		t.cancelCause = nil
		r.unsettleLocked(t)
		info := t.info
		r.mu.Unlock()
		out.AlreadyEnded = append(out.AlreadyEnded, info)
		return false
	}
	r.mu.Lock()
	r.unsettleLocked(t)
	t.cancelCause = nil
	t.info.State = hook.TaskCancelled
	t.info.Cause = cause
	t.info.Ended = r.a.clock.Now()
	info := t.info
	r.endedLocked(t.info.ID)
	r.mu.Unlock()
	t.cancel(cause)
	out.Cancelled = append(out.Cancelled, info)
	r.emit(t, hook.TaskFinished{EventHeader: r.header(t), Task: info})
	return true
}

// endedLocked moves id from the running order to the ended ones, forgetting
// the oldest ended task beyond keptEnded.
func (r *taskRegistry) endedLocked(id string) {
	r.ended = append(r.ended, id)
	if len(r.ended) > keptEnded {
		old := r.ended[0]
		r.ended = r.ended[1:]
		delete(r.tasks, old)
		for i, o := range r.order {
			if o == old {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
}

// shutDown is the agent telling its registrar it is shutting down: no task
// starts from now on, and every running task's context ends with cause
// (which wraps hook.ErrShutdown) without OnCancel or TaskFinished, so a host
// keeping records relaunches them after a restart.
func (r *taskRegistry) shutDown(cause error) {
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

// taskHandle is a starter's hold on one task.
type taskHandle struct {
	r *taskRegistry
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
	t.info.Ended = r.a.clock.Now()
	info := t.info
	r.endedLocked(t.info.ID)
	r.mu.Unlock()
	t.cancel(context.Canceled)
	r.emit(t, hook.TaskFinished{EventHeader: r.header(t), Task: info, Result: result})
	return true
}

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
		r.cancelTree(t, cause, &hook.TaskCancel{})
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
	r.mu.Unlock()
	return ok
}

// Cancel cancels the task and its own tasks.
func (h *taskHandle) Cancel(cause error) bool {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	var out hook.TaskCancel
	h.r.cancelTree(h.t, cause, &out)
	for _, c := range out.Cancelled {
		if c.ID == h.t.info.ID {
			return true
		}
	}
	return false
}
