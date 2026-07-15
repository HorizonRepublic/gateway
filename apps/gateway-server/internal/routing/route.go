// Package routing turns a registry snapshot into an HTTP method+path
// matching table. It is the only layer that understands path templates;
// everything upstream (proxy, transport) consumes Route values by reference.
package routing

import (
	"time"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// Route is the immutable descriptor of a single HTTP endpoint exposed by
// the gateway. It is produced by CollectRoutes from a
// registry.HandlerEntry and consumed by the proxy layer, which uses
// the Subject field to address the downstream NATS RPC and the
// PathTemplate as a cache key for logs and metrics.
//
// The struct is deliberately small and value-typed: routes are stored
// by value inside the indexedTable buckets and copied out on each
// successful Lookup, which keeps the hot path allocation-free.
type Route struct {
	// Subject is the pre-computed NATS RPC subject (e.g.
	// "users-svc__microservice.cmd.users.list"). Pre-computing at build
	// time keeps the per-request hot path free of string concatenation
	// and lookups in the registry.
	Subject string

	// Method is the HTTP verb this route accepts, uppercased to match
	// the incoming request convention used by both Hertz and net/http.
	Method string

	// PathTemplate is the original template string with `:param`
	// placeholders (e.g. "/users/:id"). Retained on the Route so
	// downstream layers can log the matched template rather than the
	// raw request path — the template is a bounded-cardinality label,
	// the raw path is not.
	PathTemplate string

	// Auth is the resolved auth contract populated at build time when
	// the underlying registry entry declared an Auth block AND the
	// referenced verifier existed in the VerifierRegistry. nil means
	// the route is public — no verifier sub-request is issued.
	Auth *RouteAuth

	// CORS policy from the handler registry. Nil means no CORS
	// handling — the request passes through without CORS headers.
	CORS *registry.CORSMeta

	// RateLimit policy from the handler registry. Nil means no
	// rate limiting for this route.
	RateLimit *registry.RateLimitMeta

	// Headers are static response headers from the handler registry.
	// Applied before envelope headers (which take priority).
	Headers map[string]string

	// Timeout overrides the global request timeout for this route.
	// Zero means use the global default.
	Timeout time.Duration
}

// RouteAuth is the pre-resolved auth contract a protected Route
// carries into the proxy handler. VerifierSubject is the full NATS
// subject of the verifier that must be called before forwarding this
// route — resolved once at routing-table build time so the handler
// never has to do its own lookup per request.
//
// Optional mirrors RouteAuthMeta.Optional from the registry layer:
// when true, the handler proceeds with nil claims on a verifier
// 401 reply instead of short-circuiting. 403 and transport errors
// still short-circuit regardless.
type RouteAuth struct {
	VerifierSubject string
	Optional        bool
}
