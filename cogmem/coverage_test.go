/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.bison9.com/agent/cogmem/store"
	"go.bison9.com/agent/hook"
)

// The hook adapter keeps each injection's placement: a sticky memory goes to
// the system prompt, a routed one to the current user turn.
func TestAsMemorySession_RecallKeepsPlacement(t *testing.T) {
	s := NewSession(SessionOptions{ID: "alice", Dir: filepath.Join(t.TempDir(), "cogmem")})
	defer s.Close()
	st := s.Store()
	mustMemory(t, st, store.AddMemoryParams{DomainID: mustGeneral(t, st).ID, Type: store.TypeRule, Text: "always sign off as Alice"})
	email := mustDomain(t, st, store.CreateDomainParams{Name: "Email", Triggers: "gmail"})
	mustMemory(t, st, store.AddMemoryParams{DomainID: email.ID, Type: store.TypePreference, Text: "Archive newsletters."})
	s.RecordToolUse("mcp_gmail_list")

	got := AsMemorySession(s).Recall(context.Background(), "hello")
	var system, user []string
	for _, r := range got {
		switch r.Placement {
		case hook.PlaceSystem:
			system = append(system, r.Text)
		case hook.PlaceCurrentUser:
			user = append(user, r.Text)
		}
	}
	if len(system) != 1 || !strings.Contains(system[0], "always sign off as Alice") {
		t.Errorf("system injections = %q, want the sticky rule", system)
	}
	if len(user) != 1 || !strings.Contains(user[0], "Archive newsletters.") {
		t.Errorf("current-user injections = %q, want the routed preference", user)
	}
}

func TestGuidance_DescribesTheTools(t *testing.T) {
	g := Guidance()
	for _, want := range []string{"cogmem_memory_create", "cogmem_memory_search", "**Event memories are NEVER loaded into your context.**"} {
		if !strings.Contains(g, want) {
			t.Errorf("guidance lacks %q", want)
		}
	}
}

// A memory stored without a type reads as a fact.
func TestTypePrefix(t *testing.T) {
	if got := typePrefix(""); got != "(fact)" {
		t.Errorf("typePrefix(\"\") = %q, want (fact)", got)
	}
	if got := typePrefix(store.TypeRule); got != "(rule)" {
		t.Errorf("typePrefix(rule) = %q, want (rule)", got)
	}
}

// A document owned by a memory with no text names the memory by id alone.
func TestProvenance_NoText(t *testing.T) {
	if got := provenance([]refSite{{memoryID: "m1", text: "  "}}); got != "From memory m1" {
		t.Errorf("provenance = %q, want From memory m1", got)
	}
}

// Sticky domains load in descending priority (kept in the legacy type column),
// whatever their names.
func TestCompose_StickyPriorityOrder(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	general := mustGeneral(t, st)
	mustMemory(t, st, store.AddMemoryParams{DomainID: general.ID, Type: store.TypeFact, Text: "general fact"})
	ops := mustDomain(t, st, store.CreateDomainParams{Name: "Zulu ops", Sticky: true})
	mustMemory(t, st, store.AddMemoryParams{DomainID: ops.ID, Type: store.TypeFact, Text: "ops fact"})
	if _, err := st.DB().ExecContext(ctx, `UPDATE domains SET type = '5' WHERE id = ?`, ops.ID); err != nil {
		t.Fatal(err)
	}

	res, err := New(st).Compose(ctx, RouteRequest{})
	if err != nil {
		t.Fatal(err)
	}
	zulu, gen := strings.Index(res.Stable, "ops fact"), strings.Index(res.Stable, "general fact")
	if zulu < 0 || gen < 0 || zulu > gen {
		t.Errorf("stable block does not put the higher-priority domain first:\n%s", res.Stable)
	}
}

// A store that fails under the composer is reported, not rendered as an empty
// memory.
func TestCompose_ClosedStoreFails(t *testing.T) {
	st := newStore(t)
	comp := New(st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := comp.Compose(context.Background(), RouteRequest{RouteText: "hello"}); err == nil {
		t.Fatal("Compose on a closed store succeeded")
	}
}

// The session's records carry the component attribute.
func TestSession_LogsWithComponent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s := NewSession(SessionOptions{
		ID: "alice", Dir: filepath.Join(parent, "cogmem"),
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	defer s.Close()
	if s.Store() != nil {
		t.Fatal("store opened under a regular file")
	}
	if out := buf.String(); !strings.Contains(out, "open session store failed") || !strings.Contains(out, "component=cogmem") {
		t.Errorf("log = %q, want the open failure tagged component=cogmem", out)
	}
}
