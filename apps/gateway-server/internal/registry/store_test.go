package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStore_InitialSnapshotIsEmpty(t *testing.T) {
	s := NewStore()
	assert.NotNil(t, s.Get())
	assert.Empty(t, s.Get().Entries)
}

func TestStore_ReplaceUpdatesSnapshot(t *testing.T) {
	s := NewStore()
	method := "GET"
	s.Replace(&Snapshot{Entries: map[string]HandlerEntry{
		"svc.cmd.users.get": {HTTP: &HTTPMeta{Method: method, Path: "/users/:id"}},
	}})

	snap := s.Get()
	assert.Len(t, snap.Entries, 1)
	assert.Equal(t, method, snap.Entries["svc.cmd.users.get"].HTTP.Method)
}

// TestStore_ConcurrentReadWriteNoRace exercises the atomic.Pointer read
// path under contention from multiple writer goroutines. The Go race
// detector (enabled via the Nx `test` target's `-race` flag) would fire
// if any read observed a torn update.
//
// To verify the atomic pointer is load-bearing, change `atomic.Pointer`
// to a plain map read in Store.Get and rerun with `-race`: the detector
// will report concurrent read/write access immediately.
func TestStore_ConcurrentReadWriteNoRace(t *testing.T) {
	s := NewStore()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup

	const writers = 8
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					s.Replace(&Snapshot{Entries: map[string]HandlerEntry{
						fmt.Sprintf("writer-%d", id): {HTTP: &HTTPMeta{Method: "GET", Path: "/"}},
					}})
				}
			}
		}(i)
	}

	const readers = 32
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					snap := s.Get()
					_ = snap.Entries
				}
			}
		}()
	}

	wg.Wait()
}
