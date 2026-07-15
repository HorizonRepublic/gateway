import type { IGatewayCorsConfig } from '../types/gateway-cors-config.interface';

/**
 * Request headers a CORS route allows in preflight when the config omits
 * `headers`. Covers the two headers virtually every gateway route needs —
 * `Content-Type` (the gateway accepts JSON bodies only, and
 * `application/json` is not CORS-safelisted) and `Authorization` (bearer
 * auth) — plus the `X-Request-Id` correlator clients may echo back.
 * @remarks
 * Wire contract: the default is materialized into the registry entry by
 * `withDefaultCorsHeaders` at metadata-normalization time, so the Go side
 * never has to invent implicit `Access-Control-Allow-Headers` values — an
 * explicit registry entry beats implicit server behavior. Spread this
 * list to extend rather than replace it:
 * `headers: [...DEFAULT_CORS_ALLOWED_HEADERS, 'X-Custom']`.
 */
export const DEFAULT_CORS_ALLOWED_HEADERS: readonly string[] = [
  'Content-Type',
  'Authorization',
  'X-Request-Id',
];

/**
 * Materializes the documented `headers` default into a CORS config that
 * omits the field, so the registry entry always carries an explicit
 * allow-list.
 * @param cors - The resolved CORS config for a route (per-route block or
 *               module-level default, whichever won the merge).
 * @returns The same object when `headers` is present (including an
 *          explicit `[]`, which opts out of `Access-Control-Allow-Headers`
 *          entirely); otherwise a copy with
 *          {@link DEFAULT_CORS_ALLOWED_HEADERS} filled in.
 * @remarks
 * Pure function, called from the route-metadata merge path — the single
 * seam every KV-bound CORS block flows through.
 */
export const withDefaultCorsHeaders = (cors: IGatewayCorsConfig): IGatewayCorsConfig =>
  cors.headers === undefined ? { ...cors, headers: DEFAULT_CORS_ALLOWED_HEADERS } : cors;
