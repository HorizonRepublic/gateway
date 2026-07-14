// Package ratelimit provides rate-limiting primitives and Store
// implementations. This file hosts the GCRA (Generic Cell Rate
// Algorithm) used across all Store backends so that MemoryStore
// and NATSKVStore produce semantically identical decisions for
// the same (TAT, now, rps, burst) inputs.
package ratelimit

import (
	"math"
	"time"
)

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
	// check's `now`. Zero when Allowed; always positive on reject.
	RetryAfter time.Duration

	// Remaining is the burst capacity still available after this
	// check, clamped to [0, burst]. It counts down from the
	// effective burst: a cold bucket admits exactly `burst`
	// same-instant requests, the burst-th reports Remaining: 0,
	// and the next same-instant request is rejected. Advertised
	// Remaining therefore matches the number of requests that will
	// actually be admitted.
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
// The admission boundary is exclusive: a request arriving exactly
// at allowAt (effectiveTAT - burst*period) is rejected. This keeps
// the instantaneous bucket capacity at exactly `burst`, so the
// advertised Remaining counts real admissions — an inclusive
// boundary would admit burst+1 same-instant requests while the
// burst-th response already reported Remaining: 0.
//
// rps MUST be >= 1. Behavior is undefined when rps <= 0 (the
// function divides time.Second by rps); callers MUST validate
// upstream. The gateway enforces this twice: the routing builder's
// sanitizeRateLimit drops non-positive RPS at table build, and the
// proxy handler re-checks route.RateLimit.RPS > 0 before invoking
// the store. Values above 1e9 truncate the integer division to a
// zero period; Check clamps the period to 1ns defensively (instead
// of panicking on the divide) — the routing builder clamps such
// configurations long before they reach this point.
//
// burst values large enough to overflow burst*period saturate the
// delay tolerance at math.MaxInt64 instead of wrapping negative: a
// burst too large to represent means "the bucket never empties",
// so saturation preserves the semantics where wrapping would
// silently deny every request.
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
	if period <= 0 {
		// rps > 1e9 truncated the division to zero. A zero period
		// would panic the deltaPeriods division below; clamp to the
		// finest representable rate instead of crashing the hot path.
		period = 1
	}
	delayTol := time.Duration(burst) * period
	if burst > 0 && delayTol/time.Duration(burst) != period {
		// burst*period overflowed int64 and wrapped. A wrapped
		// (negative) tolerance would push allowAt into the future and
		// reject everything; saturate to "never empties" instead.
		delayTol = math.MaxInt64
	}
	effectiveTAT := currentTAT
	if effectiveTAT.Before(now) {
		effectiveTAT = now
	}
	allowAt := effectiveTAT.Add(-delayTol)

	if now.After(allowAt) {
		newTAT := effectiveTAT.Add(period)
		// remaining = burst - ceil((newTAT - now) / period).
		// newTAT >= now + period on this branch, so elapsed >= 1 and
		// the overflow-safe ceil form (e-1)/p + 1 never underflows.
		elapsed := newTAT.Sub(now)
		deltaPeriods := int((elapsed-1)/period + 1)
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

	retryAfter := allowAt.Sub(now)
	if retryAfter <= 0 {
		// Boundary-equality reject (now == allowAt): admissible
		// strictly after allowAt, so the earliest guaranteed retry is
		// one tick later. Keeps the "RetryAfter > 0 iff rejected"
		// invariant callers rely on.
		retryAfter = 1
	}
	resetAt := effectiveTAT // effectiveTAT = max(currentTAT, now) ≥ now
	return Decision{
		Allowed:    false,
		RetryAfter: retryAfter,
		ResetAt:    resetAt,
	}, currentTAT
}
