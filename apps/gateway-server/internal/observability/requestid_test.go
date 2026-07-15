package observability

import (
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRequestID_ReturnsULIDString(t *testing.T) {
	id := NewRequestID()
	assert.Len(t, id, 26, "ULID canonical string is 26 chars")
}

func TestNewRequestID_IsUnique(t *testing.T) {
	const iterations = 1000
	seen := make(map[string]struct{}, iterations)
	for i := 0; i < iterations; i++ {
		id := NewRequestID()
		_, dup := seen[id]
		require.Falsef(t, dup, "duplicate ULID at iteration %d: %s", i, id)
		seen[id] = struct{}{}
	}
}

// constByteReader emits an endless stream of one byte value. Seeding
// a monotonic ULID source from a 0xFF stream pins its 80-bit entropy
// at the maximum, so the next same-millisecond read overflows
// deterministically.
type constByteReader byte

func (c constByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c)
	}

	return len(p), nil
}

// TestULIDMonotonic_SameMillisecondOverflow_ReturnsError pins the
// oklog/ulid v2 behaviour the overflow fallback in NewRequestID is
// built on: a monotonic source whose entropy sits at the 80-bit
// maximum returns ErrMonotonicOverflow from ulid.New on the next
// same-millisecond read. Permanent verification fixture — a library
// upgrade that changes overflow semantics fails loudly here instead
// of silently invalidating the fallback.
func TestULIDMonotonic_SameMillisecondOverflow_ReturnsError(t *testing.T) {
	src := ulid.Monotonic(constByteReader(0xFF), 0)
	ms := ulid.Timestamp(time.Now())

	_, err := ulid.New(ms, src)
	require.NoError(t, err, "first read seeds entropy at the maximum")

	_, err = ulid.New(ms, src)
	require.ErrorIs(t, err, ulid.ErrMonotonicOverflow,
		"same-millisecond increment past max entropy must surface as ErrMonotonicOverflow")
}

// TestNewRequestID_MonotonicOverflow_RecoversWithoutPanic pins the
// graceful-degradation contract: when the shared monotonic source
// overflows within a millisecond, NewRequestID returns a valid ULID
// (fresh non-monotonic entropy) instead of panicking, and reseeds the
// source so subsequent calls run monotonic again.
func TestNewRequestID_MonotonicOverflow_RecoversWithoutPanic(t *testing.T) {
	poisoned := ulid.Monotonic(constByteReader(0xFF), 0)

	requestIDMu.Lock()
	original := requestIDSource
	requestIDSource = poisoned
	requestIDMu.Unlock()

	t.Cleanup(func() {
		requestIDMu.Lock()
		requestIDSource = original
		requestIDMu.Unlock()
	})

	// The poisoned source seeds max entropy on the first call within a
	// millisecond, so any second same-millisecond call overflows. Loop
	// until the recovery path replaces the source; a tight loop crosses
	// only a handful of millisecond boundaries, so this terminates
	// almost immediately.
	deadline := time.Now().Add(10 * time.Second)
	for {
		id := NewRequestID()
		require.Len(t, id, 26, "overflow must degrade gracefully, never truncate")
		_, parseErr := ulid.ParseStrict(id)
		require.NoError(t, parseErr, "every returned ID must stay a canonical ULID")

		requestIDMu.Lock()
		reseeded := requestIDSource != poisoned
		requestIDMu.Unlock()
		if reseeded {
			break
		}

		require.True(t, time.Now().Before(deadline),
			"overflow path never triggered — poisoned source should overflow on the first same-millisecond pair")
	}

	// Post-recovery calls run on the reseeded source.
	next := NewRequestID()
	_, parseErr := ulid.ParseStrict(next)
	require.NoError(t, parseErr)
}

func TestNewRequestID_ConcurrentSafe(t *testing.T) {
	const goroutines = 32
	const perGoroutine = 100

	results := make(chan string, goroutines*perGoroutine)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				results <- NewRequestID()
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]struct{}, goroutines*perGoroutine)
	for id := range results {
		_, dup := seen[id]
		require.False(t, dup, "duplicate ULID under concurrent load")
		seen[id] = struct{}{}
	}
}
