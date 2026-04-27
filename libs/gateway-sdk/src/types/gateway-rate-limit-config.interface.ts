/**
 * Key source for rate-limit bucket resolution.
 * @remarks
 * The Go gateway walks the `keyBy` array in order and uses the first value
 * that resolves. Implicit fallback to client IP if nothing in the chain
 * matches.
 *
 * - `'ip'` — trusted-proxy-resolved client IP (always resolves).
 * - `'header:<name>'` — request header value.
 * - `'cookie:<name>'` — request cookie value.
 * - `'user:<field>'` — field from auth claims (requires auth to have succeeded).
 */
export type RateLimitKey = 'ip' | `header:${string}` | `cookie:${string}` | `user:${string}`;

/**
 * Selects the rate-limit backend for a route.
 * @remarks
 * - `'memory'` (default): in-process GCRA rate limiter. Zero latency, zero
 *   infrastructure. Each gateway replica tracks its own counters →
 *   multi-replica deployments effectively allow N× the configured rate.
 *   Appropriate for single-instance deployments or hot-path routes (health
 *   checks) where network-store latency is unacceptable.
 *
 * - `'nats-kv'`: distributed GCRA state in a NATS JetStream KV bucket. All
 *   gateway replicas share counters → correct rate enforcement regardless of
 *   replica count. Reuses the existing NATS cluster (zero extra
 *   infrastructure). Documented ceiling: ~5k req/s across all rate-limited
 *   routes.
 *
 * - `'redis'`: declared in the SDK contract; not yet implemented in the Go
 *   gateway. Using `'redis'` today logs a warning on startup and falls back
 *   to `'memory'` for the affected route. Full Redis support is a planned
 *   future addition.
 *
 * Store selection is applied per-route. A gateway process can serve
 * `'memory'`-backed and `'nats-kv'`-backed routes simultaneously — each
 * route's `store` field independently selects its backend.
 * @default `'memory'`
 */
export type RateLimitStore = 'memory' | 'nats-kv' | 'redis';

/**
 * Per-route rate limiting policy.
 * @remarks
 * Written to the `handler_registry` KV bucket as the `rateLimit` field. The
 * Go gateway enforces via a token-bucket algorithm.
 *
 * Field-level invariants are checked at module init / decoration time by
 * `assertRateLimitConfig` — `rps: 0` and negative `burst` are rejected with
 * a descriptive error rather than silently degrading to "no limit" or
 * producing undefined GCRA behaviour. Operators who want a route to be
 * unlimited MUST omit the `rateLimit` block entirely.
 *
 * The Go gateway clamps `Retry-After` responses to a minimum of 1 second
 * even when the computed wait is sub-second. A `Retry-After: 0` is
 * misleading to many client libraries (often treated as "retry immediately
 * and hammer the server"), so the gateway will never emit `Retry-After: 0`.
 * Client retry logic that needs sub-second granularity should rely on the
 * bucket-aware `X-RateLimit-Reset` instead.
 */
export interface IGatewayRateLimitConfig {
  /**
   * Maximum sustained requests per second.
   * @remarks
   * Must be a positive integer in `[1, 2^32 - 1]`. `rps: 0` is rejected at
   * registration time — the Go gateway treats `RPS <= 0` as "no limit" and a
   * developer who wrote `rps: 0` almost certainly meant the opposite. Omit
   * the entire `rateLimit` block to express "no limit" instead.
   */
  readonly rps: number;

  /**
   * Token bucket burst size — how many requests are allowed in a short spike
   * before the sustained rate kicks in. Default: `rps * 2`.
   * @remarks
   * Must be a non-negative integer in `[0, 2^32 - 1]` when provided. Negative
   * values are rejected at registration time because the Go-side GCRA
   * divisor would wrap on the hot path.
   */
  readonly burst?: number;

  /**
   * Priority chain for resolving the rate-limit bucket key. Go walks the
   * array top-to-bottom; first value that resolves wins. If nothing
   * resolves, falls back to client IP. Default: `['ip']`.
   */
  readonly keyBy?: readonly RateLimitKey[];

  /**
   * Backend store selector. See {@link RateLimitStore} for the semantics of
   * each value and the per-route coexistence model.
   */
  readonly store?: RateLimitStore;
}
