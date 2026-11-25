/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package accounting

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestIdentityNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      Identity
		want    Identity
		wantErr error
	}{
		{name: "agent and project", in: Identity{Agent: "alice", Project: "garden"}, want: Identity{Agent: "alice", Project: "garden"}},
		{name: "empty project is default", in: Identity{Agent: "alice"}, want: Identity{Agent: "alice", Project: DefaultProject}},
		{name: "blank project is default", in: Identity{Agent: "alice", Project: "  "}, want: Identity{Agent: "alice", Project: DefaultProject}},
		{name: "whitespace trimmed", in: Identity{Agent: " bob\t", Project: "\nshed "}, want: Identity{Agent: "bob", Project: "shed"}},
		{name: "empty agent", in: Identity{Project: "garden"}, wantErr: ErrAgentRequired},
		{name: "blank agent", in: Identity{Agent: " \t "}, wantErr: ErrAgentRequired},
		{name: "agent at limit", in: Identity{Agent: strings.Repeat("a", MaxNameLength)}, want: Identity{Agent: strings.Repeat("a", MaxNameLength), Project: DefaultProject}},
		{name: "agent too long", in: Identity{Agent: strings.Repeat("a", MaxNameLength+1)}, wantErr: ErrNameTooLong},
		{name: "agent is a UUID", in: Identity{Agent: "3f2b8c1e-9d4a-4e7b-a1c6-5f0e2d9b7a43"}, want: Identity{Agent: "3f2b8c1e-9d4a-4e7b-a1c6-5f0e2d9b7a43", Project: DefaultProject}},
		{name: "project at limit", in: Identity{Agent: "alice", Project: strings.Repeat("p", MaxNameLength)}, want: Identity{Agent: "alice", Project: strings.Repeat("p", MaxNameLength)}},
		{name: "project too long", in: Identity{Agent: "alice", Project: strings.Repeat("p", MaxNameLength+1)}, wantErr: ErrNameTooLong},
		{name: "name kept", in: Identity{Agent: "alice", Name: " Alice the gardener "}, want: Identity{Agent: "alice", Name: "Alice the gardener", Project: DefaultProject}},
		{name: "name at limit", in: Identity{Agent: "alice", Name: strings.Repeat("n", MaxNameLength)}, want: Identity{Agent: "alice", Name: strings.Repeat("n", MaxNameLength), Project: DefaultProject}},
		{name: "name too long is shortened", in: Identity{Agent: "alice", Name: strings.Repeat("n", MaxNameLength+1)}, want: Identity{Agent: "alice", Name: strings.Repeat("n", MaxNameLength-1) + "…", Project: DefaultProject}},
		{name: "multibyte name shortened by code points", in: Identity{Agent: "alice", Name: strings.Repeat("é", MaxNameLength+5)}, want: Identity{Agent: "alice", Name: strings.Repeat("é", MaxNameLength-1) + "…", Project: DefaultProject}},
		{name: "space before ellipsis dropped", in: Identity{Agent: "alice", Name: strings.Repeat("n", MaxNameLength-2) + "  bob"}, want: Identity{Agent: "alice", Name: strings.Repeat("n", MaxNameLength-2) + "…", Project: DefaultProject}},
		{name: "long name with bad agent still refused", in: Identity{Name: strings.Repeat("n", MaxNameLength+1)}, wantErr: ErrAgentRequired},
		// Code points of 2 bytes each: within the limit, though twice as many bytes.
		{name: "limit counts code points", in: Identity{Agent: strings.Repeat("é", MaxNameLength)}, want: Identity{Agent: strings.Repeat("é", MaxNameLength), Project: DefaultProject}},
		{name: "multibyte too long", in: Identity{Agent: strings.Repeat("é", MaxNameLength+1)}, wantErr: ErrNameTooLong},
		{name: "surrounding space not counted", in: Identity{Agent: "  " + strings.Repeat("a", MaxNameLength) + "  "}, want: Identity{Agent: strings.Repeat("a", MaxNameLength), Project: DefaultProject}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.Normalize()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if n := utf8.RuneCountInString(got.Name); n > MaxNameLength {
				t.Errorf("name has %d characters, want at most %d", n, MaxNameLength)
			}
		})
	}
}

func TestCheckProject(t *testing.T) {
	tests := []struct {
		name string
		code string
		ok   bool
	}{
		{name: "default", code: DefaultProject, ok: true},
		{name: "letters digits and punctuation", code: "Pentest-Acme_2026.q4", ok: true},
		{name: "one character", code: "a", ok: true},
		{name: "at limit", code: strings.Repeat("p", MaxNameLength), ok: true},
		{name: "only punctuation", code: ".-_", ok: true},
		{name: "empty", code: ""},
		{name: "too long", code: strings.Repeat("p", MaxNameLength+1)},
		{name: "space inside", code: "acme garden"},
		{name: "surrounding space", code: " acme"},
		{name: "slash", code: "acme/garden"},
		{name: "non-ASCII letter", code: "café"},
		{name: "control character", code: "acme\n"},
		{name: "colon", code: "acme:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckProject(tt.code)
			if tt.ok {
				if err != nil {
					t.Errorf("CheckProject(%q) = %v, want nil", tt.code, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidProject) {
				t.Errorf("CheckProject(%q) = %v, want ErrInvalidProject", tt.code, err)
			}
		})
	}
}

func TestCheckProjectIsCaseSensitive(t *testing.T) {
	// Both are valid and distinct: nothing folds case.
	for _, code := range []string{"Garden", "garden"} {
		if err := CheckProject(code); err != nil {
			t.Errorf("CheckProject(%q) = %v", code, err)
		}
	}
}
