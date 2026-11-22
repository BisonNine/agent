package hook

import "time"

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

// TaskController is the capability of an agent whose tasks a host can list
// and cancel. Our agent has it; a host finds it by type assertion.
type TaskController interface {
	// Tasks lists the agent's tasks that are running.
	Tasks() []TaskInfo
	// CancelTask cancels the task and, first, the tasks it started, with
	// cause. Cancelling a task that has ended is not an error; an unknown
	// id is.
	CancelTask(taskID string, cause error) error
}
