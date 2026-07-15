package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	d, newTAT := Check(time.Time{}, now, 1, 1)
	assert.True(t, d.Allowed)
	assert.Equal(t, 0, d.Remaining)

	d2, _ := Check(newTAT, now, 1, 1)
	assert.False(t, d2.Allowed, "burst=1 admits exactly one instantaneous request")
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

	// Remaining: 0 on the Nth admission means exactly that — the next
	// instantaneous call MUST reject. The advertised counter and the
	// admission decision share one contract.
	d, newTAT := Check(tat, now, rps, burst)
	assert.False(t, d.Allowed, "call past burst budget must be rejected")
	assert.Equal(t, tat, newTAT, "rejection must not advance TAT")
	assert.Greater(t, d.RetryAfter, time.Duration(0))
}

// TestCheck_ExactBurstAdmissions pins the burst contract: a cold
// bucket with burst=N admits exactly N instantaneous requests, then
// rejects. The counterexample this guards against is the inclusive
// boundary condition (allowAt == now still admitted) combined with a
// full burst*period tolerance, which admitted N+1.
func TestCheck_ExactBurstAdmissions(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for _, burst := range []int{1, 2, 3, 5, 50} {
		tat := time.Time{}
		admitted := 0
		for i := 0; i < burst+3; i++ {
			d, newTAT := Check(tat, now, 100, burst)
			if !d.Allowed {
				break
			}
			admitted++
			tat = newTAT
		}
		assert.Equalf(t, burst, admitted,
			"burst=%d must admit exactly %d instantaneous requests", burst, burst)
	}
}

// TestCheck_ReplenishesOneSlotPerPeriod pins the sustained-rate half
// of the contract: after the burst is drained, one additional request
// becomes admissible every period.
func TestCheck_ReplenishesOneSlotPerPeriod(t *testing.T) {
	const (
		rps   = 10 // period = 100ms
		burst = 3
	)
	now := time.Unix(1_000_000_000, 0)
	tat := time.Time{}

	for i := 0; i < burst; i++ {
		var d Decision
		d, tat = Check(tat, now, rps, burst)
		require.Truef(t, d.Allowed, "drain call %d", i+1)
	}

	d, _ := Check(tat, now.Add(99*time.Millisecond), rps, burst)
	assert.False(t, d.Allowed, "slot must not replenish before one full period")

	d, _ = Check(tat, now.Add(100*time.Millisecond), rps, burst)
	assert.True(t, d.Allowed, "exactly one period after the drain a slot is free")
}

// TestCheck_HugeRPSDoesNotPanic guards the integer division in the
// period computation. rps beyond time.Second's nanosecond resolution
// (>1e9) used to truncate the period to zero and panic with a
// divide-by-zero on the first allowed request. Registry input is
// sanitized upstream, but Check is the last line of defense and MUST
// stay panic-free for any int input.
func TestCheck_HugeRPSDoesNotPanic(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for _, rps := range []int{1_000_000_001, 2_000_000_000, int(^uint(0) >> 1)} {
		assert.NotPanicsf(t, func() {
			d, _ := Check(time.Time{}, now, rps, 10)
			assert.True(t, d.Allowed, "first request on a fresh bucket must be allowed")
		}, "rps=%d must not panic", rps)
	}
}

// TestCheck_HugeBurstDoesNotDenyAll guards the burst*period tolerance
// against int64 overflow. An unclamped burst of 1e10 at rps=1 used to
// overflow the delay tolerance into a negative duration, pushing
// allowAt into the future and rejecting every request with a garbage
// RetryAfter.
func TestCheck_HugeBurstDoesNotDenyAll(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	d, _ := Check(time.Time{}, now, 1, 10_000_000_000)
	assert.True(t, d.Allowed, "fresh bucket must admit regardless of burst magnitude")
	assert.Equal(t, time.Duration(0), d.RetryAfter)
}

// TestCheck_NonPositiveInputsDoNotPanic pins the defensive clamps for
// rps and burst at or below zero. Upstream validation rejects these,
// but Check MUST degrade to the most conservative valid config (rps=1,
// burst=1) rather than divide by zero or misbehave.
func TestCheck_NonPositiveInputsDoNotPanic(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	assert.NotPanics(t, func() {
		d, _ := Check(time.Time{}, now, 0, 0)
		assert.True(t, d.Allowed, "fresh bucket admits one request even at the clamp floor")
	})
	assert.NotPanics(t, func() {
		d, _ := Check(time.Time{}, now, -5, -5)
		assert.True(t, d.Allowed)
	})
}
