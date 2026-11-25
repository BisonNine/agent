/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

var closeID = accounting.Identity{Agent: "alice", Name: "Alice"}

// closeCatalog has a model "b" whose client the catalogue builds (a scripted
// one) and a model "r" whose client the host registered.
func closeCatalog(t *testing.T) (c *Catalog, built, registered *modelstest.Provider) {
	t.Helper()
	c = httpCatalog(t, modelspec.Model{ModelName: "b", Model: "b", Provider: "p"}, modelspec.Model{ModelName: "r", Model: "r", Provider: "p"})
	built, registered = modelstest.New(), modelstest.New()
	swapSlot(t, c, "b", built)
	if err := c.RegisterClient("r", registered); err != nil {
		t.Fatal(err)
	}
	return c, built, registered
}

// holdCall starts a chat call on c's default chain that holds on the built
// client until the returned function is called; it returns the call's error.
func holdCall(t *testing.T, c *Catalog, built *modelstest.Provider) (release func(), done <-chan error) {
	t.Helper()
	hold, started := make(chan struct{}), make(chan struct{})
	built.Script("b", modelstest.Step{Hold: hold, Started: started, Response: modelstest.Reply("ok").Response})
	ch := c.Begin(Chat)
	out := make(chan error, 1)
	go func() {
		_, err := ch.Chat(context.Background(), closeID, Request{})
		out <- err
	}()
	<-started
	return func() { close(hold) }, out
}

// closeAsync runs Close in a goroutine and returns its result channel.
func closeAsync(c *Catalog, ctx context.Context) <-chan error {
	out := make(chan error, 1)
	go func() { out <- c.Close(ctx) }()
	return out
}

func notYet(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s returned early: %v", what, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCloseClosesBuiltClientsOnly(t *testing.T) {
	c, built, registered := closeCatalog(t)
	builtClient(t, c, "b")
	builtClient(t, c, "r")
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if built.Closed() != 1 {
		t.Errorf("built client closed %d times, want 1", built.Closed())
	}
	if registered.Closed() != 0 {
		t.Errorf("registered client closed %d times, want 0", registered.Closed())
	}
}

func TestCloseRefusesLaterCalls(t *testing.T) {
	c, _, _ := closeCatalog(t)
	before := c.Begin(Chat)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := c.Begin(Chat)
	for name, ch := range map[string]*Chain{"chain begun before": before, "chain begun after": after} {
		if _, err := ch.Chat(context.Background(), closeID, Request{}); !errors.Is(err, ErrClosed) {
			t.Errorf("%s: Chat = %v, want ErrClosed", name, err)
		}
		if _, _, err := ch.Do(context.Background(), func(context.Context, Candidate) error { return nil }); !errors.Is(err, ErrClosed) {
			t.Errorf("%s: Do = %v, want ErrClosed", name, err)
		}
		if _, err := ch.Caller(closeID).Complete(context.Background(), CompleteRequest{User: "x"}); !errors.Is(err, ErrClosed) {
			t.Errorf("%s: Caller = %v, want ErrClosed", name, err)
		}
		if _, _, err := ch.Client("b"); !errors.Is(err, ErrClosed) {
			t.Errorf("%s: Client = %v, want ErrClosed", name, err)
		}
	}
	if _, err := c.Default().Caller(Chat, closeID, "").Complete(context.Background(), CompleteRequest{User: "x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Set.Caller = %v, want ErrClosed", err)
	}
	if _, _, err := c.Client("b"); !errors.Is(err, ErrClosed) {
		t.Errorf("Catalog.Client = %v, want ErrClosed", err)
	}
	writes := map[string]error{
		"AddProvider": c.AddProvider(modelspec.Provider{Name: "q", Protocol: "openai-chat", BaseURL: "https://x"}),
		"Add":         c.Add(modelspec.Model{ModelName: "n", Model: "n", Provider: "p"}),
		"Assign":      c.Assign(Summarize, "b", 0),
		"Select":      c.Select(Chat, "r"),
		"Disable":     c.Disable("b"),
		"Remove":      c.Remove("b"),
	}
	for name, err := range writes {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("%s = %v, want ErrClosed", name, err)
		}
	}
	next := httpCatalog(t, modelspec.Model{ModelName: "b", Model: "b2", Provider: "p"})
	if d := c.Adopt(next); d.Added != nil || d.Changed != nil || d.Removed != nil {
		t.Errorf("Adopt into a closed catalogue changed it: %+v", d)
	}
}

func TestCloseWaitsForInFlightCall(t *testing.T) {
	c, built, _ := closeCatalog(t)
	release, call := holdCall(t, c, built)
	closed := closeAsync(c, context.Background())
	notYet(t, closed, "Close")
	if built.Closed() != 0 {
		t.Fatal("the client was closed during its call")
	}
	release()
	if err := <-call; err != nil {
		t.Fatalf("held call: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if built.Closed() != 1 {
		t.Fatalf("closed %d times, want 1", built.Closed())
	}
}

func TestCloseWaitsForClientRelease(t *testing.T) {
	c, built, _ := closeCatalog(t)
	_, release, err := c.Client("b")
	if err != nil {
		t.Fatal(err)
	}
	closed := closeAsync(c, context.Background())
	notYet(t, closed, "Close")
	release()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if built.Closed() != 1 {
		t.Fatalf("closed %d times, want 1", built.Closed())
	}
}

// A client Adopt replaced while a call held it is still closed: Close waits
// for that call, whose end closes it.
func TestCloseAfterAdoptClosesRetiredClient(t *testing.T) {
	c, built, _ := closeCatalog(t)
	release, call := holdCall(t, c, built)
	c.Adopt(httpCatalog(t, modelspec.Model{ModelName: "b", Model: "b2", Provider: "p"}))
	closed := closeAsync(c, context.Background())
	notYet(t, closed, "Close")
	release()
	if err := <-call; err != nil {
		t.Fatalf("held call: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if built.Closed() != 1 {
		t.Fatalf("retired client closed %d times, want 1", built.Closed())
	}
}

func TestCloseContextEnds(t *testing.T) {
	c := httpCatalog(t, modelspec.Model{ModelName: "b", Model: "b", Provider: "p"}, modelspec.Model{ModelName: "idle", Model: "idle", Provider: "p"})
	built, idle := modelstest.New(), modelstest.New()
	swapSlot(t, c, "b", built)
	swapSlot(t, c, "idle", idle)
	builtClient(t, c, "idle")
	release, call := holdCall(t, c, built)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
	if idle.Closed() != 1 {
		t.Errorf("idle client closed %d times, want 1", idle.Closed())
	}
	if built.Closed() != 0 {
		t.Fatal("the held client was closed during its call")
	}
	release()
	if err := <-call; err != nil {
		t.Fatalf("held call: %v", err)
	}
	if built.Closed() != 1 {
		t.Fatalf("held client closed %d times after its call, want 1", built.Closed())
	}
}

func TestCloseIdempotent(t *testing.T) {
	c, built, _ := closeCatalog(t)
	builtClient(t, c, "b")
	for i := range 2 {
		if err := c.Close(context.Background()); err != nil {
			t.Fatalf("Close #%d = %v", i+1, err)
		}
	}
	if built.Closed() != 1 {
		t.Fatalf("closed %d times, want 1", built.Closed())
	}
}

// Closing a catalogue spent by Adopt is a no-op: its clients are now the
// live catalogue's.
func TestCloseSpentCatalogue(t *testing.T) {
	c := httpCatalog(t, modelspec.Model{ModelName: "b", Model: "b", Provider: "p"})
	next := httpCatalog(t, modelspec.Model{ModelName: "b", Model: "b2", Provider: "p"})
	built := modelstest.New()
	swapSlot(t, next, "b", built)
	c.Adopt(next)
	builtClient(t, c, "b")
	if err := next.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if built.Closed() != 0 {
		t.Fatal("closing the spent catalogue closed the live one's client")
	}
	if _, err := c.Begin(Chat).Chat(context.Background(), closeID, Request{}); err != nil {
		t.Fatalf("the live catalogue: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if built.Closed() != 1 {
		t.Fatalf("closed %d times, want 1", built.Closed())
	}
}

// Calls and Close race; every call either answers or fails with ErrClosed,
// and the client is closed once, after the last call. Run with -race.
func TestCloseRacesCalls(t *testing.T) {
	c, built, _ := closeCatalog(t)
	builtClient(t, c, "b")
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			for range 50 {
				_, err := c.Begin(Chat).Chat(context.Background(), closeID, Request{})
				if err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("Chat = %v", err)
					return
				}
				if _, release, err := c.Client("b"); err == nil {
					release()
				} else if !errors.Is(err, ErrClosed) {
					t.Errorf("Client = %v", err)
					return
				}
			}
		})
	}
	close(start)
	time.Sleep(time.Millisecond)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := built.Closed(); got != 1 {
		t.Fatalf("closed %d times right after Close, want 1", got)
	}
	wg.Wait()
	if got := built.Closed(); got != 1 {
		t.Fatalf("closed %d times, want 1", got)
	}
}
