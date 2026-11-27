/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package hook

import (
	"context"
	"errors"
	"time"

	"go.bison9.com/agent/toolkit"
)

// TaskState is where a task stands.
type TaskState string

// The task states. Every state but TaskRunning is terminal.
const (
	TaskRunning   TaskState = "running"
	TaskDone      TaskState = "done"
	TaskError     TaskState = "error"
	TaskCancelled TaskState = "cancelled"
)

// Terminal reports whether s is a final state.
func (s TaskState) Terminal() bool { return s != TaskRunning }

// TaskMode is how a task's owner receives its result.
type TaskMode string

// The task modes.
const (
	// TaskWait: the turn that started the task waits for its result. An
	// interrupt detaches it: the turn stops waiting and the task becomes a
	// TaskCallback task.
	TaskWait TaskMode = "wait"
	// TaskCallback: the result is delivered later, as a new message to the
	// owner's session.
	TaskCallback TaskMode = "callback"
)

// TaskInfo describes a task: a sub-agent or another background job running
// on its own context, registered with the agent that owns it. Tasks are not
// children of the turn that started them: an interrupt does not reach them,
// and they are cancelled by id.
type TaskInfo struct {
	// ID is the task's id, unique within the agent.
	ID string
	// Owner is the id of the agent that owns the task.
	Owner string
	// Session is the owner's session the result goes to.
	Session string
	// Parent is the id of the task that started this one; empty for a task
	// a turn started.
	Parent string
	// Turn is the id of the turn that started the task (directly, or through
	// its parent tasks); Run.Cancel cancels the tasks of its turn by it.
	// Empty for a task relaunched after a restart.
	Turn string
	// Kind says what the task is (for example "subagent", or a task model's
	// kind); free text.
	Kind string
	// Label is a short human-readable description.
	Label   string
	Mode    TaskMode
	State   TaskState
	Started time.Time
	// Ended is zero while the task runs.
	Ended time.Time
	// Cause is why a cancelled task was cancelled.
	Cause error
}

// TaskController is the capability of an agent whose tasks a host, or one
// of the agent's own tools (agent_cancel), can list and cancel. Our agent
// has it; a host finds it by type assertion, a tool through
// TaskRegistrarFrom.
type TaskController interface {
	// Tasks lists the agent's running tasks, oldest first.
	Tasks() []TaskInfo
	// CancelTask cancels the agent's task taskID and, first, the tasks it
	// started (with theirs, whatever agent owns them), with cause, and
	// waits up to the registry's grace for their workers to exit
	// (TaskHandle.Exited). A task
	// that had already ended is not an error: it is reported in
	// AlreadyEnded (its children may still have been cancelled). An id that
	// is not a task of this agent fails with ErrUnknownTask; so may the id
	// of a task that ended long ago and that the registry no longer keeps.
	CancelTask(taskID string, cause error) (TaskCancel, error)
	// CancelAll cancels every running task of the agent, with the tasks they
	// started. A task started while it runs is cancelled too: none escapes.
	CancelAll(cause error) TaskCancel
}

// TaskCancel is what a cancel did.
type TaskCancel struct {
	// Cancelled are the tasks this call cancelled, each after the tasks it
	// started.
	Cancelled []TaskInfo
	// AlreadyEnded are the tasks asked for that had already ended (done,
	// error, or cancelled earlier).
	AlreadyEnded []TaskInfo
}

// ErrUnknownTask reports a task id that is not a task of the agent asked.
var ErrUnknownTask = errors.New("unknown task")

// TaskStart is what a tool gives StartTask.
type TaskStart struct {
	// Info describes the task. ID is the caller's (a host that keeps a
	// record per task uses the record's id) or, when empty, one the
	// registrar makes. Kind, Label and Mode are the caller's. Session
	// defaults to the caller's scope's session (where the result goes);
	// Parent to the task the caller runs in (TaskID of its context); Turn to
	// the caller's scope's turn id; Started to now. A caller relaunching a
	// task after a restart sets Parent and Started from its record. The
	// registrar sets Owner (its own agent), State and Ended.
	Info TaskInfo
	// Depth is the sub-agent depth the task runs at (its scope's Depth).
	Depth int
	// OnCancel, when set, settles the caller's own record of the task as
	// cancelled. A cancel runs it exactly once, before the task's context is
	// cancelled and before TaskFinished, so a record the host keeps says
	// "cancelled" (and nothing relaunches the task) before the worker can
	// notice. It returns false when the task had in fact already ended (its
	// result is being delivered): the cancel then does nothing more to it,
	// and the task is reported in AlreadyEnded. Once it returned true (or
	// when there is none) the task ends cancelled with the cause and is
	// among those the cancel reports; a Finish racing it returns false.
	//
	// It runs on the goroutine of the cancel, never while the registrar
	// holds a lock, and may run inside StartTask itself, before it returns,
	// for a task started under a cancelled parent: a caller that holds a
	// lock of its own while starting a task must allow for that. It must not
	// call back into the registrar.
	OnCancel func(cause error) bool
	// OnDetach, when set, records in the caller's own record that the task
	// is now a TaskCallback task (so a restart relaunches it). Detach runs
	// it exactly once, before the mode changes, with the same rules as
	// OnCancel: false means the task had already ended, and Detach then
	// returns false.
	OnDetach func() bool
}

// TaskRegistrar is how a tool running inside a turn starts work that must
// outlive the turn: a sub-agent, a task-model job, an async tool's late
// part. It is the agent's task registry as a tool reaches it, on the tool
// call's context (TaskRegistrarFrom), and the same registry the agent's
// TaskController lists and cancels.
//
// The agent's task registry implements it (loop.Tasks, seen per owner): it
// emits TaskStarted and TaskFinished on the agent's event stream and is told
// when the host shuts down. A host keeps its records through TaskStart's
// OnCancel and OnDetach, and may share one registry between its agents and
// their instances (loop.Spec.Tasks), observing it with Tasks.Observe.
//
// Shutdown: the registry is told it is shutting down (the agent's Close, or
// a Cancel whose cause wraps ErrShutdown, for a registry of its own; the
// host's Tasks.Shutdown for a shared one) before the turns are cancelled.
// Shutdown is not a cancellation of the tasks: their contexts end with a
// cause wrapping ErrShutdown, but OnCancel does not run, no TaskFinished is
// emitted, and their records stay as they are, so a restart relaunches
// them. The one exception is a TaskWait task whose turn stops waiting
// (Detach). The registrar never infers shutdown from a context's cause: a
// call that reached a tool another way (a CLI model's tool call over MCP)
// may end with a plain context.Canceled.
type TaskRegistrar interface {
	TaskController

	// StartTask registers a task and returns the context it runs on and its
	// handle. The context is not a child of ctx: no interrupt, cancel or
	// deadline of the turn reaches it, and ctx's own cancellation does not
	// stop StartTask (a tool whose turn was just interrupted still starts
	// its task). It carries none of ctx's values but a Scope (see
	// TaskScope). A host that needs its own values on the task (who is
	// waiting on the turn, for instance) puts them on the returned context
	// itself, before running the work there.
	//
	// The task is running, listed by Tasks and announced with TaskStarted
	// before StartTask returns. One started under a task that is being or
	// has been cancelled is cancelled at once (OnCancel runs, and its
	// context is done when returned). It fails with ErrShutdown once the
	// agent is shutting down, and on a duplicate id.
	StartTask(ctx context.Context, start TaskStart) (context.Context, TaskHandle, error)
}

// TaskHandle is the starter's hold on one task. Every method is safe from
// any goroutine, at any time, also after the task ended.
//
// A turn waiting for a TaskWait task waits for its result from the worker
// and for the task's context to end. When the context ends first, Info's
// State says why: TaskCancelled, and the waiter builds its own result (the
// task was cancelled); otherwise the result is on its way.
type TaskHandle interface {
	// ID is the task's id.
	ID() string
	// Info is the task's description now.
	Info() TaskInfo
	// Finish ends the task with its result: TaskError when the result is an
	// error (IsError or Err), else TaskDone (a nil result: done, with
	// nothing to say). It releases the task's context and emits
	// TaskFinished with the result, whose Task.Mode says where it goes: a
	// TaskWait task's result is its waiter's, a TaskCallback task's starts a
	// new turn of the owner's session (the host routes it). It reports
	// false, and does nothing, for a task already ended or cancelled.
	Finish(result *toolkit.Result) bool
	// Detach is called by the turn waiting for a TaskWait task when it
	// stops waiting, with the cause of its stop. When the agent is shutting
	// down (see TaskRegistrar) or cause wraps ErrShutdown, the task is
	// cancelled (OnCancel runs), since the turn will be replayed and start
	// it again. Otherwise OnDetach runs and the task becomes a TaskCallback
	// task, keeps running, and its result arrives later in TaskFinished. It
	// reports whether the task was detached: false when it was cancelled,
	// had already ended, or is not (or no longer) a TaskWait task.
	Detach(cause error) bool
	// Cancel cancels the task (and, first, the tasks it started) with
	// cause, as TaskController.CancelTask does; for a starter that cannot
	// deliver the task's result. It reports whether this call cancelled it.
	Cancel(cause error) bool
	// Exited is the worker reporting it has returned and written all it
	// writes (its partial output after a cancel, say). The starter calls it
	// once, last, whether the task finished, failed or was cancelled; later
	// calls do nothing. Every cancel (CancelTask, CancelAll, Cancel, Detach
	// that cancels, Run.Cancel) waits, up to the registry's grace in all,
	// for the workers of the tasks it cancelled to exit before it returns,
	// so what they write as they unwind is written by then. A starter that
	// never calls it makes every cancel of its task wait the whole grace.
	Exited()
	// Sealed reports that a cancel stopped waiting for the worker (its grace
	// ran out, or the registry waits for none) before it exited: the worker
	// must write nothing more of the task, and the record its OnCancel wrote
	// is final. A worker checks it before writing its partial output. It
	// stays false for a worker that exited in time, and for a task that was
	// not cancelled.
	Sealed() bool
}

type taskRegistrarKey struct{}

// WithTaskRegistrar returns a child of ctx carrying r. The engine puts the
// agent's registrar on every tool call's context and every task's context.
func WithTaskRegistrar(ctx context.Context, r TaskRegistrar) context.Context {
	return context.WithValue(ctx, taskRegistrarKey{}, r)
}

// TaskRegistrarFrom returns the registrar on ctx and whether there is one.
func TaskRegistrarFrom(ctx context.Context) (TaskRegistrar, bool) {
	r, ok := ctx.Value(taskRegistrarKey{}).(TaskRegistrar)
	return r, ok && r != nil
}

// TaskID is the id of the task ctx runs in (its scope's Task); "" outside
// any task.
func TaskID(ctx context.Context) string {
	s, _ := ScopeOf(ctx)
	return s.Task
}

// TaskScope is the scope of a task's context, from the scope of the context
// it was started from (from) and the task's id and depth: the same agent
// (id, name, billing code), the session its result goes to, the turn id of
// the turn that started it (so its log lines correlate), the depth it runs
// at, and Task set to its id. A registrar puts it on the task's context with
// WithScope.
func TaskScope(from Scope, info TaskInfo, depth int) Scope {
	s := from
	s.Task = info.ID
	s.Depth = depth
	if info.Session != "" {
		s.Session = info.Session
	}
	return s
}
