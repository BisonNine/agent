/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package spawnllm

import (
	"context"
	"errors"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/internal/launch"
)

// UnconfiguredProvider is used when no model is configured at gateway startup.
// It returns a user-friendly error for any chat request, allowing the gateway
// to start and serve the config UI while no model is set up yet.
type UnconfiguredProvider struct{}

func NewUnconfiguredProvider() *UnconfiguredProvider {
	return &UnconfiguredProvider{}
}

// Chat launches nothing. The refused call is still accounted for, with
// provider "unconfigured" and no transport.
func (u *UnconfiguredProvider) Chat(
	ctx context.Context, id accounting.Identity, messages []Message, tools []ToolDefinition, model string, options map[string]any,
) (*LLMResponse, error) {
	_, l, err := launch.Begin(ctx, id, launch.Spec{Kind: accounting.KindText, Provider: "unconfigured", Model: model})
	if err != nil {
		return nil, err
	}
	err = errors.New("no model configured — enable a model and add your API key in the configuration")
	l.EndChat(nil, err)
	return nil, err
}

func (u *UnconfiguredProvider) GetDefaultModel() string {
	return ""
}
