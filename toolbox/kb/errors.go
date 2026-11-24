/******************************************************************************
 * Copyright (c) 2026 Tenebris Technologies Inc.                              *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package kb

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The errors a caller can act on. Every other error is an unexpected
// failure (I/O, the index, a broken rule schema).
var (
	// ErrUnknownKB: the resolver does not know the knowledge base, or the
	// bundle has no directory and may not create one.
	ErrUnknownKB = errors.New("unknown knowledge base")
	// ErrNotFound: no concept has the ID.
	ErrNotFound = errors.New("concept not found")
	// ErrReadOnly: the knowledge base may not be written.
	ErrReadOnly = errors.New("knowledge base is read-only")
	// ErrInvalid: the input or the stored concept breaks a rule. The error
	// is a *ValidationError, which names each field.
	ErrInvalid = errors.New("invalid")
	// ErrTooLarge: a value is over one of the Limits.
	ErrTooLarge = errors.New("too large")
)

// FieldError is one rule a concept breaks. Field is a path into the
// record, such as "concept_id", "body" or "frontmatter.sources[1].resource".
type FieldError struct {
	ID     string `json:"concept_id,omitempty"`
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// ValidationError lists the rules a concept breaks. errors.Is reports it as
// ErrInvalid.
type ValidationError struct {
	KB     string
	ID     string
	Fields []FieldError
}

// Error implements error.
func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("invalid concept")
	if e.ID != "" {
		b.WriteString(" ")
		b.WriteString(quote(e.ID))
	}
	if e.KB != "" {
		b.WriteString(" in ")
		b.WriteString(quote(e.KB))
	}
	for i, f := range e.Fields {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(f.Field)
		b.WriteString(" ")
		b.WriteString(f.Reason)
	}
	return b.String()
}

// Is reports whether target is ErrInvalid.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

// maxReason bounds the bytes of one problem's reason.
const maxReason = 300

// invalid returns a *ValidationError for the fields, with ID set on each,
// bounded as boundFields does.
func invalid(kb, id string, fields ...FieldError) *ValidationError {
	id = clip(id, maxFieldPath) // an ID refused for its length is long
	fields = boundFields(fields)
	for i := range fields {
		if fields[i].ID == "" {
			fields[i].ID = id
		}
	}
	return &ValidationError{KB: kb, ID: id, Fields: fields}
}

// boundFields keeps at most maxProblems problems for one concept (the last
// saying how many more there were) and clips each path and reason, so no
// input makes an error text long.
func boundFields(fields []FieldError) []FieldError {
	if len(fields) > maxProblems {
		more := len(fields) - (maxProblems - 1)
		fields = append(fields[:maxProblems-1:maxProblems-1], FieldError{
			ID: fields[0].ID, Field: "frontmatter", Reason: fmt.Sprintf("has %d more problems", more),
		})
	}
	for i := range fields {
		fields[i].Field = clip(fields[i].Field, maxFieldPath)
		fields[i].Reason = clip(fields[i].Reason, maxReason)
	}
	return fields
}

// quote returns s quoted, for messages.
func quote(s string) string { return strconv.Quote(s) }
