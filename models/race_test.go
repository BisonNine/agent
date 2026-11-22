package models_test

import (
	"fmt"
	"sync"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// Add, remove, adopt and disable while calls run. Run with -race.
func TestConcurrentChangesDuringCalls(t *testing.T) {
	f := newFixture(t, three)
	f.fake.Script("gpt-5", modelstest.Status(500, 0), modelstest.Reply("ok"))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	run := func(fn func(i int)) {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
					fn(i)
				}
			}
		})
	}
	for range 4 {
		run(func(int) {
			discard(f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}))
		})
	}
	run(func(i int) {
		name := fmt.Sprintf("extra-%d", i%3)
		if f.cat.Add(modelspec.Model{ModelName: name, Model: name, Provider: "openai"}, models.For(models.Chat, 5)) == nil {
			discard(f.cat.RegisterClient(name, f.fake))
		}
		discard(f.cat.Remove(name))
	})
	run(func(int) {
		discard(f.cat.Disable("haiku"))
		discard(f.cat.Enable("haiku"))
		f.cat.ResetHealth()
		discard(f.cat.Health())
	})
	run(func(i int) {
		if i%50 != 0 {
			return
		}
		next := models.New()
		discard(next.AddProvider(modelspec.Provider{Name: "anthropic", Protocol: "openai-chat", BaseURL: "https://x"}))
		discard(next.AddProvider(modelspec.Provider{Name: "openai", Protocol: "openai-chat", BaseURL: "https://x"}))
		for j, s := range three {
			discard(next.Add(modelspec.Model{ModelName: s.name, Model: s.model, Provider: s.provider}, models.For(models.Chat, j)))
			discard(next.RegisterClient(s.name, f.fake))
		}
		f.cat.Adopt(next)
	})
	for range 200 {
		discard(f.cat.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}))
	}
	close(stop)
	wg.Wait()
}
