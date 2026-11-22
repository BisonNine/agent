package hook

import "context"

// Memory is an agent's optional long-term memory. The engine opens one
// MemorySession per session it holds and closes it when it lets the session
// go; without a Memory none of this happens.
type Memory interface {
	// Open returns the memory for the session s names (s.AgentID and
	// s.Session). An error is logged and the session runs without memory.
	Open(ctx context.Context, s Scope) (MemorySession, error)
}

// MemorySession is one session's memory, as the engine uses it. The engine
// calls it from one turn at a time.
type MemorySession interface {
	// Recall returns the blocks to place in the request for a turn that
	// answers text, in order. It is called for every dispatch.
	Recall(ctx context.Context, text string) []Recalled
	// Observe records a message the engine stored, under the transcript
	// sequence number the session gave it.
	Observe(ctx context.Context, seq int64, role, content string)
	// ToolsUsed records the tools a model response called, before they run,
	// so memory tied to them can be recalled in the turn's next dispatch.
	ToolsUsed(names ...string)
	// Close releases the session's memory.
	Close() error
}

// Placement says where a recalled block goes in the request.
type Placement int

// The placements.
const (
	// PlaceSystem appends the block to the system message: for content
	// that is the same on every turn of a session, so the prompt cache
	// keeps it.
	PlaceSystem Placement = iota
	// PlaceCurrentUser folds the block into the latest user message: for
	// content chosen per turn.
	PlaceCurrentUser
)

// Recalled is one block a memory returns for a request.
type Recalled struct {
	Placement Placement
	Text      string
}

// Redactor returns s with secrets (keys, tokens) replaced. The engine applies
// the agent's redactor to the error text and the content it logs; the errors
// it returns (TurnResult.Err) are left as they are, for the host to render.
type Redactor func(s string) string
