/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// clientErrCatalog has "task" on a task-only provider (no chat client),
// "nokey" on an Anthropic provider without a key, and "good" answering from
// fake; Chat lists task, nokey, good in that order.
func clientErrCatalog(t *testing.T, fake *modelstest.Provider) *models.Catalog {
	t.Helper()
	c := models.New()
	for _, p := range []modelspec.Provider{
		{Name: "Gemini", Protocol: "gemini", APIKey: "k"},
		{Name: "Anthropic", Protocol: "anthropic", BaseURL: "https://example.invalid"},
		{Name: "OpenAI", Protocol: "openai-chat", BaseURL: "https://example.invalid/v1"},
	} {
		if err := c.AddProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	for i, m := range []modelspec.Model{
		{ModelName: "task", Model: "gemini-x", Provider: "Gemini"},
		{ModelName: "nokey", Model: "claude-x", Provider: "Anthropic"},
		{ModelName: "good", Model: "gpt-x", Provider: "OpenAI"},
	} {
		if err := c.Add(m, models.For(models.Chat, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RegisterClient("good", fake); err != nil {
		t.Fatal(err)
	}
	return c
}

// checkClientError checks err is a *ClientError for want, whose cause has no
// "models:" prefix, and whether it is ErrNoClient.
func checkClientError(t *testing.T, err error, want models.ClientError, noClient bool) {
	t.Helper()
	ce, ok := errors.AsType[*models.ClientError](err)
	if !ok {
		t.Fatalf("err = %v, want a *ClientError", err)
	}
	if ce.Model != want.Model || ce.Provider != want.Provider || ce.Protocol != want.Protocol {
		t.Errorf("ClientError = {%q %q %q}, want {%q %q %q}", ce.Model, ce.Provider, ce.Protocol, want.Model, want.Provider, want.Protocol)
	}
	if ce.Err == nil {
		t.Fatal("ClientError.Err is nil")
	}
	if strings.Contains(ce.Err.Error(), "models:") || strings.Contains(ce.Err.Error(), "provider ") {
		t.Errorf("ClientError.Err = %q, want the bare cause", ce.Err)
	}
	if got := errors.Is(err, models.ErrNoClient); got != noClient {
		t.Errorf("errors.Is(err, ErrNoClient) = %v, want %v", got, noClient)
	}
}

var (
	taskErr  = models.ClientError{Model: "task", Provider: "Gemini", Protocol: "gemini"}
	nokeyErr = models.ClientError{Model: "nokey", Provider: "Anthropic", Protocol: "anthropic"}
)

func TestClientErrorPaths(t *testing.T) {
	c := clientErrCatalog(t, modelstest.New())
	cases := []struct {
		name     string
		get      func(name string) error
		model    string
		want     models.ClientError
		noClient bool
	}{
		{"Catalog.Client task-only", catalogClient(c), "task", taskErr, true},
		{"Catalog.Client no key", catalogClient(c), "nokey", nokeyErr, false},
		{"Set.Client task-only", setClient(c.Default()), "task", taskErr, true},
		{"Chain.Client task-only", chainClient(c.Begin(models.Chat)), "task", taskErr, true},
		{"Chain.Client no key", chainClient(c.Begin(models.Chat)), "nokey", nokeyErr, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkClientError(t, tc.get(tc.model), tc.want, tc.noClient)
		})
	}
}

func catalogClient(c *models.Catalog) func(string) error {
	return func(name string) error { _, _, err := c.Client(name); return err }
}

func setClient(s *models.Set) func(string) error {
	return func(name string) error { _, _, err := s.Client(name); return err }
}

func chainClient(ch *models.Chain) func(string) error {
	return func(name string) error { _, _, err := ch.Client(name); return err }
}

// A CLI model on an isolated chain with no workspace has no client.
func TestClientErrorIsolatedWithoutWorkspace(t *testing.T) {
	c := models.New()
	if err := c.AddProvider(modelspec.Provider{Name: "Claude CLI", Protocol: "claude-cli"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "cc", Model: "claude-cli", Provider: "Claude CLI"}, models.For(models.Chat, 0)); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Begin(models.Chat).Isolated(" ").Client("cc")
	checkClientError(t, err, models.ClientError{Model: "cc", Provider: "Claude CLI", Protocol: "claude-cli"}, false)
}

// Chat stops at a model whose client cannot be built: the failure is not
// classified, so the next model is not tried and nothing is recorded.
func TestChatClientBuildFailure(t *testing.T) {
	fake := modelstest.New()
	c := clientErrCatalog(t, fake)
	_, err := c.Begin(models.Chat).Chat(context.Background(), alice, models.Request{})
	checkClientError(t, err, taskErr, true)
	if !strings.HasPrefix(err.Error(), "fallback: unclassified error from Gemini/gemini-x: ") {
		t.Errorf("err = %q", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("the next model was called %d times", n)
	}
	if h, _ := c.Get("task"); !h.Health.Available {
		t.Error("a client-build failure was recorded against the model")
	}
}

// Complete moves on past models whose client cannot be built, each attempt
// carrying the *ClientError.
func TestCompleteClientBuildFailure(t *testing.T) {
	fake := modelstest.New()
	c := clientErrCatalog(t, fake)
	var passed []models.Attempt
	caller := c.Begin(models.Chat).Caller(alice, models.WithNotify(func(p []models.Attempt, _ models.Candidate) {
		passed = append(passed, p...)
	}))
	reply, err := caller.Complete(context.Background(), models.CompleteRequest{User: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply.Model != "gpt-x" {
		t.Fatalf("answered by %q, want gpt-x", reply.Model)
	}
	if len(passed) != 2 {
		t.Fatalf("passed %d models, want 2", len(passed))
	}
	checkClientError(t, passed[0].Err, taskErr, true)
	checkClientError(t, passed[1].Err, nokeyErr, false)
	for _, a := range passed {
		if a.Reason != "" {
			t.Errorf("%s: reason %q, want none", a.Name, a.Reason)
		}
	}
	for _, n := range []string{"task", "nokey"} {
		if h, _ := c.Get(n); !h.Health.Available {
			t.Errorf("a client-build failure was recorded against %s", n)
		}
	}
}

func TestSentinels(t *testing.T) {
	c := models.New()
	if err := c.AddProvider(modelspec.Provider{Name: "OpenAI", Protocol: "openai-chat", BaseURL: "https://example.invalid/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add(modelspec.Model{ModelName: "gpt", Model: "gpt-x", Provider: "OpenAI"}); err != nil {
		t.Fatal(err)
	}
	_, buildErr := models.Build(modelspec.Document{
		Version:     modelspec.DocumentVersion,
		Providers:   []modelspec.Provider{{Name: "OpenAI", Protocol: "openai-chat", BaseURL: "https://example.invalid/v1"}},
		Models:      []modelspec.Model{{ModelName: "gpt", Model: "gpt-x", Provider: "OpenAI", Enabled: true}},
		Assignments: map[string]map[string][]modelspec.Assignment{"alice": {"chat": {}}},
	})
	_, _, clientErr := c.Client("nope")
	cases := []struct {
		name string
		err  error
		want error
		text string
	}{
		{"duplicate provider", c.AddProvider(modelspec.Provider{Name: "OpenAI", Protocol: "openai-chat", BaseURL: "https://x"}), models.ErrDuplicate, `models: provider "OpenAI" already exists`},
		{"duplicate model", c.Add(modelspec.Model{ModelName: "gpt", Model: "gpt-x", Provider: "OpenAI"}), models.ErrDuplicate, `models: model "gpt" already exists`},
		{"unknown provider", c.Add(modelspec.Model{ModelName: "x", Model: "x", Provider: "Nope"}), models.ErrUnknownProvider, `models: model "x": unknown provider "Nope"`},
		{"unknown model, Remove", c.Remove("nope"), models.ErrUnknownModel, `models: unknown model "nope"`},
		{"unknown model, Client", clientErr, models.ErrUnknownModel, `models: unknown model "nope"`},
		{"unknown model, Assign", c.Assign(models.Chat, "nope", 0), models.ErrUnknownModel, `models: unknown model "nope"`},
		{"purpose with no model", buildErr, models.ErrNoModelAvailable, `models: agent "alice": purpose "chat": no model available`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.want) {
				t.Errorf("err = %v, want one that is %v", tc.err, tc.want)
			}
			if tc.err != nil && tc.err.Error() != tc.text {
				t.Errorf("err = %q, want %q", tc.err, tc.text)
			}
		})
	}
}
