package ratelimit

import (
	"math"
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

	// The allow-condition is exclusive at the boundary: the call at
	// the burst edge (effectiveTAT-delayTol == now) is rejected, so a
	// cold bucket admits exactly `burst` same-instant requests and the
	// advertised Remaining: 0 above matches the actual admission cut.
	d, newTAT := Check(tat, now, rps, burst)
	assert.False(t, d.Allowed, "boundary call (allowAt == now) must be rejected")
	assert.Equal(t, tat, newTAT, "rejection must not advance TAT")
	assert.Greater(t, d.RetryAfter, time.Duration(0))
}

// TestCheck_SustainedRateAtExactCadence proves the exclusive boundary
// does not penalise a well-behaved client emitting at exactly the
// configured rate: after the burst is drained, requests spaced one
// period apart keep being admitted.
func TestCheck_SustainedRateAtExactCadence(t *testing.T) {
	const (
		rps   = 10
		burst = 5
	)
	period := time.Second / time.Duration(rps)
	now := time.Unix(1_000_000_000, 0)
	tat := time.Time{}

	// Drain the burst at one instant.
	for i := 0; i < burst; i++ {
		d, newTAT := Check(tat, now, rps, burst)
		require.Truef(t, d.Allowed, "burst call %d must be allowed", i+1)
		tat = newTAT
	}

	// Steady state at exact cadence stays admitted.
	for i := 0; i < 20; i++ {
		now = now.Add(period)
		d, newTAT := Check(tat, now, rps, burst)
		require.Truef(t, d.Allowed, "cadence call %d must be allowed", i+1)
		tat = newTAT
	}
}

// TestCheck_ExtremeRPSDoesNotPanic pins the defensive period clamp:
// rps > 1e9 truncates time.Second/rps to zero, which panicked the
// deltaPeriods division before the guard. The routing builder clamps
// such configs at table build; Check must stay total regardless.
func TestCheck_ExtremeRPSDoesNotPanic(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for _, rps := range []int{1_000_000_001, 2_000_000_000, math.MaxInt32, math.MaxInt64 / 2} {
		assert.NotPanics(t, func() {
			d, _ := Check(time.Time{}, now, rps, 10)
			assert.True(t, d.Allowed, "fresh bucket at rps=%d must admit", rps)
		})
	}
}

// TestCheck_HugeBurstDoesNotDenyAll pins the delay-tolerance overflow
// saturation: burst*period past int64 used to wrap negative, pushing
// allowAt into the future and rejecting 100% of traffic with garbage
// RetryAfter values. A burst too large to represent means the bucket
// never empties — every request must be admitted.
func TestCheck_HugeBurstDoesNotDenyAll(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)

	// burst=1e10 at rps=1: 1e10 * 1e9 ns overflows int64.
	d, newTAT := Check(time.Time{}, now, 1, 10_000_000_000)
	assert.True(t, d.Allowed, "overflowing burst must not deny-all")
	assert.GreaterOrEqual(t, d.Remaining, 0)
	assert.False(t, d.ResetAt.Before(now), "ResetAt must not precede now")

	// State advances normally and stays admissible.
	d2, _ := Check(newTAT, now, 1, 10_000_000_000)
	assert.True(t, d2.Allowed)
}

// FuzzCheck property-tests the GCRA math across hostile inputs: any
// (tat, rps, burst) combination that reaches Check — including values
// the sanitization layers are supposed to filter — must uphold the
// Decision invariants without panicking. Guards the hot path against
// registry schema drift and hand-crafted KV writes.
func FuzzCheck(f *testing.F) {
	f.Add(int64(0), 100, 200)
	f.Add(int64(1_000_000_000_000_000_000), 1, 1)
	f.Add(int64(-1), 1_000_000_001, 10)
	f.Add(int64(0), 2_000_000_000, 0)
	f.Add(int64(0), 1, 10_000_000_000)
	f.Add(int64(math.MaxInt64), math.MaxInt64, math.MaxInt64)
	f.Add(int64(math.MaxInt64), 1, math.MaxInt64)

	f.Fuzz(func(t *testing.T, tatNanos int64, rps, burst int) {
		if rps <= 0 {
			t.Skip("rps <= 0 is documented as undefined and rejected upstream")
		}
		now := time.Unix(1_700_000_000, 0)
		currentTAT := time.Unix(0, tatNanos)

		d, newTAT := Check(currentTAT, now, rps, burst)

		if d.Remaining < 0 {
			t.Fatalf("Remaining must be non-negative, got %d", d.Remaining)
		}
		if burst >= 0 && d.Remaining > burst {
			t.Fatalf("Remaining %d exceeds burst %d", d.Remaining, burst)
		}
		if d.ResetAt.Before(now) {
			t.Fatalf("ResetAt %v precedes now %v", d.ResetAt, now)
		}
		if d.Allowed {
			if d.RetryAfter != 0 {
				t.Fatalf("RetryAfter must be zero when allowed, got %v", d.RetryAfter)
			}
			if !newTAT.After(currentTAT) && !newTAT.After(now) {
				t.Fatalf("allowed check must advance TAT: current=%v new=%v now=%v", currentTAT, newTAT, now)
			}
		} else {
			if d.RetryAfter <= 0 {
				t.Fatalf("RetryAfter must be positive when rejected, got %v", d.RetryAfter)
			}
			if !newTAT.Equal(currentTAT) {
				t.Fatalf("rejection must not advance TAT: current=%v new=%v", currentTAT, newTAT)
			}
		}
	})
}
