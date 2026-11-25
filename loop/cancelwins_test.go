/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

// A cancel wins (package hook, "Cancel wins"): an answer that comes back
// once the turn was stopped is discarded, and the turn ends as one stopped
// with its model call in flight. Every race is made deterministic by
// cancelling from a callback or an event consumer on the turn's goroutine.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// lateUsage is the usage every scripted answer of these tests reports.
var lateUsage = &spawnllm.UsageInfo{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150}

// lateReply is a direct answer with lateUsage; held, it waits for release
// (or the call's context).
func lateReply(release chan struct{}) modelstest.Step {
	return modelstest.Step{Hold: release, Response: &spawnllm.LLMResponse{Content: "late", FinishReason: "stop", Usage: lateUsage}}
}

// lateCalls is an answer calling the tool "refund", with lateUsage.
func lateCalls(release chan struct{}) modelstest.Step {
	return modelstest.Step{Hold: release, Response: &spawnllm.LLMResponse{
		ToolCalls: []spawnllm.ToolCall{toolCall("c1", "refund", map[string]any{"order": "42"})}, FinishReason: "tool_calls", Usage: lateUsage,
	}}
}

// stopTurn is a turn of the engine for Alice that a test stops from inside
// the turn. Stop cancels it with the test's cause; what it observed is kept.
type stopTurn struct {
	f      *engineFixture
	p      *modelstest.Provider
	h      *turnHandle
	cause  error
	ran    []string // tools run
	afters int      // AfterModel calls
	befs   int      // BeforeModel calls

	mu         sync.Mutex
	interrupts []hook.Interrupted
}

// newStopTurn is a turn over steps, storing its message, with the tool
// "refund", that stop stops with cause.
func newStopTurn(t *testing.T, cause error, steps ...modelstest.Step) *stopTurn {
	t.Helper()
	p := modelstest.New().Script("alice-wire", steps...)
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.options.AlreadyStored = false
	s := &stopTurn{f: f, p: p, cause: cause}
	f.spec.onHandle = func(h *turnHandle) { s.h = h }
	f.tool("refund", func(context.Context, map[string]any) *toolkit.Result {
		s.ran = append(s.ran, "refund")
		return toolkit.SilentResult("refunded")
	})
	f.spec.hooks.add(hook.Plugin{
		Name:        "probe",
		BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) { s.befs++; return nil, nil }, //nolint:nilnil // no answer
		AfterModel:  func(context.Context, *hook.ModelInfo, *hook.ModelResponse) { s.afters++ },
		OnInterrupt: func(_ context.Context, ev hook.Interrupted) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.interrupts = append(s.interrupts, ev)
		},
	})
	return s
}

// stop cancels the turn with the test's cause.
func (s *stopTurn) stop() { s.h.Cancel(s.cause) }

// stopOn stops the turn at the first event E that match accepts.
func stopOn[E hook.Event](s *stopTurn, match func(E) bool) {
	once := false
	s.f.spec.hooks.add(hook.Plugin{Name: "stopper", OnEvent: func(_ context.Context, ev hook.Event) {
		if e, ok := ev.(E); ok && !once && match(e) {
			once = true
			s.stop()
		}
	}})
}

// run runs the turn to its end.
func (s *stopTurn) run(t *testing.T) (hook.TurnResult, error) {
	t.Helper()
	_, _, err := s.f.run(t)
	return s.h.Wait(), err
}

// eventTypes names every event recorded, in order.
func eventTypes(rec *recorder) []string {
	evs := rec.all()
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("%T", ev))
	}
	return out
}

// storedRoles is what the session stored, by role.
func storedRoles(cm *fakeContext) []string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	out := make([]string, 0, len(cm.stored))
	for _, m := range cm.stored {
		out = append(out, m.Role)
	}
	return out
}

// firstModelCall is the first ModelCall recorded.
func firstModelCall(t *testing.T, rec *recorder) hook.ModelCall {
	t.Helper()
	for _, ev := range rec.all() {
		if c, ok := ev.(hook.ModelCall); ok {
			return c
		}
	}
	t.Fatal("no ModelCall")
	return hook.ModelCall{}
}

// A cancel that lands after the model answered, before the engine took the
// answer, discards it: the turn ends exactly as one cancelled with its call
// in flight (outcome, cause, error, events, OnInterrupt, what is stored and
// the pending marker), for a direct answer and for tool calls, for an
// interrupt and for shutdown. The ModelCall still carries the response and
// its usage, with no error: the call succeeded and was paid for.
func TestCancelWins_LateAnswerDiscarded(t *testing.T) {
	for _, answer := range []struct {
		name string
		step func(chan struct{}) modelstest.Step
	}{{"direct answer", lateReply}, {"tool calls", lateCalls}} {
		for _, cause := range []error{hook.ErrInterrupted, fmt.Errorf("closing: %w", hook.ErrShutdown)} {
			t.Run(answer.name+"/"+cause.Error(), func(t *testing.T) {
				// In flight: stopped as the call starts, so the held call
				// returns its context's error.
				inFlight := newStopTurn(t, cause, answer.step(make(chan struct{})))
				stopOn(inFlight, func(hook.ModelCallStarted) bool { return true })
				wantRes, wantErr := inFlight.run(t)

				// Late: the call returned its answer (not held), then the
				// turn was stopped as the ModelCall reported it.
				late := newStopTurn(t, cause, answer.step(nil))
				stopOn(late, func(hook.ModelCall) bool { return true })
				res, err := late.run(t)

				if res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, cause) || res.Reply != "" || res.Stored != "" {
					t.Fatalf("result = %+v, want cancelled by %v with no reply", res, cause)
				}
				if res.Outcome != wantRes.Outcome || fmt.Sprint(res.Cause) != fmt.Sprint(wantRes.Cause) ||
					res.FinishReason != wantRes.FinishReason || res.Iterations != wantRes.Iterations {
					t.Fatalf("result = %+v, want as in flight %+v", res, wantRes)
				}
				if err == nil || wantErr == nil || err.Error() != wantErr.Error() || !errors.Is(err, cause) {
					t.Fatalf("error = %v, want as in flight %v", err, wantErr)
				}
				if got, want := eventTypes(late.f.rec), eventTypes(inFlight.f.rec); !slices.Equal(got, want) {
					t.Fatalf("events = %v\nwant as in flight %v", got, want)
				}
				if got, want := storedRoles(late.f.cm), storedRoles(inFlight.f.cm); !slices.Equal(got, want) || !slices.Equal(got, []string{"user"}) {
					t.Fatalf("stored = %v, want as in flight %v: the user's message alone", got, want)
				}
				if got, want := late.f.store.events(), inFlight.f.store.events(); !slices.Equal(got, want) {
					t.Fatalf("store = %v, want as in flight %v", got, want)
				}
				if fmt.Sprint(late.interrupts) != fmt.Sprint(inFlight.interrupts) || len(late.interrupts) != 1 || !late.interrupts[0].Running {
					t.Fatalf("OnInterrupt = %+v, want as in flight %+v", late.interrupts, inFlight.interrupts)
				}
				if late.afters != 0 || len(late.ran) != 0 || len(late.p.Calls()) != 1 {
					t.Fatalf("AfterModel %d, tools run %v, model calls %d: want the answer discarded unseen",
						late.afters, late.ran, len(late.p.Calls()))
				}
				call := firstModelCall(t, late.f.rec)
				if call.Err != nil || call.Response == nil || call.Response.Usage == nil || *call.Response.Usage != *lateUsage {
					t.Fatalf("ModelCall = %+v, want the answer with its usage and no error", call)
				}
				if inCall := firstModelCall(t, inFlight.f.rec); inCall.Err == nil || inCall.Response != nil {
					t.Fatalf("the call in flight ended with %+v", inCall)
				}
			})
		}
	}
}

// A cancel in the window between the call's return and AfterModel (here,
// while the engine calibrates its prompt estimate) discards the answer
// before AfterModel sees it.
func TestCancelWins_BeforeAfterModel(t *testing.T) {
	s := newStopTurn(t, hook.ErrInterrupted, lateReply(nil))
	s.f.cm.estimate = 100
	s.f.cm.onObserve = s.stop
	res, err := s.run(t)
	if res.Outcome != hook.OutcomeCancelled || !errors.Is(err, hook.ErrInterrupted) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if s.afters != 0 {
		t.Fatalf("AfterModel ran %d times on a discarded answer", s.afters)
	}
	if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user"}) {
		t.Fatalf("stored = %v", got)
	}
}

// A cancel while AfterModel has the answer discards it: no ModelAnswer, no
// reply, nothing stored, no tool run.
func TestCancelWins_DuringAfterModel(t *testing.T) {
	for _, step := range []modelstest.Step{lateReply(nil), lateCalls(nil)} {
		s := newStopTurn(t, hook.ErrInterrupted, step)
		s.f.spec.hooks.add(hook.Plugin{Name: "stop", AfterModel: func(context.Context, *hook.ModelInfo, *hook.ModelResponse) { s.stop() }})
		res, err := s.run(t)
		if res.Outcome != hook.OutcomeCancelled || res.Reply != "" || !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("result = %+v, %v", res, err)
		}
		for _, ev := range s.f.rec.all() {
			if _, ok := ev.(hook.ModelAnswer); ok {
				t.Fatal("a discarded answer was reported as the dispatch's answer")
			}
		}
		if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user"}) || len(s.ran) != 0 {
			t.Fatalf("stored = %v, tools run %v", got, s.ran)
		}
	}
}

// A cancel after the answer was taken but before the turn's outcome was
// decided still wins. For a direct answer the reply is dropped and nothing
// stored; for tool calls the batch is not started.
func TestCancelWins_AfterAnswerTaken(t *testing.T) {
	t.Run("direct answer", func(t *testing.T) {
		s := newStopTurn(t, hook.ErrInterrupted, lateReply(nil))
		stopOn(s, func(hook.ModelAnswer) bool { return true })
		res, err := s.run(t)
		if res.Outcome != hook.OutcomeCancelled || res.Reply != "" || res.Stored != "" || !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user"}) {
			t.Fatalf("stored = %v", got)
		}
		if ev := s.f.store.events(); !slices.Contains(ev, "cleared") || slices.Contains(ev, "save") {
			t.Fatalf("store = %v, want the marker cleared and nothing saved", ev)
		}
	})
	t.Run("tool calls", func(t *testing.T) {
		s := newStopTurn(t, hook.ErrInterrupted, lateCalls(nil))
		stopOn(s, func(hook.ModelAnswer) bool { return true })
		res, err := s.run(t)
		if res.Outcome != hook.OutcomeCancelled || !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("result = %+v, %v", res, err)
		}
		for _, ev := range s.f.rec.all() {
			if _, ok := ev.(hook.ToolBatchStarted); ok {
				t.Fatal("a discarded batch was started")
			}
		}
		if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user"}) || len(s.ran) != 0 {
			t.Fatalf("stored = %v, tools run %v", got, s.ran)
		}
	})
}

// The boundary for tool calls: once the batch is announced it is the
// turn's. A cancel from then on stops the batch as any cancel during tools
// does: the call message and a result per call are stored, so the next
// turn can continue from the session.
func TestCancelWins_BatchStartedIsKept(t *testing.T) {
	s := newStopTurn(t, hook.ErrInterrupted, lateCalls(nil))
	stopOn(s, func(hook.ToolBatchStarted) bool { return true })
	res, err := s.run(t)
	if res.Outcome != hook.OutcomeCancelled || !errors.Is(err, hook.ErrInterrupted) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user", "assistant", "tool"}) {
		t.Fatalf("stored = %v, want the call message and its result", got)
	}
	if len(s.p.Calls()) != 1 || s.befs != 1 {
		t.Fatalf("model calls %d, BeforeModel %d: want no further dispatch", len(s.p.Calls()), s.befs)
	}
}

// A cancel between iterations, after the batch's results were stored,
// starts no further dispatch: no assembly, no BeforeModel, no model call.
// What the earlier iteration stored stays.
func TestCancelWins_BetweenIterations(t *testing.T) {
	s := newStopTurn(t, hook.ErrInterrupted, lateCalls(nil), lateReply(nil))
	stopOn(s, func(hook.ToolResultStored) bool { return true })
	res, err := s.run(t)
	if res.Outcome != hook.OutcomeCancelled || !errors.Is(err, hook.ErrInterrupted) || res.Iterations != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if s.befs != 1 || len(s.p.Calls()) != 1 || s.afters != 1 {
		t.Fatalf("BeforeModel %d, model calls %d, AfterModel %d: want one dispatch", s.befs, len(s.p.Calls()), s.afters)
	}
	if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user", "assistant", "tool"}) || !slices.Equal(s.ran, []string{"refund"}) {
		t.Fatalf("stored = %v, tools run %v", got, s.ran)
	}
	if n := countKind(s.f.rec, "ModelCall"); n != 1 {
		t.Fatalf("ModelCall %d times, want 1", n)
	}
}

// An answer a callback supplies once the turn was stopped is discarded as a
// model's is: from BeforeModel, and from OnModelError.
func TestCancelWins_SuppliedAnswerDiscarded(t *testing.T) {
	supplied := &spawnllm.LLMResponse{Content: "supplied"}
	t.Run("BeforeModel", func(t *testing.T) {
		s := newStopTurn(t, hook.ErrInterrupted)
		s.f.spec.hooks.add(hook.Plugin{Name: "answer", BeforeModel: func(context.Context, *hook.ModelInfo) (*spawnllm.LLMResponse, error) {
			s.stop()
			return supplied, nil
		}})
		res, err := s.run(t)
		if res.Outcome != hook.OutcomeCancelled || res.Reply != "" || !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if s.afters != 0 || len(s.p.Calls()) != 0 {
			t.Fatalf("AfterModel %d, model calls %d", s.afters, len(s.p.Calls()))
		}
	})
	t.Run("OnModelError", func(t *testing.T) {
		s := newStopTurn(t, hook.ErrInterrupted, modelstest.Fail(errors.New("bad request: refused")))
		s.f.spec.hooks.add(hook.Plugin{Name: "answer", OnModelError: func(context.Context, *hook.ModelInfo, error) (*spawnllm.LLMResponse, error) {
			s.stop()
			return supplied, nil
		}})
		res, err := s.run(t)
		if res.Outcome != hook.OutcomeCancelled || res.Reply != "" || !errors.Is(err, hook.ErrInterrupted) {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if s.afters != 0 {
			t.Fatalf("AfterModel ran %d times on a discarded answer", s.afters)
		}
		if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user"}) {
			t.Fatalf("stored = %v", got)
		}
	})
}

// A turn whose budget ran out before its loop's first dispatch makes none,
// and ends with the budget's error as a call it cut short would.
func TestCancelWins_BudgetSpentBeforeDispatch(t *testing.T) {
	s := newStopTurn(t, hook.ErrInterrupted, lateReply(nil))
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	turn := newEngineTurn(s.f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
	_, err := turn.run(ctx)
	res := s.h.Wait()
	if !errors.Is(err, context.DeadlineExceeded) || res.Outcome != hook.CauseOutcome(res.Cause) || !errors.Is(res.Cause, hook.ErrDeadline) {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if s.befs != 0 || len(s.p.Calls()) != 0 {
		t.Fatalf("BeforeModel %d, model calls %d: want no dispatch", s.befs, len(s.p.Calls()))
	}
}

// A cancel after the turn's outcome was decided changes nothing: from
// AfterTurn (the reply not yet stored) or once the reply was stored and
// handed on (TurnFinished), the turn ends OK with its reply stored and no
// cause.
func TestCancelWins_AfterOutcomeDecided(t *testing.T) {
	for _, where := range []string{"AfterTurn", "TurnFinished"} {
		t.Run(where, func(t *testing.T) {
			s := newStopTurn(t, hook.ErrInterrupted, lateReply(nil))
			if where == "AfterTurn" {
				s.f.spec.hooks.add(hook.Plugin{Name: "stop", AfterTurn: func(context.Context, *hook.Turn, *hook.TurnResult) { s.stop() }})
			} else {
				stopOn(s, func(hook.TurnFinished) bool { return true })
			}
			res, err := s.run(t)
			if err != nil || res.Outcome != hook.OutcomeOK || res.Reply != "late" || res.Cause != nil {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if got := storedRoles(s.f.cm); !slices.Equal(got, []string{"user", "assistant"}) {
				t.Fatalf("stored = %v, want the reply", got)
			}
		})
	}
}
