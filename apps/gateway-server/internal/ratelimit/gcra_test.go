package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCheck_FirstRequestAllowed(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	d, newTAT := Check(time.Time{}, now, 10, 20)

	assert.True(t, d.Allowed)
	assert.Equal(t, 19, d.Remaining)
	assert.Equal(t, time.Duration(0), d.RetryAfter)
	assert.Equal(t, now.Add(100*time.Millisecond), newTAT) // period = 1s/10 = 100ms
	assert.Equal(t, newTAT, d.ResetAt)                     // allow → resetAt = newTAT
}

func TestCheck_BucketExhaustedRejected(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	tat := now.Add(3 * time.Second) // beyond burst(20)*period(100ms)=2s

	d, newTAT := Check(tat, now, 10, 20)

	assert.False(t, d.Allowed)
	assert.Equal(t, 0, d.Remaining)
	assert.Greater(t, d.RetryAfter, time.Duration(0))
	assert.Equal(t, tat, newTAT)    // rejection does not advance TAT
	assert.Equal(t, tat, d.ResetAt) // reject → resetAt = currentTAT (already in future)
}

func TestCheck_RateChangeReinterpretsTAT(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	tatOld := now.Add(50 * time.Millisecond) // under old rps=100 would mean near-full

	d, _ := Check(tatOld, now, 10, 20) // new rps=10, delayTol = 2s ≫ 50ms
	assert.True(t, d.Allowed)
}

func TestCheck_Burst1EdgeCase(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	d, _ := Check(time.Time{}, now, 1, 1)
	assert.True(t, d.Allowed)
	assert.Equal(t, 0, d.Remaining)
}

func TestCheck_ResetAtAtMaxOfTATAndNow(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	// Old TAT in the past → allowed → resetAt must be newTAT (which is now+period, > now).
	tatPast := now.Add(-5 * time.Second)
	d, newTAT := Check(tatPast, now, 10, 20)
	assert.True(t, d.Allowed)
	assert.Equal(t, newTAT, d.ResetAt)
	assert.True(t, d.ResetAt.After(now))
}

func TestCheck_SuccessiveRequestsDecrementRemaining(t *testing.T) {
	// rps high enough that the period (1µs) is negligible relative to
	// the burst budget — each successive call within the same `now`
	// consumes exactly one burst slot.
	const (
		rps   = 1_000_000
		burst = 5
	)
	now := time.Unix(1_000_000_000, 0)
	tat := time.Time{}

	expectedRemaining := []int{4, 3, 2, 1, 0}
	for i, want := range expectedRemaining {
		d, newTAT := Check(tat, now, rps, burst)
		assert.Truef(t, d.Allowed, "call %d within burst must be allowed", i+1)
		assert.Equalf(t, want, d.Remaining, "call %d remaining", i+1)
		tat = newTAT
	}

	// GCRA's allow-condition is inclusive at the boundary: one extra
	// call at the burst edge (effectiveTAT-delayTol == now) is still
	// admitted. The strict rejection kicks in on the next call, once
	// effectiveTAT-delayTol > now. Exercise both steps.
	dBoundary, boundaryTAT := Check(tat, now, rps, burst)
	assert.True(t, dBoundary.Allowed, "boundary call (allowAt == now) is still allowed")
	tat = boundaryTAT

	d, newTAT := Check(tat, now, rps, burst)
	assert.False(t, d.Allowed, "call past burst edge must be rejected")
	assert.Equal(t, tat, newTAT, "rejection must not advance TAT")
	assert.Greater(t, d.RetryAfter, time.Duration(0))
}
