/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.bison9.com/agent/clock"
	"go.bison9.com/agent/cogmem/consolidate"
	"go.bison9.com/agent/cogmem/store"
)

// Placement says where a recalled block belongs in the model request.
type Placement int

const (
	// PlaceSystemStable: append to the system message. The block is the same on
	// every turn of the session, so it joins the cached prompt prefix.
	PlaceSystemStable Placement = iota
	// PlaceCurrentUser: fold into the latest user message. The block is
	// selected per turn and must not sit ahead of the history, or it
	// invalidates the cached prefix for all of it.
	PlaceCurrentUser
)

// Injection is one block Recall asks the host to place in the request.
type Injection struct {
	Placement Placement
	Text      string
}

// maxRecentTools bounds the recent-tool ring used for tool-trigger routing.
const maxRecentTools = 8

// maxRecentTexts is how many previous incoming messages keyword triggers are
// also matched against, so a topic named a turn or two ago stays loaded.
const maxRecentTexts = 3

// recentEntry is one entry of a session's rings: a tool name or a route text,
// and when it was last seen. An entry older than the carry-over
// (PromptSettings.CarryoverMinutes) no longer routes anything. The rings are
// bounded by count as well, but count alone is no bound in time: an agent
// that receives one message a day, or the same scheduled prompt over and over
// (a repeat moves to the front and pushes nothing out), would otherwise keep
// loading the domains of a message or tool call from weeks ago.
type recentEntry struct {
	value string
	at    time.Time
	// fresh marks a tool invoked since the last Recall: the use that marks
	// its domain active. Unused for texts.
	fresh bool
}

// SessionOptions describes one memory and how the host wants it driven.
type SessionOptions struct {
	// ID labels the memory in logs and run records; an agent id, typically.
	ID string
	// Dir is the directory cogmem owns for this memory. The store is
	// store.DBPath(Dir); WAL files and pre-migration snapshots sit beside it.
	// One memory per directory: a host that wants several memories for one
	// agent passes several directories.
	Dir string
	// Workspace is where consolidation reads the curated files and COGMEM.md.
	Workspace string
	// Ephemeral marks a throwaway memory (a sub-agent working on a snapshot):
	// nothing is observed into the inbox and no consolidation is scheduled.
	Ephemeral bool
	Settings  Settings
	// Loader resolves memory file attachments. Nil renders references as
	// plain markers with no content.
	Loader AttachmentLoader
	// Manager, when set, is nudged after every observed message so its
	// message-count trigger can fire.
	Manager *consolidate.Manager
	// OnOpen runs once, right after the store is first opened, before any
	// observe or recall. Hosts use it for one-time work such as backfilling
	// the inbox from an older record of the conversation.
	OnOpen func(ctx context.Context, st *store.Store)
	// StoreOptions are passed to store.Open, e.g. store.WithFileModes. Nil
	// uses the store defaults.
	StoreOptions []store.Option
	// Logger receives the session's records, tagged with the attribute
	// component "cogmem". Nil means silent.
	Logger *slog.Logger
	// Clock is read for the carry-over of earlier messages and tool uses and
	// for the recency window. Nil means the system clock.
	Clock clock.Clock
}

// Session is one agent session's view of cognitive memory: the host hands it a
// copy of every message (Observe), tells it which tools ran (RecordToolUse),
// and asks it for the blocks to place in the next request (Recall). It knows
// nothing about the host's context engine, and the engine knows nothing about
// it; the host's turn loop is the only thing that holds both.
//
// A nil *Session is valid and does nothing, so hosts need no guard for agents
// without cognitive memory.
type Session struct {
	opt    SessionOptions
	dbPath string

	mu     sync.Mutex
	st     *store.Store
	comp   *Composer
	opened bool

	recentMu    sync.Mutex
	recentTools []recentEntry
	// recentTexts holds the distinct route texts of previous Recalls, newest
	// first. It keeps one more than maxRecentTexts because the current text
	// may already be in it (a repeated Recall), and earlierTexts leaves that
	// one out yet should still find maxRecentTexts others.
	recentTexts []recentEntry
}

// now is the session's clock reading.
func (s *Session) now() time.Time { return clock.Or(s.opt.Clock).Now() }

// live reports whether an entry last seen at at still routes at now.
func (s *Session) live(at, now time.Time) bool {
	return now.Sub(at) <= s.opt.Settings.Prompt.carryover()
}

// log returns the session's logger tagged with its component, or one that
// discards when it has none.
func (s *Session) log() *slog.Logger {
	if s.opt.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.opt.Logger.With("component", "cogmem")
}

// NewSession builds a session. The store is opened lazily on first use.
func NewSession(opt SessionOptions) *Session {
	return &Session{opt: opt, dbPath: store.DBPath(opt.Dir)}
}

// Store returns the session's store, opening it on first call. Nil when the
// open failed (logged once) or the receiver is nil.
func (s *Session) Store() *store.Store {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened {
		return s.st
	}
	s.opened = true
	// cogmem owns Dir; Open creates it so a brand-new memory does not fail its
	// first write.
	st, err := store.Open(s.dbPath, s.opt.StoreOptions...)
	if err != nil {
		s.log().Warn("open session store failed",
			"id", s.opt.ID,
			"path", s.dbPath,
			"error", err.Error(),
		)
		return nil
	}
	s.st = st
	s.comp = New(st, append(s.opt.Settings.ComposerOptions(),
		WithAttachmentLoader(s.opt.Loader), WithClock(s.opt.Clock))...)
	if s.opt.OnOpen != nil && !s.opt.Ephemeral {
		s.opt.OnOpen(context.Background(), st)
	}
	return st
}

// Observe hands memory a copy of a message the host stored under seq. It
// lands in the inbox for the next consolidation run and nudges the run's
// message-count trigger. Best-effort: a failure is logged and never affects
// the host's turn.
func (s *Session) Observe(ctx context.Context, seq int64, role, text string) {
	if s == nil || s.opt.Ephemeral || seq <= 0 {
		return
	}
	st := s.Store() //nolint:contextcheck // Store opens lazily with a background context by design (OnOpen must outlive this call); Store has no ctx parameter.
	if st == nil {
		return
	}
	stored, err := consolidate.Observe(ctx, st, seq, role, text, s.opt.Settings.Consolidation.PerMessageChars)
	if err != nil {
		s.log().WarnContext(ctx, "observe message failed",
			"id", s.opt.ID, "seq", seq, "error", err.Error(),
		)
		return
	}
	if stored && s.opt.Manager != nil {
		s.opt.Manager.OnMessage(consolidate.Job{
			ID:        s.opt.ID,
			Dir:       s.opt.Dir,
			Workspace: s.opt.Workspace,
		})
	}
}

// RecordToolUse feeds the recent-tool ring (newest-first, deduped, capped) so
// Recall can auto-load domains whose triggers match a tool the agent just used.
// A tool stays in the ring for the carry-over, counted from its latest use.
func (s *Session) RecordToolUse(names ...string) {
	if s == nil || len(names) == 0 {
		return
	}
	now := s.now()
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	for _, n := range names {
		if n == "" {
			continue
		}
		ring := make([]recentEntry, 0, len(s.recentTools)+1)
		ring = append(ring, recentEntry{value: n, at: now, fresh: true})
		for _, e := range s.recentTools {
			if e.value != n && s.live(e.at, now) {
				ring = append(ring, e)
			}
		}
		s.recentTools = ring
	}
	if len(s.recentTools) > maxRecentTools {
		s.recentTools = s.recentTools[:maxRecentTools]
	}
}

// RecentTools returns the names in the recent-tool ring, newest first: the
// tools used within the carry-over.
func (s *Session) RecentTools() []string {
	if s == nil {
		return nil
	}
	now := s.now()
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	var out []string
	for _, e := range s.recentTools {
		if s.live(e.at, now) {
			out = append(out, e.value)
		}
	}
	return out
}

// takeTools returns RecentTools and, of those, the tools used since the
// previous call. They are handed out as used once: a second Recall for the
// same tool call sees the tool as carried over.
func (s *Session) takeTools() (recent, used []string) {
	now := s.now()
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	for i, e := range s.recentTools {
		if !s.live(e.at, now) {
			continue
		}
		recent = append(recent, e.value)
		if e.fresh {
			used = append(used, e.value)
			s.recentTools[i].fresh = false
		}
	}
	return recent, used
}

// earlierTexts returns up to maxRecentTexts previous route texts, newest first,
// leaving out any entry equal to routeText: the same text again (a repeated
// Recall for one message, or the user repeating themselves) is the current
// message, not an earlier one. Texts older than the carry-over are left out.
func (s *Session) earlierTexts(routeText string) []string {
	now := s.now()
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	var out []string
	for _, e := range s.recentTexts {
		if e.value == routeText || !s.live(e.at, now) {
			continue
		}
		out = append(out, e.value)
		if len(out) == maxRecentTexts {
			break
		}
	}
	return out
}

// recordRouteText puts routeText at the front of the recent-text ring, moving
// it there if it is already in the ring, so each text appears once. Texts
// older than the carry-over are dropped. Blank text is not recorded.
func (s *Session) recordRouteText(routeText string) {
	if strings.TrimSpace(routeText) == "" {
		return
	}
	now := s.now()
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	ring := make([]recentEntry, 0, maxRecentTexts+1)
	ring = append(ring, recentEntry{value: routeText, at: now})
	for _, e := range s.recentTexts {
		if e.value != routeText && s.live(e.at, now) && len(ring) < maxRecentTexts+1 {
			ring = append(ring, e)
		}
	}
	s.recentTexts = ring
}

// Recall returns the blocks to place in the next request: the STABLE block
// (sticky domains, topic index, their attached documents) for the system
// message, and the ROUTED block (domains selected from routeText, the route
// texts and tools of the last while, and what was active lately, with their
// documents) for the current turn. Nil when there is nothing to inject or the store could not be opened.
func (s *Session) Recall(ctx context.Context, routeText string) []Injection {
	if s == nil || s.Store() == nil { //nolint:contextcheck // Store opens lazily with a background context by design (OnOpen must outlive this call); Store has no ctx parameter.
		return nil
	}
	// The composer is read under the lock: Close nils it under the same lock,
	// so a Recall racing a Close sees either a live composer or nil.
	s.mu.Lock()
	comp := s.comp
	s.mu.Unlock()
	if comp == nil {
		return nil
	}
	recentTools, usedTools := s.takeTools()
	res, err := comp.Compose(ctx, RouteRequest{
		RecentTools: recentTools,
		UsedTools:   usedTools,
		RouteText:   routeText,
		RecentTexts: s.earlierTexts(routeText),
		Trace:       s.opt.Settings.Prompt.IncludeDebugTrace,
	})
	// After composing, so this message is an earlier one only from the next.
	s.recordRouteText(routeText)
	if err != nil {
		s.log().WarnContext(ctx, "memory compose failed",
			"id", s.opt.ID, "error", err.Error(),
		)
	}
	if res.Attachments != "" || res.RoutedAttachments != "" {
		// Split by provenance: sticky bytes ride in the cached prompt and are
		// paid for once, routed bytes ride with the turn and are paid for
		// every time. One combined figure hides which is which.
		s.log().DebugContext(ctx, "attached documents injected",
			"id", s.opt.ID,
			"sticky_bytes", len(res.Attachments),
			"routed_bytes", len(res.RoutedAttachments),
		)
	}
	// Sticky documents belong with the stable block; routed documents belong
	// with the routed block, whose memory ids their headers cite.
	var out []Injection
	if stable := joinBlocks(res.Stable, res.Attachments); stable != "" {
		out = append(out, Injection{Placement: PlaceSystemStable, Text: stable})
	}
	if routed := joinBlocks(res.Routed, res.RoutedAttachments); routed != "" {
		out = append(out, Injection{Placement: PlaceCurrentUser, Text: routed})
	}
	return out
}

// Close releases the store handle. Safe on nil and when never opened.
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		_ = s.st.Close()
		s.st = nil
		s.comp = nil
	}
}

// joinBlocks concatenates non-empty prompt blocks with the separator used
// throughout a system prompt.
func joinBlocks(blocks ...string) string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b != "" {
			out = append(out, b)
		}
	}
	return strings.Join(out, "\n\n---\n\n")
}
