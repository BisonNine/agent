/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

// Package launch reports each launch of a provider call to the accounting
// package. Every exported entry point of a provider (Chat, Transcribe,
// Synthesize, Generate, Edit) calls Begin first and one End method when the
// call returns, so each call is reported exactly once.
package launch

import (
	"context"
	"sync/atomic"
	"time"

	"go.bison9.com/agent/accounting"
	"go.bison9.com/agent/spawnllm/protocoltypes"
)

// Spec describes a launch.
type Spec struct {
	Kind      accounting.Kind
	Provider  string
	Transport accounting.Transport
	// Model is the model requested. A chat response that names the model
	// that served it (a dated version, the CLI's primary model) replaces it.
	Model string
}

// Launch is one launch being accounted for.
type Launch struct {
	start      time.Time
	rec        accounting.Record
	httpStatus atomic.Int64
	reported   atomic.Bool
}

type ctxKey struct{}

// Begin validates id and starts accounting for a launch. When id is invalid
// the rejected launch is reported and the error returned; the caller must
// then launch nothing. Otherwise the returned context carries the launch, so
// the HTTP status can be recorded with HTTPStatus, and the caller must call
// one End method when the call returns.
func Begin(ctx context.Context, id accounting.Identity, s Spec) (context.Context, *Launch, error) {
	l := &Launch{
		start: time.Now(),
		rec: accounting.Record{
			Kind:      s.Kind,
			Provider:  s.Provider,
			Transport: s.Transport,
			Model:     s.Model,
		},
	}
	l.rec.Time = l.start.UTC()
	norm, err := id.Normalize()
	if err != nil {
		// Record the names as given, so the rejected caller can be found.
		l.rec.OriginAgent, l.rec.Agent, l.rec.AgentName, l.rec.BillingCode = id.Origin, id.Agent, id.Name, id.BillingCode
		l.finish(err)
		return ctx, nil, err
	}
	l.rec.OriginAgent, l.rec.Agent, l.rec.AgentName, l.rec.BillingCode = norm.Origin, norm.Agent, norm.Name, norm.BillingCode
	return context.WithValue(ctx, ctxKey{}, l), l, nil
}

// HTTPStatus records the status of the HTTP response a launch received. It
// does nothing when ctx carries no launch.
func HTTPStatus(ctx context.Context, code int) {
	if l, ok := ctx.Value(ctxKey{}).(*Launch); ok {
		l.httpStatus.Store(int64(code))
	}
}

// EndChat reports a chat launch: the tokens and cost resp's status says were
// reported, and err.
func (l *Launch) EndChat(resp *protocoltypes.LLMResponse, err error) {
	if resp != nil && resp.Status != nil {
		st := resp.Status
		if st.Model != "" {
			l.rec.Model = st.Model
		}
		l.rec.InputTokens = count(st.InputTokens, st.Reported.InputTokens)
		l.rec.OutputTokens = count(st.OutputTokens, st.Reported.OutputTokens)
		l.rec.CacheReadTokens = count(st.CacheReadTokens, st.Reported.CacheReadTokens)
		l.rec.CacheCreationTokens = count(st.CacheCreationTokens, st.Reported.CacheCreationTokens)
		l.rec.CostUSD = cost(st.CostUSD)
		l.rec.ToolCalls = count(st.ToolCalls, st.Reported.ToolCalls)
		l.rec.Images = count(st.Images, st.Reported.Images)
	}
	l.finish(err)
}

// TaskResult is what a task call reported: its usage, cost and the number of
// images it returned (unknown for calls that return none).
type TaskResult struct {
	Usage   *protocoltypes.TaskUsage
	CostUSD *float64
	Images  accounting.Int
}

// EndTask reports a task launch (speech, transcription, image): what the
// call reported, and err.
func (l *Launch) EndTask(res TaskResult, err error) {
	if usage := res.Usage; usage != nil {
		r := usage.Reported
		l.rec.InputTokens = count(usage.InputTokens, r.InputTokens)
		l.rec.OutputTokens = count(usage.OutputTokens, r.OutputTokens)
		if r.Seconds {
			l.rec.AudioSeconds = accounting.KnownFloat(usage.Seconds)
		}
		l.rec.Characters = count(usage.Characters, r.Characters)
	}
	l.rec.CostUSD = cost(res.CostUSD)
	l.rec.Images = res.Images
	l.finish(err)
}

// finish completes the record and reports it, once.
func (l *Launch) finish(err error) {
	if !l.reported.CompareAndSwap(false, true) {
		return
	}
	l.rec.DurationMs = time.Since(l.start).Milliseconds()
	if code := l.httpStatus.Load(); code != 0 {
		l.rec.HTTPStatus = accounting.KnownInt(code)
	}
	l.rec.Success = err == nil
	if err != nil {
		l.rec.Error = err.Error()
	}
	accounting.Report(l.rec)
}

func count(v int, reported bool) accounting.Int {
	if !reported {
		return accounting.Int{}
	}
	return accounting.KnownInt(int64(v))
}

func cost(v *float64) accounting.Float {
	if v == nil {
		return accounting.Float{}
	}
	return accounting.KnownFloat(*v)
}

// RunTask checks id and accounts for one task call (speech, transcription,
// image): an invalid identity is refused before call runs. result returns
// what a non-nil result of call reports.
func RunTask[T any](
	ctx context.Context, id accounting.Identity, s Spec,
	call func(context.Context) (*T, error),
	result func(*T) TaskResult,
) (*T, error) {
	ctx, l, err := Begin(ctx, id, s)
	if err != nil {
		return nil, err
	}
	out, err := call(ctx)
	var res TaskResult
	if out != nil {
		res = result(out)
	}
	l.EndTask(res, err)
	return out, err
}
