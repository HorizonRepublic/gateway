package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrMemoryStoreSaturated is returned by MemoryStore.Allow when the
// store has reached its configured cardinality cap and refuses to
// admit a brand-new key. Existing keys still resolve normally — only
// the LoadOrStore path bumps the cap. The handler's FailPolicy maps
// the error onto the deployment's allow/reject choice the same way
// it handles transient backend outages, so a saturation event under
// closed-policy returns 503 (the cleanest available signal that
// rate-limit cannot be enforced) and under open-policy passes the
// request through.
var ErrMemoryStoreSaturated = errors.New("ratelimit memory: store at capacity")

// MemoryStore is an in-process GCRA rate limiter. Each bucket's
// state is a single atomic int64 holding TAT (Theoretical Arrival
// Time) in Unix nanoseconds.
//
// Semantically identical to NATSKVStore — switching between them
// produces the same decisions for the same (key, rps, burst).
//
// Trade-off vs NATSKVStore: no cross-replica sharing. Each pod
// tracks its own buckets. Use for single-instance deployments or
// hot-path routes where network-store latency is unacceptable.
type MemoryStore struct {
	entries sync.Map // map[string]*memoryEntry
	// entriesSize tracks the live-entry cardinality so Allow can
	// short-circuit on saturation without walking the whole map. The
	// counter is incremented on a successful LoadOrStore that creates
	// a new entry, and decremented by the sweeper when an idle entry
	// is reaped. Drift from the actual map size is bounded by the
	// sweeper cadence and is acceptable — the cap is a soft ceiling,
	// not a hard quota.
	entriesSize atomic.Int64
	// maxEntries caps the number of live entries the store admits.
	// Zero means "unbounded" (legacy behaviour) so callers that build
	// a MemoryStore with NewMemoryStore (no cap) keep working unchanged.
	maxEntries int64
	ttl        time.Duration
	stop       chan struct{}
	// closeOnce serializes Close so concurrent shutdown callers cannot
	// race the select/close sequence and panic on a double close. The
	// sync.Once token is consumed on the first call regardless of
	// success, which matches the documented "idempotent" contract.
	closeOnce sync.Once

	counters struct {
		allowed   atomic.Int64
		rejected  atomic.Int64
		saturated atomic.Int64
	}
}

type memoryEntry struct {
	tat      atomic.Int64 // Unix nanoseconds
	lastSeen atomic.Int64 // Unix nanoseconds
}

// NewMemoryStore constructs a MemoryStore with the given idle-key
// TTL and no cardinality cap. Equivalent to NewMemoryStoreWithCap(ttl, 0).
//
// TTL semantics (Memory): the TTL is an idle-entry sweep interval.
// An entry is reaped only after no Allow call has touched it for
// ttl. A continuously-active key persists indefinitely; the GCRA
// state stays valid for as long as the key is in use. This differs
// from NATSKVStore, which interprets the same configuration value as
// a hard MaxAge: every key is reaped after ttl regardless of activity.
// Operators wiring RATELIMIT_KEY_TTL must understand the divergence
// when comparing per-bucket lifetime across a backend swap.
//
// Example:
//
//	store := NewMemoryStore(24 * time.Hour)
//	defer store.Close()
//	decision, err := store.Allow(ctx, "user:1234", 100, 10)
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	return NewMemoryStoreWithCap(ttl, 0)
}

// NewMemoryStoreWithCap constructs a MemoryStore with both a TTL and
// a maximum-entry cardinality cap. When the live-entry count reaches
// maxEntries, Allow refuses to admit new keys with
// ErrMemoryStoreSaturated; existing keys continue to resolve. The
// sweeper still runs on its TTL cadence and drops the count back
// below the cap as idle keys age out.
//
// maxEntries == 0 disables the cap (legacy unbounded behaviour). Use
// the cap on production deployments to bound RAM under cardinality-
// spike attacks (an attacker rotating source IP every request) — the
// observed worst case at 64-byte keys + 64-byte memoryEntry is
// roughly 122 MiB per million entries (128 MB decimal), fits comfortably
// inside a typical pod budget.
func NewMemoryStoreWithCap(ttl time.Duration, maxEntries int64) *MemoryStore {
	s := &MemoryStore{ttl: ttl, maxEntries: maxEntries, stop: make(chan struct{})}
	go s.sweep()
	return s
}

// Allow implements Store by running GCRA against an in-memory TAT.
//
// The atomic CAS loop is the hot path: LoadOrStore the entry, load
// the current TAT, compute the decision via Check, and either
// return (rejection path) or CAS the new TAT into place. A lost
// CAS means another goroutine advanced the TAT for the same key —
// retry so the late arrival sees the updated state.
//
// ctx is consulted at the top of the loop so a cancelled or
// deadline-exceeded request surfaces ctx.Err() rather than producing
// a side-effecting decision. The check is a single atomic load
// against ctx.Done(), negligible against the rest of the loop, and
// keeps Memory and NATS-KV aligned for callers wiring shared upstream
// timeout chains.
func (s *MemoryStore) Allow(ctx context.Context, key string, rps, burst int) (Decision, error) {
	// Honour ctx cancellation BEFORE any side-effect — a cancelled
	// caller MUST NOT cause a fresh entry to land in the map (and tick
	// entriesSize / waste a saturation slot). The CAS retry loop
	// re-checks ctx on every iteration; this entry-point check covers
	// the no-iteration case where the caller cancelled before Allow ran.
	if err := ctx.Err(); err != nil {
		return Decision{}, fmt.Errorf("ratelimit memory: %w", err)
	}

	now := time.Now()

	// Cardinality-cap fast path: if this is a brand-new key AND the
	// store is already at capacity, refuse without growing the map.
	// We probe with Load first to avoid the LoadOrStore allocation
	// when the map is saturated and the key is not present. Existing
	// keys (Load hit) bypass the cap regardless of size — keeping a
	// known bucket usable is more important than enforcing a
	// post-hoc cap.
	if s.maxEntries > 0 {
		if _, ok := s.entries.Load(key); !ok {
			if s.entriesSize.Load() >= s.maxEntries {
				s.counters.saturated.Add(1)
				return Decision{}, ErrMemoryStoreSaturated
			}
		}
	}

	v, loaded := s.entries.LoadOrStore(key, &memoryEntry{})
	if !loaded {
		s.entriesSize.Add(1)
	}
	e := v.(*memoryEntry)
	e.lastSeen.Store(now.UnixNano())

	for {
		if err := ctx.Err(); err != nil {
			return Decision{}, fmt.Errorf("ratelimit memory: %w", err)
		}

		currentNs := e.tat.Load()
		currentTAT := time.Unix(0, currentNs)
		decision, newTAT := Check(currentTAT, now, rps, burst)

		if !decision.Allowed {
			s.counters.rejected.Add(1)
			return decision, nil
		}
		if e.tat.CompareAndSwap(currentNs, newTAT.UnixNano()) {
			s.counters.allowed.Add(1)
			return decision, nil
		}
		// CAS failed (another goroutine won); retry.
	}
}

// FlushPrefix removes all entries whose key begins with prefix.
func (s *MemoryStore) FlushPrefix(_ context.Context, prefix string) error {
	s.entries.Range(func(k, _ any) bool {
		if ks, ok := k.(string); ok && strings.HasPrefix(ks, prefix) {
			if _, deleted := s.entries.LoadAndDelete(k); deleted {
				s.entriesSize.Add(-1)
			}
		}
		return true
	})
	return nil
}

// Close is idempotent and safe to call from multiple goroutines.
// sync.Once guards the channel close so a concurrent second invocation
// observes the consumed token and returns without re-closing the
// channel — racing two close(stop) calls would panic the runtime.
func (s *MemoryStore) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
	})
	return nil
}

// Counters returns a snapshot of internal counters for OpenTelemetry
// plumbing. Each value is read atomically so callers see a consistent
// point-in-time view.
//
// MemoryStore has no remote dependencies, so backend_errors_total
// counts saturation events (ErrMemoryStoreSaturated rejections) — the
// only "backend failure" mode the in-process store can produce.
// Operators monitoring the unified backend_errors_total metric across
// memory + nats-kv pods see saturation spikes alongside NATS-KV
// connection failures. saturated_total is exposed under the
// memory-specific key as well so a dashboard that wants the
// memory-only signal can read it without wildcard-matching the
// uniform key.
func (s *MemoryStore) Counters() map[string]int64 {
	return map[string]int64{
		"ratelimit_memory_decisions_allowed_total":  s.counters.allowed.Load(),
		"ratelimit_memory_decisions_rejected_total": s.counters.rejected.Load(),
		"ratelimit_memory_backend_errors_total":     s.counters.saturated.Load(),
		"ratelimit_memory_saturated_total":          s.counters.saturated.Load(),
	}
}

func (s *MemoryStore) sweep() {
	interval := s.ttl / 10
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			cutoff := now.Add(-s.ttl).UnixNano()
			s.entries.Range(func(k, v any) bool {
				if e, ok := v.(*memoryEntry); ok && e.lastSeen.Load() < cutoff {
					if _, deleted := s.entries.LoadAndDelete(k); deleted {
						s.entriesSize.Add(-1)
					}
				}
				return true
			})
		}
	}
}
