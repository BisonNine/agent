package hook

import "time"

// The loop's default limits.
const (
	DefaultMaxIterations = 20
	DefaultToolTimeout   = 5 * time.Minute
	DefaultTurnBudget    = 15 * time.Minute
	DefaultCancelGrace   = 5 * time.Second
	DefaultRetries       = 2
	DefaultRetryBackoff  = 5 * time.Second
)

// NoIterations, as Limits.MaxIterations, lets a turn make no model request.
const NoIterations = -1

// NoTurnBudget, as Limits.TurnBudget, gives a turn no budget of the loop's.
const NoTurnBudget time.Duration = -1

// Limits are the loop's limits for one agent. A request's own limits (its
// max_tokens, context window, thinking level, timeout) are the model's,
// modelspec.Limits. A zero field takes its default.
type Limits struct {
	// MaxIterations is the most model requests one turn makes. On the last
	// one the model is told to answer and may not call tools. A negative
	// value (NoIterations) means none: the turn makes no model request and
	// ends empty, with finish reason "max_iterations".
	MaxIterations int
	// ToolTimeout is each tool call's budget; its context ends then.
	ToolTimeout time.Duration
	// TurnBudget is the whole turn's budget, the backstop for a model or
	// tool that never returns; when it runs out the turn ends with
	// ErrDeadline. A negative value (NoTurnBudget) means none: a host that
	// times its turns itself, on the context it gives Run.
	TurnBudget time.Duration
	// CancelGrace is how long an interrupted turn waits for its tools to
	// return before abandoning them (InterruptedResult).
	CancelGrace time.Duration
	// Retries is how many times one dispatch is retried after a timeout or
	// a context overflow. A negative value means none (the loop reads
	// max(Retries, 0) after WithDefaults).
	Retries int
	// RetryBackoff is the step of the wait before retrying a timed-out
	// dispatch: the n-th retry waits n steps, spread by 20% either way.
	RetryBackoff time.Duration
}

// WithDefaults returns l with each zero (or negative) field set to its
// default, except MaxIterations, Retries and TurnBudget: only a zero one takes the default,
// and a negative one is kept (none). Applying it twice gives the same limits.
func (l Limits) WithDefaults() Limits {
	if l.MaxIterations == 0 {
		l.MaxIterations = DefaultMaxIterations
	}
	if l.ToolTimeout <= 0 {
		l.ToolTimeout = DefaultToolTimeout
	}
	if l.TurnBudget == 0 {
		l.TurnBudget = DefaultTurnBudget
	}
	if l.CancelGrace <= 0 {
		l.CancelGrace = DefaultCancelGrace
	}
	if l.Retries == 0 {
		l.Retries = DefaultRetries
	}
	if l.RetryBackoff <= 0 {
		l.RetryBackoff = DefaultRetryBackoff
	}
	return l
}
