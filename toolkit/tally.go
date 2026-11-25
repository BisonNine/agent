/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import "sync"

// MaxLastErrorRunes bounds the remembered text of a tally's last failed
// call; longer text is cut and ends in "…".
const MaxLastErrorRunes = 300

// logErrorRunes is the limit the catalogue passes to its truncate function
// (see WithTruncate) for the error text of a failed or refused call it logs.
const logErrorRunes = 500

// Tally counts the tool calls of the runs that ask for it, keyed by a run
// key (typically a session key). A run that never called Begin is not
// counted, so a key nobody watches costs one map lookup. It is safe for
// concurrent use; the zero value is not usable, use NewTally.
type Tally struct {
	redact  func(string) string
	mu      sync.Mutex
	entries map[string]*tallyEntry
}

type tallyEntry struct {
	calls     int
	errors    int
	lastError string
}

// NewTally returns an empty tally. redact, when non-nil, is applied to a
// failed call's text before it is kept (to strip secrets the text may
// carry); nil keeps the text as it is.
func NewTally(redact func(string) string) *Tally {
	return &Tally{redact: redact, entries: map[string]*tallyEntry{}}
}

// Begin starts counting the calls made under key. Beginning a key that is
// already counted resets it. An empty key is ignored.
func (t *Tally) Begin(key string) {
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[key] = &tallyEntry{}
}

// Record counts one call of toolName made under key. A nil result or one
// with IsError counts as a failure, and its text, prefixed with the tool
// name, redacted and truncated, becomes the key's last error. It does
// nothing for a key that was never begun.
func (t *Tally) Record(key, toolName string, r *Result) {
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.entries[key]
	if !ok {
		return
	}
	st.calls++
	if r != nil && !r.IsError {
		return
	}
	st.errors++
	st.lastError = toolName + ": " + t.failureText(r)
}

// End stops counting key and returns its tally. An unknown key returns
// zeros.
func (t *Tally) End(key string) (calls, errors int, lastError string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.entries[key]
	if !ok {
		return 0, 0, ""
	}
	delete(t.entries, key)
	return st.calls, st.errors, st.lastError
}

// failureText is the redacted, bounded description of a failed call.
func (t *Tally) failureText(r *Result) string {
	text := "tool returned nil result"
	if r != nil {
		text = r.ForLLM
		if text == "" && r.Err != nil {
			text = r.Err.Error()
		}
	}
	if t.redact != nil {
		text = t.redact(text)
	}
	return truncateRunes(text, MaxLastErrorRunes)
}

// truncateRunes cuts s to at most n runes, ending in "…" when cut.
func truncateRunes(s string, n int) string {
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n]) + "…"
	}
	return s
}
