/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package toolkit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// externalRefusalError is how a package that does not import toolkit marks a refusal.
type externalRefusalError struct{ error }

func (externalRefusalError) Refusal() bool { return true }

func TestIsExpectedRefusal(t *testing.T) {
	base := errors.New("no")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", base, false},
		{"marked", Refusal(base), true},
		{"marked and wrapped", fmt.Errorf("call: %w", Refusal(base)), true},
		{"marked by another package", externalRefusalError{base}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsExpectedRefusal(tc.err); got != tc.want {
				t.Fatalf("IsExpectedRefusal = %v, want %v", got, tc.want)
			}
		})
	}
	if !errors.Is(Refusal(base), base) {
		t.Fatal("Refusal must wrap its error")
	}
}

// The text models match on is kept word for word.
func TestNotEnabledMessage(t *testing.T) {
	got := NotEnabledMessage("refund")
	want := `Permission denied: the tool "refund" is not enabled for this agent. ` +
		"This is a configured access restriction, not a bug or an outage. " +
		"Do not retry this call, and do not attempt to work around it by other means. " +
		"If this tool is needed, tell the user it must be enabled for this agent."
	if got != want {
		t.Fatalf("NotEnabledMessage =\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(got, `"refund"`) {
		t.Fatal("the tool name must be quoted")
	}
}
