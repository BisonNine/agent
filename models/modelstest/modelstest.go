/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package modelstest is a scripted chat client for testing code that uses the
// models package: register a Provider for a model with
// Catalog.RegisterClient, script what each wire model answers, and read back
// the calls it received.
package modelstest

import (
	"context"
	"sync"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm"
	"go.bison9.com/agent/spawnllm/common"
)

// Step is one scripted answer.
type Step struct {
	Response *spawnllm.LLMResponse
	Err      error
	// Hold, when set, makes the call wait until it is closed or the call's
	// context ends (then the context's error is returned).
	Hold <-chan struct{}
	// Started, when set, is closed when the call begins.
	Started chan<- struct{}
}

// Reply is a step that answers with content.
func Reply(content string) Step {
	return Step{Response: &spawnllm.LLMResponse{Content: content, FinishReason: "stop"}}
}

// Fail is a step that returns err.
func Fail(err error) Step { return Step{Err: err} }

// Status is a step that fails with an HTTP status, as an API client reports
// it, with an optional Retry-After.
func Status(code int, retryAfter time.Duration) Step {
	return Step{Err: &common.HTTPStatusError{StatusCode: code, BodyPreview: "scripted", RetryAfter: retryAfter}}
}

// Call is one call the Provider received.
type Call struct {
	Identity accounting.Identity
	Model    string // the wire model id
	Messages []spawnllm.Message
	Tools    []spawnllm.ToolDefinition
	Options  map[string]any
}

// Provider is a scripted chat client. Each wire model has its own script,
// consumed in order; the last step repeats. A model with no script answers
// "ok". It is safe for concurrent use and implements
// spawnllm.StatefulProvider.
type Provider struct {
	mu      sync.Mutex
	scripts map[string][]Step
	calls   []Call
	closed  int
}

// New returns a Provider with no scripts.
func New() *Provider { return &Provider{scripts: map[string][]Step{}} }

// Script sets what model answers, in order.
func (p *Provider) Script(model string, steps ...Step) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[model] = steps
	return p
}

// Chat answers from the script of model.
func (p *Provider) Chat(ctx context.Context, id accounting.Identity, messages []spawnllm.Message, tools []spawnllm.ToolDefinition, model string, options map[string]any) (*spawnllm.LLMResponse, error) {
	p.mu.Lock()
	p.calls = append(p.calls, Call{Identity: id, Model: model, Messages: messages, Tools: tools, Options: options})
	step := Reply("ok")
	if s := p.scripts[model]; len(s) > 0 {
		step = s[0]
		if len(s) > 1 {
			p.scripts[model] = s[1:]
		}
	}
	p.mu.Unlock()
	if step.Started != nil {
		close(step.Started)
	}
	if step.Hold != nil {
		select {
		case <-step.Hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return step.Response, step.Err
}

// GetDefaultModel returns "".
func (p *Provider) GetDefaultModel() string { return "" }

// Close counts a close.
func (p *Provider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
}

// Closed returns how many times Close was called.
func (p *Provider) Closed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Calls returns the calls received so far.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Call(nil), p.calls...)
}

// Reset forgets the calls received so far.
func (p *Provider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = nil
}

// Models returns the wire model of each call received so far, in order.
func (p *Provider) Models() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	for i, c := range p.calls {
		out[i] = c.Model
	}
	return out
}
