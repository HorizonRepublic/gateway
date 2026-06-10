package http

import (
	"context"
	"fmt"
	"math"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	hertzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
)

// ReadinessSignal reports whether the gateway is ready to serve
// traffic. Implementations are typically backed by an atomic.Bool
// flipped by the bootstrap once both NATS is connected AND the
// initial routing-table snapshot has landed. A K8s readiness probe
// hitting /readyz before the table loads would otherwise be routed
// to the catch-all and 404, which Kubernetes interprets as healthy
// (the probe checks status code, not semantics) — the explicit
// readiness check separates "process up" from "process ready to
// accept production traffic".
type ReadinessSignal interface {
	Ready() bool
}

// ReadinessFunc adapts a plain func() bool to ReadinessSignal so
// callers can wire an atomic.Bool's Load method directly without
// introducing a wrapper type.
type ReadinessFunc func() bool

// Ready satisfies ReadinessSignal.
func (f ReadinessFunc) Ready() bool { return f() }

// resolveMaxBodyBytes converts the operator-supplied
// HTTP_MAX_BODY_BYTES (a signed 64-bit value) into the int Hertz
// expects for its WithMaxRequestBodySize option.
//
// Negative inputs are a deliberate misconfiguration — there is no
// sensible interpretation of a "negative-byte" body cap, and silently
// treating it as zero would block every request. Returning an error
// fails startup loud so operators correct the value before traffic
// hits the pod.
//
// Inputs above math.MaxInt32 are clamped to math.MaxInt32 with a WARN
// log. On 32-bit platforms a plain int cast would overflow and Hertz
// would silently apply a tiny limit; clamping preserves the operator's
// intent ("accept very large bodies") without losing data integrity.
func resolveMaxBodyBytes(v int64, logger zerolog.Logger) (int, error) {
	if v < 0 {
		return 0, fmt.Errorf("HTTP_MAX_BODY_BYTES must be non-negative, got %d", v)
	}

	if v > math.MaxInt32 {
		logger.Warn().
			Int64("requested", v).
			Int64("clamped", int64(math.MaxInt32)).
			Msg("HTTP_MAX_BODY_BYTES exceeds int32 range; clamping to MaxInt32")

		return math.MaxInt32, nil
	}

	return int(v), nil
}

// withNoDefaultServerHeader disables Hertz's automatic
// `Server: hertz` response header. Hertz's public option API
// does not expose a helper for this flag even though the
// underlying `config.Options.NoDefaultServerHeader` field is
// public, so we build the option in-line against the documented
// struct shape (`config.Option` is a tiny `{F func(*Options)}`
// wrapper). Keeping the server name off the wire is both a
// fingerprinting-surface reduction and a consistency choice —
// the gateway should not leak its transport implementation into
// every response.
func withNoDefaultServerHeader() hertzconfig.Option {
	return hertzconfig.Option{
		F: func(o *hertzconfig.Options) {
			o.NoDefaultServerHeader = true
		},
	}
}

// NewServer constructs a Hertz server bound to handler via a single
// catch-all route "/*path". Dynamic HTTP-to-RPC routing is performed
// INSIDE proxy.Handler via the routing.Table, so Hertz itself sees
// only one registered route and incurs no per-request routing cost.
//
// The returned *server.Hertz is NOT started — callers are expected
// to call h.Run() in a goroutine once all dependencies have been
// assembled, and then block on the lifecycle package for signal-
// driven shutdown. Returning the unstarted server lets tests
// construct it against an ephemeral port without touching the
// network.
//
// ExitWaitTimeout is aligned with cfg.ShutdownTimeout so there is a
// single operator-facing knob ("SHUTDOWN_TIMEOUT") for the total
// drain budget. Hertz internally bounds its Shutdown context by
// ExitWaitTimeout — without this override it would cap at Hertz's
// default 5s and the lifecycle package's longer budget would be
// silently ignored.
//
// The server is HTTP/1.1 with keep-alive. HTTP/2 (h2c) is not wired
// today because Hertz h2c requires additional imports that have not
// been brought in. Until that lands, the config surface carries no
// HTTP/2 knob — accepting an operator toggle that has no effect is
// worse than requiring a code change to enable it.
//
// Returns an error when cfg.MaxBodyBytes is negative — see
// resolveMaxBodyBytes for the rationale. Callers MUST treat the
// error as fatal because partial server construction would race the
// rest of the bootstrap.
func NewServer(
	cfg *config.Config,
	handler *proxy.Handler,
	readiness ReadinessSignal,
	logger zerolog.Logger,
) (*server.Hertz, error) {
	maxBody, err := resolveMaxBodyBytes(cfg.MaxBodyBytes, logger)
	if err != nil {
		return nil, fmt.Errorf("http server: %w", err)
	}

	h := server.Default(
		server.WithHostPorts(cfg.HTTPAddr),
		server.WithMaxRequestBodySize(maxBody),
		server.WithMaxHeaderBytes(cfg.MaxHeaderBytes),
		server.WithReadTimeout(cfg.ReadTimeout),
		server.WithWriteTimeout(cfg.WriteTimeout),
		server.WithIdleTimeout(cfg.IdleTimeout),
		server.WithExitWaitTime(cfg.ShutdownTimeout),
		server.WithKeepAlive(true),
		withNoDefaultServerHeader(),
	)

	// Health endpoints deliberately do NOT register here: probes,
	// metrics, and every future admin/debug surface live on the
	// operator listener (NewOperatorServer, OPERATOR_HTTP_ADDR).
	// Sharing a socket with public client traffic would blur the
	// trust boundary — kubelet probes would traverse the same
	// parser, middleware, and connection budget as untrusted
	// clients, and any future operator endpoint would inherit
	// public exposure unless it individually mounted auth.

	// Concurrency limit runs FIRST in the middleware chain so a
	// saturated semaphore short-circuits before the trusted-proxy
	// middleware allocates header parsing or the rate-limit gate
	// touches the store. Probe traffic above is registered
	// upstream of Use() so health checks bypass the cap — a
	// saturated gateway must still report ready/live so K8s does not
	// restart it during the very incident the cap is defending against.
	limiter := newConcurrencyLimitMiddleware(cfg.HTTPMaxConcurrentRequests)
	h.Use(limiter.handler)
	h.Use(newTrustedProxyMiddleware(cfg.TrustedProxies, cfg.TrustedProxyHeader))
	h.Any("/*path", NewHertzAdapter(handler))

	return h, nil
}

// NewOperatorServer constructs the operator-only Hertz engine: the
// listener for surfaces that belong to the platform operator, never
// to public clients — health probes today; metrics scrape, pprof,
// admin endpoints (e.g. the dynamic IP blocklist) tomorrow. Binding
// them to a separate port (OPERATOR_HTTP_ADDR, default :8081) keeps
// the trust boundary structural: in Kubernetes the operator port is
// reachable by the kubelet and the pod network but is simply never
// exposed through the public Service/Ingress, so a future operator
// endpoint is private BY DEFAULT instead of "private if someone
// remembers to mount auth".
//
// The engine is deliberately minimal: no trusted-proxy middleware
// (probes come from the kubelet, not through the proxy chain), no
// concurrency limiter (a saturated gateway must still answer probes,
// or K8s restarts it mid-incident), no body/header guards (probe
// requests carry no payload). ExitWaitTime mirrors the public server
// so one SHUTDOWN_TIMEOUT knob governs both drains.
//
// Like NewServer, the returned engine is NOT started.
func NewOperatorServer(
	cfg *config.Config,
	readiness ReadinessSignal,
) *server.Hertz {
	h := server.Default(
		server.WithHostPorts(cfg.OperatorHTTPAddr),
		server.WithExitWaitTime(cfg.ShutdownTimeout),
		server.WithKeepAlive(true),
		withNoDefaultServerHeader(),
	)

	h.GET("/healthz", liveHandler())
	h.GET("/readyz", readyHandler(readiness))

	return h
}

// liveHandler answers the K8s liveness probe. Returns 200 OK as long
// as the process is running and the goroutine scheduler can dispatch
// the request. Per K8s semantics, a liveness failure causes pod
// restart — the gateway has no condition under which "process up but
// dead" is meaningful, so the probe is unconditional.
func liveHandler() app.HandlerFunc {
	return func(_ context.Context, ctx *app.RequestContext) {
		ctx.SetStatusCode(consts.StatusOK)
	}
}

// readyHandler answers the K8s readiness probe. Returns 200 OK only
// when the supplied ReadinessSignal reports ready; otherwise 503.
// During bootstrap the signal is false until both the NATS connection
// is established AND the first routing-table snapshot has landed.
// Marking the pod unready during this window blocks the load balancer
// from routing traffic to a process that would only return 404 — the
// distinction K8s cannot make on its own.
//
// A nil ReadinessSignal degrades to "always ready"; tests that build
// the server without a real signal still get a working endpoint.
func readyHandler(signal ReadinessSignal) app.HandlerFunc {
	return func(_ context.Context, ctx *app.RequestContext) {
		if signal != nil && !signal.Ready() {
			ctx.SetStatusCode(consts.StatusServiceUnavailable)
			return
		}
		ctx.SetStatusCode(consts.StatusOK)
	}
}
