// Package lifecycle orchestrates the gateway's graceful shutdown
// sequence.
//
// The gateway holds four long-lived resources that each need to be
// quiesced cleanly when the process receives SIGTERM:
//
//  1. The Hertz HTTP server — must stop accepting new connections,
//     then drain in-flight requests.
//  2. The registry watcher — must stop its internal goroutine so it
//     no longer attempts to replace the store snapshot after we have
//     begun shutting down.
//  3. The NATS connection(s) — Drain flushes in-flight subscriptions
//     and publishes before tearing down the socket. nats.go's
//     Conn.Drain only INITIATES that process (it returns immediately
//     while a background goroutine finishes the drain and closes the
//     connection), so the drain step here waits for the connection to
//     report closed — that wait is what gives the gateway its "no
//     request left behind" guarantee during rolling deployments.
//  4. (Implicit) any per-request goroutines spawned by the Hertz
//     handler — these are owned by Hertz and drained by its own
//     Shutdown call.
//
// The shutdown is strictly ordered: HTTP first (stop the source of
// new work), watcher second (so a late KV delta cannot mutate the
// routing table after we have stopped serving), rate-limit router
// third (so in-flight Allow calls observe a clean closed-sentinel
// instead of a raw connection-draining error), NATS last (so any
// in-flight upstream replies — including the router's close-time
// RPCs — have a chance to land before we close the socket). A
// global deadline bounds every step so the gateway cannot hang
// forever on a stuck dependency.
//
// This package depends only on its narrow `HTTPServer`, `NATSConn`,
// `WatcherStopper`, and `RouterCloser` interfaces. Compile-time
// assertions that the concrete `*server.Hertz`, `*nats.Conn`,
// `*registry.Watcher`, and `*ratelimit.Router` types satisfy those
// interfaces land with each consumer's port (so `lifecycle` does
// not pull Hertz / nats.go / registry / ratelimit in just for an
// assertion).
package lifecycle

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// HTTPServer is the narrow contract the Drain routine needs from the
// Hertz server. Declaring it here instead of importing server.Hertz
// directly keeps Drain unit-testable with a fake implementation — the
// concrete Hertz type is only referenced at the Shutdown helper's
// construction site in main.go.
type HTTPServer interface {
	Shutdown(ctx context.Context) error
}

// NATSConn is the narrow contract the Drain routine needs from a
// nats.Conn. Same rationale as HTTPServer: keeps the drain sequence
// testable against a fake.
type NATSConn interface {
	Drain() error
}

// WatcherStopper is the narrow contract the Drain routine needs from
// the registry watcher. *registry.Watcher satisfies it natively;
// extracting an interface lets tests assert step ordering with a
// recording fake.
type WatcherStopper interface {
	Stop()
}

// RouterCloser is the narrow contract the Drain routine needs from
// the rate-limit router. *ratelimit.Router satisfies it natively;
// extracting an interface lets tests assert step ordering with a
// recording fake.
//
// The router is closed AFTER the watcher stops and BEFORE the NATS
// connection drains so two ordering invariants hold simultaneously:
//
//   - In-flight rate-limit checks (kicked off before HTTP.Shutdown
//     started draining) get to observe a clean ratelimit.ErrStoreClosed
//     sentinel via the Router's closed-state machinery instead of a
//     raw "connection draining" error from the underlying NATS-KV
//     store. The handler's FailPolicy then maps the sentinel to a
//     deterministic allow/reject decision.
//   - The NATS connection is still alive while the Router closes its
//     stores, so any close-time RPC the NATS-KV store needs to issue
//     (e.g., final ping or unsubscribe) does not race the connection
//     teardown.
type RouterCloser interface {
	Close() error
}

// Options bundles the resources Drain must quiesce on shutdown.
// Every field is required and a nil value triggers a deliberate
// nil-pointer dereference so bootstrap wiring bugs surface loudly
// instead of silently skipping a drain step. RateLimit is the one
// exception — it may be nil in tests that exercise Drain without a
// Router, and Drain skips the corresponding step in that case.
type Options struct {
	// HTTP is the HTTP server instance whose Shutdown method blocks
	// on in-flight request completion.
	HTTP HTTPServer
	// OperatorHTTP is the operator-only listener (probes, future
	// metrics/admin). Drained LAST so /readyz stays observable for
	// the kubelet during the public drain. Nil disables the step.
	OperatorHTTP HTTPServer
	// Watcher is the registry watcher whose Stop method cancels its
	// background goroutine.
	Watcher WatcherStopper
	// RateLimit is the rate-limit router whose Close method
	// transitions every Store backend into the closed-sentinel state.
	// Nil disables the corresponding drain step — useful for tests,
	// for boot paths that have no rate limiting wired, and for unit
	// fixtures of Drain itself.
	RateLimit RouterCloser
	// NATS is the NATS connection whose Drain method waits for
	// in-flight subscriptions and publishes to finish.
	NATS NATSConn
	// Timeout bounds the entire drain sequence. If a single step
	// exceeds it, the remaining steps still run but with an expired
	// context — implementations that honour ctx.Done() will exit
	// fast, which is the desired behaviour during an oversubscribed
	// shutdown.
	Timeout time.Duration
	// Logger records the start and end of each drain step plus any
	// step-level errors. Errors never fail the overall sequence —
	// the gateway always attempts every step so a failed HTTP
	// Shutdown does not leave the NATS connection leaking.
	Logger zerolog.Logger
}

// DefaultSignals is the set of OS signals the gateway treats as a
// shutdown request. Extracted so tests and alternative entry points
// can share the same list.
var DefaultSignals = []os.Signal{syscall.SIGTERM, syscall.SIGINT}

// WaitForSignal registers a buffered channel for DefaultSignals,
// blocks until one arrives, and returns the signal that fired. The
// buffer size is 1 because the kernel delivers at most one signal
// per registered name before the handler runs — anything more is
// a symptom of a broken runtime and losing duplicates is acceptable.
//
// Callers that need testability should not use WaitForSignal; they
// should construct their own channel and pass it to Drain directly
// so the test can push a synthetic signal without reaching into os
// package state.
func WaitForSignal() os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, DefaultSignals...)
	defer signal.Stop(ch)
	return <-ch
}

// Drain runs the ordered shutdown sequence against opts and returns
// once every step has completed or the global Timeout has elapsed.
//
// Step order:
//
//  1. HTTP server shutdown — stop accepting new connections and drain
//     in-flight requests so no new rate-limit / NATS work originates
//     after this point.
//  2. Registry watcher stop — the routing table snapshot is now
//     immutable; a late KV delta cannot mutate it after we stopped
//     serving.
//  3. Rate-limit router close — turns every Store backend into the
//     closed-sentinel store so any in-flight rate-limit check kicked
//     off during HTTP shutdown sees ratelimit.ErrStoreClosed instead
//     of a raw "connection draining" error from the NATS-KV store.
//     Skipped when opts.RateLimit is nil.
//  4. NATS connection drain — waits for in-flight subscriptions and
//     publishes to finish before closing the socket. Runs LAST so
//     the rate-limit router's close-time RPC traffic (if any) lands
//     before the connection goes away.
//
// Errors from individual steps are logged but do not abort the
// sequence — a failed HTTP Shutdown must NOT prevent the NATS
// connection from being drained, because the process is about to
// exit and the cleanest finalization we can offer the operator is
// to attempt every drain unconditionally.
func Drain(opts Options) {
	opts.Logger.Info().Dur("timeout", opts.Timeout).Msg("gateway shutdown: draining resources")
	overallStart := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer cancel()

	shutdownHTTP(ctx, opts)
	stopWatcher(opts)
	closeRateLimitRouter(opts)
	drainNATS(ctx, opts)
	shutdownOperatorHTTP(ctx, opts)

	opts.Logger.Info().
		Dur("elapsed", time.Since(overallStart)).
		Msg("gateway shutdown: drain complete")
}

// shutdownHTTP stops the Hertz server from accepting new connections
// and waits for in-flight requests to finish. Errors are logged at
// ERROR but do not abort the rest of the drain. Per-step elapsed
// time is logged at INFO so a slow-drain postmortem can pinpoint
// which resource burned the budget without correlating timestamps.
func shutdownHTTP(ctx context.Context, opts Options) {
	opts.Logger.Debug().Msg("shutdown step: http")
	start := time.Now()
	if err := opts.HTTP.Shutdown(ctx); err != nil {
		opts.Logger.Error().
			Err(err).
			Dur("elapsed", time.Since(start)).
			Msg("http shutdown failed; continuing drain")
		return
	}
	opts.Logger.Info().
		Dur("elapsed", time.Since(start)).
		Msg("shutdown step: http complete")
}

// shutdownOperatorHTTP drains the operator-only listener. Runs as
// the FINAL step so health probes answer truthfully for as long as
// the process holds any other resource — a kubelet that loses
// /readyz mid-drain would mark the pod failed while in-flight
// requests are still completing. Nil-safe.
func shutdownOperatorHTTP(ctx context.Context, opts Options) {
	if opts.OperatorHTTP == nil {
		return
	}
	opts.Logger.Debug().Msg("shutdown step: operator http")
	start := time.Now()
	if err := opts.OperatorHTTP.Shutdown(ctx); err != nil {
		opts.Logger.Error().
			Err(err).
			Dur("elapsed", time.Since(start)).
			Msg("operator http shutdown failed")
		return
	}
	opts.Logger.Info().
		Dur("elapsed", time.Since(start)).
		Msg("shutdown step: operator http complete")
}

// stopWatcher cancels the registry watcher's background goroutine.
// Stop is idempotent (guarded by sync.Once in the watcher) and
// cannot fail, so there is nothing to log on the error branch.
// Elapsed duration is logged at INFO so operators can spot a stuck
// initial-load that prevents Stop from returning quickly.
func stopWatcher(opts Options) {
	opts.Logger.Debug().Msg("shutdown step: registry watcher")
	start := time.Now()
	opts.Watcher.Stop()
	opts.Logger.Info().
		Dur("elapsed", time.Since(start)).
		Msg("shutdown step: registry watcher complete")
}

// closeRateLimitRouter transitions the rate-limit router into its
// terminal closed state so in-flight Store.Allow calls observe a
// well-defined ratelimit.ErrStoreClosed sentinel instead of a raw
// "connection draining" error from the underlying NATS-KV store.
// The handler's FailPolicy then maps the sentinel to a deterministic
// allow/reject decision.
//
// Skipped when opts.RateLimit is nil — that mode exists for tests
// and for boot paths that wire no rate-limiting at all.
//
// Errors are logged at ERROR but do not abort the rest of the drain.
// A failing Close must NOT prevent the NATS drain from running,
// because the process is about to exit and leaving the NATS socket
// open is strictly worse than leaking a half-closed Store.
func closeRateLimitRouter(opts Options) {
	if opts.RateLimit == nil {
		return
	}

	opts.Logger.Debug().Msg("shutdown step: ratelimit router")
	start := time.Now()
	if err := opts.RateLimit.Close(); err != nil {
		opts.Logger.Error().
			Err(err).
			Dur("elapsed", time.Since(start)).
			Msg("ratelimit router close failed; continuing drain")
		return
	}
	opts.Logger.Info().
		Dur("elapsed", time.Since(start)).
		Msg("shutdown step: ratelimit router complete")
}

// drainNATS waits for in-flight subscriptions and publishes on the
// NATS connection to finish before tearing down the socket. Errors
// are logged but do not abort the shutdown.
//
// Drain runs on a worker goroutine so a hung NATS connection cannot
// block the gateway past the configured shutdown timeout. The
// deadline is the SHARED ctx from Drain — not a fresh timer — so
// the NATS step cannot escape the global shutdown budget. If HTTP
// shutdown already consumed most of it, NATS gets only what remains;
// on timeout the lifecycle layer logs at WARN (timeout is a
// lifecycle signal, not a defect) and yields control to the process
// exit path. The orphan goroutine completes whenever the underlying
// socket finally drops.
func drainNATS(ctx context.Context, opts Options) {
	opts.Logger.Debug().Msg("shutdown step: nats drain")
	start := time.Now()

	done := make(chan error, 1)
	go func() {
		done <- opts.NATS.Drain()
	}()

	select {
	case err := <-done:
		if err != nil {
			opts.Logger.Error().
				Err(err).
				Dur("elapsed", time.Since(start)).
				Msg("nats drain failed")
			return
		}
		opts.Logger.Info().
			Dur("elapsed", time.Since(start)).
			Msg("shutdown step: nats drain complete")
	case <-ctx.Done():
		opts.Logger.Warn().
			Err(ctx.Err()).
			Dur("elapsed", time.Since(start)).
			Msg("nats drain timed out; forcing shutdown")
	}
}
