/**
 * CORS policy for a gateway-exposed endpoint.
 * @remarks
 * Written to the `handler_registry` KV bucket as the `cors` field. The Go
 * gateway reads this and handles `OPTIONS` preflight + response headers
 * without a NATS round-trip.
 */
export interface IGatewayCorsConfig {
  /** Allowed origins. Wildcard `'*'` matches all. */
  readonly origins: readonly string[];

  /**
   * Allowed HTTP methods for preflight.
   * @remarks
   * Default: the route's own method plus all other methods registered on
   * the same path (resolved by the Go side at table build time).
   */
  readonly methods?: readonly string[];

  /**
   * Allowed request headers for preflight.
   * @remarks
   * Default: `['Content-Type', 'Authorization', 'X-Request-Id']`.
   */
  readonly headers?: readonly string[];

  /** Whether the browser should send credentials (cookies, auth headers). */
  readonly credentials?: boolean;

  /** How long (seconds) the browser caches a preflight response. */
  readonly maxAge?: number;

  /**
   * Response headers the browser should expose to cross-origin JavaScript via
   * `Access-Control-Expose-Headers`.
   * @remarks
   * Without this header, browsers hide every non-CORS-safelisted response
   * header from `fetch` / `XMLHttpRequest` callers on cross-origin responses.
   * That means client code cannot read gateway-stamped correlators like
   * `X-Request-Id` or the rate-limit budget (`X-RateLimit-*`) even though
   * they land on the wire.
   *
   * Omit this field to let the gateway emit its standard expose list:
   * `X-Request-Id`, `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
   * `X-RateLimit-Reset`, `Retry-After`. Provide an explicit list to override
   * entirely — per-route values replace the default list, they do not extend
   * it (shallow replace, same contract as the other CORS fields).
   *
   * Mirrors `CORSMeta.ExposeHeaders` on the Go side (wire contract).
   */
  readonly exposeHeaders?: readonly string[];
}
