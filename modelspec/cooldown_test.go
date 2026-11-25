/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package modelspec

import (
	"testing"
	"time"
)

func TestCooldown(t *testing.T) {
	tests := []struct {
		name string
		c    Cooldown
		want [5]time.Duration // billing/auth, rate limit, bad request, client error, server error
	}{
		{
			name: "zero uses the defaults",
			want: [5]time.Duration{30 * time.Minute, 10 * time.Minute, 0, 10 * time.Minute, 10 * time.Minute},
		},
		{
			name: "set values win",
			c:    Cooldown{BillingAuthMinutes: 5, RateLimitMinutes: 1, BadRequestMinutes: 2, ClientErrorMinutes: 3, ServerErrorMinutes: 4},
			want: [5]time.Duration{5 * time.Minute, time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute},
		},
		{
			name: "negative disables",
			c:    Cooldown{BillingAuthMinutes: -1, RateLimitMinutes: -1, BadRequestMinutes: -1, ClientErrorMinutes: -1, ServerErrorMinutes: -1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := [5]time.Duration{tc.c.BillingAuth(), tc.c.RateLimit(), tc.c.BadRequest(), tc.c.ClientError(), tc.c.ServerError()}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// A host that keeps its own struct with extra tags (environment variable
// names, for one) converts it to Cooldown: the field names and types must stay
// those of the host struct, which Go conversion requires.
func TestCooldown_ConvertsFromATaggedHostStruct(t *testing.T) {
	type hostCooldown struct {
		BillingAuthMinutes int `env:"APP_BILLING" json:"billing_auth_minutes,omitempty"`
		RateLimitMinutes   int `env:"APP_RATE"    json:"rate_limit_minutes,omitempty"`
		BadRequestMinutes  int `env:"APP_BAD"     json:"bad_request_minutes,omitempty"`
		ClientErrorMinutes int `env:"APP_CLIENT"  json:"client_error_minutes,omitempty"`
		ServerErrorMinutes int `env:"APP_SERVER"  json:"server_error_minutes,omitempty"`
	}
	c := Cooldown(hostCooldown{RateLimitMinutes: 7})
	if c.RateLimit() != 7*time.Minute {
		t.Errorf("RateLimit() = %v, want 7m", c.RateLimit())
	}
}
