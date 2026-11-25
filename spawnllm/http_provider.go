/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"context"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/openai_compat"
)

type HTTPProvider struct {
	delegate *openai_compat.Provider
}

func NewHTTPProviderWithOptions(apiKey, apiBase, proxy string, opts ...openai_compat.Option) *HTTPProvider {
	return &HTTPProvider{
		delegate: openai_compat.NewProvider(apiKey, apiBase, proxy, opts...),
	}
}

// Chat delegates to the openai_compat provider, which checks id and
// accounts for the call; it is not accounted for again here.
func (p *HTTPProvider) Chat(
	ctx context.Context,
	id accounting.Identity,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	return p.delegate.Chat(ctx, id, messages, tools, model, options)
}

func (p *HTTPProvider) GetDefaultModel() string {
	return ""
}

// Delegate returns the underlying openai_compat.Provider so tests can inspect
// per-entry state (ResponseLogFile, ReasoningEffort, ...) attached at
// construction time. Internal callers should keep going through the
// LLMProvider interface.
func (p *HTTPProvider) Delegate() *openai_compat.Provider {
	return p.delegate
}
