package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// ErrStoreClosed is returned by the sentinel store installed once a
// Router has been Close()d. In-flight requests that race shutdown
// observe this error from Store.Allow and route through the
// configured FailPolicy, turning the race into a well-defined
// backend-outage decision instead of a panic or a call into a
// half-torn-down store.
var ErrStoreClosed = errors.New("ratelimit: router closed")

// Router dispatches Allow calls to the appropriate Store backend
// based on a route's declared store id ("memory", "nats-kv",
// "redis"). Both gateway startup and the registry watcher's
// hot-reload path register backends through the single EnsureBackend
// method so the wiring stays uniform across cold boot and live
// reconfiguration.
//
// StoreFor falls back to "memory" when a route has no RateLimit
// config, declares an empty store id, or declares a backend the
// gateway has not registered (e.g. "redis" before a Redis adapter
// lands). The fallback is safe — memory is always registered at
// startup and produces correct rate-limit decisions, though per-pod
// rather than multi-replica. Every fallback bumps a counter so
// operators can surface misconfiguration through metrics once
// observability is wired.
type Router struct {
	mu         sync.RWMutex
	stores     map[string]Store
	closed     bool
	failPolicy Policy
	logger     zerolog.Logger
	// fallbackLogged dedupes the per-route "declared backend not
	// registered" WARN. Keys are `<method>:<pathTemplate>|<declared>`;
	// LoadOrStore decides whether the current goroutine emits the log
	// line for that tuple. The fallback counter still ticks per
	// request — only the log message is throttled.
	fallbackLogged sync.Map
	counters       struct {
		fallback        atomic.Int64
		claimsUnmarshal atomic.Int64
	}
}

// closedStore is the Store implementation returned by StoreFor after
// Close(). Allow always surfaces ErrStoreClosed so the handler's
// FailPolicy path runs; FlushPrefix and Close are idempotent no-ops.
//
// The Decision returned alongside ErrStoreClosed is intentionally
// populated with header-safe defaults: Allowed=true so a fail-open
// caller that ignores the error sees an unambiguous pass-through, and
// ResetAt=time.Now() so downstream BuildHeaders does not encode
// time.Time{}.Unix() (a negative year-1 epoch) as X-RateLimit-Reset.
// Conscientious callers that honour the error path are unaffected by
// these defaults; the safeguard exists for the failure mode where
// they don't.
type closedStore struct{}

func (closedStore) Allow(_ context.Context, _ string, _, _ int) (Decision, error) {
	return Decision{Allowed: true, Remaining: 0, ResetAt: time.Now()}, ErrStoreClosed
}

func (closedStore) FlushPrefix(_ context.Context, _ string) error { return nil }

func (closedStore) Close() error { return nil }

// Counters returns the minimum schema with zero-valued metrics. The
// closed sentinel never serves real traffic, so the values are
// constants — but the keys are still emitted so the dashboard schema
// stays uniform across the gateway's lifetime regardless of when
// shutdown happens.
func (closedStore) Counters() map[string]int64 {
	return map[string]int64{
		"ratelimit_closed_decisions_allowed_total":  0,
		"ratelimit_closed_decisions_rejected_total": 0,
		"ratelimit_closed_backend_errors_total":     0,
	}
}

// NewRouter creates an empty Router bound to the given fail-policy
// and logger. Callers MUST register at least the "memory" backend
// via EnsureBackend before any Allow call reaches the router; the
// gateway bootstrap is responsible for this invariant.
//
// Example:
//
//	policy := FailPolicyOpen.Resolve()
//	router := NewRouter(policy, log)
//	// Register backends during startup.
//	router.EnsureBackend("memory", func() (Store, error) {
//		return NewMemoryStore(24 * time.Hour), nil
//	})
//	// Routes automatically select their store; hot-reload re-calls EnsureBackend.
func NewRouter(failPolicy Policy, logger zerolog.Logger) *Router {
	return &Router{
		stores:     make(map[string]Store),
		failPolicy: failPolicy,
		logger:     logger,
	}
}

// EnsureBackend registers a Store for the given id if one is not
// already present. Once a backend is in the map, subsequent calls for
// the same id are no-ops so the watcher hot-reload path can re-scan
// the registry on every delta without duplicate instantiation or side
// effects.
//
// factory runs OUTSIDE the router mutex. Backend construction can
// involve multiple network round trips (the NATS-KV factory opens and
// possibly creates a JetStream bucket); holding the exclusive lock
// across that I/O would stall StoreFor — and with it every
// rate-limited request — for the whole factory duration, precisely
// while the backend is degraded. The trade-off is double-checked
// registration: two callers racing the same unregistered id may both
// invoke factory, in which case the first result to reach the map
// wins and every surplus store is Close()d before the loser returns
// nil. Callers that need strict once-only factory semantics MUST
// serialize externally.
//
// If factory returns an error the router is left untouched and the
// error is propagated wrapped. The next EnsureBackend call for the
// same id will retry the factory because nothing was registered on
// the failed attempt; the API does not memoise failures. Callers that
// need retry-with-backoff on failure MUST wrap the factory themselves.
//
// Returns ErrStoreClosed if the router has already been Close()d.
// When shutdown lands while factory is in flight, the late-built
// store is Close()d before ErrStoreClosed is returned so no backend
// connection leaks past shutdown.
//
// Safe for concurrent use alongside StoreFor.
func (r *Router) EnsureBackend(id string, factory func() (Store, error)) error {
	r.mu.RLock()
	closed := r.closed
	_, exists := r.stores[id]
	r.mu.RUnlock()

	if closed {
		return ErrStoreClosed
	}
	if exists {
		return nil
	}

	s, err := factory()
	if err != nil {
		return fmt.Errorf("ratelimit: register backend %q: %w", id, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		r.closeSurplusStore(id, s, "router closed during factory")
		return ErrStoreClosed
	}

	if _, ok := r.stores[id]; ok {
		r.closeSurplusStore(id, s, "lost registration race")
		return nil
	}

	r.stores[id] = s
	return nil
}

// closeSurplusStore disposes of a store that was built but will never
// be registered (lost a registration race or arrived after shutdown).
// A Close failure cannot change the caller's outcome — the store is
// unreachable either way — so it is logged rather than propagated.
func (r *Router) closeSurplusStore(id string, s Store, reason string) {
	if err := s.Close(); err != nil {
		r.logger.Warn().
			Str("event", "ratelimit.store.surplus_close_failed").
			Str("backend", id).
			Str("reason", reason).
			Err(err).
			Msg("failed to close surplus rate-limit store")
	}
}

// StoreFor returns the Store that should service the given route.
// Resolution order:
//
//  1. Router has been Close()d → the closed-sentinel store, whose
//     Allow returns ErrStoreClosed so the handler's FailPolicy
//     decides allow/reject.
//  2. `route.RateLimit == nil` or `route.RateLimit.Store == ""` → memory.
//  3. Declared id matches a registered backend → that backend.
//  4. Declared id has no matching backend → memory + fallback warn log
//     + fallback counter bump.
//  5. Memory backend itself missing (misconfiguration: bootstrap forgot
//     to register it) → the closed-sentinel store. Allow surfaces
//     ErrStoreClosed so the FailPolicy turns the broken wiring into a
//     well-defined backend-outage decision instead of a nil-Store
//     panic on the hot path.
//
// The hot-path memory backend lookup uses RLock; it is safe to call
// concurrently with EnsureBackend and Close. In-flight requests that
// race Close() cannot panic on a closed-but-still-mapped store
// because Close installs the sentinel before releasing the lock.
func (r *Router) StoreFor(route routing.Route) Store {
	declared := "memory"
	if route.RateLimit != nil && route.RateLimit.Store != "" {
		declared = route.RateLimit.Store
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return closedStore{}
	}

	if s, ok := r.stores[declared]; ok {
		return s
	}

	if declared != "memory" {
		r.counters.fallback.Add(1)
		routeKey := route.Method + ":" + route.PathTemplate
		// LoadOrStore-based dedupe: the first goroutine to observe the
		// (route, declared) tuple wins the empty struct{} slot and emits
		// the WARN; later observers see the existing entry and skip the
		// log. Operators get a single signal per misconfiguration, not a
		// flood proportional to RPS, while distinct tuples each retain
		// their own one-shot record so a misconfiguration on one route
		// does not silence another.
		if _, alreadyLogged := r.fallbackLogged.LoadOrStore(routeKey+"|"+declared, struct{}{}); !alreadyLogged {
			r.logger.Warn().
				Str("event", "ratelimit.store.fallback").
				Str("route", routeKey).
				Str("declared_backend", declared).
				Str("fallback", "memory").
				Msg("declared rate-limit backend is not registered; falling back to memory")
		}
	}

	memory, ok := r.stores["memory"]
	if !ok {
		// Defensive bottom of the resolution chain: the gateway
		// bootstrap is contractually required to register the memory
		// backend before any Allow call reaches the router. If that
		// invariant is broken we MUST NOT return nil — the proxy
		// handler would dereference it on the hot path. The closed
		// sentinel makes Allow return ErrStoreClosed so the
		// configured FailPolicy decides allow/reject, the log line
		// gives operators a loud signal that startup wiring is broken,
		// and the fallback counter bumps so the misconfiguration is
		// observable through metrics.
		r.logger.Error().
			Str("event", "ratelimit.store.memory_missing").
			Str("route", route.Method+":"+route.PathTemplate).
			Msg("ratelimit memory backend missing; using closed sentinel")
		r.counters.fallback.Add(1)

		return closedStore{}
	}

	return memory
}

// FailPolicy returns the resolved Policy. Request handlers invoke
// Apply on it when Store.Allow returns a non-nil error so the
// open/closed/etc. decision is made consistently across all
// backends served by this router.
func (r *Router) FailPolicy() Policy { return r.failPolicy }

// RecordClaimsUnmarshalError bumps the router's
// ratelimit_claims_unmarshal_errors counter. The proxy handler calls
// this when a verifier's claim payload fails to JSON-unmarshal; the
// counter surfaces a multi-tenant NAT-collision risk (tenants sharing
// one IP-fallback bucket because their claims were unparseable) through
// metrics so operators do not need to grep logs to find the drift.
//
// Safe for concurrent use.
func (r *Router) RecordClaimsUnmarshalError() {
	r.counters.claimsUnmarshal.Add(1)
}

// Close closes every registered Store. The first error encountered
// is returned; the remaining stores are still closed on a
// best-effort basis so a failing backend does not leak the others.
//
// After Close returns, the router flips into a terminal closed state:
// every subsequent StoreFor call returns a sentinel whose Allow
// surfaces ErrStoreClosed, which the handler's FailPolicy turns into
// a well-defined outage decision. EnsureBackend likewise refuses
// further registrations. Close is idempotent; a second call is a
// no-op and returns nil.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	var firstErr error
	for id, s := range r.stores {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", id, err)
		}
	}

	// Drop references to the underlying stores and flip the closed
	// flag in a single critical section. The next StoreFor observes a
	// consistent closed state rather than a half-populated map.
	r.stores = map[string]Store{}
	r.closed = true

	return firstErr
}

// Counters returns a snapshot of router-level observability
// counters. The keys are namespaced for OpenTelemetry plumbing.
//
// This snapshot covers ONLY the router's own dispatch-layer metrics
// (e.g., backend fallback) plus the package-level
// ratelimit_claim_nondeterministic counter — non-zero readings of
// the latter indicate a JWT verifier producing claim shapes that
// cannot be deterministically stringified, a misconfiguration that
// would otherwise hide behind a lossy fallback bucket key. For
// per-backend rate-limit counters use CountersAll, which walks every
// registered backend and the router in a single map.
func (r *Router) Counters() map[string]int64 {
	return map[string]int64{
		"ratelimit_store_fallback_total":          r.counters.fallback.Load(),
		"ratelimit_claim_nondeterministic_total":  int64(ClaimNondeterministicCount()),
		"ratelimit_claims_unmarshal_errors_total": r.counters.claimsUnmarshal.Load(),
		"ratelimit_cookie_collision_total":        int64(CookieCollisionCount()),
	}
}

// CountersAll returns a unified snapshot of every backend's metric
// surface plus the router's dispatch-layer counters. The outer key
// is the backend id (the same string used by EnsureBackend, plus
// "router" for the dispatch layer); the inner map carries the keys
// that backend exposes via Store.Counters.
//
// The shape stays stable across backend swaps: a deployment that
// migrates memory → natskv keeps the same outer-map structure (the
// memory entry simply disappears) so dashboards do not need a rewrite.
//
// Safe for concurrent use; each call allocates a fresh map.
func (r *Router) CountersAll() map[string]map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]map[string]int64, len(r.stores)+1)
	for id, s := range r.stores {
		out[id] = s.Counters()
	}
	out["router"] = map[string]int64{
		"ratelimit_store_fallback_total":          r.counters.fallback.Load(),
		"ratelimit_claim_nondeterministic_total":  int64(ClaimNondeterministicCount()),
		"ratelimit_claims_unmarshal_errors_total": r.counters.claimsUnmarshal.Load(),
		"ratelimit_cookie_collision_total":        int64(CookieCollisionCount()),
	}

	return out
}
