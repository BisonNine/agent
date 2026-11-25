/******************************************************************************
 * Copyright © 2026 Bison9 Software                                           *
 * Please see LICENSE file for details.                                       *
 ******************************************************************************/

package modelspec

import "time"

// Cooldown sets, per HTTP-status category, how long a model that keeps
// failing is taken out of rotation (the "settled" cooldown reached after the
// short 1/3/5-minute escalation on the first three consecutive failures). Each
// value is in MINUTES: 0 uses the built-in default; a negative value disables
// cooldown for that category (the model is never taken out for it). 413
// (context-too-large) and errors with no HTTP status never cool: they are
// per-request or transient.
type Cooldown struct {
	// BillingAuthMinutes covers HTTP 401, 402, 403 (auth / out-of-credits).
	BillingAuthMinutes int `json:"billing_auth_minutes,omitempty"`
	// RateLimitMinutes covers HTTP 429.
	RateLimitMinutes int `json:"rate_limit_minutes,omitempty"`
	// BadRequestMinutes covers HTTP 400.
	BadRequestMinutes int `json:"bad_request_minutes,omitempty"`
	// ClientErrorMinutes covers other 4xx (404, 408, …; not 400/401/402/403/429/413).
	ClientErrorMinutes int `json:"client_error_minutes,omitempty"`
	// ServerErrorMinutes covers 5xx.
	ServerErrorMinutes int `json:"server_error_minutes,omitempty"`
}

// Cooldown category defaults (minutes). Billing/auth is the longest because the
// operator usually has to top up or rotate a key, but it is still bounded at 30
// minutes: it doubles as the recheck interval, so a longer value means a
// resolved billing problem goes unnoticed for that much longer. A rejected 402
// costs a round trip and no inference, so probing more often is cheap.
const (
	DefaultCooldownBillingAuthMinutes = 30
	DefaultCooldownRateLimitMinutes   = 10
	// DefaultCooldownBadRequestMinutes is 0 (never cool): a bad request is a
	// request-shape rejection, so the fallback should try the next candidate
	// (including a sibling config of the same provider+model, e.g. thinking-off)
	// instead of parking the model.
	DefaultCooldownBadRequestMinutes  = 0
	DefaultCooldownClientErrorMinutes = 10
	DefaultCooldownServerErrorMinutes = 10
)

// minutesOrDefault maps a config value to a duration: 0 → def, <0 → 0 (disabled).
func minutesOrDefault(v, def int) time.Duration {
	if v < 0 {
		return 0
	}
	if v == 0 {
		return time.Duration(def) * time.Minute
	}
	return time.Duration(v) * time.Minute
}

// BillingAuth is the cooldown for HTTP 401, 402 and 403; 0 means never.
func (c Cooldown) BillingAuth() time.Duration {
	return minutesOrDefault(c.BillingAuthMinutes, DefaultCooldownBillingAuthMinutes)
}

// RateLimit is the cooldown for HTTP 429; 0 means never.
func (c Cooldown) RateLimit() time.Duration {
	return minutesOrDefault(c.RateLimitMinutes, DefaultCooldownRateLimitMinutes)
}

// BadRequest is the cooldown for HTTP 400; 0 means never.
func (c Cooldown) BadRequest() time.Duration {
	return minutesOrDefault(c.BadRequestMinutes, DefaultCooldownBadRequestMinutes)
}

// ClientError is the cooldown for the other 4xx statuses; 0 means never.
func (c Cooldown) ClientError() time.Duration {
	return minutesOrDefault(c.ClientErrorMinutes, DefaultCooldownClientErrorMinutes)
}

// ServerError is the cooldown for 5xx statuses; 0 means never.
func (c Cooldown) ServerError() time.Duration {
	return minutesOrDefault(c.ServerErrorMinutes, DefaultCooldownServerErrorMinutes)
}
