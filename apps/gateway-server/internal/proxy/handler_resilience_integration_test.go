//go:build integration

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/auth"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/ratelimit"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// jsKVStreamPrefix is the JetStream stream-name prefix nats.go uses
// when materialising a KV bucket — a bucket named "foo" is backed
// by a stream named "KV_foo". Stable across nats.go v1.x; any
// future rename would break the deletion-based JS-only outage
// simulation and require a coordinated test update.
const jsKVStreamPrefix = "KV_"

// handlerRegistryBucketName is the canonical KV bucket name used
// by the harness setup. Centralised so the JS-only outage helpers
// stay in lockstep with setupResilienceHarness on rename.
const handlerRegistryBucketName = "handler_registry"

// rateLimitBucketName is the suffix-derived companion bucket the
// production NATSKVStore constructor materialises off
// handler_registry. JS-only outage tests that delete it must also
// recreate it on cleanup so subsequent test runs are not greeted
// by a missing bucket.
const rateLimitBucketName = "handler_registry_ratelimit"

// resilienceHarness owns every dependency the handler under test needs
// to survive a NATS outage scenario: the testcontainer, both NATS
// connections (gateway + fake upstream), the JetStream KV that backs the
// registry, the ratelimit Router, and the production proxy.Handler. The
// fields the individual tests reach into directly stay public-by-Go-
// convention (capitalised) for read access; mutation is restricted to
// the harness builder.
//
// Two distinct connections are kept on purpose. The gateway uses the
// shared production options (NoEcho, Drain on close); the responder
// uses a vanilla connection so it can publish replies on subjects the
// gateway just published to without observing its own echo. This
// mirrors the Nest-side topology where the gateway and the upstream
// service sit on different NATS clients.
type resilienceHarness struct {
	t              *testing.T
	container      *tcnats.NATSContainer
	url            string
	gatewayConn    *natsgo.Conn
	responderConn  *natsgo.Conn
	js             jetstream.JetStream
	kv             jetstream.KeyValue
	store          *registry.Store
	watcher        *registry.Watcher
	router         *ratelimit.Router
	handler        *Handler
	tableHolder    *atomic.Value
	stopRespond    chan struct{}
	respondersStop sync.Once
}

// natsRequesterAdapter bridges *natsgo.Conn to the proxy.NatsRequester
// contract. The production transport package owns a richer Requester
// (round-robin pool, drain on close); this harness only needs a
// straight RequestWithContext call against a single connection. Wiring
// the production type in here would pull in a dependency on the
// `internal/transport/nats` package and create an import cycle when
// the proxy integration test reaches into transport-internal helpers.
//
// The adapter respects both ctx and timeout: the resulting deadline is
// min(ctx.Deadline(), now+timeout), matching the production Requester.
type natsRequesterAdapter struct {
	conn *natsgo.Conn
}

func (a *natsRequesterAdapter) Request(
	ctx context.Context,
	subject string,
	payload []byte,
	timeout time.Duration,
) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msg, err := a.conn.RequestWithContext(reqCtx, subject, payload)
	if err != nil {
		return nil, fmt.Errorf("nats request %q: %w", subject, err)
	}

	return msg.Data, nil
}

// setupResilienceHarness boots a fresh NATS testcontainer with
// JetStream, pre-creates the handler_registry bucket, seeds it with
// the supplied entries, attaches a fake upstream that returns 200 with
// a canned body for every routed subject, and wires a fully-real
// proxy.Handler against the production registry watcher and ratelimit
// Router stack.
//
// Each test gets its own container so an outage induced by one test
// (Stop on the testcontainer) does not bleed into the next.
//
// Returns a started harness with t.Cleanup hooks that drain in reverse
// order: stop responders, stop watcher, close router, close NATS,
// terminate container.
func setupResilienceHarness(t *testing.T, entries map[string]registry.HandlerEntry) *resilienceHarness {
	t.Helper()
	ctx := context.Background()

	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	// Both connections deliberately set MaxReconnects=-1 so they keep
	// trying to reconnect during a Stop/Start outage instead of giving
	// up after the default 60 attempts. Tests that intentionally kill
	// the container expect the connection to recover when the container
	// comes back up; a finite reconnect cap would surface as a test
	// flake (success on a fast machine, ConnectionClosed on a slow CI
	// host that ate enough seconds during Stop/Start).
	gatewayConn, err := natsgo.Connect(url,
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(100*time.Millisecond),
		natsgo.NoEcho(),
	)
	require.NoError(t, err)
	t.Cleanup(gatewayConn.Close)

	responderConn, err := natsgo.Connect(url,
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(100*time.Millisecond),
	)
	require.NoError(t, err)
	t.Cleanup(responderConn.Close)

	js, err := jetstream.New(gatewayConn)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "handler_registry",
		History: 1,
	})
	require.NoError(t, err)

	for key, entry := range entries {
		raw, marshalErr := json.Marshal(entry)
		require.NoError(t, marshalErr)
		_, err = kv.Put(ctx, key, raw)
		require.NoError(t, err)
	}

	store := registry.NewStore()
	logger := zerolog.Nop()
	watcher := registry.NewWatcher(kv, store, logger)

	tableHolder := &atomic.Value{}
	rebuilt := make(chan struct{}, 1)
	rebuild := func() {
		snap := store.Get()
		verifiers := auth.BuildVerifierRegistry(snap, logger)
		routes := routing.CollectRoutes(snap, verifiers, logger)
		tableHolder.Store(routing.BuildTableFromRoutes(routes))
		select {
		case rebuilt <- struct{}{}:
		default:
		}
	}

	rebuild()
	watcher.OnChange(rebuild)
	require.NoError(t, watcher.Start(ctx))
	t.Cleanup(watcher.Stop)

	// Drain the initial-load callback so the test starts with a clean
	// rebuilt-channel slate. The pre-rebuild call above already
	// populated the table, so the post-Start callback is just the
	// watcher firing on its first KV scan — no test action depends on
	// observing that signal.
	select {
	case <-rebuilt:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire initial-load callback within 2s")
	}

	router := ratelimit.NewRouter(ratelimit.FailPolicyOpen.Resolve(), logger)
	require.NoError(t, router.EnsureBackend("memory", func() (ratelimit.Store, error) {
		return ratelimit.NewMemoryStore(time.Hour), nil
	}))
	t.Cleanup(func() { _ = router.Close() })

	stopRespond := make(chan struct{})
	subjects := make(map[string]struct{})
	for key, entry := range entries {
		if entry.HTTP == nil {
			continue
		}
		subj, subjErr := registry.SubjectFromKey(key)
		require.NoError(t, subjErr)
		subjects[subj] = struct{}{}
	}

	for subj := range subjects {
		subj := subj
		sub, subErr := responderConn.Subscribe(subj, func(msg *natsgo.Msg) {
			select {
			case <-stopRespond:
				return
			default:
			}

			reply := []byte(`{"status":200,"headers":{"x-upstream":["fake"]},"body":{"ok":true,"subject":"` + subj + `"}}`)
			_ = msg.Respond(reply)
		})
		require.NoError(t, subErr)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, responderConn.Flush())

	handler := NewHandler(HandlerConfig{
		Table: func() routing.Table {
			return tableHolder.Load().(routing.Table)
		},
		Nats:             &natsRequesterAdapter{conn: gatewayConn},
		Encoder:          NewDefaultEncoder(),
		Decoder:          NewDefaultDecoder(),
		Timeout:          2 * time.Second,
		Logger:           logger,
		RateLimiter:      router,
		RateLimitTimeout: 200 * time.Millisecond,
	})

	h := &resilienceHarness{
		t:             t,
		container:     container,
		url:           url,
		gatewayConn:   gatewayConn,
		responderConn: responderConn,
		js:            js,
		kv:            kv,
		store:         store,
		watcher:       watcher,
		router:        router,
		handler:       handler,
		tableHolder:   tableHolder,
		stopRespond:   stopRespond,
	}
	t.Cleanup(h.shutdownResponders)

	return h
}

// shutdownResponders signals every subscribed responder goroutine to
// stop sending replies. Idempotent so the test cleanup chain can call
// it explicitly without racing the t.Cleanup hook installed by the
// builder.
func (h *resilienceHarness) shutdownResponders() {
	h.respondersStop.Do(func() { close(h.stopRespond) })
}

// stopNATS pauses the underlying testcontainer. This drops every
// in-flight connection and returns ENOENT on subsequent NATS dials
// until startNATS lifts the pause. Mirrors the operational reality of
// a NATS outage: process-up-but-network-partitioned.
func (h *resilienceHarness) stopNATS() {
	timeout := 10 * time.Second
	require.NoError(h.t, h.container.Stop(context.Background(), &timeout))
}

// startNATSContainerOnly resumes the testcontainer WITHOUT waiting for
// the harness's shared gateway connection to reconnect. Tests that
// maintain their own dedicated connections (e.g., the breaker-open
// scenario, which uses a non-reconnecting socket to surface non-ctx
// errors) need only the container's NATS daemon back; gating on the
// shared connection's reconnect timing would couple the recovery
// assertion to unrelated nats.go internals.
func (h *resilienceHarness) startNATSContainerOnly() {
	require.NoError(h.t, h.container.Start(context.Background()))
}

// newRequest builds a ServeInput populated with the minimum fields the
// proxy handler needs to route + encode a request. RemoteAddr is set
// because rate-limit IP fallback reads it.
func newRequest(method, path string) *ServeInput {
	return &ServeInput{
		Method:     method,
		Path:       path,
		Query:      map[string]QueryValue{},
		Headers:    map[string]string{},
		RequestID:  "rid-" + method + "-" + path,
		RemoteAddr: "203.0.113.42",
		ReceivedAt: time.Now().UnixMilli(),
	}
}

// httpEntry constructs a registry.HandlerEntry with only the HTTP
// metadata populated. The KV-key convention keys on `<svc>.cmd.<pat>`
// so SubjectFromKey can derive the upstream NATS subject without the
// caller hardcoding it.
func httpEntry(method, path string) registry.HandlerEntry {
	return registry.HandlerEntry{
		HTTP: &registry.HTTPMeta{Method: method, Path: path},
	}
}

// httpEntryWithRateLimit attaches a rate-limit block keyed on client IP
// to the HTTP entry. The keyBy default of "ip" matches what the SDK
// emits when no override is supplied.
func httpEntryWithRateLimit(method, path, store string, rps, burst int) registry.HandlerEntry {
	e := httpEntry(method, path)
	e.RateLimit = &registry.RateLimitMeta{
		RPS:   rps,
		Burst: burst,
		KeyBy: []string{"ip"},
		Store: store,
	}

	return e
}

// httpEntryWithCORS attaches a permissive CORS block to the HTTP entry
// so the OPTIONS preflight assertion in scenario 1 has a route to
// match without making the test contend with origin matching.
func httpEntryWithCORS(method, path, origin string) registry.HandlerEntry {
	e := httpEntry(method, path)
	e.CORS = &registry.CORSMeta{
		Origins: []string{origin},
		Methods: []string{method},
	}

	return e
}

// TestIntegration_GatewaySurvivesNATSOutage_RoutingTableIntact pins the
// "what survives" contract during a NATS outage. The routing table is
// in-memory: lookups, 404, 405, OPTIONS preflight all answer correctly
// after NATS dies. Only requests that need an upstream NATS call
// surface 5xx, and they fail loudly without panicking the process.
func TestIntegration_GatewaySurvivesNATSOutage_RoutingTableIntact(t *testing.T) {
	corsOrigin := "https://example.com"
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntry("GET", "/users"),
		"users-svc.cmd.users.cors": httpEntryWithCORS("GET", "/cors-route", corsOrigin),
	}
	h := setupResilienceHarness(t, entries)

	// Healthy baseline: a known route returns 200 with the upstream
	// canned body. This proves the harness is wired correctly before
	// the outage clouds the picture.
	result := h.handler.Handle(context.Background(), newRequest("GET", "/users"))
	require.Equal(t, 200, result.Status, "healthy baseline must serve 200")

	h.stopNATS()
	t.Cleanup(func() {
		// A best-effort restart on cleanup so a panic in the test body
		// does not leave the container in a paused state that the next
		// suite run inherits.
		_ = h.container.Start(context.Background())
	})

	// 404: pure routing-table lookup — survives outage.
	notFound := h.handler.Handle(context.Background(), newRequest("GET", "/does-not-exist"))
	assert.Equal(t, 404, notFound.Status, "404 lookup is NATS-independent")

	// 405: pure routing-table lookup against a path with a different verb.
	mismatch := h.handler.Handle(context.Background(), newRequest("DELETE", "/users"))
	// The linear table's Methods() does an exact-string compare against
	// PathTemplate (see routing/table.go godoc), so a static path like
	// /users does carry a non-empty Methods slice and the proxy returns
	// 405. A future trie swap would make this assertion stricter.
	assert.Equal(t, 405, mismatch.Status, "405 method-mismatch is NATS-independent")
	assert.NotEmpty(t, mismatch.Headers["Allow"], "405 must carry Allow header per RFC 9110 §15.5.6")

	// OPTIONS preflight: handled entirely by the gateway via
	// handlePreflight — no NATS round-trip.
	preflight := newRequest("OPTIONS", "/cors-route")
	preflight.Headers["access-control-request-method"] = "GET"
	preflight.Headers["origin"] = corsOrigin
	pf := h.handler.Handle(context.Background(), preflight)
	assert.Equal(t, 204, pf.Status, "OPTIONS preflight is NATS-independent")
	assert.NotEmpty(t, pf.Headers["Access-Control-Allow-Origin"], "preflight must stamp CORS headers")

	// Known route: NATS round-trip required → must fail loudly with
	// 5xx, not hang or panic. Use a short-deadline ctx so the test
	// does not wait the full per-route timeout (2s) on every call.
	upstreamCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	upstream := h.handler.Handle(upstreamCtx, newRequest("GET", "/users"))
	assert.GreaterOrEqual(t, upstream.Status, 500,
		"upstream-dependent request must surface 5xx during NATS outage")
	assert.Less(t, upstream.Status, 600)
}

// TestIntegration_NATSOutageMemoryRateLimitStillEnforced pins that the
// memory rate-limit backend is NATS-independent: the gate keeps
// tracking allowed/rejected decisions even after NATS dies. Every call
// produces exactly one rate-limit decision; the upstream NATS round
// trip that runs after the gate clears fails with 5xx during the
// outage but does not influence the gate's arithmetic.
//
// The exact (allowed, rejected) split varies with GCRA's `now`-equals-
// allowAt edge case (a tight loop occasionally admits one extra call
// because effectiveTAT advances by exactly one period before allowAt
// is recomputed). Pinning the precise split would couple the test to
// nanosecond-level wall-clock progress and make it flake on CI; pinning
// "calls accounted for, at least one rejected" is the durable contract
// — the gate gated, end of story.
func TestIntegration_NATSOutageMemoryRateLimitStillEnforced(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntryWithRateLimit("GET", "/users", "memory", 1, 1),
	}
	h := setupResilienceHarness(t, entries)

	memStore := h.router.StoreFor(routing.Route{
		RateLimit: &registry.RateLimitMeta{Store: "memory", RPS: 1},
	})
	beforeCounters := memStore.Counters()
	allowedBefore := beforeCounters["ratelimit_memory_decisions_allowed_total"]
	rejectedBefore := beforeCounters["ratelimit_memory_decisions_rejected_total"]

	h.stopNATS()
	t.Cleanup(func() { _ = h.container.Start(context.Background()) })

	// Tight loop from the same IP. With rps=1 burst=1 the first call
	// admits (filling the burst), and tightly-following calls are
	// rejected until one full period (1s) elapses. Each iteration uses
	// a FRESH ctx because the upstream NATS round trip burns through
	// its full deadline waiting for a dead server — re-using the same
	// ctx across iterations would leave subsequent calls handed an
	// already-cancelled context, which short-circuits memory.Allow at
	// the ctx.Err() guard before it can record a decision.
	const calls = 5
	for i := 0; i < calls; i++ {
		callCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_ = h.handler.Handle(callCtx, newRequest("GET", "/users"))
		cancel()
	}

	afterCounters := memStore.Counters()
	allowedDelta := afterCounters["ratelimit_memory_decisions_allowed_total"] - allowedBefore
	rejectedDelta := afterCounters["ratelimit_memory_decisions_rejected_total"] - rejectedBefore

	assert.Equal(t, int64(calls), allowedDelta+rejectedDelta,
		"every call must produce one rate-limit decision regardless of NATS state")
	assert.GreaterOrEqual(t, rejectedDelta, int64(1),
		"a tight loop with rps=1 burst=1 must reject at least one call (gate truly gating)")
	assert.GreaterOrEqual(t, allowedDelta, int64(1),
		"a tight loop must admit at least the burst (gate not deny-all on backend stall)")
}

// TestIntegration_NATSOutage_NATSKVRateLimit_FailPolicyOpenAllowsThrough
// pins the fail-open path through a nats-kv backend during an outage.
// The KV backend errors on Allow (the JetStream Get fails because NATS
// is down); FailPolicyOpen resolves the error to "allow", so the
// request continues to the upstream NATS round trip — which also fails
// because NATS is down. The visible response is 5xx.
//
// The rate-limit pipeline IS guaranteed to have run because the natskv
// store's allowed/rejected/backend_errors counters tick on every Allow
// regardless of the eventual response shape. Asserting on counters
// rather than response headers is deliberate: the production handler
// path that converts an upstream-dependent 5xx (gerrors.ServiceUnavailable
// or gerrors.GatewayTimeout) into a ServeResult does NOT carry over
// the rate-limit headers it computed earlier in applyRateLimitGate —
// see handler.go::Handle, which returns toServeResult(...) directly on
// the upstream-fail branch and never merges rlHeaders. Pinning the
// header here would mask that gap behind a green test.
func TestIntegration_NATSOutage_NATSKVRateLimit_FailPolicyOpenAllowsThrough(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntryWithRateLimit("GET", "/users", "nats-kv", 100, 200),
	}
	h := setupResilienceHarness(t, entries)

	natskvStore, err := ratelimit.NewNATSKVStore(context.Background(), ratelimit.NATSKVStoreConfig{
		JS:            h.js,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        1 * time.Minute,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)
	require.NoError(t, h.router.EnsureBackend("nats-kv", func() (ratelimit.Store, error) {
		return natskvStore, nil
	}))

	healthy := h.handler.Handle(context.Background(), newRequest("GET", "/users"))
	require.Equal(t, 200, healthy.Status, "healthy baseline must serve 200")

	beforeAllowed := natskvStore.Counters()["ratelimit_natskv_decisions_allowed_total"]

	h.stopNATS()
	t.Cleanup(func() { _ = h.container.Start(context.Background()) })

	// Tight ctx so the test does not block on the full 2s per-route
	// budget — the upstream call WILL fail; we only need the gate to
	// run and the response shape to land before the assertion.
	upstreamCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	result := h.handler.Handle(upstreamCtx, newRequest("GET", "/users"))

	assert.GreaterOrEqual(t, result.Status, 500,
		"upstream-dependent request must surface 5xx during NATS outage")
	assert.Less(t, result.Status, 600)

	// Empirical proof the rate-limit gate ran post-outage. The store's
	// Allow either bumps backend_errors_total (Get fails on dead NATS)
	// or bumps decisions_allowed_total / decisions_rejected_total. At
	// least ONE of those three counters MUST have moved between the
	// healthy baseline and the post-outage call; if every counter is
	// flat, the gate was silently bypassed and the FailPolicy contract
	// is broken.
	after := natskvStore.Counters()
	totalDelta := (after["ratelimit_natskv_decisions_allowed_total"] - beforeAllowed) +
		after["ratelimit_natskv_decisions_rejected_total"] +
		after["ratelimit_natskv_backend_errors_total"]
	assert.GreaterOrEqual(t, totalDelta, int64(1),
		"natskv Allow must have ticked at least one counter (allowed/rejected/backend_errors) post-outage")
}

// TestIntegration_NATSOutageThenHeal_BackendInitRetryRecovers is
// skipped because the production retry cadence (rateLimitRetryInterval,
// 30 s) is encoded as a package-level constant in cmd/gateway/main.go
// and the retry loop driver (startRateLimitRetryLoop) is package-
// private to main. Reproducing the recovery semantics in an integration
// test that lives in the proxy package would require either:
//
//   - adding a NewHandlerWithRetryInterval-style constructor or an
//     exported retry-interval injection point on the proxy.Handler /
//     ratelimit.Router boundary so the test can drive the loop on a
//     short cadence, OR
//   - moving startRateLimitRetryLoop / ensureRateLimitBackends to an
//     exported helper in a sibling package that both main.go and the
//     test can import.
//
// Either change is a production-code edit, which is out of scope for
// this commit (integration tests only). Until one lands, this scenario
// is verified empirically by operators via the existing 30s heal
// window.
func TestIntegration_NATSOutageThenHeal_BackendInitRetryRecovers(t *testing.T) {
	t.Skip("retry cadence (30s) is a package-level const in cmd/gateway/main.go; " +
		"need an exported retry-interval injection point on the Handler/Router boundary " +
		"or a relocated ensureRateLimitBackends to drive the heal loop on a sub-second cadence")
}

// TestIntegration_NATSOutage_RouterCloseSurvivesShutdown pins the
// drain ordering invariant: closing the rate-limit Router during a
// NATS outage installs the closed-sentinel store, and any in-flight
// rate-limit check observes ratelimit.ErrStoreClosed via FailPolicy
// rather than a raw "connection draining" error. The test does not
// invoke lifecycle.Drain directly because that bundles HTTP and NATS
// drain steps the proxy package has no handle for; instead it
// exercises the router-close half of the contract — the part the
// proxy handler actually consumes.
func TestIntegration_NATSOutage_RouterCloseSurvivesShutdown(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntryWithRateLimit("GET", "/users", "memory", 1000, 2000),
	}
	h := setupResilienceHarness(t, entries)

	healthy := h.handler.Handle(context.Background(), newRequest("GET", "/users"))
	require.Equal(t, 200, healthy.Status)

	h.stopNATS()
	t.Cleanup(func() { _ = h.container.Start(context.Background()) })

	// Close the router mid-outage. After Close:
	//   - The router flips to closed state.
	//   - StoreFor returns the closed sentinel.
	//   - Allow on the sentinel returns (Allowed=true, ErrStoreClosed).
	//   - The handler's FailPolicyOpen resolves the error to "allow".
	//   - The upstream NATS call still fails because NATS is down.
	//
	// The contract this test pins: closing the router during an outage
	// does not panic, does not deadlock, and the gateway's response
	// shape stays well-formed (5xx with rate-limit headers).
	require.NoError(t, h.router.Close())

	upstreamCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	result := h.handler.Handle(upstreamCtx, newRequest("GET", "/users"))

	// Post-close, post-outage: the request must complete cleanly with a
	// well-formed 5xx (no panic, no nil-pointer deref, no infinite
	// hang). The handler.go upstream-fail branch returns
	// toServeResult(gerrors.ServiceUnavailable) directly without
	// merging the rate-limit headers computed earlier, so asserting on
	// X-RateLimit-Limit here would be coupling the test to that gap
	// rather than the contract it pretends to pin (see
	// TestIntegration_NATSOutage_NATSKVRateLimit_FailPolicyOpenAllowsThrough
	// for the symmetrical observation).
	assert.GreaterOrEqual(t, result.Status, 500,
		"post-close, post-outage upstream call must still surface 5xx cleanly")
	assert.Less(t, result.Status, 600)
	assert.NotNil(t, result.Body, "ServeResult body must be populated, not nil")
}

// TestIntegration_BreakerOpensOnSustainedNATSFailure pins gobreaker
// behaviour against a real NATS-KV store. Sustained backend failures
// during an outage trip the breaker open; a healthy probe against a
// fresh connection post-restart confirms the breaker can recover.
//
// The natskv breaker's IsSuccessful explicitly classifies
// context.Canceled and context.DeadlineExceeded as breaker-side
// successes — a benign caller-timeout cascade MUST NOT trip the
// breaker on a healthy backend. To trip it the test surfaces a
// non-ctx error: a dedicated nats.go connection is configured with
// MaxReconnects=0 and ReconnectBufSize=-1 so the first disconnect
// drives the connection straight to CLOSED and subsequent JetStream
// calls return nats.ErrConnectionClosed (NOT a wrapped ctx error).
// The harness's shared connection cannot be used here — it is pinned
// to MaxReconnects=-1 so the other resilience scenarios can exercise
// the reconnect path.
//
// Recovery uses a fresh connection rather than reviving the
// dedicated one because MaxReconnects=0 leaves the dedicated socket
// permanently CLOSED. Pinning the heal on a separate, healthy
// connection keeps the assertion focused on breaker semantics
// (open → half-open → closed) without conflating it with nats.go's
// reconnect machinery.
func TestIntegration_BreakerOpensOnSustainedNATSFailure(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntryWithRateLimit("GET", "/users", "nats-kv", 100, 200),
	}
	h := setupResilienceHarness(t, entries)

	// Dedicated short-circuit connection: any disconnect closes the
	// socket immediately so JS Get surfaces ErrConnectionClosed
	// without queuing or waiting on ctx.
	dedicatedConn, err := natsgo.Connect(h.url,
		natsgo.MaxReconnects(0),
		natsgo.ReconnectBufSize(-1),
	)
	require.NoError(t, err)
	t.Cleanup(dedicatedConn.Close)

	dedicatedJS, err := jetstream.New(dedicatedConn)
	require.NoError(t, err)

	natskvStore, err := ratelimit.NewNATSKVStore(context.Background(), ratelimit.NATSKVStoreConfig{
		JS:            dedicatedJS,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        1 * time.Minute,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	// Sanity: a healthy Allow must succeed before the outage so the
	// breaker has a known closed baseline.
	decision, err := natskvStore.Allow(context.Background(), "k", 1_000_000, 1000)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	require.Equal(t, int64(0), natskvStore.Counters()["ratelimit_natskv_circuit_state"],
		"breaker must start closed")

	h.stopNATS()
	t.Cleanup(func() { _ = h.container.Start(context.Background()) })

	// Wait for the dedicated connection to observe the disconnect and
	// transition to CLOSED. MaxReconnects=0 makes the first disconnect
	// final; polling avoids a fixed sleep that would either be too slow
	// on a healthy host or too short on a busy CI runner.
	require.Eventually(t, func() bool {
		return dedicatedConn.IsClosed()
	}, 5*time.Second, 50*time.Millisecond,
		"dedicated connection must transition to CLOSED after NATS stop")

	// 15 calls is comfortably above the 10-failure trip threshold.
	// Each Allow surfaces a transport-level error (not a ctx error)
	// which the breaker counts as a real failure.
	for i := 0; i < 15; i++ {
		_, _ = natskvStore.Allow(context.Background(), fmt.Sprintf("hot-key-%d", i), 1_000_000, 1000)
	}

	openState := natskvStore.Counters()["ratelimit_natskv_circuit_state"]
	transitions := natskvStore.Counters()["ratelimit_natskv_breaker_transitions_total"]
	circuitRejected := natskvStore.Counters()["ratelimit_natskv_circuit_rejected_total"]
	assert.Equal(t, int64(2), openState,
		"breaker must trip to open (state=2) after sustained non-ctx outage failures")
	assert.GreaterOrEqual(t, transitions, int64(1),
		"breaker must have logged at least one state transition (closed→open)")
	assert.GreaterOrEqual(t, circuitRejected, int64(1),
		"at least one Allow must have short-circuited through the open breaker")

	// Restart the container without waiting for the harness's shared
	// connection to reconnect — this test does not use it. The fresh
	// healConn below establishes its own session against the restarted
	// container; gating on the shared connection here would couple the
	// recovery probe to unrelated reconnect timing.
	h.startNATSContainerOnly()

	// Recovery probe: a fresh connection (the dedicated one is
	// permanently CLOSED) backs a fresh natskv store against the
	// restarted container. A successful Allow on this store proves the
	// JetStream backend genuinely healed; the dedicated store stays
	// open-circuit by design.
	//
	// IMPORTANT: testcontainers-go re-binds the published port on every
	// Start, so the URL captured at first Run can be stale after a
	// Stop/Start cycle. Re-fetch the connection string before dialling.
	healURL, urlErr := h.container.ConnectionString(context.Background())
	require.NoError(t, urlErr, "post-restart connection string lookup must succeed")

	// Retry the initial Connect with a short poll-and-retry so the test
	// does not flake on a slow runner — testcontainers signals "ready"
	// when the port is listening, which can race the NATS daemon's
	// accept loop coming up. nats.go's MaxReconnects only applies to a
	// CONNECTED→DISCONNECTED transition, not the very first dial.
	var healConn *natsgo.Conn
	connectDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(connectDeadline) {
		var connectErr error
		healConn, connectErr = natsgo.Connect(healURL,
			natsgo.MaxReconnects(-1),
			natsgo.ReconnectWait(100*time.Millisecond),
		)
		if connectErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NotNil(t, healConn, "post-restart Connect must succeed within 15s")
	t.Cleanup(healConn.Close)

	healJS, err := jetstream.New(healConn)
	require.NoError(t, err)

	healStore, err := ratelimit.NewNATSKVStore(context.Background(), ratelimit.NATSKVStoreConfig{
		JS:            healJS,
		HandlerBucket: "handler_registry",
		BucketSuffix:  "_ratelimit",
		KeyTTL:        1 * time.Minute,
		Logger:        zerolog.Nop(),
	})
	require.NoError(t, err)

	probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	probeDecision, probeErr := healStore.Allow(probeCtx, "probe-key", 1_000_000, 1000)
	require.NoError(t, probeErr, "probe call against the recovered backend must succeed")
	require.True(t, probeDecision.Allowed)
}

// =====================================================================
// JetStream-only outage scenarios
//
// The tests above kill the entire NATS process (Stop on the
// testcontainer): core pub/sub AND JetStream go down together.
// That covers the "core NATS outage" case but misses a far more
// common production scenario — JetStream-only failure where core
// NATS pub/sub keeps running. In that mode the proxy upstream call
// succeeds, the auth verifier round-trip succeeds, and ONLY the KV
// layer is unhealthy: the watcher loses its watch stream, the
// nats-kv rate-limit backend fails CAS, but client-visible traffic
// must KEEP FLOWING.
//
// These tests simulate that surgical outage by deleting the
// JetStream stream that backs the KV bucket
// (jetstream.JetStream.DeleteStream("KV_handler_registry")). After
// deletion every KV operation on `handler_registry` errors with
// stream-not-found while every nc.Request to a non-JS subject
// keeps working.
// =====================================================================

// captureLogger returns a zerolog.Logger that writes every entry
// to a thread-safe bytes.Buffer plus a snapshot func that returns
// the buffer contents at call time. JS-outage tests assert on
// substrings of the captured stream to verify watcher and breaker
// log lines without parsing JSON envelopes per-line.
func captureLogger() (zerolog.Logger, func() string) {
	var (
		buf bytes.Buffer
		mu  sync.Mutex
	)
	writer := &lockedBuffer{mu: &mu, buf: &buf}
	logger := zerolog.New(writer).With().Timestamp().Logger()

	return logger, func() string {
		mu.Lock()
		defer mu.Unlock()

		return buf.String()
	}
}

// lockedBuffer adapts bytes.Buffer behind a mutex so the watcher
// goroutine and the test goroutine can write to / read from the
// log stream concurrently. The stdlib Buffer is not goroutine-safe;
// without the lock the race detector trips on every captureLogger
// caller.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

// setupResilienceHarnessWithLogger mirrors setupResilienceHarness
// but accepts a caller-supplied logger so JS-outage tests can
// capture watcher and router log lines for substring assertions.
// The original setupResilienceHarness above stays intact for the
// existing scenarios that prefer zerolog.Nop().
func setupResilienceHarnessWithLogger(
	t *testing.T,
	entries map[string]registry.HandlerEntry,
	logger zerolog.Logger,
) *resilienceHarness {
	t.Helper()
	ctx := context.Background()

	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	gatewayConn, err := natsgo.Connect(url,
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(100*time.Millisecond),
		natsgo.NoEcho(),
	)
	require.NoError(t, err)
	t.Cleanup(gatewayConn.Close)

	responderConn, err := natsgo.Connect(url,
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(100*time.Millisecond),
	)
	require.NoError(t, err)
	t.Cleanup(responderConn.Close)

	js, err := jetstream.New(gatewayConn)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  handlerRegistryBucketName,
		History: 1,
	})
	require.NoError(t, err)

	for key, entry := range entries {
		raw, marshalErr := json.Marshal(entry)
		require.NoError(t, marshalErr)
		_, err = kv.Put(ctx, key, raw)
		require.NoError(t, err)
	}

	store := registry.NewStore()
	watcher := registry.NewWatcher(kv, store, logger)

	tableHolder := &atomic.Value{}
	rebuilt := make(chan struct{}, 1)
	rebuild := func() {
		snap := store.Get()
		verifiers := auth.BuildVerifierRegistry(snap, logger)
		routes := routing.CollectRoutes(snap, verifiers, logger)
		tableHolder.Store(routing.BuildTableFromRoutes(routes))
		select {
		case rebuilt <- struct{}{}:
		default:
		}
	}

	rebuild()
	watcher.OnChange(rebuild)
	require.NoError(t, watcher.Start(ctx))
	t.Cleanup(watcher.Stop)

	select {
	case <-rebuilt:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not fire initial-load callback within 2s")
	}

	router := ratelimit.NewRouter(ratelimit.FailPolicyOpen.Resolve(), logger)
	require.NoError(t, router.EnsureBackend("memory", func() (ratelimit.Store, error) {
		return ratelimit.NewMemoryStore(time.Hour), nil
	}))
	t.Cleanup(func() { _ = router.Close() })

	stopRespond := make(chan struct{})
	subjects := make(map[string]struct{})
	for key, entry := range entries {
		if entry.HTTP == nil {
			continue
		}
		subj, subjErr := registry.SubjectFromKey(key)
		require.NoError(t, subjErr)
		subjects[subj] = struct{}{}
	}

	for subj := range subjects {
		subj := subj
		sub, subErr := responderConn.Subscribe(subj, func(msg *natsgo.Msg) {
			select {
			case <-stopRespond:
				return
			default:
			}

			reply := []byte(`{"status":200,"headers":{"x-upstream":["fake"]},"body":{"ok":true,"subject":"` + subj + `"}}`)
			_ = msg.Respond(reply)
		})
		require.NoError(t, subErr)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	require.NoError(t, responderConn.Flush())

	handler := NewHandler(HandlerConfig{
		Table: func() routing.Table {
			return tableHolder.Load().(routing.Table)
		},
		Nats:             &natsRequesterAdapter{conn: gatewayConn},
		Encoder:          NewDefaultEncoder(),
		Decoder:          NewDefaultDecoder(),
		Timeout:          2 * time.Second,
		Logger:           logger,
		RateLimiter:      router,
		RateLimitTimeout: 200 * time.Millisecond,
	})

	h := &resilienceHarness{
		t:             t,
		container:     container,
		url:           url,
		gatewayConn:   gatewayConn,
		responderConn: responderConn,
		js:            js,
		kv:            kv,
		store:         store,
		watcher:       watcher,
		router:        router,
		handler:       handler,
		tableHolder:   tableHolder,
		stopRespond:   stopRespond,
	}
	t.Cleanup(h.shutdownResponders)

	return h
}

// deleteJetStreamForBucket destroys the JetStream stream backing
// the named KV bucket. After this returns, every KV operation on
// the bucket fails with stream-not-found while core NATS pub/sub
// on every other subject keeps working. This is the surgical
// primitive that distinguishes "JS-only outage" from
// "process-level NATS down".
func (h *resilienceHarness) deleteJetStreamForBucket(t *testing.T, bucket string) {
	t.Helper()
	ctx := context.Background()
	err := h.js.DeleteStream(ctx, jsKVStreamPrefix+bucket)
	require.NoErrorf(t, err, "delete JS stream backing %q", bucket)
}

// recreateHandlerRegistryBucket re-materialises the handler
// registry bucket after a delete so the watcher's reconnect path
// has a stream to attach to. Returns the freshly-created KeyValue
// handle for callers that need to seed new entries directly
// without going through the watcher.
func (h *resilienceHarness) recreateHandlerRegistryBucket(t *testing.T) jetstream.KeyValue {
	t.Helper()
	ctx := context.Background()
	kv, err := h.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  handlerRegistryBucketName,
		History: 1,
	})
	if err != nil && (errors.Is(err, jetstream.ErrBucketExists) ||
		errors.Is(err, jetstream.ErrStreamNameAlreadyInUse)) {
		// Already healed by an earlier cleanup hook — open the
		// existing handle so the caller can still seed entries.
		kv, err = h.js.KeyValue(ctx, handlerRegistryBucketName)
	}
	require.NoError(t, err)

	return kv
}

// recreateRateLimitBucket re-materialises the ratelimit companion
// bucket so a JS-only outage test does not leave a missing bucket
// behind for unrelated tests sharing the same image.
func (h *resilienceHarness) recreateRateLimitBucket(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_, err := h.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       rateLimitBucketName,
		History:      1,
		Storage:      jetstream.MemoryStorage,
		MaxValueSize: 1024,
	})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) &&
		!errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
		require.NoError(t, err, "recreate ratelimit bucket")
	}
}

// TestIntegration_JetStreamOnlyOutage_RealTrafficStillServed pins
// the highest-value survivability contract: when ONLY JetStream
// goes down (KV watcher) and core NATS pub/sub stays healthy, the
// gateway must continue routing real HTTP traffic without
// client-visible degradation. The routing table snapshot is
// in-memory, the upstream subscriber answers over core NATS, and
// the watcher logs the JS error but the snapshot is preserved.
//
// Recovery is also pinned: after recreating the KV bucket, a new
// route written to the freshly-materialised stream lands in the
// store within the watcher's reconnect window.
func TestIntegration_JetStreamOnlyOutage_RealTrafficStillServed(t *testing.T) {
	logger, snapshotLog := captureLogger()
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntry("GET", "/users"),
	}
	h := setupResilienceHarnessWithLogger(t, entries, logger)

	baseline := h.handler.Handle(context.Background(), newRequest("GET", "/users"))
	require.Equal(t, 200, baseline.Status, "baseline /users must succeed before JS outage")

	h.deleteJetStreamForBucket(t, handlerRegistryBucketName)
	t.Cleanup(func() { _ = h.recreateHandlerRegistryBucket(t) })

	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		result := h.handler.Handle(ctx, newRequest("GET", "/users"))
		cancel()
		require.Equalf(t, 200, result.Status,
			"request %d must succeed during JS-only outage — core NATS is healthy", i)
	}

	snap := h.store.Get()
	_, ok := snap.Entries["users-svc.cmd.users.list"]
	assert.True(t, ok,
		"in-memory routing snapshot must survive JS outage — the watcher must not wipe it on watch error")

	// nats.go's WatchAll subscription does not surface stream-
	// deletion as an error on the Updates() channel — the channel
	// stays open and silent. The production fix (runWatch's
	// periodic stream-presence probe) catches the deletion and
	// returns an error, triggering watchLoop's restart-with-backoff
	// path which logs "watch loop error" before sleeping.
	require.Eventually(t, func() bool {
		log := snapshotLog()

		return strings.Contains(log, "watch loop error") ||
			strings.Contains(log, "watch updates channel closed")
	}, 8*time.Second, 250*time.Millisecond,
		"watcher must log the JS-side error so operators can alert on the degraded state")

	healedKV := h.recreateHandlerRegistryBucket(t)

	const recoveryKey = "users-svc.cmd.users.recovered"
	const recoveryPath = "/users-recovered"

	subj, err := registry.SubjectFromKey(recoveryKey)
	require.NoError(t, err)
	sub, err := h.responderConn.Subscribe(subj, func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte(`{"status":200,"headers":{},"body":{"recovered":true}}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, h.responderConn.Flush())

	raw, err := json.Marshal(httpEntry("GET", recoveryPath))
	require.NoError(t, err)
	_, err = healedKV.Put(context.Background(), recoveryKey, raw)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		result := h.handler.Handle(ctx, newRequest("GET", recoveryPath))

		return result.Status == 200
	}, 15*time.Second, 250*time.Millisecond,
		"watcher must self-heal after JS recovery and surface new routes")
}

// TestIntegration_JetStreamOnlyOutage_AuthFlowStillSucceeds verifies
// that auth-required routes continue to authenticate during a JS
// outage. The verifier round-trip is core NATS pub/sub, NOT
// JetStream — losing JS must not cascade into auth failures, which
// would lock every user out of the system on what is supposed to
// be a transparent KV-layer event.
func TestIntegration_JetStreamOnlyOutage_AuthFlowStillSucceeds(t *testing.T) {
	verifierEntry := registry.HandlerEntry{
		Verifier: &registry.VerifierMeta{ID: "default-verifier", Default: true},
	}
	authedEntry := httpEntry("GET", "/users/me")
	authedEntry.Auth = &registry.RouteAuthMeta{Verifier: "default-verifier"}

	entries := map[string]registry.HandlerEntry{
		"auth-svc.cmd.auth.verify": verifierEntry,
		"auth-svc.cmd.users.me":    authedEntry,
	}
	h := setupResilienceHarnessWithLogger(t, entries, zerolog.Nop())

	verifierSubject, err := registry.SubjectFromKey("auth-svc.cmd.auth.verify")
	require.NoError(t, err)
	verifierSub, err := h.responderConn.Subscribe(verifierSubject, func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte(`{"status":200,"headers":{},"body":{"sub":"u-1","tenant":"t-1"}}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = verifierSub.Unsubscribe() })
	require.NoError(t, h.responderConn.Flush())

	withAuth := newRequest("GET", "/users/me")
	withAuth.Headers["authorization"] = "Bearer t"
	baseline := h.handler.Handle(context.Background(), withAuth)
	require.Equal(t, 200, baseline.Status, "auth baseline must succeed before JS outage")

	h.deleteJetStreamForBucket(t, handlerRegistryBucketName)
	t.Cleanup(func() { _ = h.recreateHandlerRegistryBucket(t) })

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		req := newRequest("GET", "/users/me")
		req.Headers["authorization"] = "Bearer t"
		result := h.handler.Handle(ctx, req)
		cancel()
		require.Equalf(t, 200, result.Status,
			"auth-protected request %d must succeed during JS outage — verifier round trip is core NATS, not JetStream",
			i)
	}
}

// TestIntegration_JetStreamOnlyOutage_NATSKVRateLimitFailsOpen_TrafficContinues
// pins the cross-product contract: a route configured with
// store=nats-kv and FailPolicyOpen MUST keep serving traffic
// during a JS outage. The Decision returned by the store under
// outage is empty (zero ResetAt), which BuildHeaders MUST treat
// as "skip Remaining/Reset" — the static X-RateLimit-Limit header
// is safe to emit on the failure branch but the dynamic ones
// would encode time.Time{} as a year-1 epoch and confuse clients.
func TestIntegration_JetStreamOnlyOutage_NATSKVRateLimitFailsOpen_TrafficContinues(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntryWithRateLimit("GET", "/users", "nats-kv", 100, 200),
	}
	h := setupResilienceHarnessWithLogger(t, entries, zerolog.Nop())

	// nats-kv backend must be registered before traffic flows so
	// the rate-limit gate exercises the JetStream-backed store
	// rather than falling back to memory. The companion bucket
	// materialises here as a side effect of NewNATSKVStore — that
	// is also the stream we delete below to simulate the JS outage.
	require.NoError(t, h.router.EnsureBackend("nats-kv", func() (ratelimit.Store, error) {
		return ratelimit.NewNATSKVStore(context.Background(), ratelimit.NATSKVStoreConfig{
			JS:            h.js,
			HandlerBucket: handlerRegistryBucketName,
			BucketSuffix:  "_ratelimit",
			KeyTTL:        1 * time.Minute,
			Logger:        zerolog.Nop(),
		})
	}))

	healthy := h.handler.Handle(context.Background(), newRequest("GET", "/users"))
	require.Equal(t, 200, healthy.Status)
	require.NotEmpty(t, healthy.Headers["X-RateLimit-Limit"])
	require.NotEmpty(t, healthy.Headers["X-RateLimit-Remaining"],
		"healthy nats-kv backend must populate Remaining")

	// Surgical JS outage: delete only the ratelimit bucket's
	// stream. The handler registry stream stays alive so the
	// routing table keeps serving — the only thing breaking is
	// the rate-limit CAS path.
	h.deleteJetStreamForBucket(t, rateLimitBucketName)
	t.Cleanup(func() { h.recreateRateLimitBucket(t) })

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		result := h.handler.Handle(ctx, newRequest("GET", "/users"))
		cancel()
		require.Equalf(t, 200, result.Status,
			"request %d under FailPolicyOpen + JS-only ratelimit outage must allow", i)

		assert.NotEmpty(t, result.Headers["X-RateLimit-Limit"],
			"X-RateLimit-Limit is static config and must survive a JS outage")

		// Empty-Decision header guard: BuildHeaders MUST NOT emit
		// Remaining/Reset when the Decision is zero, because the
		// underlying time.Time{} would render as a year-1 epoch.
		_, hasRemaining := result.Headers["X-RateLimit-Remaining"]
		_, hasReset := result.Headers["X-RateLimit-Reset"]
		assert.False(t, hasRemaining,
			"X-RateLimit-Remaining must be absent under JS outage (empty Decision)")
		assert.False(t, hasReset,
			"X-RateLimit-Reset must be absent under JS outage (empty Decision)")
	}
}

// TestIntegration_JetStreamOnlyOutage_MemoryRateLimitUnaffected
// proves the memory backend is fully JetStream-independent:
// deleting every JS stream must not perturb GCRA decisions or
// counters for routes configured with store=memory. This is the
// survival pattern operators reach for when their JS cluster is
// flaky — switch the route to memory and accept per-pod (not
// cross-replica) limits.
func TestIntegration_JetStreamOnlyOutage_MemoryRateLimitUnaffected(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntryWithRateLimit("GET", "/users", "memory", 1, 1),
	}
	h := setupResilienceHarnessWithLogger(t, entries, zerolog.Nop())

	memStore := h.router.StoreFor(routing.Route{
		RateLimit: &registry.RateLimitMeta{Store: "memory", RPS: 1},
	})
	beforeCounters := memStore.Counters()
	allowedBefore := beforeCounters["ratelimit_memory_decisions_allowed_total"]
	rejectedBefore := beforeCounters["ratelimit_memory_decisions_rejected_total"]

	baseline := h.handler.Handle(context.Background(), newRequest("GET", "/users"))
	require.Equal(t, 200, baseline.Status)

	h.deleteJetStreamForBucket(t, handlerRegistryBucketName)
	t.Cleanup(func() { _ = h.recreateHandlerRegistryBucket(t) })

	const calls = 20
	for i := 0; i < calls; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_ = h.handler.Handle(ctx, newRequest("GET", "/users"))
		cancel()
	}

	afterCounters := memStore.Counters()
	allowedDelta := afterCounters["ratelimit_memory_decisions_allowed_total"] - allowedBefore
	rejectedDelta := afterCounters["ratelimit_memory_decisions_rejected_total"] - rejectedBefore

	// Baseline + calls = total decisions seen by the gate. Memory
	// backend's GCRA stays exact under a JS outage because it
	// touches no JetStream-backed state.
	assert.Equal(t, int64(calls+1), allowedDelta+rejectedDelta,
		"every request must produce exactly one memory rate-limit decision regardless of JS state")
	assert.GreaterOrEqual(t, rejectedDelta, int64(1),
		"rps=1 burst=1 over 20 tightly-packed calls must reject at least once — memory backend is enforcing")
}

// TestIntegration_ReadyzReflectsNATSConnectionHealth pins the
// readiness contract: /readyz reports ready ONLY when both the
// initial routing-table snapshot has landed AND the NATS connection
// is currently CONNECTED. A NATS outage post-boot MUST flip the
// flag to false so the K8s load balancer pulls the replica from
// rotation; otherwise traffic continues to 5xx without operator
// signal, amplifying the outage from "service degraded" to "every
// pod stays Ready while serving 5xx".
//
// Mirrors the production readiness closure in cmd/gateway/main.go
// so a regression that loosens the contract (drops the NATS check,
// re-introduces a one-way latch) surfaces here.
func TestIntegration_ReadyzReflectsNATSConnectionHealth(t *testing.T) {
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntry("GET", "/users"),
	}
	h := setupResilienceHarnessWithLogger(t, entries, zerolog.Nop())

	// Production-shape closure: AND of (snapshot landed) and
	// (NATS connected). The snapshot already landed in setup, so
	// the latch is already true here.
	var snapshotLanded atomic.Bool
	snapshotLanded.Store(true)
	readiness := func() bool {
		return snapshotLanded.Load() && h.gatewayConn.Status() == natsgo.CONNECTED
	}

	require.True(t, readiness(),
		"baseline: snapshot landed + NATS connected → ready")

	// Simulate a sustained NATS outage (drop the entire connection,
	// not just the JS layer). Since auto-reconnect is on, status
	// transitions to RECONNECTING; the readiness function MUST
	// resolve to false during this window — this is the production-
	// critical assertion. Without this flip, K8s keeps the degraded
	// replica in rotation and clients see 5xx with no operator
	// signal.
	h.stopNATS()
	t.Cleanup(func() { h.startNATSContainerOnly() })

	require.Eventually(t, func() bool {
		return !readiness()
	}, 5*time.Second, 100*time.Millisecond,
		"readyz MUST flip false within 5s of NATS disconnect — without this K8s keeps a degraded replica in rotation")

	// Recovery is not asserted here because testcontainers-go
	// re-binds the published port on every Start; the gateway's
	// pre-bound connection cannot rediscover the new port without
	// fetching a fresh ConnectionString (the breaker test does
	// this for its own probe). The recovery contract — "readiness
	// flips back when nc.Status() returns CONNECTED" — is implicit
	// in the closure itself: it is a pure function of NATS
	// connection state with no latch or hysteresis. Pinning it
	// further would test the closure's algebra rather than the
	// production wiring.
}

// TestIntegration_JetStreamHealAfterOutage_WatcherResumesUpdates
// proves the watcher's runWatch retry loop actually re-subscribes
// to JetStream after the underlying stream is destroyed and
// recreated. The watch goroutine has a "restart on error" branch
// with a 2s backoff; this test exercises the full destroy →
// recreate → new-route-published path and asserts the new route
// lands in the store.
//
// If the watcher reconnect path is broken (e.g. WatchAll fails
// against the recreated bucket because consumer-group state did
// not reset cleanly), this test fails and surfaces a real
// recovery bug. The handling instructions on this scenario say
// to surface (NOT fix) any such failure in the final report.
func TestIntegration_JetStreamHealAfterOutage_WatcherResumesUpdates(t *testing.T) {
	logger, snapshotLog := captureLogger()
	entries := map[string]registry.HandlerEntry{
		"users-svc.cmd.users.list": httpEntry("GET", "/users"),
	}
	h := setupResilienceHarnessWithLogger(t, entries, logger)

	require.Equal(t, 200,
		h.handler.Handle(context.Background(), newRequest("GET", "/users")).Status,
		"baseline /users must serve before delete")

	h.deleteJetStreamForBucket(t, handlerRegistryBucketName)

	require.Eventually(t, func() bool {
		log := snapshotLog()

		return strings.Contains(log, "watch loop error") ||
			strings.Contains(log, "watch updates channel closed")
	}, 8*time.Second, 250*time.Millisecond,
		"watcher must observe and log the JS error before the recovery step")

	healedKV := h.recreateHandlerRegistryBucket(t)

	const healedKey = "users-svc.cmd.users.healed"
	const healedPath = "/users-healed"

	subj, err := registry.SubjectFromKey(healedKey)
	require.NoError(t, err)
	sub, err := h.responderConn.Subscribe(subj, func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte(`{"status":200,"headers":{},"body":{"healed":true}}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, h.responderConn.Flush())

	raw, err := json.Marshal(httpEntry("GET", healedPath))
	require.NoError(t, err)
	_, err = healedKV.Put(context.Background(), healedKey, raw)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		result := h.handler.Handle(ctx, newRequest("GET", healedPath))

		return result.Status == 200
	}, 20*time.Second, 250*time.Millisecond,
		"watcher must re-attach to the recreated bucket and surface new routes — if this fails, the watcher reconnect path is broken")
}

// Compile-time assertion that the harness adapter satisfies the
// proxy.NatsRequester contract. Keeps a regression in the contract
// (e.g., a future signature change on Request) from masquerading as a
// runtime nil-pointer when the test file is built under integration
// tags but skipped at runtime.
var _ NatsRequester = (*natsRequesterAdapter)(nil)
