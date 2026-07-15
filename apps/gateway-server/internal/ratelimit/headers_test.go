package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

func TestBuildHeaders_Allowed(t *testing.T) {
	reset := time.Unix(1_735_837_293, 0)
	d := Decision{Allowed: true, Remaining: 87, ResetAt: reset}
	rl := &registry.RateLimitMeta{RPS: 100, Burst: 200}

	h := BuildHeaders(rl, d)
	assert.Equal(t, "100", h["X-RateLimit-Limit"])
	assert.Equal(t, "87", h["X-RateLimit-Remaining"])
	assert.Equal(t, "1735837293", h["X-RateLimit-Reset"])
	_, hasRetry := h["Retry-After"]
	assert.False(t, hasRetry)
}

func TestBuildHeaders_Rejected(t *testing.T) {
	reset := time.Unix(1_735_837_293, 0)
	d := Decision{Allowed: false, Remaining: 0, RetryAfter: 2700 * time.Millisecond, ResetAt: reset}
	rl := &registry.RateLimitMeta{RPS: 100}

	h := BuildHeaders(rl, d)
	assert.Equal(t, "3", h["Retry-After"]) // ceil(2.7) = 3
	assert.Equal(t, "0", h["X-RateLimit-Remaining"])
	assert.Equal(t, "1735837293", h["X-RateLimit-Reset"])
}

func TestBuildHeaders_RetryAfterMinOne(t *testing.T) {
	d := Decision{Allowed: false, RetryAfter: 100 * time.Millisecond, ResetAt: time.Unix(1_000_000_001, 0)}
	rl := &registry.RateLimitMeta{RPS: 10}

	h := BuildHeaders(rl, d)
	assert.Equal(t, "1", h["Retry-After"]) // clamped up to min 1
}

func TestBuildHeaders_LimitReportsRPSNotBurst(t *testing.T) {
	d := Decision{Allowed: true, Remaining: 50, ResetAt: time.Unix(1_000_000_000, 0)}
	rl := &registry.RateLimitMeta{RPS: 100, Burst: 200}

	h := BuildHeaders(rl, d)
	assert.Equal(t, "100", h["X-RateLimit-Limit"])
}

// TestBuildHeaders_RemainingClampedToLimit pins the countdown
// invariant of the de-facto X-RateLimit-* convention: Remaining never
// exceeds Limit. Decision.Remaining counts burst slots, which with the
// default burst = 2×RPS would otherwise put Remaining: 199 next to
// Limit: 100 on a cold bucket — nonsense for any client computing
// used = limit - remaining. The clamp under-reports available burst
// (the safe direction: clients may back off early, never get a
// surprise 429) while low readings — the ones adaptive throttlers act
// on — stay exact.
func TestBuildHeaders_RemainingClampedToLimit(t *testing.T) {
	d := Decision{Allowed: true, Remaining: 199, ResetAt: time.Unix(1_000_000_000, 0)}
	rl := &registry.RateLimitMeta{RPS: 100, Burst: 200}

	h := BuildHeaders(rl, d)
	assert.Equal(t, "100", h["X-RateLimit-Limit"])
	assert.Equal(t, "100", h["X-RateLimit-Remaining"],
		"Remaining above the advertised Limit must clamp to it")

	// Below the limit the exact burst-slot count passes through.
	d.Remaining = 42
	h = BuildHeaders(rl, d)
	assert.Equal(t, "42", h["X-RateLimit-Remaining"])
}

// TestBuildHeaders_ZeroDecisionEmitsOnlyLimit pins the fail-open
// defence: when Store.Allow errors out and the configured FailPolicy
// resolves to "allow", the handler passes a zero Decision{} into
// BuildHeaders. The bucket state fields (Remaining, Reset, Retry-After)
// are not meaningful on that path and a zero ResetAt would otherwise
// emit `X-RateLimit-Reset: -62135596800` (the Unix encoding of
// time.Time{}.Unix()), which is misleading. Only the static
// X-RateLimit-Limit must reach the wire on this branch.
func TestBuildHeaders_ZeroDecisionEmitsOnlyLimit(t *testing.T) {
	rl := &registry.RateLimitMeta{RPS: 100}

	h := BuildHeaders(rl, Decision{})

	assert.Equal(t, "100", h["X-RateLimit-Limit"])
	_, hasRemaining := h["X-RateLimit-Remaining"]
	assert.False(t, hasRemaining, "zero Decision must not emit X-RateLimit-Remaining")
	_, hasReset := h["X-RateLimit-Reset"]
	assert.False(t, hasReset, "zero Decision must not emit X-RateLimit-Reset")
	_, hasRetry := h["Retry-After"]
	assert.False(t, hasRetry, "zero Decision must not emit Retry-After")
}
