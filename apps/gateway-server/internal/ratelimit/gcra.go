// Package ratelimit provides rate-limiting primitives and Store
// implementations. This file hosts the GCRA (Generic Cell Rate
// Algorithm) used across all Store backends so that MemoryStore
// and NATSKVStore produce semantically identical decisions for
// the same (TAT, now, rps, burst) inputs.
package ratelimit

import "time"

const (
	// maxSaneRPS caps the rps input of Check. The GCRA period is
	// computed as time.Second / rps in integer nanoseconds, so
	// precision degrades once the division starts truncating:
	// at rps = 1e6 the period is exactly 1µs (no error), while above
	// it the enforced rate drifts from the declared value and at
	// rps > 1e9 the period truncates to zero — a divide-by-zero panic
	// in the remaining-capacity computation. 1e6 rps per bucket key is
	// far beyond any realistic per-client quota, so the clamp is loss-
	// free in practice and keeps the arithmetic exact.
	maxSaneRPS = 1_000_000

	// maxSaneBurst caps the burst input of Check so the delay
	// tolerance (burst-1)*period cannot overflow int64. With the
	// largest possible period (1s at rps=1), a burst of 1e9 yields a
	// tolerance of ~1e18 ns — within int64 range with an order of
	// magnitude to spare. A larger burst also could not be reported:
	// Decision.Remaining is an int and dashboards treat it as a
	// request count, not an astronomic constant.
	maxSaneBurst = 1_000_000_000
)

// clampLimits normalizes rps and burst into the domain where the
// GCRA arithmetic in Check is exact and overflow-free. Registry input
// is sanitized upstream at route build; this is the in-package last
// line of defense so Check stays panic-free and never degrades into
// deny-all for any int input (hand-crafted KV writes, schema drift,
// or a buggy SDK included).
func clampLimits(rps, burst int) (int, int) {
	if rps < 1 {
		rps = 1
	} else if rps > maxSaneRPS {
		rps = maxSaneRPS
	}
	if burst < 1 {
		burst = 1
	} else if burst > maxSaneBurst {
		burst = maxSaneBurst
	}
	return rps, burst
}

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
// rps and burst are clamped to [1, maxSaneRPS] and [1, maxSaneBurst]
// respectively before any arithmetic, so Check is total: no int input
// can panic (divide-by-zero on a truncated period) or overflow the
// delay tolerance into deny-all. The clamp is a defense-in-depth
// backstop — the routing layer's sanitization remains the authoritative
// rejection point for out-of-range registry input.
//
// Burst semantics: a cold bucket with burst=N admits exactly N
// instantaneous requests, then one more per period. The delay
// tolerance is therefore (burst-1)*period: the Nth back-to-back
// request arrives with TAT already N-1 periods ahead, which is the
// last admissible position. Decision.Remaining shares the same
// contract — Remaining: 0 on the Nth admission means the next
// instantaneous request rejects.
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
	rps, burst = clampLimits(rps, burst)
	period := time.Second / time.Duration(rps)
	delayTol := time.Duration(burst-1) * period
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
