package ratelimit

import (
	"math"
	"strconv"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// BuildHeaders returns the X-RateLimit-* response headers for a given
// decision, plus Retry-After on rejection.
//
// Always emits:
//   - X-RateLimit-Limit = configured rps. Static config; safe to
//     report on every path including the fail-open branch where the
//     Decision is unpopulated.
//
// Emits when the Decision was populated by a successful Store.Allow
// call (d.ResetAt is non-zero):
//   - X-RateLimit-Remaining = decision.Remaining clamped to
//     [0, rl.RPS]. Decision.Remaining counts burst slots, whose
//     ceiling (the effective burst, 2×RPS by default) exceeds the
//     advertised Limit; emitting it raw would break the countdown
//     invariant remaining <= limit that GitHub/Stripe-style clients
//     assume when computing used = limit - remaining. The clamp
//     under-reports spare burst capacity — the conservative
//     direction: a client honoring the header backs off early and is
//     never surprised by a 429, and low readings (the ones adaptive
//     throttlers act on) pass through exact. Remaining: 0 means the
//     next instantaneous request rejects; the GCRA admission check
//     and this counter share one contract.
//   - X-RateLimit-Reset     = decision.ResetAt as Unix seconds
//
// Omits Remaining and Reset on a zero Decision (d.ResetAt.IsZero()).
// The handler reaches this state when Store.Allow returned an error
// and the configured FailPolicy resolved to "allow"; the Decision
// fields are not meaningful there. Emitting Remaining: 0 / Reset:
// -62135596800 (Unix encoding of time.Time{}) would tell clients the
// bucket is exhausted and reset in year 1, which is worse than no
// information at all. Wire format: under fail-open the client sees
// only the static X-RateLimit-Limit and infers nothing about bucket
// state.
//
// On rejection (decision.Allowed == false) additionally emits:
//   - Retry-After = ceil(decision.RetryAfter seconds), clamped to a
//     minimum of 1. Retry-After: 0 is misleading to clients because
//     many client libraries treat it as "retry immediately"; a
//     fractional sub-second wait always rounds up to a full second.
//
// Keys use the canonical casing clients (GitHub, Stripe, etc.)
// expect. On the wire, Hertz canonicalises header names to MIME
// canonical form (`X-Ratelimit-Limit`, `X-Ratelimit-Remaining`,
// `X-Ratelimit-Reset`) before transmission; case-insensitive matching
// is required when asserting against a real HTTP client response. The
// returned map is fresh and safe to mutate / merge into any
// http.Header-compatible collection.
func BuildHeaders(rl *registry.RateLimitMeta, d Decision) map[string]string {
	h := make(map[string]string, 4)
	h["X-RateLimit-Limit"] = strconv.Itoa(rl.RPS)
	if !d.ResetAt.IsZero() {
		remaining := d.Remaining
		if remaining > rl.RPS {
			remaining = rl.RPS
		}
		h["X-RateLimit-Remaining"] = strconv.Itoa(remaining)
		h["X-RateLimit-Reset"] = strconv.FormatInt(d.ResetAt.Unix(), 10)
	}
	if !d.Allowed && !d.ResetAt.IsZero() {
		secs := int64(math.Ceil(d.RetryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		h["Retry-After"] = strconv.FormatInt(secs, 10)
	}
	return h
}
