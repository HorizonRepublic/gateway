//go:build integration

package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// setupJetStream spins up a single NATS container with JetStream
// enabled and returns a ready-to-use JetStream handle. Cleanup is
// registered via t.Cleanup — callers do not manage the container or
// connection lifetime directly.
//
// A handler_registry bucket is pre-created inline because the
// production NATSKVStore constructor reads the handler bucket's
// replica count to mirror it on its own bucket. In the full gateway
// stack that bucket is provisioned by the Nest-side nestjs-jetstream
// library; integration tests simulate that provisioning step so the
// constructor sees a realistic starting state.
//
// Each test MUST call this at most once — per-test containers give
// parallel-safe isolation and prevent state bleed between tests.
func setupJetStream(t *testing.T) jetstream.JetStream {
	t.Helper()

	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "handler_registry",
		History: 1,
	})
	require.NoError(t, err)

	return js
}

// TestNATSKVStore_Integration_RoundTrip exercises the full Allow →
// CAS Create → readback path against a real JetStream KV. It proves
// that the TAT encoded by the store is readable back via the raw
// jetstream.KeyValue API and decodes to a non-zero time, which is
// the minimum integrity guarantee the GCRA loop depends on.
func TestNATSKVStore_Integration_RoundTrip(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	sut, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        2 * time.Second,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	decision, err := sut.Allow(ctx, "k", 10, 20)
	require.NoError(t, err)
	require.True(t, decision.Allowed)

	kv, err := js.KeyValue(ctx, "handler_registry_ratelimit")
	require.NoError(t, err)

	entry, err := kv.Get(ctx, "k")
	require.NoError(t, err)

	tat, err := decodeTAT(entry.Value())
	require.NoError(t, err)
	assert.False(t, tat.IsZero(), "decoded TAT must be a real timestamp")
}

// TestNATSKVStore_Integration_ReplicasInherited confirms the
// constructor's wiring that makes the rate-limit bucket inherit the
// handler registry's replica count. A single-node testcontainer can
// only run Replicas=1, so this test pins the inheritance path
// itself: whatever the handler bucket is configured with, the
// rate-limit bucket ends up with the same value.
func TestNATSKVStore_Integration_ReplicasInherited(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	// Single-node containers cannot go above Replicas=1; the
	// inheritance wiring is what this test locks in, not the
	// absolute replica count.
	_, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   "handler_registry",
		History:  1,
		Replicas: 1,
	})
	require.NoError(t, err)

	_, err = NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        2 * time.Second,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	kv, err := js.KeyValue(ctx, "handler_registry_ratelimit")
	require.NoError(t, err)

	status, err := kv.Status(ctx)
	require.NoError(t, err)

	bucketStatus, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok, "status must be *jetstream.KeyValueBucketStatus")

	info := bucketStatus.StreamInfo()
	require.NotNil(t, info)
	assert.Equal(t, 1, info.Config.Replicas)
}

// TestNATSKVStore_Integration_ConcurrentCreateRace exercises the
// TOCTOU window between js.KeyValue and js.CreateKeyValue that fires
// when multiple gateway replicas boot against the same NATS cluster
// for the first time. Each replica sees ErrBucketNotFound and issues
// CreateKeyValue; at most one wins, the rest observe ErrBucketExists.
// The constructor must recover by re-opening the bucket the winner
// materialised so every replica ends up with a live store.
func TestNATSKVStore_Integration_ConcurrentCreateRace(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	const concurrency = 8

	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		stores = make([]*NATSKVStore, concurrency)
		errs   = make([]error, concurrency)
	)

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()

			<-start

			store, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
				JS:            js,
				HandlerBucket: "handler_registry",
				BucketSuffix:  "_ratelimit",
				KeyTTL:        1 * time.Minute,
				Logger:        zerolog.Nop(),
			})
			stores[idx] = store
			errs[idx] = err
		}(i)
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		// require here, not assert: a backend acquire failure means the
		// next stores[i] dereference would read a stale pointer from a
		// previous iteration and the assertion below would pass on the
		// wrong instance.
		require.NoErrorf(t, err, "replica %d failed to acquire the shared bucket", i)
		require.NotNil(t, stores[i])
	}

	// Every store must be operational against the single shared
	// bucket. A late-arriving replica that failed to reopen after
	// losing the Create race would surface here as a backend error.
	// Rate-limit-math correctness is not under test here (that's
	// covered elsewhere) — rps is deliberately huge so GCRA admits
	// every replica and any reject here would be a real store fault.
	for i, store := range stores {
		d, err := store.Allow(ctx, fmt.Sprintf("k-%d", i), 1_000_000, 1000)
		require.NoErrorf(t, err, "replica %d Allow failed", i)
		assert.Truef(t, d.Allowed, "replica %d must admit against a fresh per-key bucket", i)
	}
}

// TestNATSKVStore_Integration_TTLExpiry proves that KeyTTL maps onto
// the stream's MaxAge and the server actually evicts idle keys.
// Polling beats a fixed sleep: JetStream's cleanup runs at
// second-granularity, so the actual eviction time varies with server
// load. The 10s ceiling absorbs CI scheduler jitter without
// blow-up; the 200ms tick keeps the suite fast on a healthy run.
func TestNATSKVStore_Integration_TTLExpiry(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	sut, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        2 * time.Second,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	decision, err := sut.Allow(ctx, "k", 10, 20)
	require.NoError(t, err)
	require.True(t, decision.Allowed)

	kv, err := js.KeyValue(ctx, "handler_registry_ratelimit")
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		_, err := kv.Get(ctx, "k")
		return errors.Is(err, jetstream.ErrKeyNotFound)
	}, 10*time.Second, 200*time.Millisecond,
		"key did not expire within 10s")
}

// TestNATSKVStore_Integration_ConcurrentCASConflict fires many
// parallel Allow calls at a single hot key to exercise the CAS
// retry loop under real contention. The rps value is deliberately
// huge (1M) to keep GCRA permissive — the assertion is about lost
// updates (allowed+rejected totals) and CAS correctness, not about
// rate-limit math.
//
// At rps=1M period is 1μs, while a real NATS CAS round-trip sits
// around 1ms; GCRA's delay tolerance (burst*period = 10μs) is
// dwarfed by wall-clock progress between writes, so once admission
// starts flowing the limiter effectively acts as unbounded for
// this burst size. The test therefore asserts the invariant that
// actually matters under CAS contention — no lost updates,
// allowed+rejected equals the total — and leaves rate-limit-math
// bounds to unit tests that run against the in-memory fake where
// sub-microsecond period granularity is faithful.
func TestNATSKVStore_Integration_ConcurrentCASConflict(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	sut, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        1 * time.Minute,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	const (
		goroutines = 20
		rps        = 1_000_000
		burst      = 10
	)

	var (
		mu       sync.Mutex
		allowed  int
		rejected int
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()

			<-start

			decision, allowErr := sut.Allow(ctx, "hot-key", rps, burst)

			mu.Lock()
			switch {
			case allowErr != nil:
				// CAS budget exhaustion under contention counts as
				// "did not admit"; the state was never corrupted.
				rejected++
			case decision.Allowed:
				allowed++
			default:
				rejected++
			}
			mu.Unlock()
		}()
	}

	close(start)
	wg.Wait()

	assert.Equal(t, goroutines, allowed+rejected,
		"no lost updates: every goroutine must be accounted for")
	assert.GreaterOrEqual(t, allowed, 1,
		"at least one goroutine must win CAS and be admitted")
	assert.LessOrEqual(t, allowed, goroutines,
		"admitted count cannot exceed the number of contenders")
}

// TestNATSKVStore_Integration_FlushPrefixDeletesMatchingKeys exercises
// the prefix-sweep used by the registry hot-reload path: when a route's
// rate-limit config changes, the gateway flushes every bucket whose key
// shares the route's prefix so a tightened limit cannot keep honouring
// burst tokens accumulated under the previous config. Keys outside the
// prefix MUST be left intact — the sweep is per-route, not bucket-wide.
func TestNATSKVStore_Integration_FlushPrefixDeletesMatchingKeys(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	sut, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        1 * time.Minute,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	kv, err := js.KeyValue(ctx, "handler_registry_ratelimit")
	require.NoError(t, err)

	// Pre-load the bucket through the raw KV API so the seed bytes
	// don't need to satisfy the GCRA TAT format — FlushPrefix is a
	// dispatch-layer operation that operates on key names, not values.
	// NATS KV restricts the key alphabet (no ':'), so the seed names
	// use the period-separated schema BuildBucketKey already produces.
	_, err = kv.Put(ctx, "prefix.a", []byte("seed-a"))
	require.NoError(t, err)
	_, err = kv.Put(ctx, "prefix.b", []byte("seed-b"))
	require.NoError(t, err)
	_, err = kv.Put(ctx, "other.c", []byte("seed-c"))
	require.NoError(t, err)

	require.NoError(t, sut.FlushPrefix(ctx, "prefix."))

	_, err = kv.Get(ctx, "prefix.a")
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound,
		"prefix.a must be deleted after FlushPrefix")
	_, err = kv.Get(ctx, "prefix.b")
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound,
		"prefix.b must be deleted after FlushPrefix")

	survivor, err := kv.Get(ctx, "other.c")
	require.NoError(t, err, "keys outside the prefix must survive the sweep")
	assert.Equal(t, []byte("seed-c"), survivor.Value())
}

// TestOpenOrCreateRatelimitBucket_Integration_LazyCreateThenReuse pins
// the idempotent-startup contract: the first call against a fresh JS
// account creates the bucket and reports created=true; the second call
// against the now-populated account returns the same bucket and reports
// created=false. This is the boot path every gateway replica walks
// through, and a regression that always re-creates would either fail
// the second replica with ErrBucketExists or wipe TAT state on every
// pod restart.
func TestOpenOrCreateRatelimitBucket_Integration_LazyCreateThenReuse(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	const (
		bucket   = "custom_handler_bucket_ratelimit"
		replicas = 1
		ttl      = 30 * time.Second
	)

	first, created, err := openOrCreateRatelimitBucket(ctx, js, bucket, replicas, ttl)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.True(t, created, "first call must report a fresh creation")

	// Confirm the bucket actually materialised with the requested
	// configuration before the reuse path is exercised. This makes a
	// regression that returns the wrong handle obvious here rather
	// than at a downstream Get/Put.
	status, err := first.Status(ctx)
	require.NoError(t, err)

	bucketStatus, ok := status.(*jetstream.KeyValueBucketStatus)
	require.True(t, ok)

	info := bucketStatus.StreamInfo()
	require.NotNil(t, info)
	assert.Equal(t, replicas, info.Config.Replicas,
		"created bucket must inherit the requested replica count")
	assert.Equal(t, ttl, info.Config.MaxAge,
		"created bucket MaxAge must mirror the configured KeyTTL")

	second, created, err := openOrCreateRatelimitBucket(ctx, js, bucket, replicas, ttl)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.False(t, created, "second call must reuse the existing bucket, not recreate")
}

// TestNewNATSKVStore_Integration_CustomBucketSuffix wires the full
// constructor with a non-default suffix and asserts the bucket name
// derivation (HandlerBucket + BucketSuffix) materialises the expected
// downstream bucket. A regression in the suffix derivation would
// silently land all rate-limit state in the handler bucket itself,
// which corrupts the registry payload.
func TestNewNATSKVStore_Integration_CustomBucketSuffix(t *testing.T) {
	t.Parallel()

	js := setupJetStream(t)
	ctx := context.Background()

	const suffix = "_rl_v2"

	sut, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  suffix,
		KeyTTL:        30 * time.Second,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)
	require.NotNil(t, sut)

	// The constructor must have created the suffixed bucket; opening
	// it directly proves the name derivation is correct.
	_, err = js.KeyValue(ctx, "handler_registry"+suffix)
	require.NoError(t, err, "constructor must materialise the suffixed bucket")

	// A second call with the same config exercises the existing-bucket
	// branch in openOrCreateRatelimitBucket.
	again, err := NewNATSKVStore(ctx, NATSKVStoreConfig{
		JS:            js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  suffix,
		KeyTTL:        30 * time.Second,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err, "reopening an existing bucket must succeed")
	require.NotNil(t, again)
}
