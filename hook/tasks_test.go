package hook_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// fakeRegistrar shows the shape of the seam; it is not a conforming
// registrar (no cascade, no events, no times, no shutdown). Its tasks run
// on contexts of their own carrying TaskScope, a cancel runs OnCancel
// before the context ends, and a waiting turn that stops waiting detaches
// its task through OnDetach.
type fakeRegistrar struct {
	mu       sync.Mutex
	tasks    map[string]*fakeHandle
	shutdown bool
}

type fakeHandle struct {
	reg    *fakeRegistrar
	mu     sync.Mutex
	info   hook.TaskInfo
	cancel context.CancelCauseFunc
	start  hook.TaskStart
}

func newFakeRegistrar() *fakeRegistrar { return &fakeRegistrar{tasks: map[string]*fakeHandle{}} }

func (r *fakeRegistrar) StartTask(ctx context.Context, start hook.TaskStart) (context.Context, hook.TaskHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shutdown {
		return nil, nil, hook.ErrShutdown
	}
	from, _ := hook.ScopeOf(ctx)
	info := start.Info
	info.Owner, info.State = from.AgentID, hook.TaskRunning
	if info.Parent == "" {
		info.Parent = hook.TaskID(ctx)
	}
	if info.Turn == "" {
		info.Turn = from.TurnID
	}
	if info.Session == "" {
		info.Session = from.Session
	}
	if _, dup := r.tasks[info.ID]; dup {
		return nil, nil, fmt.Errorf("duplicate task %q", info.ID)
	}
	tctx := hook.WithScope(context.Background(), hook.TaskScope(from, info, start.Depth))
	tctx = hook.WithTaskRegistrar(tctx, r)
	tctx, cancel := context.WithCancelCause(tctx)
	h := &fakeHandle{reg: r, info: info, cancel: cancel, start: start}
	r.tasks[info.ID] = h
	return tctx, h, nil
}

func (r *fakeRegistrar) Tasks() []hook.TaskInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []hook.TaskInfo
	for _, h := range r.tasks {
		if i := h.Info(); i.State == hook.TaskRunning {
			out = append(out, i)
		}
	}
	return out
}

func (r *fakeRegistrar) CancelTask(id string, cause error) (hook.TaskCancel, error) {
	r.mu.Lock()
	h := r.tasks[id]
	r.mu.Unlock()
	if h == nil {
		return hook.TaskCancel{}, hook.ErrUnknownTask
	}
	if !h.Cancel(cause) {
		return hook.TaskCancel{AlreadyEnded: []hook.TaskInfo{h.Info()}}, nil
	}
	return hook.TaskCancel{Cancelled: []hook.TaskInfo{h.Info()}}, nil
}

func (r *fakeRegistrar) CancelAll(cause error) hook.TaskCancel {
	var out hook.TaskCancel
	for _, info := range r.Tasks() {
		if c, err := r.CancelTask(info.ID, cause); err == nil {
			out.Cancelled = append(out.Cancelled, c.Cancelled...)
		}
	}
	return out
}

func (h *fakeHandle) ID() string { return h.info.ID }

func (h *fakeHandle) Info() hook.TaskInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.info
}

func (h *fakeHandle) end(state hook.TaskState, cause error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.info.State.Terminal() {
		return false
	}
	h.info.State, h.info.Cause = state, cause
	h.cancel(cause)
	return true
}

func (h *fakeHandle) Finish(r *toolkit.Result) bool {
	if r != nil && (r.IsError || r.Err != nil) {
		return h.end(hook.TaskError, nil)
	}
	return h.end(hook.TaskDone, nil)
}

func (h *fakeHandle) Cancel(cause error) bool {
	if h.Info().State.Terminal() {
		return false
	}
	if h.start.OnCancel != nil && !h.start.OnCancel(cause) {
		return false
	}
	return h.end(hook.TaskCancelled, cause)
}

func (h *fakeHandle) Detach(cause error) bool {
	h.reg.mu.Lock()
	shutdown := h.reg.shutdown
	h.reg.mu.Unlock()
	if shutdown || errors.Is(cause, hook.ErrShutdown) {
		h.Cancel(cause)
		return false
	}
	if i := h.Info(); i.State.Terminal() || i.Mode != hook.TaskWait {
		return false
	}
	if h.start.OnDetach != nil && !h.start.OnDetach() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.info.Mode = hook.TaskCallback
	return true
}

func TestTaskContextHelpers(t *testing.T) {
	ctx := context.Background()
	if _, ok := hook.TaskRegistrarFrom(ctx); ok {
		t.Fatal("registrar found on an empty context")
	}
	if hook.TaskID(ctx) != "" {
		t.Fatal("task id outside a task")
	}
	r := newFakeRegistrar()
	ctx = hook.WithTaskRegistrar(hook.WithScope(ctx, alice), r)
	got, ok := hook.TaskRegistrarFrom(ctx)
	if !ok || got != r {
		t.Fatalf("TaskRegistrarFrom = %v, %v", got, ok)
	}
	if hook.TaskID(ctx) != "" {
		t.Fatal("a turn's context runs in a task")
	}
	if _, ok := hook.TaskRegistrarFrom(hook.WithTaskRegistrar(context.Background(), nil)); ok {
		t.Fatal("a nil registrar was found")
	}
}

// TaskScope keeps the agent, project and turn id, takes the task's id,
// depth and session, and a task started inside it records it as the parent.
func TestTaskScope(t *testing.T) {
	s := hook.TaskScope(alice, hook.TaskInfo{ID: "t-1"}, 2)
	want := alice
	want.Task, want.Depth = "t-1", 2
	if s != want {
		t.Fatalf("TaskScope = %+v, want %+v", s, want)
	}
	if s := hook.TaskScope(alice, hook.TaskInfo{ID: "t-1", Session: "main"}, 1); s.Session != "main" {
		t.Fatalf("session = %q", s.Session)
	}

	r := newFakeRegistrar()
	turn := hook.WithTaskRegistrar(hook.WithScope(context.Background(), alice), r)
	tctx, h, err := r.StartTask(turn, hook.TaskStart{Info: hook.TaskInfo{ID: "t-1", Mode: hook.TaskWait}, Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if hook.TaskID(tctx) != "t-1" || h.Info().Owner != "alice" || h.Info().Parent != "" || h.Info().Turn != alice.TurnID {
		t.Fatalf("task = %+v, id on context %q", h.Info(), hook.TaskID(tctx))
	}
	// Not a child of the turn's context.
	cancelled, cancel := context.WithCancel(turn)
	cancel()
	child, _, err := r.StartTask(cancelled, hook.TaskStart{Info: hook.TaskInfo{ID: "t-2"}})
	if err != nil || child.Err() != nil {
		t.Fatalf("task context ended with its starter: %v, %v", err, child.Err())
	}
	_, ch, err := r.StartTask(tctx, hook.TaskStart{Info: hook.TaskInfo{ID: "t-3"}})
	if err != nil || ch.Info().Parent != "t-1" {
		t.Fatalf("child of t-1: %+v, %v", ch.Info(), err)
	}
	if _, err := r.CancelTask("nope", hook.ErrInterrupted); !errors.Is(err, hook.ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	if !h.Detach(hook.ErrInterrupted) || h.Detach(hook.ErrInterrupted) {
		t.Fatal("a wait task detaches once")
	}
	if c, err := r.CancelTask("t-1", hook.ErrInterrupted); err != nil || len(c.Cancelled) != 1 || h.Info().State != hook.TaskCancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if c, err := r.CancelTask("t-1", hook.ErrInterrupted); err != nil || len(c.AlreadyEnded) != 1 || h.Finish(nil) {
		t.Fatalf("second cancel = %+v; Finish after cancel must be false", c)
	}
}

// A tool that starts a sub-agent as a wait task: the work runs on the
// task's own context, so interrupting the turn does not reach it; when the
// turn stops waiting, the task is detached and its result arrives later.
func ExampleTaskRegistrar() {
	turn := hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s-42", TurnID: "0a1b2c3d"})
	turn = hook.WithTaskRegistrar(turn, newFakeRegistrar())
	turn, interrupt := context.WithCancelCause(turn)
	defer interrupt(nil)

	reg, _ := hook.TaskRegistrarFrom(turn)
	taskCtx, task, err := reg.StartTask(turn, hook.TaskStart{
		Info:  hook.TaskInfo{ID: "task-1", Kind: "subagent", Label: "summarise the report", Mode: hook.TaskWait},
		Depth: 1,
		OnCancel: func(error) bool {
			return true // the host marks its own record cancelled here
		},
		OnDetach: func() bool {
			return true // the host marks its own record "callback" here
		},
	})
	if err != nil {
		fmt.Println(err)
		return
	}

	interrupt(hook.ErrInterrupted) // the person stopped the reply
	fmt.Println("turn:", context.Cause(turn))
	fmt.Println("task context still live:", taskCtx.Err() == nil)
	fmt.Println("detached:", task.Detach(context.Cause(turn)), task.Info().Mode)
	fmt.Println("finished:", task.Finish(toolkit.NewResult("The report says ...")), task.Info().State)
	// Output:
	// turn: interrupted by user
	// task context still live: true
	// detached: true callback
	// finished: true done
}
