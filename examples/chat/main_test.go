package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.bison9.com/agent"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/spawnllm"
)

// scripted is the example's two models on a scripted client, no network.
func scripted(t *testing.T, p *modelstest.Provider) *models.Catalog {
	t.Helper()
	m := models.New()
	if err := m.AddProvider(modelspec.Provider{Name: "anthropic", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"sonnet", "haiku"} {
		if err := m.Add(modelspec.Model{ModelName: name, Model: name + "-wire", Provider: "anthropic"}, models.For(models.Chat, i+1)); err != nil {
			t.Fatal(err)
		}
		if err := m.RegisterClient(name, p); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func call(name string, args map[string]any) modelstest.Step {
	return modelstest.Step{Response: &spawnllm.LLMResponse{
		ToolCalls: []spawnllm.ToolCall{{ID: "c1", Name: name, Arguments: args}}, FinishReason: "tool_calls",
	}}
}

// Bob asks about an order and for a refund: the lookup runs, the refund is
// refused by the approval callback, and both replies are written out.
func TestChat_ToolsAndReplies(t *testing.T) {
	p := modelstest.New().Script("sonnet-wire",
		call("lookup_order", map[string]any{"order": "42"}), modelstest.Reply("Order 42 shipped yesterday."),
		call("refund", map[string]any{"order": "42"}), modelstest.Reply("A person must approve refunds."))
	var out strings.Builder
	in := strings.NewReader("Where is order 42?\nRefund it.\n")
	if err := chat(context.Background(), scripted(t, p), in, &out, make(chan os.Signal, 1)); err != nil {
		t.Fatal(err)
	}
	want := "> [lookup_order]\nOrder 42 shipped yesterday.\n> [refund]\nA person must approve refunds.\n> "
	if out.String() != want {
		t.Fatalf("output:\n%q\nwant:\n%q", out.String(), want)
	}
	calls := p.Calls()
	if len(calls) != 4 || !strings.Contains(calls[3].Messages[len(calls[3].Messages)-1].Content, "approval") {
		t.Fatalf("the refusal did not reach the model: %+v", calls)
	}
}

// Ctrl-C stops the reply in progress and the conversation goes on.
func TestChat_Interrupt(t *testing.T) {
	started := make(chan struct{})
	p := modelstest.New().Script("sonnet-wire",
		modelstest.Step{Started: started, Hold: make(chan struct{}), Response: &spawnllm.LLMResponse{Content: "never"}},
		modelstest.Reply("Still here."))
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	interrupts := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- chat(context.Background(), scripted(t, p), inR, outW, interrupts)
		_ = outW.Close() //nolint:errcheck // a pipe's Close never fails
	}()
	r := bufio.NewReader(outR)
	expect := func(want string) {
		t.Helper()
		got := make([]byte, len(want))
		if _, err := io.ReadFull(r, got); err != nil || string(got) != want {
			t.Fatalf("read %q (%v), want %q", got, err, want)
		}
	}
	expect("> ")
	go func() { _, _ = io.WriteString(inW, "Think hard.\n") }() //nolint:errcheck // the test reads what arrives
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was not called")
	}
	interrupts <- os.Interrupt
	expect("(stopped)\n> ")
	go func() { _, _ = io.WriteString(inW, "Are you there?\n"); _ = inW.Close() }() //nolint:errcheck // as above
	expect("Still here.\n> ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The example stays the size section 1 of the plan promises: under 100
// lines of Go, not counting comments and blank lines.
func TestChat_IsSmall(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for line := range strings.Lines(string(src)) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "//") {
			n++
		}
	}
	if n >= 100 {
		t.Fatalf("main.go has %d lines of code, want fewer than 100", n)
	}
}

// A catalogue with no chat model is reported, not run.
func TestChat_NoChatModel(t *testing.T) {
	var out strings.Builder
	err := chat(context.Background(), models.New(), strings.NewReader("Hello\n"), &out, make(chan os.Signal, 1))
	if !errors.Is(err, agent.ErrInvalidSpec) {
		t.Fatalf("chat = %v, want ErrInvalidSpec", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output %q, want none", out.String())
	}
}

// A Ctrl-C pressed at the prompt does not stop the next reply.
func TestChat_InterruptAtPrompt(t *testing.T) {
	p := modelstest.New().Script("sonnet-wire", modelstest.Reply("Hello, Bob."))
	interrupts := make(chan os.Signal, 1)
	interrupts <- os.Interrupt
	var out strings.Builder
	if err := chat(context.Background(), scripted(t, p), strings.NewReader("Hello\n"), &out, interrupts); err != nil {
		t.Fatal(err)
	}
	if want := "> Hello, Bob.\n> "; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}

// A failed reply is written as an error and the conversation goes on.
func TestChat_ModelError(t *testing.T) {
	down := errors.New("service down")
	p := modelstest.New().
		Script("sonnet-wire", modelstest.Fail(down), modelstest.Reply("Back again.")).
		Script("haiku-wire", modelstest.Fail(down))
	var out strings.Builder
	if err := chat(context.Background(), scripted(t, p), strings.NewReader("Hello\nHello?\n"), &out, make(chan os.Signal, 1)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "> error: ") || !strings.Contains(lines[0], "service down") ||
		lines[1] != "> Back again." || lines[2] != "> " {
		t.Fatalf("output %q", out.String())
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// Output that cannot be written ends the conversation with the error,
// before anything is sent to the model.
func TestChat_WriteError(t *testing.T) {
	closed := errors.New("stdout closed")
	p := modelstest.New()
	err := chat(context.Background(), scripted(t, p), strings.NewReader("Hello\n"), failingWriter{closed}, make(chan os.Signal, 1))
	if !errors.Is(err, closed) {
		t.Fatalf("chat = %v, want the write error", err)
	}
	if n := len(p.Calls()); n != 0 {
		t.Fatalf("%d model calls, want none", n)
	}
}

// main builds the example's models and chats on stdin and stdout; at the end
// of the input it exits without calling a model.
func TestMain_EndOfInput(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }() //nolint:errcheck // a read-only file
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	main()
	if err = outW.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "> " {
		t.Fatalf("output %q, want the prompt alone", got)
	}
}
