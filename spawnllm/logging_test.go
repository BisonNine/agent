package spawnllm

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"

	"go.bison9.com/agent/spawnllm/internal/launch/launchtest"
)

// cliFailure is what the failing mock CLI writes to stderr.
const cliFailure = "alice's prompt leaked"

// failClaude runs a claude provider whose CLI writes cliFailure to stderr and
// exits 1.
func failClaude(t *testing.T, p *ClaudeCliProvider) {
	t.Helper()
	p.command = createMockCLI(t, "", cliFailure, 1)
	if _, err := p.Chat(context.Background(), testID, []Message{{Role: "user", Content: "hello"}}, nil, "", nil); err == nil {
		t.Fatal("Chat succeeded, want the CLI's failure")
	}
}

func TestLogging_DefaultIsSilent(t *testing.T) {
	// Nothing reaches the process's default logger when no logger is set.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	failClaude(t, NewClaudeCliProvider("", t.TempDir(), nil, nil))
	if buf.Len() != 0 {
		t.Errorf("default logger got output: %s", buf.String())
	}
}

func TestLogging_ComponentAndContent(t *testing.T) {
	tests := []struct {
		name        string
		logContent  bool
		wantContent bool
	}{
		{name: "content off by default", logContent: false, wantContent: false},
		{name: "content on", logContent: true, wantContent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &launchtest.Logs{}
			p := NewClaudeCliProvider("", t.TempDir(), nil, nil)
			p.SetLogger(logs.Logger())
			p.SetLogContent(tt.logContent)
			failClaude(t, p)

			e, ok := logs.Find("claude-cli subprocess failed")
			if !ok {
				t.Fatalf("no failure record; got %+v", logs.Entries())
			}
			if e.Level != slog.LevelError || e.Attrs["component"] != "provider" {
				t.Errorf("record = %s component %v, want an error with component provider", e.Level, e.Attrs["component"])
			}
			_, hasStderr := e.Attrs["stderr"]
			if hasStderr != tt.wantContent {
				t.Errorf("stderr logged = %v, want %v (attrs %v)", hasStderr, tt.wantContent, e.Attrs)
			}
		})
	}
}

func TestLogging_WorkerPassesLoggerToProvider(t *testing.T) {
	// WithLogger and WithLogContent reach the provider WithProvider builds,
	// whichever order the options come in.
	logs := &launchtest.Logs{}
	script := createMockCLI(t, "", cliFailure, 1)
	w, err := New(
		WithLogger(logs.Logger()),
		WithProvider(ProviderSpec{Kind: "claude-cli", CLIPath: script, Workspace: t.TempDir()}),
		WithLogContent(true),
		WithAccounting(testID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(context.Background(), []Message{{Role: "user", Content: "hello"}}); err == nil {
		t.Fatal("Run succeeded, want the CLI's failure")
	}
	e, ok := logs.Find("claude-cli subprocess failed")
	if !ok {
		t.Fatalf("no failure record; got %+v", logs.Entries())
	}
	if e.Attrs["stderr"] != cliFailure {
		t.Errorf("stderr = %v, want %q", e.Attrs["stderr"], cliFailure)
	}
}

func TestLogging_ProvidersAreIsolated(t *testing.T) {
	alice, bob := &launchtest.Logs{}, &launchtest.Logs{}
	pa := NewClaudeCliProvider("", t.TempDir(), nil, nil)
	pa.SetLogger(alice.Logger())
	pb := NewClaudeCliProvider("", t.TempDir(), nil, nil)
	pb.SetLogger(bob.Logger())

	failClaude(t, pa)
	if len(alice.Entries()) == 0 {
		t.Fatal("alice's logger got no records")
	}
	if got := bob.Entries(); len(got) != 0 {
		t.Errorf("bob's logger got alice's records: %+v", got)
	}
}

func TestLogging_SettersRaceWithCalls(t *testing.T) {
	// A host may change the logger and the content setting while a call is
	// running; run under -race.
	logs := &launchtest.Logs{}
	p := NewClaudeCliProvider("", t.TempDir(), nil, nil)
	p.command = createMockCLI(t, "", cliFailure, 1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 50 {
			p.SetLogger(logs.Logger())
			p.SetLogContent(i%2 == 0)
			p.SetLogger(nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range 5 {
			if _, err := p.Chat(context.Background(), testID, []Message{{Role: "user", Content: "hello"}}, nil, "", nil); err == nil {
				t.Error("Chat succeeded, want the CLI's failure")
			}
		}
	}()
	wg.Wait()
}
