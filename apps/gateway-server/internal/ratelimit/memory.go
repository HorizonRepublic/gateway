package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tombstoneNs is the lastSeen sentinel a sweeper stamps on an entry it
// has claimed for deletion. The claim is a CompareAndSwap from the
// stale timestamp the sweeper observed, so a concurrent Allow that
// refreshes lastSeen first wins the race and keeps the entry alive.
// Once tombstoned, an entry is dead: Allow never writes into it and
// instead removes the corpse and creates a fresh entry. UnixNano can
// never produce this value (math.MinInt64 is ~292 years before the
// epoch, outside time.Time's representable Unix-nanosecond range).
const tombstoneNs = math.MinInt64

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
	// counter is incremented when an insert creates a new entry and
	// decremented by whichever party's pointer-guarded CompareAndDelete
	// removes one (the sweeper, or an Allow helping a claimed corpse
	// out of the map). Drift from the actual map size is bounded by
	// the sweeper cadence and is acceptable — the cap is a soft
	// ceiling, not a hard quota.
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
//
// ttl MUST be > 0; the constructor panics otherwise, mirroring
// time.NewTicker's contract for non-positive durations. A non-positive
// TTL would place the sweep cutoff at or beyond "now" and reap every
// bucket on every tick — a silent fail-open where each key regains its
// full burst each second. Config validation rejects
// RATELIMIT_KEY_TTL <= 0 at startup; the panic is the in-package
// backstop for programmatic misuse.
func NewMemoryStoreWithCap(ttl time.Duration, maxEntries int64) *MemoryStore {
	if ttl <= 0 {
		panic("ratelimit: MemoryStore ttl must be > 0")
	}
	s := &MemoryStore{ttl: ttl, maxEntries: maxEntries, stop: make(chan struct{})}
	go s.sweep()
	return s
}

// Allow implements Store by running GCRA against an in-memory TAT.
//
// The atomic CAS loop is the hot path: acquire the live entry (a
// lock-free Load plus a lastSeen CAS; allocation only on a key's
// first sight), load the current TAT, compute the decision via
// Check, and either return (rejection path) or CAS the new TAT into
// place. A lost CAS means another goroutine advanced the TAT for the
// same key — retry so the late arrival sees the updated state.
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

	e, err := s.acquireEntry(key, now.UnixNano())
	if err != nil {
		s.counters.saturated.Add(1)
		return Decision{}, err
	}

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

// acquireEntry returns a live *memoryEntry for key with lastSeen
// refreshed to nowNs, creating one when absent. Allocation happens
// only on first sight of a key — the hit path is a lock-free Load
// plus a lastSeen CAS.
//
// The tombstone handshake with the sweeper guarantees the returned
// entry is the key's only live incarnation: an entry the sweeper has
// claimed (lastSeen == tombstoneNs) is never returned. Instead the
// corpse is removed — helping the sweeper along so the loop cannot
// spin against a claimed-but-not-yet-deleted entry — and the retry
// creates a fresh entry. Both removal sites use the pointer-guarded
// CompareAndDelete, so exactly one party decrements entriesSize.
//
// Returns ErrMemoryStoreSaturated when the key is new and the store
// is at its cardinality cap. Existing keys bypass the cap — keeping a
// known bucket usable is more important than enforcing a post-hoc cap.
func (s *MemoryStore) acquireEntry(key string, nowNs int64) (*memoryEntry, error) {
	for {
		if v, ok := s.entries.Load(key); ok {
			e := v.(*memoryEntry)
			if touchEntry(e, nowNs) {
				return e, nil
			}
			if s.entries.CompareAndDelete(key, v) {
				s.entriesSize.Add(-1)
			}
			continue
		}

		if s.maxEntries > 0 && s.entriesSize.Load() >= s.maxEntries {
			return nil, ErrMemoryStoreSaturated
		}

		fresh := &memoryEntry{}
		// Stamp lastSeen before publication so no other goroutine can
		// ever observe the zero value (which a sweeping Range would
		// treat as ancient and instantly claim).
		fresh.lastSeen.Store(nowNs)
		v, loaded := s.entries.LoadOrStore(key, fresh)
		if !loaded {
			s.entriesSize.Add(1)
			return fresh, nil
		}

		// Lost the insert race to a concurrent Allow; adopt the
		// winner's entry through the same tombstone-aware touch.
		e := v.(*memoryEntry)
		if touchEntry(e, nowNs) {
			return e, nil
		}
		if s.entries.CompareAndDelete(key, v) {
			s.entriesSize.Add(-1)
		}
	}
}

// touchEntry refreshes e.lastSeen to nowNs unless the sweeper has
// tombstoned the entry. Reports whether the entry is live. The CAS
// (rather than a plain Store) is what closes the lost-update race:
// the sweeper's claim is also a CAS from the timestamp it observed,
// so exactly one of the two transitions wins and the loser observes
// the winner's value.
func touchEntry(e *memoryEntry, nowNs int64) bool {
	for {
		last := e.lastSeen.Load()
		if last == tombstoneNs {
			return false
		}
		if last >= nowNs {
			return true
		}
		if e.lastSeen.CompareAndSwap(last, nowNs) {
			return true
		}
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
			s.sweepOnce(now.Add(-s.ttl).UnixNano())
		}
	}
}

// sweepOnce reaps every entry idle since before cutoffNs. Extracted
// from the ticker loop so tests can drive sweeps deterministically.
//
// Reaping is a two-phase handshake with Allow rather than a bare
// delete: first claim the entry by CAS-ing lastSeen from the observed
// stale timestamp to the tombstone, then remove it with a
// pointer-guarded CompareAndDelete. A concurrent Allow that refreshes
// lastSeen between the Range read and the claim makes the CAS fail and
// the entry survives — its in-flight TAT write cannot be orphaned. The
// pointer guard on the delete protects the other direction: if a
// racing Allow already removed the corpse and installed a fresh entry
// under the same key, the sweeper's delete misses and the fresh entry
// lives on. entriesSize is decremented exactly once, by whichever
// party's CompareAndDelete succeeds.
func (s *MemoryStore) sweepOnce(cutoffNs int64) {
	s.entries.Range(func(k, v any) bool {
		e, ok := v.(*memoryEntry)
		if !ok {
			return true
		}
		last := e.lastSeen.Load()
		if last == tombstoneNs || last >= cutoffNs {
			return true
		}
		if !e.lastSeen.CompareAndSwap(last, tombstoneNs) {
			return true
		}
		if s.entries.CompareAndDelete(k, e) {
			s.entriesSize.Add(-1)
		}
		return true
	})
}
