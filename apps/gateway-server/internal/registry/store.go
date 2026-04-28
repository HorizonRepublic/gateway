package registry

import "sync/atomic"

// Snapshot is an immutable view of the handler_registry bucket at one
// point in time.
//
// Readers MUST treat the Entries map as read-only — it is shared across
// all goroutines that called Store.Get() since the snapshot was published.
// Mutating a returned snapshot would create a race with concurrent readers
// and violate the atomic-swap invariant.
type Snapshot struct {
	Entries map[string]HandlerEntry
}

// Store holds the current Snapshot of the handler_registry bucket.
//
// Reads are lock-free via atomic.Pointer: callers get a stable view that
// cannot tear or partially-update, even during concurrent writes. Writes
// happen only from the Watcher's goroutine and replace the entire
// snapshot atomically — there is no in-place map mutation.
//
// This "all or nothing" swap eliminates torn reads, removes the need for
// RWMutex contention on the hot path, and makes reasoning about the read
// semantics trivial: every Get() returns a consistent snapshot. The cost
// is rebuilding the whole map on each change, which is cheap because the
// bucket is bounded (typically fewer than a thousand handler entries in
// practice) and changes are infrequent compared to request throughput.
type Store struct {
	snapshot atomic.Pointer[Snapshot]
}

// NewStore returns a Store pre-populated with an empty snapshot so that
// callers never see a nil map on the read path.
func NewStore() *Store {
	s := &Store{}
	s.snapshot.Store(&Snapshot{Entries: map[string]HandlerEntry{}})
	return s
}

// Replace atomically swaps in a new snapshot. Called only by the Watcher's
// goroutine as a response to KV change events.
func (s *Store) Replace(next *Snapshot) {
	s.snapshot.Store(next)
}

// Get returns the current snapshot. The returned value is immutable from
// the caller's perspective and may safely outlive subsequent Replace calls.
func (s *Store) Get() *Snapshot {
	return s.snapshot.Load()
}
