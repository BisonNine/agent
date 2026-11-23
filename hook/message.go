package hook

import "time"

// Message is what a turn answers: the text someone sent and what came with
// it. It is not a provider message; the engine turns it into the user
// message it stores. Host-specific facts about the message (its chat, its
// id, whether a reply is owed) travel on the context passed to Run, in the
// host's own keys.
type Message struct {
	// Text is the message as the model should read it.
	Text string
	// Media are references to attachments (media-store refs, data URLs),
	// in order.
	Media []string
	// Source names the sender, for the stored message; empty for none.
	Source string
	// Kind classifies the message for the session (the stored message's
	// type, such as "callback" for a background result); empty for an
	// ordinary message.
	Kind string
}

// UserText is a message of text alone.
func UserText(text string) Message {
	return Message{Text: text}
}

// RunOptions are the settings of one Run. The zero value is an ordinary turn.
type RunOptions struct {
	// AlreadyStored: the message is already in the session (a retry of a
	// turn that failed), so it is not stored again.
	AlreadyStored bool
	// ResetSession: the session's context is reset (its history archived,
	// not deleted) before the message is stored.
	ResetSession bool
	// TurnBudget replaces Limits.TurnBudget for this turn when positive.
	TurnBudget time.Duration
	// Depth is the sub-agent depth the turn runs at (Scope.Depth).
	Depth int
	// TurnID is the turn's id (Scope.TurnID) when the host made one before
	// the run (for its own logs and records); empty means the agent makes
	// one.
	TurnID string
}

// RunOption sets one of the RunOptions.
type RunOption func(*RunOptions)

// AlreadyStored marks a message that is already in the session.
func AlreadyStored() RunOption { return func(o *RunOptions) { o.AlreadyStored = true } }

// ResetSession resets the session's context before the turn.
func ResetSession() RunOption { return func(o *RunOptions) { o.ResetSession = true } }

// TurnBudget gives the turn its own budget, replacing Limits.TurnBudget.
func TurnBudget(d time.Duration) RunOption { return func(o *RunOptions) { o.TurnBudget = d } }

// AtDepth runs the turn at sub-agent depth n.
func AtDepth(n int) RunOption { return func(o *RunOptions) { o.Depth = n } }

// WithTurnID gives the turn the id the host made for it.
func WithTurnID(id string) RunOption { return func(o *RunOptions) { o.TurnID = id } }

// ApplyRunOptions returns the RunOptions opts set, in order. A nil option is
// skipped.
func ApplyRunOptions(opts ...RunOption) RunOptions {
	var o RunOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return o
}
