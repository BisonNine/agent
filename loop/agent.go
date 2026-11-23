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

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
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
	spec Spec
	id   string
	// identity is who the agent's calls are accounted to, and its scope.
	identity accounting.Identity
	clock    clock.Clock
	log      *slog.Logger
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
	// queue holds the runs not yet started; running are the runs whose turns
	// run (one at most unless Spec.Unqueued); busy while a goroutine serves
	// the queue.
	queue   []*run
	running []*run
	busy    bool
	// openMu serialises making cm and mem (unqueued turns may race to it).
	openMu sync.Mutex

	// cm and mem are made on the session's first turn and closed at Close or
	// ForgetSession; never when the host supplies the session (Spec.Session).
	cm  ctxengine.ContextManager
	mem hook.MemorySession
}

// New builds the agent spec describes. It fails, wrapping ErrInvalidSpec, on
// an empty id; without Spec.TurnModels, on a missing model set or a chat
// purpose without models; and on two tools offered under one name.
func New(spec Spec) (*Agent, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	a := &Agent{
		spec:      spec,
		id:        spec.ID,
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
	a.identity = spec.Identity
	if a.identity == (accounting.Identity{}) {
		a.identity = accounting.Identity{Agent: spec.ID, Name: spec.Name, Project: spec.Project}
	}
	a.tasks = newTaskRegistry(a)
	a.life, a.endLife = context.WithCancel(context.Background())
	return a, nil
}

// ID is the agent's id, as given.
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
	if a.spec.Unqueued && !s.busy { // busy: being forgotten; the run waits for it
		a.startLocked(s, r)
		a.wg.Add(1)
		go a.serveOne(s, r) //nolint:contextcheck // the run's goroutine runs it on its own context
		a.mu.Unlock()
		return r
	}
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
			a.dropIdleLocked(s)
			a.mu.Unlock()
			return
		}
		r := s.queue[0]
		s.queue = s.queue[1:]
		a.startLocked(s, r)
		a.mu.Unlock()
		a.runStarted(s, r)
	}
}

// serveOne runs one run of an unqueued agent at once.
func (a *Agent) serveOne(s *sessionState, r *run) {
	defer a.wg.Done()
	a.runStarted(s, r)
	a.mu.Lock()
	a.dropIdleLocked(s)
	a.mu.Unlock()
}

// startLocked marks r started on s: its turn id and its cancel. a.mu is held.
func (a *Agent) startLocked(s *sessionState, r *run) {
	s.running = append(s.running, r)
	r.started = true
	switch {
	case r.opts.NoTurnID:
		r.turnID = ""
	case r.opts.TurnID != "":
		r.turnID = r.opts.TurnID
	default:
		r.turnID = newTurnID()
	}
	r.turnCtx, r.cancel = context.WithCancelCause(r.ctx)
}

// runStarted runs r's turn, then ends it on s and, for the agent's own
// session context, runs the compaction the turn deferred.
func (a *Agent) runStarted(s *sessionState, r *run) {
	a.runTurn(r.turnCtx, s, r)
	r.cancel(nil)
	a.mu.Lock()
	s.running = slices.DeleteFunc(s.running, func(x *run) bool { return x == r })
	r.ended = true
	a.mu.Unlock()
	if a.spec.Session == nil {
		a.compactIfDue(s)
	}
}

// dropIdleLocked forgets s when nothing of it is kept. a.mu is held.
func (a *Agent) dropIdleLocked(s *sessionState) {
	if !s.busy && len(s.running) == 0 && s.cm == nil && s.mem == nil {
		delete(a.states, s.key)
	}
}

// newTurnID is a fresh turn id.
func newTurnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// forRun is the agent's hooks with r's event buffer as the last consumer
// and, with env, a last BeforeTurn that hands env the message as the
// BeforeTurn callbacks left it (what memory recalls for).
func (a *Agent) forRun(r *run, env *agentEnv) *turnHooks {
	h := *a.hooks
	h.onEvent = append(slices.Clip(a.hooks.onEvent), named[hook.OnEventFunc]{plugin: "run", fn: r.add})
	if env != nil {
		h.beforeTurn = append(slices.Clip(a.hooks.beforeTurn), named[hook.BeforeTurnFunc]{plugin: "loop", fn: env.turnMessage})
	}
	return &h
}

// scopeOf is the agent's scope for session: who it is accounted as.
func (a *Agent) scopeOf(session string) hook.Scope {
	return hook.Scope{AgentID: a.identity.Agent, AgentName: a.identity.Name, Session: session, Project: a.identity.Project}
}

// finishUnstarted ends a run that never started: TurnFinished alone, to the
// agent's consumers and the run.
// Its consumers get ctx, without its cancellation.
func (a *Agent) finishUnstarted(ctx context.Context, r *run, tr hook.TurnResult) {
	scope := a.scopeOf(r.session)
	ev := hook.TurnFinished{TurnResult: tr}
	ev.Scope, ev.At = scope, a.clock.Now()
	a.forRun(r, nil).emit(hook.WithScope(context.WithoutCancel(ctx), scope), ev)
}

// runTurn runs r's turn on ctx through the engine.
func (a *Agent) runTurn(ctx context.Context, s *sessionState, r *run) {
	scope := a.scopeOf(s.key)
	scope.TurnID, scope.Depth = r.turnID, r.opts.Depth
	if from, ok := hook.ScopeOf(ctx); ok {
		scope.Task = from.Task
	}
	limits := a.limits(ctx, scope)
	budget := limits.TurnBudget
	if r.opts.TurnBudget > 0 {
		budget = r.opts.TurnBudget
	}
	if budget > 0 {
		var stopBudget context.CancelFunc
		ctx, stopBudget = a.clock.WithTimeout(ctx, budget)
		defer stopBudget()
	}
	env := &agentEnv{a: a, s: s, scope: scope, text: r.msg.Text}
	hooks := a.forRun(r, env)
	engineRan := false
	defer func() {
		// A panic in the engine itself (not in a callback) has already ended
		// the turn with OutcomeError and an *EnginePanicError; one before the
		// engine started ends it here. Either stops here: re-raised on this
		// goroutine it would end the process. It is not logged: the run's
		// TurnFinished (and Wait) deliver it, value and stack, to the host.
		if p := recover(); p != nil {
			if !engineRan {
				a.finishUnstarted(ctx, r, hook.TurnResult{Outcome: hook.OutcomeError, Err: &EnginePanicError{Value: p, Stack: debug.Stack(), SetUp: true}})
			}
		}
	}()
	ctx = beginTurnContext(ctx, scope, hooks, a.clock)
	// A turn on a task's context (a sub-agent's) keeps the registrar the task
	// was started from, so its tasks are that task's children there.
	if _, ok := hook.TaskRegistrarFrom(ctx); !ok {
		ctx = hook.WithTaskRegistrar(ctx, a.tasks)
	}

	tm := a.turnModels(ctx, s.key, scope)
	var candidates []models.Candidate
	lim := modelspec.Limits{}
	if tm.Chain != nil {
		candidates = tm.Chain.Candidates()
		lim = tm.Chain.Limits()
	}
	if tm.Limits != nil {
		lim = *tm.Limits
	}
	env.noTools = lim.NoTools
	cm, mem, release, err := a.sessionOf(ctx, s, scope, env)
	if err != nil {
		engineRan = true
		a.finishUnstarted(ctx, r, hook.TurnResult{Outcome: hook.OutcomeError, Err: err})
		return
	}
	defer release()
	// The agent's own manager follows the turn's model; a host that supplies
	// the session sets its window itself.
	if a.spec.Session == nil && lim.ContextWindow > 0 {
		if err := cm.SetContextWindow(lim.ContextWindow); err != nil {
			a.log.Warn("context window not set", "component", "agent", "error", err.Error())
		}
	}
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
		toolTimeout:   a.toolTimeout(ctx, scope, limits),
		retries:       max(limits.Retries, 0),
		retryBackoff:  limits.RetryBackoff,
		temperature:   a.spec.Temperature,
		repeatHint:    a.spec.RepeatHint,

		clock:      a.clock,
		log:        a.log,
		logContent: a.spec.LogContent,
		redact:     a.spec.Redactor,
		truncate:   a.spec.Truncator,

		chain:          tm.Chain,
		candidates:     candidates,
		model:          lim0Name(candidates),
		modelMaxTokens: lim.MaxTokens,
		contextWindow:  lim.ContextWindow,
		noTools:        lim.NoTools,

		cm:    cm,
		store: a.sessions,
		mem:   turnMemoryOf(ctx, mem),
		env:   env,
		hooks: hooks,
	}
	engineRan = true
	// The run's TurnFinished (and Wait) carry the turn's error.
	_, _ = newEngineTurn(spec, nil).run(ctx) //nolint:errcheck // delivered in TurnFinished
}

// limits are the turn's limits, with their defaults.
func (a *Agent) limits(ctx context.Context, scope hook.Scope) hook.Limits {
	if a.spec.TurnLimits != nil {
		return a.spec.TurnLimits(ctx, scope).WithDefaults()
	}
	return a.spec.Limits.WithDefaults()
}

// toolTimeout is the engine's per-batch tool budget: asked again for each
// batch when the limits are the host's.
func (a *Agent) toolTimeout(ctx context.Context, scope hook.Scope, limits hook.Limits) func() time.Duration {
	if a.spec.TurnLimits == nil {
		return func() time.Duration { return limits.ToolTimeout }
	}
	return func() time.Duration { return a.limits(ctx, scope).ToolTimeout }
}

// turnModels are the turn's models: the host's, or the chat models of
// Spec.Models in the session's scope.
func (a *Agent) turnModels(ctx context.Context, session string, scope hook.Scope) TurnModels {
	if a.spec.TurnModels != nil {
		return a.spec.TurnModels(ctx, scope)
	}
	return TurnModels{Chain: a.spec.Models.Begin(models.Chat, models.Scope(session))}
}

// lim0Name is the name of the turn's first model; "" without one.
func lim0Name(c []models.Candidate) string {
	if len(c) == 0 {
		return ""
	}
	return c[0].Name
}

// sessionOf is the turn's session context: the host's (Spec.Session), held
// until release, or the agent's own, made on the session's first turn.
func (a *Agent) sessionOf(ctx context.Context, s *sessionState, scope hook.Scope, env *agentEnv) (ctxengine.ContextManager, hook.MemorySession, func(), error) {
	if a.spec.Session == nil {
		a.openSession(ctx, s, scope)
		env.cm, env.mem = s.cm, s.mem
		return s.cm, s.mem, func() {}, nil
	}
	sess, err := a.spec.Session(ctx, scope)
	if err != nil {
		if sess.Release != nil {
			sess.Release()
		}
		return nil, nil, nil, fmt.Errorf("session %q: %w", s.key, err)
	}
	// A nil Context is only for a turn a BeforeTurn answers: the engine
	// touches no session context then, and fails one that is not answered
	// with ErrNoSessionContext.
	env.cm, env.mem, env.amend, env.compact = sess.Context, sess.Memory, sess.Assemble, sess.Compaction
	release := sess.Release
	if release == nil {
		release = func() {}
	}
	return sess.Context, sess.Memory, release, nil
}

// openSession makes the session's context manager and memory session on its
// first turn.
func (a *Agent) openSession(ctx context.Context, s *sessionState, scope hook.Scope) {
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if s.cm == nil {
		opts := make([]ctxengine.Option, 0, 3+len(a.spec.Context))
		opts = append(opts, ctxengine.WithLogger(a.log), ctxengine.WithCompactionStartFunc(CompactionStart))
		if a.spec.Models != nil {
			opts = append(opts, ctxengine.WithModelCaller(a.spec.Models.Caller(models.Summarize, a.identity, "")))
		}
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

// ForgetSession drops what the agent keeps of an idle session: its own
// context manager and memory session (closed) and its entry. It reports
// false, and drops nothing, while the session has a turn running or runs
// queued, or is being forgotten by another call. A run given meanwhile
// waits until they are closed, then starts on new ones. The session store is
// not touched. With Spec.Session the agent
// keeps nothing of an idle session, and this only reports. After Close it
// does nothing.
func (a *Agent) ForgetSession(key string) bool {
	a.mu.Lock()
	s := a.states[key]
	if a.closed || s == nil {
		a.mu.Unlock()
		return true
	}
	if s.busy || len(s.running) > 0 {
		a.mu.Unlock()
		return false
	}
	// Held as busy while it closes, so a Run queues behind it and Close
	// waits for it.
	s.busy = true
	a.wg.Add(1)
	cm, mem := s.cm, s.mem
	a.mu.Unlock()
	func() {
		// A panic while closing must not leave the session busy for good.
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("panic closing a session", "component", "agent", "agent_id", a.id,
					"session", key, "panic", fmt.Sprintf("%v", p), "stack", string(debug.Stack()))
			}
		}()
		if cm != nil {
			if err := cm.Close(context.WithoutCancel(a.life)); err != nil {
				a.log.Warn("session close failed", "component", "agent", "agent_id", a.id,
					"session", key, "error", a.redact(err.Error()))
			}
		}
		if mem != nil {
			if err := mem.Close(); err != nil {
				a.log.Warn("memory close failed", "component", "agent", "agent_id", a.id,
					"session", key, "error", a.redact(err.Error()))
			}
		}
	}()
	a.mu.Lock()
	s.cm, s.mem = nil, nil
	if len(s.queue) > 0 {
		a.mu.Unlock()
		go a.serve(s)
		return true
	}
	s.busy = false
	delete(a.states, key)
	a.mu.Unlock()
	a.wg.Done()
	return true
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
	scope := a.scopeOf(s.key)
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

	scope := a.scopeOf(r.session)
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
	var cur []*run
	for _, r := range s.running {
		if !r.ended {
			cur = append(cur, r)
		}
	}
	a.mu.Unlock()

	for _, r := range queued {
		a.finishUnstarted(r.ctx, r, droppedResult(cause)) //nolint:contextcheck // a dropped run's consumers get its own context
	}
	scope := a.scopeOf(sessionKey)
	for _, r := range cur {
		r.cancel(cause)
		a.endServed(r)
		if len(cur) == 1 {
			scope.TurnID = r.turnID
		}
	}
	ev := hook.Interrupted{Scope: scope, Cause: cause, Running: len(cur) > 0, Dropped: len(queued)}
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
		for _, r := range s.running {
			if !r.ended {
				stopped = append(stopped, r)
			}
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
		Scope: a.scopeOf(""),
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
	a.states = nil // Close owns them now; ForgetSession leaves them alone
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
