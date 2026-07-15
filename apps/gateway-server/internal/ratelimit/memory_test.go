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

	// rps=100 → period=10ms, delayTol=(burst-1)*period=40ms. The full
	// drain loop finishes in well under a period, so no mid-test refill
	// can mask the rejection — any sub-μs advance of wall time is
	// dwarfed by delayTol. burst=5 admits exactly 5 back-to-back calls
	// (see TestCheck_ExactBurstAdmissions); the 6th MUST reject.
	for i := 0; i < 5; i++ {
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
	// material refill: period = 1s/100 = 10ms, delayTol =
	// (burst-1)*period = 490ms. The test completes in milliseconds, so
	// refill is at most a handful of slots. The assertion verifies the
	// CAS loop admits exactly the burst budget — never below, and above
	// only by the small refill slack a slow CI machine can introduce.
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

// TestNewMemoryStore_RejectsNonPositiveTTL pins the construction-time
// contract: a zero or negative TTL previously made the sweeper reap
// EVERY entry on every tick (cutoff at or beyond now), silently
// resetting all buckets to full burst each second — a fail-open
// nobody asked for. Config validation rejects RATELIMIT_KEY_TTL <= 0
// at startup; the constructor panic is the in-package backstop for
// programmatic misuse, mirroring time.NewTicker's contract for
// non-positive durations.
func TestNewMemoryStore_RejectsNonPositiveTTL(t *testing.T) {
	assert.Panics(t, func() { NewMemoryStore(0) },
		"ttl=0 must be rejected at construction")
	assert.Panics(t, func() { NewMemoryStore(-time.Second) },
		"negative ttl must be rejected at construction")
	assert.Panics(t, func() { NewMemoryStoreWithCap(0, 100) },
		"the capped constructor shares the ttl contract")
}

// TestMemoryStore_SweeperSkipsFreshlyTouchedEntry pins the Allow-wins
// side of the sweeper handshake: an entry whose lastSeen was refreshed
// after the sweeper computed its cutoff MUST survive the sweep — the
// tombstone CAS fails against the fresh timestamp, so the in-flight
// request's TAT advance stays in the live map entry.
func TestMemoryStore_SweeperSkipsFreshlyTouchedEntry(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_, err := s.Allow(ctx, "k", 100, 5)
	require.NoError(t, err)

	v, ok := s.entries.Load("k")
	require.True(t, ok)
	before := v.(*memoryEntry)

	// The entry was touched "now"; a sweep with the regular cutoff
	// (now - ttl) must leave it alone.
	s.sweepOnce(time.Now().Add(-s.ttl).UnixNano())

	after, ok := s.entries.Load("k")
	require.True(t, ok, "freshly touched entry must survive the sweep")
	assert.Same(t, before, after.(*memoryEntry),
		"the sweep must not replace the live entry")
}

// TestMemoryStore_AllowDoesNotResurrectTombstonedEntry pins the
// sweeper-wins side of the handshake: once the sweeper claims an entry
// via the tombstone CAS, a racing Allow MUST NOT write its TAT into
// the doomed object. It removes the corpse, creates a fresh entry, and
// accounts the decision there — so the key's GCRA state never splits
// across a dead and a live entry.
func TestMemoryStore_AllowDoesNotResurrectTombstonedEntry(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	_, err := s.Allow(ctx, "k", 100, 5)
	require.NoError(t, err)

	v, ok := s.entries.Load("k")
	require.True(t, ok)
	doomed := v.(*memoryEntry)
	doomedTAT := doomed.tat.Load()

	// Simulate the sweeper winning the claim: tombstone the entry but
	// pause before its CompareAndDelete (the widest possible race
	// window). Allow must neither spin forever nor touch the corpse.
	doomed.lastSeen.Store(tombstoneNs)

	d, err := s.Allow(ctx, "k", 100, 5)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 4, d.Remaining, "decision must come from a fresh bucket")

	assert.Equal(t, doomedTAT, doomed.tat.Load(),
		"the tombstoned entry's TAT must stay untouched")

	fresh, ok := s.entries.Load("k")
	require.True(t, ok, "Allow must install a fresh live entry")
	assert.NotSame(t, doomed, fresh.(*memoryEntry))

	// The delayed sweeper delete must not remove the fresh entry.
	assert.False(t, s.entries.CompareAndDelete("k", doomed),
		"the sweeper's pointer-guarded delete must miss the fresh entry")
	_, ok = s.entries.Load("k")
	assert.True(t, ok)
}

// TestMemoryStore_AllowRacesSweeperWithoutStateDrift hammers Allow on
// a small key set while a rival goroutine sweeps with an aggressive
// cutoff, forcing the tombstone handshake to fire continuously. Run
// under -race this shakes out unsynchronized access; the closing
// assertions pin the bookkeeping invariant that entriesSize matches
// the live map cardinality after the dust settles (a lost decrement or
// double decrement would silently corrupt the saturation cap).
func TestMemoryStore_AllowRacesSweeperWithoutStateDrift(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	const (
		workers    = 8
		iterations = 2_000
	)
	var wg sync.WaitGroup

	stopSweep := make(chan struct{})
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		for {
			select {
			case <-stopSweep:
				return
			default:
				// Cutoff at "now": everything not touched in this very
				// instant is claimed — maximal contention with Allow.
				s.sweepOnce(time.Now().UnixNano())
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := "k-" + strconv.Itoa(w%3)
			for i := 0; i < iterations; i++ {
				_, err := s.Allow(ctx, key, 1_000_000, 100)
				assert.NoError(t, err)
			}
		}(w)
	}

	wg.Wait()
	close(stopSweep)
	<-sweepDone
	// Run one final deterministic sweep to flush any half-claimed
	// entries before checking the bookkeeping.
	s.sweepOnce(time.Now().Add(time.Hour).UnixNano())

	assert.Equal(t, int64(countEntries(s)), s.entriesSize.Load(),
		"entriesSize must equal live map cardinality after concurrent sweeps")
}

// TestMemoryStore_HotKeyAllowDoesNotAllocate pins the hot-path
// allocation contract: Allow on an existing key must not heap-allocate.
// The regression this guards: LoadOrStore's unconditionally evaluated
// &memoryEntry{} argument allocated 16 B per call even on hits.
func TestMemoryStore_HotKeyAllowDoesNotAllocate(t *testing.T) {
	s := NewMemoryStore(time.Hour)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Prime the key and let sync.Map promote it to the read-only map.
	for i := 0; i < 100; i++ {
		_, _ = s.Allow(ctx, "hot", 1_000_000, 1_000_000)
	}

	allocs := testing.AllocsPerRun(1_000, func() {
		_, _ = s.Allow(ctx, "hot", 1_000_000, 1_000_000)
	})
	assert.Zero(t, allocs, "steady-state Allow on an existing key must not allocate")
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
