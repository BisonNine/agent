// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"

	"go.bison9.com/agent/hook"
)

// Memory is an agent's long-term memory as the agent loop takes it
// (hook.Memory): open returns the cogmem session for an agent's session,
// built the way the host builds its sessions (its store, its settings). A nil
// session with a nil error is a session without memory.
type Memory func(ctx context.Context, s hook.Scope) (*Session, error)

// Open opens the memory of the session s names.
func (m Memory) Open(ctx context.Context, s hook.Scope) (hook.MemorySession, error) {
	sess, err := m(ctx, s)
	if err != nil {
		return nil, err
	}
	return AsMemorySession(sess), nil
}

// AsMemorySession is s as the agent loop uses a session's memory
// (hook.MemorySession). A nil s does nothing.
func AsMemorySession(s *Session) hook.MemorySession { return memorySession{s: s} }

// memorySession adapts a *Session to hook.MemorySession.
type memorySession struct{ s *Session }

func (m memorySession) Recall(ctx context.Context, text string) []hook.Recalled {
	in := m.s.Recall(ctx, text)
	if len(in) == 0 {
		return nil
	}
	out := make([]hook.Recalled, 0, len(in))
	for _, r := range in {
		p := hook.PlaceSystem
		if r.Placement == PlaceCurrentUser {
			p = hook.PlaceCurrentUser
		}
		out = append(out, hook.Recalled{Placement: p, Text: r.Text})
	}
	return out
}

func (m memorySession) Observe(ctx context.Context, seq int64, role, content string) {
	m.s.Observe(ctx, seq, role, content)
}

func (m memorySession) ToolsUsed(_ context.Context, names ...string) { m.s.RecordToolUse(names...) }

func (m memorySession) Close() error {
	m.s.Close()
	return nil
}
