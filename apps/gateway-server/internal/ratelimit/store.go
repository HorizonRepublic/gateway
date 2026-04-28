// Package ratelimit provides a rate-limiting Store interface and
// GCRA-based backend implementations.
package ratelimit

import "context"

// Store is the rate-limit backend contract. Implementations MUST be
// goroutine-safe — the proxy handler calls Allow from every
// inbound HTTP request without external locking.
//
// See Router for per-route dispatch over multiple Stores coexisting
// in the gateway process.
type Store interface {
	// Allow checks whether the request identified by key is within
	// the rate and burst constraints. The returned Decision always
	// carries Remaining / RetryAfter / ResetAt so callers can
	// populate X-RateLimit-* response headers uniformly, whether
	// the result is allow or reject.
	//
	// A non-nil error signals backend failure (network, circuit
	// open, budget exhausted). The Decision is NOT meaningful in
	// that case — the caller MUST consult FailPolicy for
	// allow vs reject.
	//
	// ctx bounds how long Allow may block; implementations SHOULD
	// respect ctx.Done() in any retry loop and cancel backend I/O.
	Allow(ctx context.Context, key string, rps, burst int) (Decision, error)

	// FlushPrefix removes all buckets whose key starts with prefix.
	// Intended for administrative reset paths (e.g., operator CLI).
	// Hot-reload of a route's rate-limit config does NOT flush —
	// flushing on config change would let a client reset their
	// bucket by provoking any config update, defeating the limit.
	FlushPrefix(ctx context.Context, prefix string) error

	// Close releases resources. MUST be idempotent.
	Close() error

	// Counters returns a point-in-time snapshot of internal metric
	// counters for OpenTelemetry / Prometheus export. Keys are stable
	// metric names — dashboards rely on the schema staying constant
	// across gateway restarts and across backend swaps.
	//
	// Naming convention: monotonic counter keys MUST end in the
	// `_total` suffix (Prometheus / OpenMetrics / OTel semantic
	// convention). Gauge keys end in a unit suffix (`_seconds`,
	// `_bytes`) or a state name (e.g. `_state`). Counters that ship
	// without `_total` now would force a breaking rename once the
	// OTel exporter lands, invalidating every dashboard and alert
	// built on top of them.
	//
	// Implementations MUST include at minimum:
	//   - ratelimit_<backend>_decisions_allowed_total
	//   - ratelimit_<backend>_decisions_rejected_total
	//   - ratelimit_<backend>_backend_errors_total
	// where <backend> is the registered backend id (memory, natskv,
	// ...). Backends with no concept of remote failure (e.g. memory)
	// MUST still surface backend_errors_total with value 0 so a
	// dashboard graphing the metric across backends does not go dark
	// on a memory-only deployment. Additional, backend-specific keys
	// are allowed and use the same prefixed shape.
	//
	// Returned maps are safe to mutate; each call allocates a fresh
	// snapshot.
	Counters() map[string]int64
}
