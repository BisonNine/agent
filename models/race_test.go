/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package models_test

import (
	"fmt"
	"sync"
	"testing"

	"go.bison9.com/agent/models"
	"go.bison9.com/agent/models/modelstest"
	"go.bison9.com/agent/modelspec"
)

// Add, remove, select, adopt and disable while calls run. Run with -race.
func TestConcurrentChangesDuringCalls(t *testing.T) {
	f := newFixture(t, three)
	set := f.cat.Set("alice")
	for i, n := range []string{"sonnet", "gpt", "haiku"} {
		if err := set.Assign(models.Chat, n, i); err != nil {
			t.Fatal(err)
		}
	}
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
	for g := range 4 {
		run(func(i int) {
			scope := models.Scope(fmt.Sprintf("s%d", g))
			ch := set.Begin(models.Chat, scope)
			discard(ch.Chat(t.Context(), alice, models.Request{}))
			discard(ch.Caller(alice).Complete(t.Context(), models.CompleteRequest{}))
			discard(set.Current(models.Chat, scope))
		})
	}
	run(func(i int) {
		name := fmt.Sprintf("extra-%d", i%3)
		if f.cat.Add(modelspec.Model{ModelName: name, Model: name, Provider: "openai"}) == nil {
			discard(f.cat.RegisterClient(name, f.fake))
			discard(set.Assign(models.Chat, name, 5))
		}
		discard(f.cat.Remove(name))
	})
	run(func(i int) {
		discard(set.Select(models.Chat, []string{"sonnet", "gpt", "haiku"}[i%3], "s0"))
		set.Reset(models.Chat, "s1")
	})
	run(func(i int) {
		discard(f.cat.Disable("haiku"))
		discard(set.Disable("sonnet"))
		discard(f.cat.Enable("haiku"))
		discard(set.Enable("sonnet"))
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
		for _, s := range three {
			discard(next.Add(modelspec.Model{ModelName: s.name, Model: s.model, Provider: s.provider}))
			discard(next.RegisterClient(s.name, f.fake))
		}
		for j, n := range []string{"sonnet", "gpt", "haiku"} {
			discard(next.Set("alice").Assign(models.Chat, n, j))
		}
		f.cat.Adopt(next)
	})
	for range 200 {
		discard(set.Begin(models.Chat).Chat(t.Context(), alice, models.Request{}))
	}
	close(stop)
	wg.Wait()
}
