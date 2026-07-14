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
