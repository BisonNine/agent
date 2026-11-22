package models_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/spawnllm/openai_compat"
)

// The single-shot walk summaries and memory consolidation use: excluded,
// disabled and parked models are skipped, any error moves on, and the reply
// names the wire model that gave it.
func TestCallerComplete(t *testing.T) {
	f := newFixture(t, append(three, m("opus", "anthropic", "claude-opus")))
	set := f.cat.Default()
	f.park(t, "gpt", 429)
	if err := f.cat.Disable("haiku"); err != nil {
		t.Fatal(err)
	}
	f.fake.Script("claude-sonnet", modelstest.Fail(errors.New("completely unknown internal error")))
	f.fake.Script("claude-opus", modelstest.Step{Response: &spawnllm.LLMResponse{Content: "{}", FinishReason: "stop"}})

	caller := set.Caller(models.Chat, alice, "")
	reply, err := caller.Complete(t.Context(), models.CompleteRequest{System: "sys", User: "usr", JSONObject: true})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Model != "claude-opus" || reply.Content != "{}" || reply.FinishReason != "stop" {
		t.Fatalf("reply = %+v", reply)
	}
	calls := f.fake.Calls()
	if got := f.fake.Models(); !slices.Equal(got, []string{"claude-sonnet", "claude-opus"}) {
		t.Fatalf("calls = %q", got)
	}
	last := calls[1]
	if last.Messages[0].Role != "system" || last.Messages[0].Content != "sys" || last.Messages[1].Content != "usr" ||
		last.Options[openai_compat.ResponseFormatJSONObjectOption] != true || last.Identity != alice {
		t.Fatalf("call = %+v", last)
	}

	f.fake.Reset()
	_, err = caller.Complete(t.Context(), models.CompleteRequest{Exclude: []string{"claude-opus"}})
	if err == nil || !strings.Contains(err.Error(), "unknown internal error") {
		t.Fatalf("err = %v, want the last model's", err)
	}

	_, err = caller.Complete(t.Context(), models.CompleteRequest{Exclude: []string{"claude-opus", "claude-sonnet"}})
	if !errors.Is(err, models.ErrNoModelAvailable) || !strings.Contains(err.Error(), "2 excluded, 1 in cooldown, 1 disabled") {
		t.Fatalf("err = %v", err)
	}
}

// A failure on the walk is recorded against the model's health, shared with
// the chat chains.
func TestCallerRecordsHealth(t *testing.T) {
	f := newFixture(t, three)
	f.fake.Script("claude-sonnet", modelstest.Status(402, 0))
	reply, err := f.cat.Default().Begin(models.Chat).Caller(alice).Complete(t.Context(), models.CompleteRequest{})
	if err != nil || reply.Model != "gpt-5" {
		t.Fatalf("%v %+v", err, reply)
	}
	if h, _ := f.cat.Get("sonnet"); h.Health.Available {
		t.Fatal("the failure did not park the model")
	}
	if len(f.obs.parked) != 1 {
		t.Fatalf("parked events = %d", len(f.obs.parked))
	}
}

// Set.Caller begins a chain for each call, so it follows selections.
func TestSetCallerFollowsSelection(t *testing.T) {
	f := newFixture(t, three)
	caller := f.cat.Default().Caller(models.Chat, alice, "s")
	if r, err := caller.Complete(t.Context(), models.CompleteRequest{}); err != nil || r.Model != "claude-sonnet" {
		t.Fatalf("first = %+v, %v", r, err)
	}
	if err := f.cat.Select(models.Chat, "haiku", "s"); err != nil {
		t.Fatal(err)
	}
	if r, err := caller.Complete(t.Context(), models.CompleteRequest{}); err != nil || r.Model != "claude-haiku" {
		t.Fatalf("after select = %+v, %v", r, err)
	}
	if _, err := f.cat.Default().Caller(models.Chat, accounting.Identity{}, "").Complete(context.Background(), models.CompleteRequest{}); err == nil {
		t.Fatal("a call without an agent was made")
	}
}
