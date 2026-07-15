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
   * When omitted, the gateway answers each preflight with the request's
   * own validated `Access-Control-Request-Method` — the preflight route
   * lookup has already proved that method exists on the path, so the
   * browser always sees the method it asked for. Provide an explicit
   * list only to advertise additional methods in a single preflight
   * answer.
   */
  readonly methods?: readonly string[];

  /**
   * Allowed request headers for preflight.
   * @remarks
   * Default: `['Content-Type', 'Authorization', 'X-Request-Id']`,
   * materialized into the registry entry at metadata-normalization time —
   * the Go side adds no implicit `Access-Control-Allow-Headers` values,
   * so the wire contract stays self-describing. An explicit list
   * replaces the default entirely; an explicit `[]` opts out of
   * `Access-Control-Allow-Headers` altogether.
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
