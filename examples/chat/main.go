// Command chat is the smallest host of the agent SDK: Alice, a support agent
// with two models and two tools, talked to on stdin and stdout. Ctrl-C stops
// her reply; Ctrl-D quits. It reads ANTHROPIC_API_KEY.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"go.bison9.com/agent"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"
	"go.bison9.com/agent/toolspec"
)

func main() {
	m := models.New()
	err := errors.Join(
		m.AddProvider(modelspec.Provider{Name: "anthropic", Protocol: "anthropic", BaseURL: "https://api.anthropic.com", APIKey: os.Getenv("ANTHROPIC_API_KEY")}),
		m.Add(modelspec.Model{ModelName: "sonnet", Model: "claude-sonnet-5-5", Provider: "anthropic"}, models.For(models.Chat, 1)),
		m.Add(modelspec.Model{ModelName: "haiku", Model: "claude-haiku-5-5", Provider: "anthropic"}, models.For(models.Chat, 2), models.For(models.Summarize, 1)),
	)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	if err == nil {
		err = chat(context.Background(), m, os.Stdin, os.Stdout, interrupts)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "chat:", err)
		os.Exit(1)
	}
}

// chat holds the conversation: each line read is a message to Alice, each
// reply is written out; a value on interrupts stops the reply in progress.
func chat(ctx context.Context, m *models.Catalog, in io.Reader, out io.Writer, interrupts <-chan os.Signal) error {
	reg := toolkit.NewRegistry()
	err := reg.Add( // the shop's tools
		orderTool("lookup_order", "Look up an order's status by its number.", "Order %v shipped yesterday."),
		orderTool("refund", "Refund an order.", "Order %v refunded."))
	alice, nerr := agent.New("alice",
		agent.WithName("Alice"),
		agent.WithInstruction("You are Alice, a support agent for an online shop. Be brief."),
		agent.WithModels(m),
		agent.WithTools(reg),
		agent.WithSessions(session.NewMemory()),
		agent.WithBeforeTool(approveRefunds),
	)
	if err = errors.Join(err, nerr); err != nil {
		return err
	}
	defer func() { _ = alice.Close(ctx) }() //nolint:errcheck // nothing is left to report at exit

	var werr error
	say := func(format string, a ...any) {
		_, err := fmt.Fprintf(out, format, a...)
		werr = errors.Join(werr, err)
	}
	lines := bufio.NewScanner(in)
	for say("> "); lines.Scan() && werr == nil; say("> ") {
		for len(interrupts) > 0 { // a Ctrl-C at the prompt stops nothing
			<-interrupts
		}
		run := alice.Run(ctx, "session-42", agent.UserText(lines.Text()))
		go func() {
			select {
			case <-interrupts:
				run.Interrupt() // stop this reply; the conversation goes on
			case <-run.Done():
			}
		}()
		for ev, err := range run.Events() {
			switch e := ev.(type) {
			case agent.ToolCallStarted:
				say("[%s]\n", e.Call.Name)
			case agent.TurnFinished:
				if e.Outcome == agent.OutcomeCancelled {
					e.Reply = "(stopped)"
				} else if err != nil {
					e.Reply = "error: " + err.Error()
				}
				say("%s\n", e.Reply)
			}
		}
	}
	return errors.Join(lines.Err(), werr)
}

// orderTool is a tool taking an order number that answers reply about it.
func orderTool(name, description, reply string) toolspec.ToolDefinition {
	return toolspec.ToolDefinition{
		Name: name, Description: description,
		Parameters: []toolspec.Parameter{{Name: "order", Type: "string", Required: true}},
		Handler: func(c *toolspec.ToolCall) (*toolspec.Result, error) {
			return &toolspec.Result{ForLLM: fmt.Sprintf(reply, c.Args["order"])}, nil
		},
	}
}

// approveRefunds refuses every refund: a person approves those.
func approveRefunds(_ context.Context, call *agent.ToolCall) (*agent.ToolResult, error) {
	if call.Name == "refund" {
		return nil, agent.Refusal(errors.New("refunds need a person's approval"))
	}
	return nil, nil //nolint:nilnil // (nil, nil): run the tool
}
