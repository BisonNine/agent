# agenttest

`go.bison9.com/agent/agenttest` is the conformance suite of the
`agent.Agent` interface: what any implementation must do before a host is
given it. The SDK's own agent runs it in the gate (`conformance_test.go` at
the module root).

```go
func TestConformance(t *testing.T) {
    agenttest.Run(t, func(f agenttest.Fixture) agent.Agent {
        a, err := agent.New("alice", f.Options()...)
        if err != nil {
            t.Fatal(err)
        }
        return a
    })
}
```

Each test builds a new `Fixture` and asks the factory for an agent over it:
`Models`, a catalogue of two chat models (`Primary`, then `Backup`) both
answered by `Provider`, a scripted client (`models/modelstest`); `Tools`, a
registry the test fills; `Sessions`, an in-memory store; `Limits` (a short
cancel grace, no retries); and `Plugins`, the test's callbacks. An
implementation must use all of them; `Options` gives them as the root
package's options. The suite closes the agent when the test ends.

It checks, on every run, the event rules of package `hook` (exactly one
`TurnFinished`, last; `TurnStarted` first; a `ModelCall` for every
`ModelCallStarted`; a `ToolCallFinished` for every `ToolCallStarted`; the
batch order), and then:

- outcomes: ok, empty, error (with the failed `ModelCall`);
- fallback to the second model, with its `Fallback` event and `OnFallback`;
- tool calls in a batch, a failing tool, a panicking tool (only its call
  fails, with a `*hook.ToolPanicError`), a call refused by `BeforeTool`
  (a refusal), a tool removed mid-turn;
- interrupt: the running turn stops, queued runs are dropped and counted
  and emit only `TurnFinished`, tasks keep running;
- `Run.Cancel` stops the turn and its tasks; `Agent.Cancel` stops all;
- a cancel wins: an answer that returns after the interrupt is discarded
  (turn cancelled, no `ModelAnswer`, nothing stored), its `ModelCall`
  still carrying the usage;
- a tool that ignores its context is abandoned with `InterruptedResult`;
- a panic in a step callback ends the turn with a `*hook.PanicError`; one
  in an observer does not;
- different sessions run concurrently;
- `Close`: a run after it ends with an error; a second `Close` is fine.

It imports the root package, never `loop`, so it knows nothing of the
implementation it tests.
