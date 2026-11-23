package loop

import (
	"context"
	"testing"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/models/modelstest"
)

// held is how many events the run's handle holds for Events.
func (r *run) held() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.events {
		if ev != nil {
			n++
		}
	}
	return n
}

// A run WithoutEvents holds no event while its turn runs and only its
// TurnFinished after: a host that only calls Wait keeps no request,
// response or tool result. OnEvent consumers still see every event, and
// Events yields TurnFinished alone. A run without the option holds its
// events until they are read.
func TestRun_WithoutEventsHoldsNone(t *testing.T) {
	tests := []struct {
		name     string
		opts     []hook.RunOption
		wantMid  bool // events held while the tool runs
		wantLast int  // events held once the turn finished
	}{
		{name: "default holds until read", wantMid: true, wantLast: -1},
		{name: "WithoutEvents holds none", opts: []hook.RunOption{hook.WithoutEvents()}, wantLast: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAgentFixture(t)
			handle := make(chan *run, 1)
			heldMid := -1
			f.tool(t, "lookup", func(context.Context, map[string]any) (string, error) {
				r := <-handle
				heldMid = r.held()
				handle <- r
				return "found", nil
			})
			f.p.Script("w1", calls("lookup"), modelstest.Reply("done"))
			observed := 0
			a := f.agent(t, func(s *Spec) {
				s.Plugins = []hook.Plugin{{Name: "observer", OnEvent: func(context.Context, hook.Event) { observed++ }}}
			})
			r, ok := a.Run(context.Background(), "s1", hook.UserText("look"), tt.opts...).(*run)
			if !ok {
				t.Fatal("Run did not return a *run")
			}
			handle <- r
			if res := r.Wait(); res.Outcome != hook.OutcomeOK || res.Reply != "done" {
				t.Fatalf("result = %+v", res)
			}
			if (heldMid > 0) != tt.wantMid {
				t.Fatalf("events held while the tool ran = %d, want held: %v", heldMid, tt.wantMid)
			}
			if tt.wantLast >= 0 && r.held() != tt.wantLast {
				t.Fatalf("events held after the turn = %d, want %d", r.held(), tt.wantLast)
			}
			if observed < 5 {
				t.Fatalf("OnEvent saw %d events, want every event of the turn", observed)
			}
			var got []hook.Event
			for ev, err := range r.Events() {
				if _, last := ev.(hook.TurnFinished); !last && err != nil {
					t.Fatalf("Events: %v before TurnFinished", err)
				}
				got = append(got, ev)
			}
			if _, last := got[len(got)-1].(hook.TurnFinished); !last {
				t.Fatalf("Events ended with %T, want TurnFinished", got[len(got)-1])
			}
			if len(tt.opts) > 0 && len(got) != 1 {
				t.Fatalf("Events yielded %d events, want TurnFinished alone", len(got))
			}
			if len(tt.opts) == 0 && len(got) != observed {
				t.Fatalf("Events yielded %d events, OnEvent saw %d", len(got), observed)
			}
		})
	}
}
