/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"context"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/common"
	"go.bison9.com/agent/spawnllm/internal/launch"
	"go.bison9.com/agent/spawnllm/protocoltypes"
)

// chatCLI checks id and accounts for one CLI provider call: an invalid
// identity is refused before the CLI is started, and chat runs otherwise.
// A CLI launch has no HTTP status, so none is recorded.
func chatCLI(
	ctx context.Context, id accounting.Identity, provider, model string,
	chat func(context.Context) (*LLMResponse, error),
) (*LLMResponse, error) {
	ctx, l, err := launch.Begin(ctx, id, launch.Spec{
		Kind:      accounting.KindText,
		Provider:  provider,
		Transport: accounting.TransportCLI,
		Model:     model,
	})
	if err != nil {
		return nil, err
	}
	resp, err := chat(ctx)
	l.EndChat(resp, err)
	return resp, err
}

// cliUsageKeys names the fields of a CLI's usage object that hold each count
// DispatchStatus reports; an empty name is a count the CLI never reports.
type cliUsageKeys struct {
	input, output, cacheRead, cacheCreation string
}

// reported returns which counts the usage object b carried as numbers.
func (k cliUsageKeys) reported(b []byte) protocoltypes.Reported {
	n := common.ReportedNumbers(b)
	has := func(name string) bool { return name != "" && n[name] }
	return protocoltypes.Reported{
		InputTokens:         has(k.input),
		OutputTokens:        has(k.output),
		CacheReadTokens:     has(k.cacheRead),
		CacheCreationTokens: has(k.cacheCreation),
	}
}
