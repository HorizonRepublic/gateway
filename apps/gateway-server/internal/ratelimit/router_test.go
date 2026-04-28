package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

type stubStore struct {
	name     string
	counters map[string]int64
}

func (s *stubStore) Allow(context.Context, string, int, int) (Decision, error) {
	return Decision{Allowed: true}, nil
}

func (*stubStore) FlushPrefix(context.Context, string) error {
	return nil
}

func (*stubStore) Close() error {
	return nil
}

func (s *stubStore) Counters() map[string]int64 {
	if s.counters == nil {
		return map[string]int64{
			"ratelimit_stub_decisions_allowed":  0,
			"ratelimit_stub_decisions_rejected": 0,
			"ratelimit_stub_backend_errors":     0,
		}
	}
	return s.counters
}

func TestRouter_DispatchByStoreField(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()
	kv := &stubStore{name: "nats-kv"}

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))
	require.NoError(t, r.EnsureBackend("nats-kv", func() (Store, error) { return kv, nil }))

	assert.Same(t, mem, r.StoreFor(routing.Route{RateLimit: &registry.RateLimitMeta{Store: "memory"}}))
	assert.Same(t, kv, r.StoreFor(routing.Route{RateLimit: &registry.RateLimitMeta{Store: "nats-kv"}}))
}

func TestRouter_EmptyStoreDefaultsToMemory(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	assert.Same(t, mem, r.StoreFor(routing.Route{RateLimit: &registry.RateLimitMeta{Store: ""}}))
}

func TestRouter_NilRateLimitReturnsMemory(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	assert.Same(t, mem, r.StoreFor(routing.Route{}))
}

func TestRouter_UnknownStoreFallsBackToMemory(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	got := r.StoreFor(routing.Route{RateLimit: &registry.RateLimitMeta{Store: "redis"}})
	assert.Same(t, mem, got)
	assert.Equal(t, int64(1), r.Counters()["ratelimit_store_fallback_total"],
		"each fallback to memory bumps the observability counter")
}

func TestRouter_EnsureBackendIdempotent(t *testing.T) {
	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	var calls atomic.Int32
	factory := func() (Store, error) {
		calls.Add(1)
		mem := NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = mem.Close() })
		return mem, nil
	}
	require.NoError(t, r.EnsureBackend("memory", factory))
	require.NoError(t, r.EnsureBackend("memory", factory))
	assert.Equal(t, int32(1), calls.Load(), "factory must run once")
}

func TestRouter_EnsureBackendFactoryError(t *testing.T) {
	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	err := r.EnsureBackend("nats-kv", func() (Store, error) { return nil, errors.New("boom") })
	assert.Error(t, err)
}

// TestRouter_StoreForAfterCloseReturnsClosedSentinel guards the race
// where a request is dispatched through the router while shutdown is
// in flight. Before the fix, Close() closed every store but left the
// map populated, so StoreFor returned a closed store whose Allow
// could panic or misbehave. The router now flips into a terminal
// closed state and StoreFor returns a sentinel whose Allow surfaces
// ErrStoreClosed, which the handler's FailPolicy maps to a
// well-defined decision.
func TestRouter_StoreForAfterCloseReturnsClosedSentinel(t *testing.T) {
	mem := NewMemoryStore(time.Minute)

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	require.NoError(t, r.Close())

	store := r.StoreFor(routing.Route{})
	require.NotNil(t, store, "StoreFor must never return nil; the handler dereferences it on the hot path")

	// Allow on the sentinel must surface ErrStoreClosed — never a
	// panic — so the FailPolicy path picks up the decision.
	_, err := store.Allow(context.Background(), "k", 10, 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStoreClosed)

	// FlushPrefix and Close on the sentinel are idempotent no-ops.
	assert.NoError(t, store.FlushPrefix(context.Background(), "k"))
	assert.NoError(t, store.Close())
}

func TestRouter_CloseIsIdempotent(t *testing.T) {
	mem := NewMemoryStore(time.Minute)

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	require.NoError(t, r.Close())
	require.NoError(t, r.Close(),
		"second Close must be a no-op rather than double-closing the underlying stores")
}

// TestRouter_StoreForReturnsClosedSentinelWhenMemoryMissing pins
// the defensive behaviour at the bottom of the resolution chain: if
// bootstrap forgets to register the memory backend, StoreFor MUST
// return a non-nil Store whose Allow surfaces ErrStoreClosed. The
// hot-path handler dereferences the result without nil checks, so a
// nil return here would be a panic.
func TestRouter_StoreForReturnsClosedSentinelWhenMemoryMissing(t *testing.T) {
	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())

	store := r.StoreFor(routing.Route{Method: "GET", PathTemplate: "/x"})
	require.NotNil(t, store, "StoreFor must never return nil; the handler dereferences it on the hot path")

	_, err := store.Allow(context.Background(), "k", 10, 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStoreClosed)

	// The fallback counter must have bumped so operators can surface
	// the misconfiguration through metrics — otherwise a missing
	// backend hides forever behind the nil-safe FailPolicy decision.
	assert.GreaterOrEqual(t, r.Counters()["ratelimit_store_fallback_total"], int64(1),
		"missing memory backend must bump the fallback counter")
}

// TestRouter_StoreForUnknownStoreWithoutMemoryReturnsClosedSentinel
// covers the combined path: declared backend missing, memory backend
// also missing. Both fallbacks log and bump the counter, but the
// final return must still be a non-nil Store.
func TestRouter_StoreForUnknownStoreWithoutMemoryReturnsClosedSentinel(t *testing.T) {
	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())

	store := r.StoreFor(routing.Route{
		Method:       "POST",
		PathTemplate: "/y",
		RateLimit:    &registry.RateLimitMeta{Store: "redis"},
	})
	require.NotNil(t, store, "StoreFor must never return nil even when both declared and fallback backends are missing")

	_, err := store.Allow(context.Background(), "k", 10, 5)
	require.ErrorIs(t, err, ErrStoreClosed)
}

// TestRouter_UnknownStoreFallbackLogDeduped guards the per-route log
// dedupe: a route declaring an unwired backend (e.g. "redis") must
// emit the WARN exactly once, no matter how many requests flow through
// it. The fallback counter still ticks per-request so operators can
// gauge the volume — only the log line is throttled. Distinct
// (route, declared) tuples each get their own one-shot WARN so a
// misconfiguration on one route does not silence a subsequent one
// on another.
func TestRouter_UnknownStoreFallbackLogDeduped(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	r := NewRouter(FailPolicyOpen.Resolve(), logger)
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	route := routing.Route{
		Method:       "GET",
		PathTemplate: "/x",
		RateLimit:    &registry.RateLimitMeta{Store: "redis"},
	}

	const calls = 10
	for i := 0; i < calls; i++ {
		_ = r.StoreFor(route)
	}

	count := countWarnRecords(t, buf.Bytes(), "ratelimit.store.fallback")
	assert.Equal(t, 1, count, "fallback WARN must be emitted once per (route, declared) tuple")
	assert.Equal(t, int64(calls), r.Counters()["ratelimit_store_fallback_total"],
		"counter ticks per request even when log is deduped")

	// A different declared backend on a different route must produce a
	// fresh WARN — dedupe is keyed by (route, declared), not global.
	otherRoute := routing.Route{
		Method:       "POST",
		PathTemplate: "/y",
		RateLimit:    &registry.RateLimitMeta{Store: "dynamodb"},
	}
	_ = r.StoreFor(otherRoute)

	totalWarn := countWarnRecords(t, buf.Bytes(), "ratelimit.store.fallback")
	assert.Equal(t, 2, totalWarn, "distinct (route, declared) tuples each get their own WARN")
}

// TestRouter_FallbackLogIncludesDeclaredBackend pins the structured
// fields operators rely on for log filtering — both the route name
// and the declared backend id must appear in every fallback record.
func TestRouter_FallbackLogIncludesDeclaredBackend(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	r := NewRouter(FailPolicyOpen.Resolve(), logger)
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))

	_ = r.StoreFor(routing.Route{
		Method:       "PUT",
		PathTemplate: "/items/:id",
		RateLimit:    &registry.RateLimitMeta{Store: "redis"},
	})

	var sawFallback bool
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))

		if record["event"] == "ratelimit.store.fallback" {
			sawFallback = true
			assert.Equal(t, "PUT:/items/:id", record["route"])
			assert.Equal(t, "redis", record["declared_backend"])
		}
	}
	assert.True(t, sawFallback, "fallback record must be emitted on first hit")
}

// countWarnRecords scans newline-delimited zerolog JSON output and
// returns the number of records whose event field equals event.
func countWarnRecords(t *testing.T, payload []byte, event string) int {
	t.Helper()
	n := 0
	for _, line := range bytes.Split(bytes.TrimSpace(payload), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		if record["event"] == event {
			n++
		}
	}
	return n
}

// TestRouter_CountersAllAggregatesAcrossBackends pins the unified
// counter export contract: dashboards walking the rate-limit module
// see a stable shape of {backend_id: {metric: value}} regardless of
// which backends are wired. Backend swap (memory → natskv) does not
// require a dashboard rewrite — the keys remain the same shape.
func TestRouter_CountersAllAggregatesAcrossBackends(t *testing.T) {
	mem := NewMemoryStore(time.Minute)
	defer func() { _ = mem.Close() }()
	stub := &stubStore{name: "nats-kv"}

	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.EnsureBackend("memory", func() (Store, error) { return mem, nil }))
	require.NoError(t, r.EnsureBackend("nats-kv", func() (Store, error) { return stub, nil }))

	all := r.CountersAll()

	require.Contains(t, all, "memory")
	require.Contains(t, all, "nats-kv")
	require.Contains(t, all, "router")

	// Memory must surface the minimum schema even when no requests
	// have flowed through it yet.
	memCounters := all["memory"]
	require.NotNil(t, memCounters)
	assert.Contains(t, memCounters, "ratelimit_memory_decisions_allowed_total")
	assert.Contains(t, memCounters, "ratelimit_memory_decisions_rejected_total")
	assert.Contains(t, memCounters, "ratelimit_memory_backend_errors_total",
		"every backend must expose backend_errors for dashboard parity")

	routerCounters := all["router"]
	assert.Contains(t, routerCounters, "ratelimit_store_fallback_total")
	assert.Contains(t, routerCounters, "ratelimit_cookie_collision_total",
		"router-level cookie-collision counter must surface for dashboards")
}

// TestMemoryStoreCountersIncludeMinimumSchema enforces the cross-
// backend metric parity. backend_errors is always 0 on memory (no
// remote dependencies can fail) but the key MUST be present so a
// dashboard graphing it across backends does not go dark on the
// memory pod.
func TestMemoryStoreCountersIncludeMinimumSchema(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	defer func() { _ = s.Close() }()

	c := s.Counters()
	assert.Contains(t, c, "ratelimit_memory_decisions_allowed_total")
	assert.Contains(t, c, "ratelimit_memory_decisions_rejected_total")
	assert.Contains(t, c, "ratelimit_memory_backend_errors_total")
	assert.Equal(t, int64(0), c["ratelimit_memory_backend_errors_total"],
		"memory has no remote dependencies so backend_errors stays 0")
}

// TestRouter_ClosedSentinelReturnsDefensibleDecision pins the
// invariant that every Decision returned by closedStore.Allow carries
// non-zero, header-safe fields. A consumer that ignores the
// ErrStoreClosed error and feeds the Decision into BuildHeaders must
// not encode time.Time{}.Unix() (negative epoch, year 1) as
// X-RateLimit-Reset; the Allowed flag must default to true so a
// fail-open caller sees an unambiguous pass-through.
func TestRouter_ClosedSentinelReturnsDefensibleDecision(t *testing.T) {
	d, err := closedStore{}.Allow(context.Background(), "k", 100, 5)

	require.ErrorIs(t, err, ErrStoreClosed)
	assert.True(t, d.Allowed,
		"closed sentinel must default Allowed=true so fail-open callers ignoring the error see a pass-through")
	assert.False(t, d.ResetAt.IsZero(),
		"closed sentinel must populate ResetAt so BuildHeaders does not encode a year-1 reset timestamp")
	assert.GreaterOrEqual(t, d.ResetAt.Unix(), int64(0),
		"closed sentinel ResetAt must be a real Unix-epoch timestamp")
}

// TestClosedStore_CountersExposesMinimumSchemaWithZeros pins the
// dashboard parity contract for the post-Close sentinel: even though
// the closed sentinel never serves real traffic, its Counters call
// MUST emit the same minimum-schema keys every other backend exports
// so a metric pipeline aggregating `Counters` across the router's
// lifetime does not see keys disappear after shutdown. All values are
// zero because the sentinel has nothing to count.
func TestClosedStore_CountersExposesMinimumSchemaWithZeros(t *testing.T) {
	c := closedStore{}.Counters()

	require.Len(t, c, 3, "closed sentinel exposes exactly the minimum-schema keys")
	for _, key := range []string{
		"ratelimit_closed_decisions_allowed_total",
		"ratelimit_closed_decisions_rejected_total",
		"ratelimit_closed_backend_errors_total",
	} {
		require.Contains(t, c, key, "minimum schema must include %q", key)
		assert.Equal(t, int64(0), c[key],
			"closed sentinel never serves traffic so %q stays zero", key)
	}
}

// TestRouter_FailPolicyAccessorRoundTrips pins the read-back contract
// for the resolved Policy. A handler that needs to apply the configured
// fail-policy after a Store.Allow error MUST be able to read the same
// Policy NewRouter was constructed with — a closed-policy router MUST
// NOT silently revert to the open-policy default through the accessor.
func TestRouter_FailPolicyAccessorRoundTrips(t *testing.T) {
	policy := FailPolicyClosed.Resolve()

	r := NewRouter(policy, zerolog.Nop())
	defer func() { _ = r.Close() }()

	got := r.FailPolicy()
	assert.IsType(t, closedPolicy{}, got,
		"FailPolicy must round-trip the closed-policy resolution; receiving openPolicy would silently flip availability mode")
	assert.Equal(t, policy, got,
		"FailPolicy must return the same Policy value the router was constructed with")
}

// TestRouter_RecordClaimsUnmarshalErrorBumpsCounter exercises the
// observability hook the proxy handler calls when a verifier claim
// payload fails JSON unmarshal. Two calls must produce a delta of
// exactly two on the dedicated counter so operators graphing the
// metric see one tick per drop.
func TestRouter_RecordClaimsUnmarshalErrorBumpsCounter(t *testing.T) {
	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	defer func() { _ = r.Close() }()

	before := r.Counters()["ratelimit_claims_unmarshal_errors_total"]

	r.RecordClaimsUnmarshalError()
	r.RecordClaimsUnmarshalError()

	after := r.Counters()["ratelimit_claims_unmarshal_errors_total"]
	assert.Equal(t, before+2, after,
		"two RecordClaimsUnmarshalError calls must bump the counter by exactly 2")
}

func TestRouter_EnsureBackendAfterCloseRefuses(t *testing.T) {
	r := NewRouter(FailPolicyOpen.Resolve(), zerolog.Nop())
	require.NoError(t, r.Close())

	// Factory must not even be invoked after close — that would leak
	// resources the router cannot track through its shutdown path.
	var called atomic.Bool
	err := r.EnsureBackend("memory", func() (Store, error) {
		called.Store(true)
		mem := NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = mem.Close() })

		return mem, nil
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStoreClosed)
	assert.False(t, called.Load(),
		"factory must not run after Close; the sentinel prevents registration")
}
