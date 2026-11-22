package hook_test

import (
	"context"
	"errors"
	"fmt"

	"go.bison9.com/agent/hook"
	"go.bison9.com/agent/toolkit"
)

// A BeforeTool callback that changes a step: refunds above a limit are
// refused without running the tool, and order lookups for a known order are
// answered from a cache instead of the tool. Every other call runs as the
// model asked.
func ExampleBeforeToolFunc() {
	cache := map[string]string{"A-1001": "Order A-1001 shipped on 20 November."}
	var before hook.BeforeToolFunc = func(_ context.Context, call *hook.ToolCall) (*toolkit.Result, error) {
		switch call.Name {
		case "refund":
			if amount, ok := call.Arguments["amount"].(float64); ok && amount > 100 {
				return nil, toolkit.Refusal(errors.New("refunds over 100 need a person's approval"))
			}
		case "lookup_order":
			if text, ok := cache[fmt.Sprint(call.Arguments["order"])]; ok {
				return toolkit.NewResult(text), nil // the tool is not called
			}
		}
		return nil, nil //nolint:nilnil // (nil, nil): run the tool
	}

	ctx := hook.WithScope(context.Background(), hook.Scope{AgentID: "alice", Session: "s-42"})
	for _, call := range []hook.ToolCall{
		{ID: "1", Name: "refund", Arguments: map[string]any{"amount": 250.0}},
		{ID: "2", Name: "lookup_order", Arguments: map[string]any{"order": "A-1001"}},
		{ID: "3", Name: "lookup_order", Arguments: map[string]any{"order": "B-2002"}},
	} {
		res, err := before(ctx, &call)
		switch {
		case err != nil:
			fmt.Printf("%s: refused (%v), refusal=%v\n", call.Name, err, toolkit.IsExpectedRefusal(err))
		case res != nil:
			fmt.Printf("%s: answered: %s\n", call.Name, res.ForLLM)
		default:
			fmt.Printf("%s: runs\n", call.Name)
		}
	}
	// Output:
	// refund: refused (refunds over 100 need a person's approval), refusal=true
	// lookup_order: answered: Order A-1001 shipped on 20 November.
	// lookup_order: runs
}

// An AfterTurn callback that rewrites the reply of a turn that produced
// none, without storing the replacement in the session.
func ExampleAfterTurnFunc() {
	var after hook.AfterTurnFunc = func(_ context.Context, _ *hook.Turn, r *hook.TurnResult) {
		if r.Outcome == hook.OutcomeEmpty && r.Reply == "" {
			r.Reply = "Sorry, I couldn't compose a reply to that. Please try again."
			// r.Stored stays empty: the apology is not the model's reply.
		}
	}
	r := hook.TurnResult{Outcome: hook.OutcomeEmpty, Degenerate: true}
	after(context.Background(), &hook.Turn{Message: hook.UserText("Where is my order?")}, &r)
	fmt.Printf("%s: %q (stored %q)\n", r.Outcome, r.Reply, r.Stored)
	// Output:
	// empty: "Sorry, I couldn't compose a reply to that. Please try again." (stored "")
}

// A host consuming a run's events: partial text is streamed, tool calls are
// shown as they start, and the loop ends with TurnFinished, whose error is
// the turn's.
func ExampleRun_consumer() {
	h := hook.EventHeader{Scope: hook.Scope{AgentID: "alice", Session: "s-42", TurnID: "0a1b2c3d"}}
	var run hook.Run = newFakeRun(
		hook.TurnStarted{EventHeader: h, Message: hook.UserText("Where is my order?")},
		hook.ToolCallStarted{EventHeader: h, Call: hook.ToolCall{ID: "1", Name: "lookup_order"}},
		hook.ToolCallFinished{
			EventHeader: h, Call: hook.ToolCall{ID: "1", Name: "lookup_order"},
			Result: toolkit.NewResult("shipped"),
		},
		hook.TextDelta{EventHeader: h, Text: "It shipped"},
		hook.TextDelta{EventHeader: h, Text: "on 20 November."},
		hook.TurnFinished{
			EventHeader: h,
			Outcome:     hook.OutcomeOK, Reply: "It shipped on 20 November.",
		},
	)

	for ev, err := range run.Events() {
		switch ev := ev.(type) {
		case hook.TurnStarted:
			fmt.Printf("turn %s: %s\n", ev.Scope.TurnID, ev.Message.Text)
		case hook.ToolCallStarted:
			fmt.Printf("using %s\n", ev.Call.Name)
		case hook.TextDelta:
			fmt.Printf("> %s\n", ev.Text)
		case hook.TurnFinished:
			if err != nil {
				fmt.Println("failed:", err)
				break
			}
			fmt.Printf("%s: %s\n", ev.Outcome, ev.Reply)
		}
	}
	// Output:
	// turn 0a1b2c3d: Where is my order?
	// using lookup_order
	// > It shipped
	// > on 20 November.
	// ok: It shipped on 20 November.
}
