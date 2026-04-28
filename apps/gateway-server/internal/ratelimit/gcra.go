// Package ratelimit provides rate-limiting primitives and Store
// implementations. This file hosts the GCRA (Generic Cell Rate
// Algorithm) used across all Store backends so that MemoryStore
// and NATSKVStore produce semantically identical decisions for
// the same (TAT, now, rps, burst) inputs.
package ratelimit

import "time"

// Decision carries the outcome of a rate-limit check. All fields
// are populated whether or not the request was Allowed, so callers
// can use Remaining / ResetAt for X-RateLimit-* response headers
// on both the happy path and 429 responses.
type Decision struct {
	// Allowed is true when the request is under the rate limit.
	// Callers MUST reject with 429 when false.
	Allowed bool

	// RetryAfter is the earliest time at which a request with the
	// same key is guaranteed to be allowed, as a delta from the
	// check's `now`. Zero when Allowed.
	RetryAfter time.Duration

	// Remaining is the burst capacity still available after this
	// check, clamped to [0, burst].
	Remaining int

	// ResetAt is the wall-clock time at which the bucket will be
	// fully replenished. Emitted as X-RateLimit-Reset (Unix secs).
	// Computed as max(currentTAT, now) when rejected, or newTAT
	// when allowed. Always >= now, so callers can emit it as a
	// Unix timestamp without additional clamping.
	ResetAt time.Time
}

// Check runs GCRA. Returns the decision plus the TAT that MUST be
// persisted iff Allowed is true. Rejections return the unchanged
// TAT — callers MUST NOT persist on reject.
//
// currentTAT is the Theoretical Arrival Time of the next request;
// zero time.Time for keys with no prior state.
//
// now is the pod's wall-clock time. Clock drift across pods is
// bounded by NTP SLA (<10ms in a healthy Kubernetes cluster);
// pathological skew causes soft anomalies (over-admit on the
// faster pod, over-reject on the slower) but never corrupts
// state, since TAT advances monotonically on each CAS write.
//
// rps MUST be >= 1. Behavior is undefined when rps <= 0 (the
// function divides time.Second by rps and will panic on zero);
// callers MUST validate upstream. The gateway handler enforces
// this by checking route.RateLimit.RPS > 0 before invoking Check.
//
// Example (typical Store backend usage):
//
//	currentTAT := storedTAT() // from persistent state
//	now := time.Now()
//	decision, newTAT := Check(currentTAT, now, 100, 10)
//	if decision.Allowed {
//		persist(newTAT) // GCRA state advances
//		return OK, decision.Remaining, decision.ResetAt
//	}
//	// Reject; do not persist. Client should retry after decision.RetryAfter.
//	return TooManyRequests, decision.RetryAfter, decision.ResetAt
func Check(currentTAT, now time.Time, rps, burst int) (Decision, time.Time) {
	period := time.Second / time.Duration(rps)
	delayTol := time.Duration(burst) * period
	effectiveTAT := currentTAT
	if effectiveTAT.Before(now) {
		effectiveTAT = now
	}
	allowAt := effectiveTAT.Add(-delayTol)

	if !now.Before(allowAt) {
		newTAT := effectiveTAT.Add(period)
		// remaining = burst - ceil((newTAT - now) / period)
		deltaPeriods := int((newTAT.Sub(now) + period - 1) / period)
		remaining := burst - deltaPeriods
		if remaining < 0 {
			remaining = 0
		}
		resetAt := newTAT
		if resetAt.Before(now) {
			resetAt = now
		}
		return Decision{
			Allowed:   true,
			Remaining: remaining,
			ResetAt:   resetAt,
		}, newTAT
	}

	resetAt := effectiveTAT // effectiveTAT = max(currentTAT, now) ≥ now
	return Decision{
		Allowed:    false,
		RetryAfter: allowAt.Sub(now),
		ResetAt:    resetAt,
	}, currentTAT
}
