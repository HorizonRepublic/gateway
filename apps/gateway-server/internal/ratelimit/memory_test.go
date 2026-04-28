package ratelimit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStore_FirstRequestAllowed(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer func() { _ = s.Close() }()

	d, err := s.Allow(context.Background(), "k", 10, 20)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 19, d.Remaining)
}

func TestMemoryStore_RejectsWhenBurstExhausted(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// rps=100 → period=10ms, delayTol=50ms. The full drain-plus-boundary
	// loop finishes in well under a period, so no mid-test refill can
	// mask the rejection — any sub-μs advance of wall time is dwarfed
	// by delayTol. GCRA admits burst+1 calls at the same instant (the
	// boundary call at effectiveTAT - delayTol == now — see
	// TestCheck_SuccessiveRequestsDecrementRemaining); the 7th call
	// strictly past the edge MUST reject.
	for i := 0; i < 6; i++ {
		d, err := s.Allow(ctx, "k", 100, 5)
		require.NoError(t, err)
		require.True(t, d.Allowed, "attempt %d should be allowed", i)
	}

	d, err := s.Allow(ctx, "k", 100, 5)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, 0, d.Remaining)
}

func TestMemoryStore_ConcurrentAllowsSerializeCorrectly(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer func() { _ = s.Close() }()

	// 100 goroutines race a single-key bucket. rps is chosen so that
	// wall-clock goroutine dispatch (single-digit ms) cannot drive a
	// material refill: period = 1s/100 = 10ms, delayTol = burst*period
	// = 500ms. The test completes in milliseconds, so refill is at
	// most a handful of slots. The assertion verifies the CAS loop
	// admits exactly the burst budget plus the GCRA boundary extra
	// (see TestCheck_SuccessiveRequestsDecrementRemaining) — never
	// below, never by a wide margin above.
	const goroutines = 100
	const rps = 100
	const burst = 50
	var wg sync.WaitGroup
	var allowed atomic.Int32

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := s.Allow(context.Background(), "shared-key", rps, burst)
			if err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	got := int(allowed.Load())
	// Lower bound: the CAS loop MUST admit at least burst requests.
	// Upper bound: burst + 1 boundary + small refill slack during
	// goroutine spawn on a slow CI machine.
	assert.GreaterOrEqual(t, got, burst, "must allow at least burst")
	assert.LessOrEqual(t, got, burst+5, "must not exceed burst by more than 5 (boundary+refill tolerance)")
}

func TestMemoryStore_FlushPrefixRemovesMatchingKeys(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	_, _ = s.Allow(ctx, "GET.a.b", 10, 20)
	_, _ = s.Allow(ctx, "GET.a.c", 10, 20)
	_, _ = s.Allow(ctx, "POST.x.y", 10, 20)

	require.NoError(t, s.FlushPrefix(ctx, "GET."))

	// Flushed keys should behave like fresh buckets.
	d, err := s.Allow(ctx, "GET.a.b", 10, 20)
	require.NoError(t, err)
	assert.Equal(t, 19, d.Remaining, "fresh bucket after flush")
}

func TestMemoryStore_TTLSweepRemovesIdleEntries(t *testing.T) {
	// Short TTL so entries age out quickly; the sweeper interval is
	// max(ttl/10, 1s), so we must wait longer than one full tick
	// after entry expiry to observe the delete.
	s := NewMemoryStore(100 * time.Millisecond)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	_, _ = s.Allow(ctx, "k", 10, 20)
	assert.Equal(t, 1, countEntries(s))

	// ttl expires at t=100ms; sweeper ticks every 1s (floor). Poll up
	// to 3s to avoid flaking on a busy CI scheduler.
	deadline := time.Now().Add(3 * time.Second)
	for countEntries(s) > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	assert.Equal(t, 0, countEntries(s), "idle entry should be swept")
}

// Helper: count entries in the internal sync.Map.
func countEntries(s *MemoryStore) int {
	n := 0
	s.entries.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestMemoryStore_AllowHonoursCtxCancellation pins the cancellation
// contract: even though the CAS loop is pure CPU, callers that
// cancel their context before invocation expect Allow to surface
// ctx.Err() rather than producing a side-effecting decision. Honouring
// the cancellation keeps Memory and NATS-KV semantically aligned for
// shared upstream timeout chains.
func TestMemoryStore_AllowHonoursCtxCancellation(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Allow(ctx, "k", 10, 5)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled),
		"cancelled ctx must propagate as context.Canceled")
}

// TestMemoryStore_Close_ConcurrentCallsAreIdempotent pins the
// guarantee that concurrent shutdown does not race the channel close.
// The previous select/close implementation could panic when two
// goroutines passed the default arm before either reached close(stop).
func TestMemoryStore_Close_ConcurrentCallsAreIdempotent(t *testing.T) {
	s := NewMemoryStore(time.Minute)

	const goroutines = 32
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		errMu  sync.Mutex
		errors []error
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := s.Close(); err != nil {
				errMu.Lock()
				errors = append(errors, err)
				errMu.Unlock()
			}
		}()
	}

	assert.NotPanics(t, func() {
		close(start)
		wg.Wait()
	}, "concurrent Close calls must not panic on the channel close")

	assert.Empty(t, errors, "every Close call must return nil")
}

// TestMemoryStore_SaturationRejectsNewKey pins the cardinality cap:
// once entriesSize reaches maxEntries, a brand-new key receives
// ErrMemoryStoreSaturated. Existing keys still resolve normally —
// the cap protects against carpet-bombing attacks rotating IPs, not
// against legitimate active clients.
func TestMemoryStore_SaturationRejectsNewKey(t *testing.T) {
	s := NewMemoryStoreWithCap(time.Minute, 2)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	dec, err := s.Allow(ctx, "k1", 100, 10)
	require.NoError(t, err)
	assert.True(t, dec.Allowed)

	dec, err = s.Allow(ctx, "k2", 100, 10)
	require.NoError(t, err)
	assert.True(t, dec.Allowed)

	// Brand-new key beyond the cap must surface ErrMemoryStoreSaturated.
	_, err = s.Allow(ctx, "k3-new", 100, 10)
	assert.ErrorIs(t, err, ErrMemoryStoreSaturated,
		"new key beyond cap must surface ErrMemoryStoreSaturated for FailPolicy routing")

	// Counters reflect the saturation event.
	c := s.Counters()
	assert.Equal(t, int64(1), c["ratelimit_memory_saturated_total"],
		"saturation counter must tick once per refused new key")

	// Existing keys still work — the cap only refuses new admissions.
	dec, err = s.Allow(ctx, "k1", 100, 10)
	require.NoError(t, err)
	assert.True(t, dec.Allowed,
		"existing keys must keep resolving even when the store is at capacity")
}

// TestMemoryStore_SaturationCapZeroDisablesCheck pins the legacy
// behaviour: NewMemoryStore (no cap) leaves the door open. Operators
// who do not set RATELIMIT_MEMORY_MAX_ENTRIES keep the pre-cap
// semantics.
func TestMemoryStore_SaturationCapZeroDisablesCheck(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		// strconv.Itoa(i) yields 1000 distinct keys; the previous
		// strings.Repeat("a", i%20) cycled and only generated 20
		// distinct keys, weakening the cardinality-spike assertion.
		key := "key-" + strconv.Itoa(i)
		_, err := s.Allow(ctx, key, 100, 10)
		require.NoError(t, err, "uncapped store must never refuse new keys")
	}
}
