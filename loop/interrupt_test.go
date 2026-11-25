/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package loop

// The interrupt rules of the turn engine (plan 5.2), on the fake clock.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/toolkit"
)

// startTurn runs turn in the background and returns its error channel.
func startTurn(turn *engineTurn) chan error {
	done := make(chan error, 1)
	go func() { _, err := turn.run(context.Background()); done <- err }()
	return done
}

// waitHandle waits for the turn's handle (set as the turn starts).
func waitHandle(t *testing.T, got chan *turnHandle) *turnHandle {
	t.Helper()
	select {
	case h := <-got:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("the turn gave no handle")
		return nil
	}
}

// countKind counts the recorded events of kind.
func countKind(rec *recorder, kind string) int {
	n := 0
	for _, k := range rec.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// Rule 1 and 6: an interrupt during the model call aborts it; the turn ends
// with exactly one TurnFinished, cancelled, AfterTurn runs on a live
// context, and OnInterrupt is told a running turn was stopped.
func TestInterrupt_DuringModelCall(t *testing.T) {
	hold, started := make(chan struct{}), make(chan struct{})
	p := modelstest.New().Script("alice-wire", modelstest.Step{Hold: hold, Started: started})
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	var afterTurnErr error
	var interrupts []hook.Interrupted
	var mu sync.Mutex
	f.spec.hooks.add(hook.Plugin{
		Name:      "probe",
		AfterTurn: func(ctx context.Context, _ *hook.Turn, _ *hook.TurnResult) { afterTurnErr = ctx.Err() },
		OnInterrupt: func(_ context.Context, ev hook.Interrupted) {
			mu.Lock()
			interrupts = append(interrupts, ev)
			mu.Unlock()
		},
	})
	handles := make(chan *turnHandle, 1)
	f.spec.onHandle = func(h *turnHandle) { handles <- h }
	turn := newEngineTurn(f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
	done := startTurn(turn)
	h := waitHandle(t, handles)
	<-started
	h.Interrupt()
	if err := <-done; err == nil {
		t.Fatal("the interrupted turn reported no error")
	}
	res := h.Wait()
	if res.Outcome != hook.OutcomeCancelled || !errors.Is(res.Cause, hook.ErrInterrupted) {
		t.Fatalf("result = %+v, want cancelled by an interrupt", res)
	}
	if n := countKind(f.rec, "TurnFinished"); n != 1 {
		t.Fatalf("TurnFinished %d times, want 1", n)
	}
	if afterTurnErr != nil {
		t.Fatalf("AfterTurn ran on a cancelled context: %v", afterTurnErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(interrupts) != 1 || !interrupts[0].Running || !errors.Is(interrupts[0].Cause, hook.ErrInterrupted) {
		t.Fatalf("OnInterrupt = %+v, want one for a running turn", interrupts)
	}
}

// Rule 2: an interrupt during a tool cancels the tool's context. A tool
// that returns within the grace keeps its result; one that does not is
// abandoned when the grace runs out, with the synthetic result stored, so
// no stored call is without a result, and what it returns later is
// dropped.
func TestInterrupt_DuringToolAbandonsAfterGrace(t *testing.T) {
	p := modelstest.New().Script("alice-wire",
		callStep(toolCall("c1", "polite", nil), toolCall("c2", "stubborn", nil)),
		modelstest.Reply("never asked"))
	clk := newFake()
	_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
	f := newEngineFixture(t, chain, clk)
	f.spec.cancelGrace = 3 * time.Second
	running := make(chan struct{}, 2)
	unblock := make(chan struct{})
	stubbornDone := make(chan struct{})
	f.tool("polite", func(ctx context.Context, _ map[string]any) *toolkit.Result {
		running <- struct{}{}
		<-ctx.Done()
		return toolkit.ErrorResult("stopped: " + ctx.Err().Error())
	})
	f.tool("stubborn", func(context.Context, map[string]any) *toolkit.Result {
		defer close(stubbornDone)
		running <- struct{}{}
		<-unblock
		return toolkit.SilentResult("too late")
	})
	handles := make(chan *turnHandle, 1)
	f.spec.onHandle = func(h *turnHandle) { handles <- h }
	turn := newEngineTurn(f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
	done := startTurn(turn)
	h := waitHandle(t, handles)
	<-running
	<-running
	h.Interrupt()
	// The turn waits the grace on the clock: nothing ends before it.
	if err := clk.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// The polite tool returns on its cancelled context, within the grace.
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(f.rec.all(), func(ev hook.Event) bool {
		fin, ok := ev.(hook.ToolCallFinished)
		return ok && fin.Call.ID == "c1"
	}) {
		if time.Now().After(deadline) {
			t.Fatal("the polite tool never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("the turn ended before the grace ran out: %v", err)
	default:
	}
	clk.Advance(3 * time.Second)
	if err := <-done; err == nil {
		t.Fatal("the interrupted turn reported no error")
	}
	close(unblock)
	<-stubbornDone

	var results []string
	for _, m := range f.cm.stored {
		if m.Role == "tool" {
			results = append(results, m.ToolCallID+"="+m.Content)
		}
	}
	want := []string{"c1=stopped: context canceled", "c2=" + hook.InterruptedText}
	if !slices.Equal(results, want) {
		t.Fatalf("stored results = %v, want %v", results, want)
	}
	var interrupted []string
	for _, ev := range f.rec.all() {
		if fin, ok := ev.(hook.ToolCallFinished); ok && fin.Interrupted {
			interrupted = append(interrupted, fin.Call.ID)
		}
	}
	if !slices.Equal(interrupted, []string{"c2"}) {
		t.Fatalf("abandoned calls = %v, want [c2]", interrupted)
	}
	if n := countKind(f.rec, "ToolCallFinished"); n != 2 {
		t.Fatalf("ToolCallFinished %d times, want 2 (the late return dropped)", n)
	}
	if n := countKind(f.rec, "TurnFinished"); n != 1 {
		t.Fatalf("TurnFinished %d times, want 1", n)
	}
}

// Rule 7: the cause is recorded; a turn stopped by shutdown keeps its
// pending marker for restart recovery, an interrupted one does not.
func TestInterrupt_CauseAndPendingMarker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cause     error
		wantClear bool
	}{
		{"interrupt", hook.ErrInterrupted, true},
		{"superseded", hook.ErrSuperseded, true},
		{"shutdown", hook.ErrShutdown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold, started := make(chan struct{}), make(chan struct{})
			p := modelstest.New().Script("alice-wire", modelstest.Step{Hold: hold, Started: started})
			clk := newFake()
			_, chain := testChain(t, clk, engineModel{name: "Alice Model", wire: "alice-wire", client: p})
			f := newEngineFixture(t, chain, clk)
			f.spec.options.AlreadyStored = false
			handles := make(chan *turnHandle, 1)
			f.spec.onHandle = func(h *turnHandle) { handles <- h }
			turn := newEngineTurn(f.spec, []spawnllm.Message{{Role: "user", Content: "hello"}})
			done := startTurn(turn)
			h := waitHandle(t, handles)
			<-started
			h.Cancel(tc.cause)
			if err := <-done; err == nil {
				t.Fatal("the cancelled turn reported no error")
			}
			if res := h.Wait(); !errors.Is(res.Cause, tc.cause) || res.Outcome != hook.OutcomeCancelled {
				t.Fatalf("result = %+v, want cancelled by %v", res, tc.cause)
			}
			if cleared := slices.Contains(f.store.events(), "cleared"); cleared != tc.wantClear {
				t.Fatalf("store = %v, marker cleared = %v, want %v", f.store.events(), cleared, tc.wantClear)
			}
		})
	}
}
