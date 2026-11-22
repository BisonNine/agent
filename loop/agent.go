package loop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/ident"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/toolkit"
)

// Agent is the agent: its settings, its sessions with their queues, its
// tasks and the turn engine that runs each turn. New builds one; a host holds
// it as the root package's agent.Agent interface. Every method is safe for
// concurrent use.
//
// Runs of one session are served one at a time, in the order given: a Run on
// a busy session waits in the session's queue. Runs of different sessions run
// concurrently. Each session keeps its context manager (over the session
// store) and its memory session from its first run until Close.
type Agent struct {
	spec  Spec
	id    string
	clock clock.Clock
	log   *slog.Logger
	// hooks are the agent's callbacks and consumers; each turn runs on a copy
	// that also feeds its run's events.
	hooks *turnHooks
	tasks *taskRegistry
	// life is cancelled once Close has stopped everything: background
	// compaction runs on it.
	life     context.Context
	endLife  context.CancelFunc
	sessions session.SessionStore

	mu     sync.Mutex
	states map[string]*sessionState
	closed bool
	wg     sync.WaitGroup
	// closeDone is closed once the first Close has finished.
	closeDone chan struct{}
}

// sessionState is one session as the agent holds it.
type sessionState struct {
	key string
	// queue holds the runs not yet started; running is the run whose turn
	// runs; busy while a goroutine serves the session.
	queue   []*run
	running *run
	busy    bool

	// cm and mem are made on the session's first turn and closed at Close.
	cm  ctxengine.ContextManager
	mem hook.MemorySession
}

// New builds the agent spec describes. It fails, wrapping ErrInvalidSpec, on
// an invalid id, a missing model set, a chat purpose without models, or two
// tools offered under one name.
func New(spec Spec) (*Agent, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	a := &Agent{
		spec:      spec,
		id:        ident.NormalizeID(spec.ID),
		clock:     clock.Or(spec.Clock),
		log:       spec.Logger,
		sessions:  spec.Sessions,
		states:    map[string]*sessionState{},
		closeDone: make(chan struct{}),
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if a.sessions == nil {
		a.sessions = session.NewMemory()
	}
	if spec.Gate == nil && spec.Allow != nil {
		allow := spec.Allow
		a.spec.Gate = func(_ context.Context, info toolkit.Info) *toolkit.Result {
			if allow.IsToolAllowed(info.Name) {
				return nil
			}
			err := toolkit.Refusal(fmt.Errorf("tool %q is not permitted for this agent", info.Published))
			return toolkit.ErrorResult(err.Error()).WithError(err)
		}
	}
	a.hooks = &turnHooks{log: a.log}
	for _, p := range spec.Plugins {
		a.hooks.add(p)
	}
	a.tasks = newTaskRegistry(a)
	a.life, a.endLife = context.WithCancel(context.Background())
	return a, nil
}

// ID is the agent's id, normalised.
func (a *Agent) ID() string { return a.id }

// Name is the agent's friendly name.
func (a *Agent) Name() string { return a.spec.Name }

// Description says what the agent is for.
func (a *Agent) Description() string { return a.spec.Description }

// Tools are the agent's tools (nil for none).
func (a *Agent) Tools() toolkit.Registry { return a.spec.Tools }

// Models are the agent's models.
func (a *Agent) Models() *models.Set { return a.spec.Models }

// Tasks lists the agent's running tasks, oldest first.
func (a *Agent) Tasks() []hook.TaskInfo { return a.tasks.Tasks() }

// CancelTask cancels task id and, first, the tasks it started.
func (a *Agent) CancelTask(id string, cause error) (hook.TaskCancel, error) {
	return a.tasks.CancelTask(id, cause)
}

// CancelAll cancels every running task of the agent.
func (a *Agent) CancelAll(cause error) hook.TaskCancel { return a.tasks.CancelAll(cause) }

var _ hook.TaskController = (*Agent)(nil)

// Run starts a turn of session answering msg and returns its handle at once.
// On a busy session the run waits behind the running turn and those queued
// before it. ctx is the turn's context: its values reach every callback and
// tool, and cancelling it stops the turn. A run given to a closed agent ends
// at once with ErrClosed.
func (a *Agent) Run(ctx context.Context, sessionKey string, msg hook.Message, opts ...hook.RunOption) hook.Run {
	r := newRun(a, ctx, sessionKey, msg, hook.ApplyRunOptions(opts...))
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		a.finishUnstarted(ctx, r, hook.TurnResult{Outcome: hook.OutcomeError, Err: ErrClosed})
		return r
	}
	s := a.state(sessionKey)
	s.queue = append(s.queue, r)
	if !s.busy {
		s.busy = true
		a.wg.Add(1)
		go a.serve(s) //nolint:contextcheck // the session's goroutine serves runs of many contexts, each on its own
	}
	a.mu.Unlock()
	return r
}

// state returns the session's state, made on first use. a.mu is held.
func (a *Agent) state(key string) *sessionState {
	s := a.states[key]
	if s == nil {
		s = &sessionState{key: key}
		a.states[key] = s
	}
	return s
}

// serve runs the session's queued runs one at a time until its queue is
// empty.
func (a *Agent) serve(s *sessionState) {
	defer a.wg.Done()
	for {
		a.mu.Lock()
		if len(s.queue) == 0 {
			s.busy = false
			a.mu.Unlock()
			return
		}
		r := s.queue[0]
		s.queue = s.queue[1:]
		s.running = r
		r.started = true
		r.turnID = newTurnID()
		ctx, cancel := context.WithCancelCause(r.ctx)
		r.cancel = cancel
		a.mu.Unlock()

		a.runTurn(ctx, s, r)
		cancel(nil)

		a.mu.Lock()
		s.running = nil
		r.ended = true
		a.mu.Unlock()
		a.compactIfDue(s)
	}
}

// newTurnID is a fresh turn id.
func newTurnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// forRun is the agent's hooks with r's event buffer as the last consumer.
func (a *Agent) forRun(r *run) *turnHooks {
	h := *a.hooks
	h.onEvent = append(slices.Clip(a.hooks.onEvent), named[hook.OnEventFunc]{plugin: "run", fn: r.add})
	return &h
}

// finishUnstarted ends a run that never started: TurnFinished alone, to the
// agent's consumers and the run.
// Its consumers get ctx, without its cancellation.
func (a *Agent) finishUnstarted(ctx context.Context, r *run, tr hook.TurnResult) {
	scope := hook.Scope{AgentID: a.id, AgentName: a.spec.Name, Session: r.session, Project: a.spec.Project}
	ev := hook.TurnFinished{TurnResult: tr}
	ev.Scope, ev.At = scope, a.clock.Now()
	a.forRun(r).emit(hook.WithScope(context.WithoutCancel(ctx), scope), ev)
}

// runTurn runs r's turn on ctx through the engine.
func (a *Agent) runTurn(ctx context.Context, s *sessionState, r *run) {
	scope := hook.Scope{
		AgentID: a.id, AgentName: a.spec.Name, Session: s.key, TurnID: r.turnID,
		Depth: r.opts.Depth, Project: a.spec.Project,
	}
	if from, ok := hook.ScopeOf(ctx); ok {
		scope.Task = from.Task
	}
	limits := a.spec.Limits.WithDefaults()
	budget := limits.TurnBudget
	if r.opts.TurnBudget > 0 {
		budget = r.opts.TurnBudget
	}
	ctx, stopBudget := a.clock.WithTimeout(ctx, budget)
	defer stopBudget()
	hooks := a.forRun(r)
	engineRan := false
	defer func() {
		// A panic in the engine itself (not in a callback) has already ended
		// the turn with OutcomeError; one before the engine started ends it
		// here. Either stops here rather than in the host.
		if p := recover(); p != nil {
			a.log.Error("panic in turn", "component", "agent", "agent_id", a.id,
				"session", s.key, "panic", fmt.Sprintf("%v", p), "stack", string(debug.Stack()))
			if !engineRan {
				a.finishUnstarted(ctx, r, hook.TurnResult{Outcome: hook.OutcomeError, Err: fmt.Errorf("turn set-up panicked: %v", p)})
			}
		}
	}()
	ctx = beginTurnContext(ctx, scope, hooks, a.clock)
	// A turn on a task's context (a sub-agent's) keeps the registrar the task
	// was started from, so its tasks are that task's children there.
	if _, ok := hook.TaskRegistrarFrom(ctx); !ok {
		ctx = hook.WithTaskRegistrar(ctx, a.tasks)
	}

	chain := a.spec.Models.Begin(models.Chat, models.Scope(s.key))
	candidates := chain.Candidates()
	lim := chain.Limits()
	a.openSession(ctx, s, scope)
	if lim.ContextWindow > 0 {
		if err := s.cm.SetContextWindow(lim.ContextWindow); err != nil {
			a.log.Warn("context window not set", "component", "agent", "error", err.Error())
		}
	}

	env := &agentEnv{a: a, s: s, scope: scope, text: r.msg.Text, noTools: lim.NoTools}
	if a.spec.Served != nil && len(candidates) > 0 && isCLIProtocol(candidates[0].Protocol) {
		env.served = a.serveTurn(ctx, r, scope)
	}
	defer a.endServed(r)

	spec := engineSpec{
		agentID: a.id,
		scope:   scope,
		message: r.msg,
		options: r.opts,

		maxIterations: limits.MaxIterations,
		cancelGrace:   limits.CancelGrace,
		toolTimeout:   func() time.Duration { return limits.ToolTimeout },
		retries:       max(limits.Retries, 0),
		retryBackoff:  limits.RetryBackoff,
		temperature:   a.spec.Temperature,
		repeatHint:    a.spec.RepeatHint,

		clock:      a.clock,
		log:        a.log,
		logContent: func() bool { return a.spec.LogContent },
		redact:     a.spec.Redactor,
		truncate:   a.spec.Truncator,

		chain:          chain,
		candidates:     candidates,
		model:          lim0Name(candidates),
		modelMaxTokens: lim.MaxTokens,
		contextWindow:  lim.ContextWindow,
		noTools:        lim.NoTools,

		cm:    s.cm,
		store: a.sessions,
		mem:   turnMemoryOf(ctx, s.mem),
		env:   env,
		hooks: hooks,
	}
	engineRan = true
	if _, err := newEngineTurn(spec, nil).run(ctx); err != nil {
		// The run's TurnFinished carries the error; a host renders it.
		a.log.Debug("turn ended with an error", "component", "agent", "agent_id", a.id,
			"session", s.key, "error", a.redact(err.Error()))
	}
}

// lim0Name is the name of the turn's first model; "" without one.
func lim0Name(c []models.Candidate) string {
	if len(c) == 0 {
		return ""
	}
	return c[0].Name
}

// openSession makes the session's context manager and memory session on its
// first turn.
func (a *Agent) openSession(ctx context.Context, s *sessionState, scope hook.Scope) {
	if s.cm == nil {
		id := scope.Identity()
		id.Agent = a.id
		opts := make([]ctxengine.Option, 0, 3+len(a.spec.Context))
		opts = append(opts,
			ctxengine.WithLogger(a.log),
			ctxengine.WithModelCaller(a.spec.Models.Caller(models.Summarize, id, "")),
			ctxengine.WithCompactionStartFunc(engineCompactionStart),
		)
		s.cm = ctxengine.New(s.key, a.sessions, append(opts, a.spec.Context...)...)
	}
	if s.mem == nil && a.spec.Memory != nil {
		// The session's memory outlives this turn: it is opened on a context
		// the turn's end does not cancel.
		ms, err := a.spec.Memory.Open(context.WithoutCancel(ctx), scope)
		if err != nil {
			a.log.Warn("memory not opened; the session runs without it", "component", "agent",
				"agent_id", a.id, "session", s.key, "error", a.redact(err.Error()))
		}
		s.mem = ms
	}
}

// redact is the agent's redactor applied to s.
func (a *Agent) redact(s string) string {
	if a.spec.Redactor == nil {
		return s
	}
	return a.spec.Redactor(s)
}

// serveTurn issues the turn's served-tools credential and returns its prompt.
func (a *Agent) serveTurn(ctx context.Context, r *run, scope hook.Scope) string {
	prompt, end, err := a.spec.Served.ServeTurn(ctx, scope)
	if err != nil {
		a.log.Warn("tools not served to the CLI model; the turn runs without them", "component", "agent",
			"agent_id", a.id, "session", scope.Session, "error", a.redact(err.Error()))
		return ""
	}
	a.mu.Lock()
	r.served = end
	a.mu.Unlock()
	return prompt
}

// endServed revokes r's served-tools credential, once.
func (a *Agent) endServed(r *run) {
	a.mu.Lock()
	end := r.served
	r.served = nil
	a.mu.Unlock()
	if end != nil {
		end()
	}
}

// compactIfDue runs the compaction the session's adds deferred, after a
// turn, outside any turn: its report reaches OnCompaction and OnEvent only.
func (a *Agent) compactIfDue(s *sessionState) {
	if s.cm == nil {
		return
	}
	scope := hook.Scope{AgentID: a.id, AgentName: a.spec.Name, Session: s.key, Project: a.spec.Project}
	ctx := beginTurnContext(a.life, scope, a.hooks, a.clock)
	if _, err := s.cm.CompactIfDue(ctx); err != nil && a.life.Err() == nil {
		a.log.Warn("deferred compaction failed", "component", "agent", "agent_id", a.id,
			"session", s.key, "error", a.redact(err.Error()))
	}
}

// stopRun stops one run: drops it from its queue, or cancels its turn with
// cause; withTasks also cancels the tasks its turn started.
func (a *Agent) stopRun(r *run, cause error, withTasks bool) {
	a.mu.Lock()
	s := a.states[r.session]
	dropped := false
	if s != nil && !r.started {
		if i := slices.Index(s.queue, r); i >= 0 {
			s.queue = slices.Delete(s.queue, i, i+1)
			dropped = true
		}
	}
	running := r.started && !r.ended
	cancel, turnID := r.cancel, r.turnID
	a.mu.Unlock()

	scope := hook.Scope{AgentID: a.id, AgentName: a.spec.Name, Session: r.session, Project: a.spec.Project}
	ev := hook.Interrupted{Scope: scope, Cause: cause, Running: running}
	if dropped {
		ev.Dropped = 1
		a.finishUnstarted(r.ctx, r, droppedResult(cause))
	}
	if running {
		ev.Scope.TurnID = turnID
		cancel(cause)
		a.endServed(r)
	}
	if withTasks {
		ev.Cancelled = a.tasks.cancelTurn(turnID, cause)
	}
	if !dropped && !running && len(ev.Cancelled) == 0 {
		return
	}
	a.hooks.callOnInterrupt(hook.WithScope(context.WithoutCancel(r.ctx), ev.Scope), ev)
}

// droppedResult is the result of a run dropped from its queue.
func droppedResult(cause error) hook.TurnResult {
	return hook.TurnResult{Outcome: hook.CauseOutcome(cause), Err: cause, Cause: cause}
}

// Interrupt stops session's running turn with hook.ErrInterrupted and drops
// its queued runs; every task is left running. It reports whether a turn was
// running and how many runs were dropped.
func (a *Agent) Interrupt(sessionKey string) (running bool, dropped int) {
	return a.interrupt(context.Background(), sessionKey, hook.ErrInterrupted)
}

func (a *Agent) interrupt(ctx context.Context, sessionKey string, cause error) (bool, int) {
	a.mu.Lock()
	s := a.states[sessionKey]
	if s == nil {
		a.mu.Unlock()
		return false, 0
	}
	queued := s.queue
	s.queue = nil
	cur := s.running
	var cancel context.CancelCauseFunc
	turnID := ""
	if cur != nil && !cur.ended {
		cancel, turnID = cur.cancel, cur.turnID
	}
	a.mu.Unlock()

	for _, r := range queued {
		a.finishUnstarted(r.ctx, r, droppedResult(cause)) //nolint:contextcheck // a dropped run's consumers get its own context
	}
	if cancel != nil {
		cancel(cause)
		a.endServed(cur)
	}
	scope := hook.Scope{AgentID: a.id, AgentName: a.spec.Name, Session: sessionKey, TurnID: turnID, Project: a.spec.Project}
	ev := hook.Interrupted{Scope: scope, Cause: cause, Running: cancel != nil, Dropped: len(queued)}
	if !ev.Running && ev.Dropped == 0 {
		return false, 0 // nothing was stopped: no one is told
	}
	for _, t := range a.tasks.Tasks() {
		if t.Session == sessionKey {
			ev.StillRunning = append(ev.StillRunning, t)
		}
	}
	a.hooks.callOnInterrupt(hook.WithScope(context.WithoutCancel(ctx), scope), ev)
	return ev.Running, ev.Dropped
}

// Cancel stops every turn, queue and task of the agent with cause (nil means
// hook.ErrInterrupted). A cause wrapping hook.ErrShutdown tells the agent it
// is shutting down: its tasks' contexts end without being cancelled (no
// OnCancel, no TaskFinished), so a host keeping their records relaunches them
// after a restart, and no task starts from then on.
func (a *Agent) Cancel(cause error) {
	if cause == nil {
		cause = hook.ErrInterrupted
	}
	shutdown := errors.Is(cause, hook.ErrShutdown)
	if shutdown {
		a.tasks.shutDown(cause)
	}
	a.mu.Lock()
	var stopped []*run
	var dropped []*run
	for _, s := range a.states {
		dropped = append(dropped, s.queue...)
		s.queue = nil
		if s.running != nil && !s.running.ended {
			stopped = append(stopped, s.running)
		}
	}
	a.mu.Unlock()
	for _, r := range dropped {
		a.finishUnstarted(r.ctx, r, droppedResult(cause))
	}
	for _, r := range stopped {
		r.cancel(cause)
		a.endServed(r)
	}
	ev := hook.Interrupted{
		Scope: hook.Scope{AgentID: a.id, AgentName: a.spec.Name, Project: a.spec.Project},
		Cause: cause, Running: len(stopped) > 0, Dropped: len(dropped),
	}
	if !shutdown {
		ev.Cancelled = a.tasks.CancelAll(cause).Cancelled
	}
	a.hooks.callOnInterrupt(hook.WithScope(context.Background(), ev.Scope), ev)
}

// errClosing is the cause Close stops what is still running with.
var errClosing = fmt.Errorf("agent closing: %w", hook.ErrShutdown)

// Close shuts the agent down. It refuses new runs at once (they end with
// ErrClosed), lets the running and queued turns finish until ctx is done,
// then stops whatever is left with a cause wrapping hook.ErrShutdown (see
// Cancel) and waits for it to end. Finally it closes the sessions' context
// managers and memory. Every goroutine the agent started has ended when it
// returns, except a tool call's that ignored its cancellation past
// Limits.CancelGrace: the turn abandoned it, and it ends when the tool
// returns. It returns ctx's error when ctx ended first, else the first error
// of closing a session.
func (a *Agent) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		select {
		case <-a.closeDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a.closed = true
	a.mu.Unlock()

	idle := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(idle)
	}()
	var err error
	select {
	case <-idle:
		a.tasks.shutDown(errClosing)
	case <-ctx.Done():
		err = ctx.Err()
		a.endLife()          // a deferred compaction stops too
		a.Cancel(errClosing) //nolint:contextcheck // Cancel is told its cause; its observers get a context that is never cancelled
		<-idle
	}
	a.endLife()

	a.mu.Lock()
	states := a.states
	a.mu.Unlock()
	for _, s := range states {
		if s.cm != nil {
			if cerr := s.cm.Close(context.WithoutCancel(ctx)); cerr != nil && err == nil {
				err = fmt.Errorf("close session %q: %w", s.key, cerr)
			}
		}
		if s.mem != nil {
			if cerr := s.mem.Close(); cerr != nil && err == nil {
				err = fmt.Errorf("close memory of session %q: %w", s.key, cerr)
			}
		}
	}
	close(a.closeDone)
	return err
}
