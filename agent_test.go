package agent_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.bison9.com/agent"
	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
	"go.bison9.com/agent/toolkit"
)

// catalog is a catalogue of one scripted chat model.
func catalog(t *testing.T, p *modelstest.Provider) *models.Catalog {
	t.Helper()
	cat := models.New()
	if err := cat.AddProvider(modelspec.Provider{Name: "api", Protocol: "openai-chat", BaseURL: "http://127.0.0.1:0/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.Add(modelspec.Model{ModelName: "m1", Model: "w1", Provider: "api"}, models.For(models.Chat, 1)); err != nil {
		t.Fatal(err)
	}
	if err := cat.RegisterClient("m1", p); err != nil {
		t.Fatal(err)
	}
	return cat
}

func TestNew_RefusesWithoutModels(t *testing.T) {
	if _, err := agent.New("alice"); !errors.Is(err, agent.ErrInvalidSpec) {
		t.Fatalf("New = %v", err)
	}
}

// Options build the agent: models from a catalogue or a set, the name, and
// callbacks of one kind in the order given; the agent has every capability.
func TestNew_Options(t *testing.T) {
	p := modelstest.New().Script("w1", modelstest.Reply("hi"))
	cat := catalog(t, p)
	for _, m := range []agent.Option{agent.WithModels(cat), agent.WithModels(cat.Default())} {
		var order []string
		reg := toolkit.NewRegistry()
		a, err := agent.New("Alice", m,
			agent.WithName("Alice"),
			agent.WithTools(reg),
			agent.WithAfterTurn(func(context.Context, *agent.Turn, *agent.TurnResult) { order = append(order, "first") }),
			agent.WithPlugin(agent.Plugin{Name: "second", AfterTurn: func(context.Context, *agent.Turn, *agent.TurnResult) {
				order = append(order, "second")
			}}),
			agent.WithAfterTurn(func(context.Context, *agent.Turn, *agent.TurnResult) { order = append(order, "third") }),
		)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID() != "Alice" || a.Name() != "Alice" {
			t.Fatalf("ID, Name = %q, %q", a.ID(), a.Name())
		}
		if res := a.Run(context.Background(), "s1", agent.UserText("hello")).Wait(); res.Outcome != agent.OutcomeOK {
			t.Fatalf("result = %+v", res)
		}
		if !slices.Equal(order, []string{"first", "second", "third"}) {
			t.Fatalf("AfterTurn order = %v", order)
		}
		if _, ok := a.(agent.TaskController); !ok {
			t.Fatal("no TaskController")
		}
		if th, ok := a.(agent.ToolHolder); !ok || th.Tools() != toolkit.Registry(reg) {
			t.Fatal("no ToolHolder")
		}
		if mh, ok := a.(agent.ModelHolder); !ok || mh.Models() != cat.Default() {
			t.Fatal("no ModelHolder")
		}
		sf, ok := a.(agent.SessionForgetter)
		if !ok {
			t.Fatal("no SessionForgetter")
		}
		// The session may still be finishing its turn for a moment.
		for i := 0; !sf.ForgetSession("s1"); i++ {
			if i > 5000 {
				t.Fatal("the session stayed busy")
			}
			time.Sleep(time.Millisecond)
		}
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
