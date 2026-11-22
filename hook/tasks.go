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
	// started (with theirs, whatever agent owns them), with cause. It
	// returns the tasks it cancelled, children first. A task that has
	// already ended is not an error: it returns none. An id that is not a
	// task of this agent fails with ErrUnknownTask (another agent's task is
	// unknown, not refused). To cancel every task, cancel each of Tasks.
	CancelTask(taskID string, cause error) ([]TaskInfo, error)
}

// ErrUnknownTask reports a task id that is not a running or ended task of
// the agent asked.
var ErrUnknownTask = errors.New("unknown task")

// TaskStart is what a tool gives StartTask.
type TaskStart struct {
	// Info describes the task. ID is the caller's (a host that keeps a
	// record per task uses the record's id) or, when empty, one the
	// registrar makes. Kind, Label and Mode are the caller's. Session
	// defaults to the caller's scope's session (where the result goes).
	// Owner, Parent, State and the times are set by the registrar: Owner is
	// the caller's scope's agent, Parent the task the caller runs in
	// (TaskID of the caller's context).
	Info TaskInfo
	// Depth is the sub-agent depth the task runs at (its scope's Depth).
	Depth int
	// OnCancel, when set, settles the caller's own record of the task as
	// cancelled. A cancel runs it exactly once, before the task's context is
	// cancelled and before TaskFinished, so a record the host keeps says
	// "cancelled" (and nothing relaunches the task) before the worker can
	// notice. It returns false when the task had in fact already ended (its
	// result is being delivered): the cancel then does nothing more to it,
	// and the task is not among those reported cancelled. It must not call
	// back into the registrar.
	OnCancel func(cause error) bool
}

// TaskRegistrar is how a tool running inside a turn starts work that must
// outlive the turn: a sub-agent, a task-model job, an async tool's late
// part. It is the agent's task registry as a tool reaches it, on the tool
// call's context (TaskRegistrarFrom), and it is the same registry the
// agent's TaskController lists and cancels.
//
// Shutdown: a registrar is told that its agent is shutting down (by the
// agent's Close, or a Cancel whose cause wraps ErrShutdown) before the
// agent's turns are cancelled. It never infers shutdown from a context's
// cause: a call that reached a tool another way (a CLI model's tool call
// over MCP) may end with a plain context.Canceled.
type TaskRegistrar interface {
	TaskController

	// StartTask registers a task and returns the context it runs on and its
	// handle. The context is not a child of ctx: no interrupt, cancel or
	// deadline of the turn reaches it, and it carries none of ctx's values
	// but a Scope (see TaskScope). A host that needs its own values on the
	// task (who is waiting on the turn, for instance) puts them on the
	// returned context itself, before running the work there.
	//
	// The task is running, listed by Tasks and announced with TaskStarted
	// before StartTask returns. One started under a task that is being
	// cancelled is cancelled at once (its context is done when returned).
	// It fails with ErrShutdown once the agent is shutting down, and on a
	// duplicate id.
	StartTask(ctx context.Context, start TaskStart) (context.Context, TaskHandle, error)
}

// TaskHandle is the starter's hold on one task. Every method is safe from
// any goroutine, at any time, also after the task ended.
type TaskHandle interface {
	// ID is the task's id.
	ID() string
	// Info is the task's description now.
	Info() TaskInfo
	// Finish ends the task with its result: TaskError when the result is an
	// error (IsError or Err), else TaskDone. It releases the task's context
	// and emits TaskFinished with the result, whose Task.Mode says where it
	// goes: a TaskWait task's result is its waiter's, a TaskCallback task's
	// starts a new turn of the owner's session (the host routes it). It
	// reports false, and does nothing, for a task already ended or
	// cancelled.
	Finish(result *toolkit.Result) bool
	// Detach is called by the turn waiting for a TaskWait task when it
	// stops waiting, with the cause of its stop. The registrar decides:
	// when the agent is shutting down (the registrar is told; see
	// TaskRegistrar) or cause wraps ErrShutdown, the task is cancelled, since the turn will be replayed
	// and start it again; otherwise it becomes a TaskCallback task, keeps
	// running, and its result arrives later in TaskFinished. It reports
	// whether the task was detached: false when it was cancelled or had
	// already ended (its result is then the waiter's).
	Detach(cause error) bool
	// Cancel cancels the task (and, first, the tasks it started) with
	// cause, as TaskController.CancelTask does; for a starter that cannot
	// deliver the task's result. It reports whether this call cancelled it.
	Cancel(cause error) bool
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
// (id, name, project), the session its result goes to, the turn id of the
// turn that started it (so its log lines correlate), the depth it runs at,
// and Task set to its id. A registrar puts it on the task's context with
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
