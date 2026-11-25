/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Result is the structured return value of a tool call: what the model
// reads, what the user is shown, and status flags for async work, errors
// and media.
type Result struct {
	// ForLLM is the content sent to the model. Required for all results.
	ForLLM string `json:"for_llm"`

	// ForUser is the content sent directly to the user. Empty means no user
	// message. Silent overrides it.
	ForUser string `json:"for_user,omitempty"`

	// Silent suppresses any message to the user, even when ForUser is set.
	Silent bool `json:"silent"`

	// IsError marks a failed call.
	IsError bool `json:"is_error"`

	// Async marks a call that completes later and reports through its
	// callback.
	Async bool `json:"async"`

	// Err is the underlying error, for logging and errors.Is/As. It is not
	// serialised.
	Err error `json:"-"`

	// Media holds media-store references produced by the tool, for the host
	// to deliver to the user.
	Media []string `json:"media,omitempty"`

	// Images holds base64 data URIs ("data:<mime>;base64,...") the tool
	// returned for the model to see (e.g. a screenshot). Distinct from
	// Media, which goes to the user: a host delivers these to a vision
	// model, typically as a follow-up user turn, since some provider APIs
	// reject image content in tool messages.
	Images []string `json:"images,omitempty"`

	// ToolReferences names the tools this result surfaces (search hits, the
	// tool a details lookup describes), as offered in the tool list. A host
	// with progressive discovery loads the deferred tools a result
	// references.
	ToolReferences []string `json:"tool_references,omitempty"`
}

// NewResult returns a basic Result carrying content for the model.
func NewResult(forLLM string) *Result {
	return &Result{
		ForLLM: forLLM,
	}
}

// SilentResult returns a Result for the model only, with no user message:
// file reads and writes, status updates, background operations.
func SilentResult(forLLM string) *Result {
	return &Result{
		ForLLM:  forLLM,
		Silent:  true,
		IsError: false,
		Async:   false,
	}
}

// AsyncResult returns a Result for work that continues in the background
// and reports later (sub-agent spawns, long external calls).
func AsyncResult(forLLM string) *Result {
	return &Result{
		ForLLM:  forLLM,
		Silent:  false,
		IsError: false,
		Async:   true,
	}
}

// ErrorResult returns a Result representing a failure, with IsError set.
func ErrorResult(message string) *Result {
	return &Result{
		ForLLM:  message,
		Silent:  false,
		IsError: true,
		Async:   false,
	}
}

// restrictionNotice tells a model that a refusal is configuration, not a
// fault, so it neither retries nor improvises a workaround.
const restrictionNotice = "This is a configured access restriction, not a bug or an outage. " +
	"Do not retry this call, and do not attempt to work around it by other means."

// NotEnabledMessage is the result text for a call the caller may not make:
// the tool is not granted to this agent, or a policy refused it. The
// wording is deliberately instructive: a model that sees tools it cannot
// call must read this as a configuration decision rather than a fault,
// otherwise it retries or improvises a workaround instead of telling the
// user. It does not say which cause applied.
func NotEnabledMessage(name string) string {
	return fmt.Sprintf("Permission denied: the tool %q is not enabled for this agent. ", name) +
		restrictionNotice + " " +
		"If this tool is needed, tell the user it must be enabled for this agent."
}

// UserResult returns a Result with the same content for the model and the
// user: command output, fetched content, query results.
func UserResult(content string) *Result {
	return &Result{
		ForLLM:  content,
		ForUser: content,
		Silent:  false,
		IsError: false,
		Async:   false,
	}
}

// MediaResult returns a Result carrying media references for the user.
func MediaResult(forLLM string, mediaRefs []string) *Result {
	return &Result{
		ForLLM: forLLM,
		Media:  mediaRefs,
	}
}

// MarshalJSON serialises the result; Err is left out (its tag is "-").
func (tr *Result) MarshalJSON() ([]byte, error) {
	type Alias Result
	return json.Marshal(&struct {
		*Alias
	}{
		Alias: (*Alias)(tr),
	})
}

// WithError sets Err and returns the result for chaining, keeping the error
// for logging while leaving it out of JSON:
//
//	result := ErrorResult("Operation failed").WithError(err)
func (tr *Result) WithError(err error) *Result {
	tr.Err = err
	return tr
}

// refusalError marks a tool error as an expected refusal: a configured
// permission or limit said no, nothing failed. The registry logs such a
// result at WARN instead of ERROR.
type refusalError struct{ err error }

func (e refusalError) Error() string { return e.err.Error() }
func (e refusalError) Unwrap() error { return e.err }
func (refusalError) Refusal() bool   { return true }

// Refusal marks err as an expected refusal (see IsExpectedRefusal).
func Refusal(err error) error { return refusalError{err: err} }

// IsExpectedRefusal reports whether err, or an error it wraps, is an
// expected refusal: one created by Refusal, or any error with a
// Refusal() bool method that returns true (so packages that do not import
// toolkit can mark theirs).
func IsExpectedRefusal(err error) bool {
	var r interface{ Refusal() bool }
	return errors.As(err, &r) && r.Refusal()
}
