// Package main is the entry point for horizon-gateway-server.
//
// The binary is a thin bootstrap: it loads configuration, constructs
// every internal component in dependency order, starts the HTTP
// server in a background goroutine, and blocks on SIGTERM. All
// non-bootstrap logic lives in the internal packages so this file
// stays auditable at a glance.
//
// Failure-path discipline: anything that happens before the zerolog
// logger is built writes to stderr and exits with code 1, because
// emitting structured JSON through a non-existent logger is
// impossible. Everything after goes through logger.Fatal() so
// operators get the same JSON shape for startup failures as for
// runtime errors. logger.Fatal() already calls os.Exit(1) internally,
// so no explicit exit follows it.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/auth"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/lifecycle"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/ratelimit"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
	httptransport "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/transport/http"
	natstransport "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/transport/nats"
)

// rateLimitRetryInterval is how often the gateway re-runs
// ensureRateLimitBackends in the background to recover from
// transient init failures (e.g., a NATS-KV bucket that was
// momentarily unreachable at boot). Thirty seconds balances
// "recover within the typical operator's eyes-on window after a
// flap" against "no observable load on the cluster from a healthy
// gateway" — by definition the loop is a no-op for any backend
// already registered.
const rateLimitRetryInterval = 30 * time.Second

// main wires the gateway end-to-end. The body is intentionally a flat
// sequence of helper calls so the control flow — config, logger,
// NATS, KV, registry, routing, requester, handler, HTTP server,
// block-on-signal, drain — is readable in under 30 lines.
func main() {
	cfg := loadConfigOrDie()
	logger := buildLoggerOrDie(cfg)
	logger.Info().
		Str("http_addr", cfg.HTTPAddr).
		Strs("nats_urls", cfg.NATSUrls).
		Str("kv_bucket", cfg.KVBucket).
		Msg("starting horizon-gateway-server")

	// retryCtx bounds the lifetime of the rate-limit backend retry
	// goroutine. Cancelling it on shutdown prevents the goroutine from
	// re-touching the Router after lifecycle.Drain has called its
	// Close — re-touching a closed Router only loses the operator a
	// stack trace; the Router refuses post-Close registrations
	// gracefully, but cancelling avoids the noise.
	retryCtx, cancelRetry := context.WithCancel(context.Background())
	defer cancelRetry()

	// Metrics are built before every other component so each can hook
	// its own counters during construction. The registry is private —
	// only the operator listener's /metrics endpoint serialises it.
	metrics := observability.NewMetrics()

	nc := connectNATSOrDie(cfg, logger)
	validatePayloadBudgetOrDie(nc, cfg, logger)
	js, kv := openKVOrDie(retryCtx, nc, cfg, logger)

	store := registry.NewStore()
	watcher := registry.NewWatcher(kv, store, logger)
	currentTable := installRoutingRebuild(store, watcher, logger)

	// Reload accounting registers BEFORE watcher.Start so the initial
	// snapshot counts as the first reload — a gateway whose
	// gateway_registry_reloads_total is still 0 after bootstrap is a
	// gateway whose routing table never loaded.
	watcher.OnChange(metrics.RecordRegistryReload)

	rlRouter := ratelimit.NewRouter(
		ratelimit.FailPolicy(cfg.RateLimitFailPolicy).Resolve(),
		logger,
	)
	metrics.RegisterRateLimit(rlRouter.CountersAll)

	// snapshotLanded latches true the moment the first watcher
	// snapshot lands and stays true forever — operators rely on
	// "process completed bootstrap" being a one-way transition (a
	// brief NATS blip mid-snapshot must not re-arm the bootstrap
	// gate). NATS-side health is layered ON TOP of this latch via
	// the readiness function below.
	var snapshotLanded atomic.Bool

	// Register the rate-limit ensure callback BEFORE Start so the
	// initial-snapshot path inside Start fires the callback together
	// with the routing rebuild — not after, which would leave a
	// brief window where the routing table refers to backends the
	// Router has not yet registered.
	watcher.OnChange(func() {
		ensureRateLimitBackends(retryCtx, rlRouter, store, cfg, js, logger)
		snapshotLanded.Store(true)
	})

	if err := watcher.Start(retryCtx); err != nil {
		logger.Fatal().Err(err).Msg("registry watcher start failed")
	}

	// readinessSignal reports the gateway as ready ONLY when both
	// (a) the initial routing-table snapshot has landed AND
	// (b) the NATS connection is currently CONNECTED.
	//
	// (a) prevents serving traffic against an empty routing table
	// at cold boot — every request would otherwise hit the catch-
	// all 404. (b) prevents staying "ready" while NATS is down,
	// which in turn would mislead the K8s load balancer into
	// pinning traffic to a replica that 5xx's every request
	// because no upstream call can complete. Without (b), a NATS
	// outage post-boot is amplified into a per-replica outage:
	// every pod stays "Ready", clients hit 5xx until the operator
	// notices.
	//
	// `nc.Status() == nats.CONNECTED` flips false on disconnect
	// and true on successful reconnect, driven by the nats.go
	// reconnect loop — no manual subscription to disconnect/
	// reconnect handlers is needed.
	readinessSignal := httptransport.ReadinessFunc(func() bool {
		return snapshotLanded.Load() && nc.Status() == natsgo.CONNECTED
	})

	// Background retry goroutine: re-runs ensureRateLimitBackends on
	// a fixed cadence so a backend that failed to initialise at boot
	// (e.g., transient NATS-KV unavailability) eventually recovers
	// without waiting for a registry KV change to wake the watcher
	// callback. EnsureBackend is a no-op for already-registered
	// backends, so the retry path is observable load only when there
	// is something to fix. The goroutine exits when retryCtx is
	// cancelled by main's defer on shutdown.
	startRateLimitRetryLoop(retryCtx, rlRouter, store, cfg, js, logger)

	requester := buildRequesterOrDie(nc, logger)
	resilient := natstransport.NewResilientRequester(requester, natstransport.ResilientConfig{
		MaxInflight:      cfg.NATSMaxInflight,
		QueueTimeout:     cfg.NATSInflightQueueTimeout,
		BreakerEnabled:   cfg.CircuitBreakerEnabled,
		FailureThreshold: cfg.CircuitBreakerFailureThreshold,
		RecoveryTimeout:  cfg.CircuitBreakerRecoveryTimeout,
		HalfOpenProbes:   cfg.CircuitBreakerHalfOpenProbes,
		// Per-service breaker map cardinality cap; see ResilientConfig.
		MaxBreakerSubjects: cfg.CircuitBreakerMaxSubjects,
	}, logger)
	metrics.RegisterNATS(resilient)
	handler := buildProxyHandler(cfg, currentTable, resilient, rlRouter, metrics, logger)
	httpServer, err := httptransport.NewServer(
		cfg,
		handler,
		readinessSignal,
		logger,
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("http server construction failed")
	}

	// Run the Hertz server directly instead of Spin() so that its
	// built-in SIGTERM/SIGINT handler does not race with our own
	// lifecycle.WaitForSignal. Spin() always registers its own
	// signal waiter, and when two goroutines listen for the same
	// signal the one that wakes first tears down the engine — if
	// Hertz wins, lifecycle.Drain's httpServer.Shutdown sees
	// "engine is not running" and in-flight requests are dropped
	// instead of drained. Run() blocks until Shutdown is called
	// externally, which is exactly the handoff our lifecycle
	// package expects.
	go func() {
		if err := httpServer.Run(); err != nil {
			logger.Error().Err(err).Msg("hertz server exited unexpectedly")
		}
	}()
	logger.Info().Str("addr", cfg.HTTPAddr).Msg("http server started")

	operatorServer := httptransport.NewOperatorServer(cfg, readinessSignal, metrics.Handler(),
		func() routing.Table {
			t, _ := currentTable.Load().(routing.Table)

			return t
		})
	go func() {
		if err := operatorServer.Run(); err != nil {
			logger.Error().Err(err).Msg("operator http server exited unexpectedly")
		}
	}()
	logger.Info().Str("addr", cfg.OperatorHTTPAddr).Msg("operator http server started (probes, metrics, pprof)")

	sig := lifecycle.WaitForSignal()
	logger.Info().Str("signal", sig.String()).Msg("shutdown signal received")

	// Cancel the retry loop before Drain so the background goroutine
	// stops touching the Router while Drain.closeRateLimitRouter is
	// running. Defer-driven cancel would also work but explicit here
	// keeps the shutdown ordering visible at the call site.
	cancelRetry()

	lifecycle.Drain(lifecycle.Options{
		HTTP:         httpServer,
		OperatorHTTP: operatorServer,
		Watcher:      watcher,
		RateLimit:    rlRouter,
		NATS:         nc,
		Timeout:      cfg.ShutdownTimeout,
		Logger:       logger,
	})
}

// loadConfigOrDie loads the operator-facing config and terminates the
// process via stderr + os.Exit if it is missing or malformed. The
// logger is not yet available at this point, so the error path
// bypasses zerolog entirely and writes to stderr in plain text —
// this is the ONE place in the bootstrap where structured logging
// cannot be used because its dependency has not been built yet.
func loadConfigOrDie() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: config load failed: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// buildLoggerOrDie constructs the zerolog logger from cfg. Any failure
// is terminal — without a working logger the rest of the bootstrap
// would emit to /dev/null and operators would have no diagnostic
// surface to debug why the pod is in CrashLoopBackOff. Like
// loadConfigOrDie, the error path writes to stderr because the
// logger that WOULD carry the error is the very thing that just
// failed to construct.
func buildLoggerOrDie(cfg *config.Config) zerolog.Logger {
	logger, err := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: logger init failed: %v\n", err)
		os.Exit(1)
	}
	return logger
}

// connectNATSOrDie dials the NATS cluster and returns a live
// connection. Failure is fatal because the gateway has no reason to
// run without a NATS link — every request path ends in a Core NATS
// request/reply, and a gateway that cannot reach NATS is strictly
// worse than no gateway (it would 503 every request with zero
// useful diagnostic signal).
func connectNATSOrDie(cfg *config.Config, logger zerolog.Logger) *natsgo.Conn {
	nc, err := natstransport.Connect(cfg, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("nats connect failed")
	}
	return nc
}

// validatePayloadBudgetOrDie verifies at startup that a maximal HTTP
// request (body at HTTP_MAX_BODY_BYTES, headers at
// HTTP_MAX_HEADER_BYTES) still fits the connected NATS server's
// max_payload once wrapped in the request envelope. Fatal on misfit:
// a gateway whose HTTP guards admit requests that deterministically
// fail at NATS publish time would surface a permanent config error as
// per-request 4xx/5xx noise instead of one loud startup failure. The
// live Conn.MaxPayload() is used — the server advertises the value in
// the INFO handshake, so the check reflects the actual cluster.
func validatePayloadBudgetOrDie(nc *natsgo.Conn, cfg *config.Config, logger zerolog.Logger) {
	if err := natstransport.ValidatePayloadBudget(nc.MaxPayload(), cfg.MaxBodyBytes, cfg.MaxHeaderBytes); err != nil {
		logger.Fatal().Err(err).Msg("nats payload budget validation failed")
	}
}

// openKVOrDie initializes the JetStream client and opens the
// handler_registry KV bucket that holds the routing metadata.
// It returns both the JetStream context and the KeyValue handle:
// the JetStream context is needed by downstream components that
// create additional KV buckets at startup (e.g., the ratelimit
// Router).
//
// Bucket-presence policy: if the bucket does not exist yet (cold
// cluster, fresh deploy where no SDK service has booted, ordering
// race during a docker-compose stack-up), the gateway creates it
// itself with sensible defaults instead of refusing to start. This
// removes a chicken-and-egg between the gateway and SDK consumers:
// either side can come up first; the bucket appears for whoever wins.
// Existing entries written by the SDK survive — CreateKeyValue is a
// no-op once the underlying stream is present, but here we only call
// it on the explicit ErrBucketNotFound path so a healthy SDK-created
// bucket is never touched.
//
// All other JetStream errors remain fatal — a transport fault or
// permission error is not a recoverable bootstrap state.
func openKVOrDie(
	ctx context.Context,
	nc *natsgo.Conn,
	cfg *config.Config,
	logger zerolog.Logger,
) (jetstream.JetStream, jetstream.KeyValue) {
	js, err := jetstream.New(nc)
	if err != nil {
		logger.Fatal().Err(err).Msg("jetstream init failed")
	}
	kv, err := js.KeyValue(ctx, cfg.KVBucket)
	if err == nil {
		return js, kv
	}
	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		logger.Fatal().Err(err).Str("bucket", cfg.KVBucket).Msg("open kv bucket failed")
	}
	logger.Info().
		Str("bucket", cfg.KVBucket).
		Msg("kv bucket missing; creating with default config")
	kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  cfg.KVBucket,
		History: 1,
	})
	if err != nil {
		logger.Fatal().Err(err).Str("bucket", cfg.KVBucket).Msg("kv bucket create failed")
	}
	return js, kv
}

// installRoutingRebuild wires the routing table rebuild callback into
// the watcher. The first rebuild runs synchronously against whatever
// snapshot the store currently holds so a nil table is never observed
// by the proxy handler. Subsequent rebuilds fire on every KV change
// because the watcher invokes registered callbacks in registration
// order after every successful Store.Replace.
//
// The closure captured below tracks prevRoutes and firstLoad across
// rebuilds to emit lifecycle log entries: a single INFO "initial
// route set published" on the first rebuild, then an INFO or DEBUG
// "table rebuilt" on every subsequent rebuild depending on whether
// the delta is empty. The closure is touched by exactly one
// goroutine at a time — the watcher invokes OnChange callbacks
// serially on its single watch goroutine (see registry.Watcher
// godoc) and the initial synchronous call happens on the main
// goroutine before OnChange is registered and before Start is
// called, so the two phases never overlap. A future refactor that
// parallelises callbacks MUST add explicit synchronisation here.
//
// The returned *atomic.Value stores the current routing.Table; the
// proxy handler's TableProvider closure calls Load().(routing.Table)
// for a lock-free, always-consistent snapshot. atomic.Value is used
// rather than atomic.Pointer[routing.Table] because routing.Table is
// an interface — atomic.Pointer[Interface] would store a pointer-to-
// interface, introducing a second indirection on every request
// lookup for no benefit.
func installRoutingRebuild(
	store *registry.Store,
	watcher *registry.Watcher,
	logger zerolog.Logger,
) *atomic.Value {
	var (
		current    atomic.Value
		prevRoutes []routing.Route
		firstLoad  = true
	)

	rebuild := func() {
		snapshot := store.Get()
		verifiers := auth.BuildVerifierRegistry(snapshot, logger)
		nextRoutes := routing.CollectRoutes(snapshot, verifiers, logger)

		if firstLoad {
			routing.LogInitialLoad(nextRoutes, logger)
			firstLoad = false
		} else {
			delta := routing.ComputeDelta(prevRoutes, nextRoutes)
			routing.LogDelta(delta, nextRoutes, logger)
		}

		// The routing builder pre-resolves every verifier id into the
		// corresponding Route.Auth.VerifierSubject at build time, so
		// the proxy handler never needs live access to the registry —
		// it reads the subject directly off the matched route. The
		// VerifierRegistry itself is short-lived: only the routing
		// builder consumes it, and it is discarded at the end of each
		// rebuild. Future verifier-result caching can reintroduce a
		// long-lived registry handle if id-keyed lookups become
		// necessary at request time.
		current.Store(routing.BuildTableFromRoutes(nextRoutes))
		prevRoutes = nextRoutes
	}

	rebuild()
	watcher.OnChange(rebuild)

	return &current
}

// buildRequesterOrDie constructs the NATS Requester pool. By default
// the pool holds a single connection; increasing the pool size is a
// tuning knob justified only by benchmark evidence of contention on
// the single-socket send path. Raising it speculatively would add
// reconnect complexity and connection-limit pressure on the NATS
// cluster with no demonstrable throughput benefit.
func buildRequesterOrDie(nc *natsgo.Conn, logger zerolog.Logger) *natstransport.Requester {
	requester, err := natstransport.NewRequester([]*natsgo.Conn{nc})
	if err != nil {
		logger.Fatal().Err(err).Msg("nats requester init failed")
	}
	return requester
}

// buildProxyHandler assembles the HTTP->NATS orchestration handler
// with its dependencies. The Table provider closure captures the
// *atomic.Value returned by installRoutingRebuild so every request
// sees the latest routing snapshot without any coordination between
// the request path and the watcher goroutine.
func buildProxyHandler(
	cfg *config.Config,
	currentTable *atomic.Value,
	requester proxy.NatsRequester,
	rlRouter *ratelimit.Router,
	metrics *observability.Metrics,
	logger zerolog.Logger,
) *proxy.Handler {
	return proxy.NewHandler(proxy.HandlerConfig{
		Table: func() routing.Table {
			return currentTable.Load().(routing.Table)
		},
		Nats:             requester,
		Encoder:          proxy.NewDefaultEncoder(),
		Decoder:          proxy.NewDefaultDecoder(),
		Timeout:          cfg.RequestTimeout,
		Logger:           logger,
		RateLimiter:      rlRouter,
		RateLimitTimeout: cfg.RateLimitTimeout,
		Metrics:          metrics,
		AccessLog:        cfg.AccessLogEnabled,
	})
}

// scanNeededBackends returns the distinct set of rate-limit backend
// ids currently referenced by any route in the registry snapshot.
//
// The "memory" backend is always included because it is the router's
// fallback when a route declares no store, an empty string, or an
// unknown backend id — the router invariant "memory is always
// registered" must hold regardless of what routes happen to be in
// the KV bucket when the scan runs.
func scanNeededBackends(store *registry.Store) map[string]struct{} {
	needed := map[string]struct{}{"memory": {}}
	for _, entry := range store.Get().Entries {
		if entry.RateLimit != nil && entry.RateLimit.Store != "" {
			needed[entry.RateLimit.Store] = struct{}{}
		}
	}

	return needed
}

// rateLimitBackendFactory returns a factory for the rate-limit Store
// implementation named by id, or nil when the build has no
// implementation for that id. A nil return is NOT an error: the
// Router's StoreFor falls back to memory at request time with a
// warning log, which is the correct behaviour for an operator who
// references a backend that has not shipped yet (e.g., "redis"
// before a Redis adapter lands).
//
// New backends ship by adding a case here; no other wiring changes
// are required because ensureRateLimitBackends walks every declared
// id on every registry delta.
func rateLimitBackendFactory(
	ctx context.Context,
	id string,
	cfg *config.Config,
	js jetstream.JetStream,
	logger zerolog.Logger,
) func() (ratelimit.Store, error) {
	switch id {
	case "memory":
		return func() (ratelimit.Store, error) {
			return ratelimit.NewMemoryStoreWithCap(
				cfg.RateLimitKeyTTL,
				cfg.RateLimitMemoryMaxEntries,
			), nil
		}
	case "nats-kv":
		return func() (ratelimit.Store, error) {
			return ratelimit.NewNATSKVStore(ctx, ratelimit.NATSKVStoreConfig{
				JS:            js,
				HandlerBucket: cfg.KVBucket,
				BucketSuffix:  "_ratelimit",
				KeyTTL:        cfg.RateLimitKeyTTL,
				Logger:        logger,
			})
		}
	case "redis":
		// Declared by the SDK for forward-compatibility; no Go
		// implementation yet. Router.StoreFor falls back to memory
		// with a warning counter so operators see the gap through
		// metrics instead of a silent downgrade.
		return nil
	default:
		return nil
	}
}

// startRateLimitRetryLoop spawns a background goroutine that
// periodically re-runs ensureRateLimitBackends on a rateLimitRetryInterval
// cadence so a backend that failed to initialise at boot (e.g., the
// NATS-KV bucket was momentarily unreachable during the very first
// EnsureBackend call) recovers without waiting for a watcher delta
// to fire the registry callback.
//
// The retry path is idempotent because Router.EnsureBackend short-
// circuits for already-registered backends — a healthy gateway pays
// only the cost of one map lookup per backend id per tick. The
// goroutine exits when ctx is cancelled, which the main bootstrap
// does immediately before invoking lifecycle.Drain so the goroutine
// cannot race the router's Close path.
func startRateLimitRetryLoop(
	ctx context.Context,
	router *ratelimit.Router,
	store *registry.Store,
	cfg *config.Config,
	js jetstream.JetStream,
	logger zerolog.Logger,
) {
	go func() {
		ticker := time.NewTicker(rateLimitRetryInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ensureRateLimitBackends(ctx, router, store, cfg, js, logger)
			}
		}
	}()
}

// ensureRateLimitBackends registers a Store with the Router for
// every backend id declared by any route in the current registry
// snapshot. Idempotent: EnsureBackend is a no-op for ids that are
// already registered, so the same call is safe to run at startup
// AND on every registry delta. The delta path is how new backends
// become available lazily — when an operator publishes a route
// declaring a previously-unseen backend id, the next watcher
// callback picks it up and instantiates the Store.
//
// Unknown ids (no factory) are skipped silently because the Router
// already logs a fallback warning at request time, and double-logging
// from the startup path would produce operator-confusing duplicates
// without adding signal.
//
// MUST run after watcher.Start has published the initial snapshot,
// otherwise the scan sees an empty map and only the implicit
// "memory" entry is registered.
func ensureRateLimitBackends(
	ctx context.Context,
	router *ratelimit.Router,
	store *registry.Store,
	cfg *config.Config,
	js jetstream.JetStream,
	logger zerolog.Logger,
) {
	for id := range scanNeededBackends(store) {
		factory := rateLimitBackendFactory(ctx, id, cfg, js, logger)
		if factory == nil {
			continue
		}
		if err := router.EnsureBackend(id, factory); err != nil {
			logger.Warn().
				Err(err).
				Str("backend", id).
				Msg("ratelimit backend init failed")
		}
	}
}
