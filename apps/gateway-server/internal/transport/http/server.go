package http

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	hertzconfig "github.com/cloudwego/hertz/pkg/common/config"
	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
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

// buildPublicTLSConfig loads the operator-supplied certificate/key
// pair and returns a TLS config pinned to TLS 1.2+ for the public
// listener. A load failure (missing file, mismatched pair, malformed
// PEM) is returned to the caller, which fails startup closed — a
// gateway configured for TLS that cannot present a certificate must
// not fall back to plaintext.
func buildPublicTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair (cert=%q key=%q): %w", certFile, keyFile, err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
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

	// server.New instead of server.Default: Default's only addition is
	// the stock recovery middleware (hlog plain-text logging, empty 500
	// body), which the gateway replaces with its own zerolog-integrated
	// recovery below. Registering the custom recovery on top of Default
	// would leave dead middleware in the chain — the inner handler
	// would win every panic and the stock one would never fire.
	// WithSenseClientDisconnection makes Hertz cancel the
	// per-connection context (the stdCtx forwarded to the proxy
	// handler by the adapter) when the client's TCP connection drops.
	// The option defaults to false, in which case an abandoned request
	// keeps its NATS round trip alive for the full per-route timeout —
	// pinning one NATS in-flight semaphore slot and one HTTP
	// concurrency slot per fire-and-disconnect request, capacity the
	// admission layer can never shed early. Enabling it is what makes
	// the requester's no-orphan-IO contract hold end-to-end. The
	// cancellation is wired inside Hertz's transports (netpoll — the
	// default on Linux/macOS — registers an OnDisconnect callback that
	// cancels the connection ctx; the standard transport polls the
	// socket); a future transport override must re-verify the option
	// still applies.
	opts := []hertzconfig.Option{
		server.WithHostPorts(cfg.HTTPAddr),
		server.WithMaxRequestBodySize(maxBody),
		server.WithMaxHeaderBytes(cfg.MaxHeaderBytes),
		server.WithReadTimeout(cfg.ReadTimeout),
		server.WithWriteTimeout(cfg.WriteTimeout),
		server.WithIdleTimeout(cfg.IdleTimeout),
		server.WithExitWaitTime(cfg.ShutdownTimeout),
		server.WithKeepAlive(true),
		server.WithSenseClientDisconnection(true),
		withNoDefaultServerHeader(),
	}

	if cfg.PublicTLSEnabled() {
		tlsCfg, err := buildPublicTLSConfig(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("http server: %w", err)
		}
		// server.WithTLS switches Hertz off its netpoll transport onto
		// the standard Go-net transport (netpoll does not support TLS),
		// a measurable hot-path change at high RPS. Log it loud so the
		// operator knows the throughput profile shifted and can decide
		// whether mesh/LB termination is the better fit.
		logger.Warn().
			Msg("TLS enabled on the public listener: Hertz falls back from netpoll to the standard transport (netpoll has no TLS); prefer terminating TLS at the mesh/LB for maximum throughput")
		opts = append(opts, server.WithTLS(tlsCfg))
	}

	h := server.New(opts...)

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
	// Recovery registers before everything else so its deferred
	// recover() encloses the limiter, the trusted-proxy middleware,
	// and the adapter — a panic anywhere in the chain produces one
	// structured log event and the shared 500 JSON body instead of a
	// torn connection.
	h.Use(newRecoveryMiddleware(logger))
	limiter := newConcurrencyLimitMiddleware(cfg.HTTPMaxConcurrentRequests)
	h.Use(limiter.handler)
	h.Use(newTrustedProxyMiddleware(cfg.TrustedProxies, cfg.TrustedProxyHeader))

	// IP filter runs directly after trust resolution so it sees the
	// genuine client, and only when a policy is configured so the
	// default hot path carries no extra middleware.
	if len(cfg.IPAllowList) > 0 || len(cfg.IPDenyList) > 0 {
		h.Use(newIPFilterMiddleware(cfg.IPAllowList, cfg.IPDenyList))
	}

	h.Any("/*path", NewHertzAdapter(handler))

	return h, nil
}

// operatorReadHeaderTimeout bounds how long the operator listener
// waits for request headers. The listener is reachable from the pod
// network, so a slowloris-shaped defence is still warranted even
// though clients are supposed to be the kubelet and Prometheus.
const operatorReadHeaderTimeout = 10 * time.Second

// OperatorServer is the operator-only listener: the surfaces that
// belong to the platform operator, never to public clients — health
// probes, the Prometheus scrape endpoint, and pprof. Binding them to
// a separate port (OPERATOR_HTTP_ADDR, default :8081) keeps the
// trust boundary structural: in Kubernetes the operator port is
// reachable by the kubelet and the pod network but is simply never
// exposed through the public Service/Ingress, so every operator
// endpoint is private BY DEFAULT instead of "private if someone
// remembers to mount auth".
//
// The listener is plain net/http rather than Hertz: both promhttp and
// net/http/pprof are stdlib http.Handler surfaces, the traffic is a
// handful of requests per second at most, and skipping the framework
// adaptor keeps the debug path independent of the machinery it is
// meant to debug — a Hertz-level incident must not take the pprof
// endpoint down with it.
type OperatorServer struct {
	srv *http.Server
	// boundAddr publishes the listener's actual address once Run has
	// bound the socket. Lets tests (and future tooling) target an
	// ephemeral ":0" port deterministically instead of racing a
	// pre-picked free port.
	boundAddr atomic.Value
}

// Compile-time proof the operator server satisfies the lifecycle
// drain contract alongside the Hertz public server.
var _ interface {
	Shutdown(ctx context.Context) error
} = (*OperatorServer)(nil)

// NewOperatorServer constructs the operator listener serving:
//
//   - /healthz          — K8s liveness probe (unconditional 200).
//   - /readyz           — K8s readiness probe via ReadinessSignal.
//   - /metrics          — Prometheus scrape handler (when non-nil).
//   - /debug/pprof/...  — net/http/pprof profiling endpoints.
//
// pprof handlers are registered on this private mux explicitly —
// NEVER on the public listener. The net/http/pprof import also
// self-registers on http.DefaultServeMux as an import side effect,
// but the gateway never serves DefaultServeMux, so that registration
// is inert.
//
// The mux is deliberately minimal: no trusted-proxy middleware
// (probes come from the kubelet, not through the proxy chain), no
// concurrency limiter (a saturated gateway must still answer probes,
// or K8s restarts it mid-incident), no body guards (operator
// requests carry no payload). No write timeout is set because CPU
// and trace profiles stream for a caller-chosen number of seconds;
// the header-read timeout above still bounds slow-header abuse.
//
// Like NewServer, the returned server is NOT started — call Run.
func NewOperatorServer(
	cfg *config.Config,
	readiness ReadinessSignal,
	metrics http.Handler,
	routeProvider func() routing.Table,
) *OperatorServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", liveHandler())
	mux.HandleFunc("/readyz", readyHandler(readiness))

	if metrics != nil {
		mux.Handle("/metrics", metrics)
	}

	if routeProvider != nil {
		mux.HandleFunc("/admin/routes", newRouteDumpHandler(routeProvider))
	}

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return &OperatorServer{
		srv: &http.Server{
			Addr:              cfg.OperatorHTTPAddr,
			Handler:           mux,
			ReadHeaderTimeout: operatorReadHeaderTimeout,
		},
	}
}

// Run binds the configured address and serves until Shutdown is
// called. A clean Shutdown-driven exit returns nil so the bootstrap's
// "server exited unexpectedly" log fires only for genuine failures.
func (s *OperatorServer) Run() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("operator listener bind %s: %w", s.srv.Addr, err)
	}
	s.boundAddr.Store(ln.Addr().String())

	if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("operator listener serve: %w", err)
	}

	return nil
}

// Addr returns the listener's bound address once Run has taken the
// socket, or "" before that. Intended for tests that bind ":0".
func (s *OperatorServer) Addr() string {
	if v, ok := s.boundAddr.Load().(string); ok {
		return v
	}

	return ""
}

// Shutdown gracefully drains the operator listener. Satisfies the
// lifecycle package's HTTPServer contract; the drain sequence runs it
// LAST so /readyz stays observable for the kubelet during the public
// drain.
func (s *OperatorServer) Shutdown(ctx context.Context) error {
	if err := s.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("operator listener shutdown: %w", err)
	}

	return nil
}

// liveHandler answers the K8s liveness probe. Returns 200 OK as long
// as the process is running and the goroutine scheduler can dispatch
// the request. Per K8s semantics, a liveness failure causes pod
// restart — the gateway has no condition under which "process up but
// dead" is meaningful, so the probe is unconditional.
func liveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
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
func readyHandler(signal ReadinessSignal) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if signal != nil && !signal.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
