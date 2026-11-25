/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package ctxengine_test

import (
	"context"
	"fmt"

	"go.bison9.com/agent/ctxengine"
	"go.bison9.com/agent/ctxengine/session"
	"go.bison9.com/agent/spawnllm"
)

// A manager over the in-memory store: a host with no disk at all.
func Example_inMemoryStore() {
	ctx := context.Background()
	store := session.NewMemory()
	mgr := ctxengine.New("alice", store)

	if _, err := mgr.AddUserMessage(ctx, spawnllm.Message{Role: "user", Content: "What is 2 + 2?"}); err != nil {
		fmt.Println(err)
		return
	}
	asm, err := mgr.Assemble(ctx, ctxengine.AssembleRequest{
		Layers: []ctxengine.Layer{{Name: "static", Text: "You are a careful assistant."}},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, m := range asm.Messages {
		fmt.Printf("request %s: %s\n", m.Role, m.Content)
	}
	if _, err := mgr.AddAssistantMessage(ctx, spawnllm.Message{Role: "assistant", Content: "4"}); err != nil {
		fmt.Println(err)
		return
	}
	if err := mgr.Close(ctx); err != nil {
		fmt.Println(err)
		return
	}

	for _, m := range store.GetHistoryWithSeqs("alice") {
		fmt.Printf("stored #%d %s: %s\n", m.Seq, m.Role, m.Content)
	}
	// Output:
	// request system: You are a careful assistant.
	// request user: What is 2 + 2?
	// stored #1 user: What is 2 + 2?
	// stored #2 assistant: 4
}
