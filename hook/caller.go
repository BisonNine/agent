/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package hook

import (
	"context"
	"errors"
)

// CompleteRequest is one single-shot model call: a system and a user message.
// It is the request a summarizer (ctxengine) and a memory consolidator
// (cogmem) make.
type CompleteRequest struct {
	System, User string
	// JSONObject asks for a JSON-object response where the client supports it.
	JSONObject bool
	// Exclude lists wire model ids (as CompleteReply.Model reports them) that
	// must not serve this request: models that refused this content, or gave
	// an unusable reply earlier in the same pass.
	Exclude []string
}

// CompleteReply is the answer to a CompleteRequest. Model is the wire model id
// of the model that gave it; on an error, the last model tried, if any.
// FinishReason is the provider's stop reason ("stop", "length", "refusal",
// "content_filter") when it gave one.
type CompleteReply struct {
	Content      string
	FinishReason string
	Model        string
}

// Caller is the one model-caller interface: what a summarizer or a memory
// consolidator needs from a model chain. It owns model selection, fallback and
// cooldowns; it returns an error wrapping ErrNoModelAvailable when nothing can
// serve the request (every model excluded, parked or unconfigured). A
// models.Chain and a models.Set give one (Caller).
type Caller interface {
	Complete(ctx context.Context, req CompleteRequest) (CompleteReply, error)
}

// ErrNoModelAvailable reports that no model could be tried: none assigned for
// the purpose, or every one excluded, parked or disabled.
var ErrNoModelAvailable = errors.New("no model available")
