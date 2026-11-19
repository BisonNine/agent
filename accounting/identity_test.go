package accounting

import (
	"errors"
	"strings"
	"testing"
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
		{name: "project too long", in: Identity{Agent: "alice", Project: strings.Repeat("p", MaxNameLength+1)}, wantErr: ErrNameTooLong},
		// 16 code points of 2 bytes each: within the limit, though 32 bytes.
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
		})
	}
}
