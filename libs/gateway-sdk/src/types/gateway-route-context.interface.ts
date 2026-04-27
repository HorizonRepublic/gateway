import type { HttpMethod } from './http-method.type';

/**
 * Routing context attached to every `IGatewayRequest` — tells the handler
 * which HTTP route matched this invocation.
 * @remarks
 * Populated by `gateway-server` after the routing trie resolves the
 * incoming request. Handlers can use this to distinguish which registered
 * route fired when the same implementation is bound to multiple paths, and
 * to reconstruct canonical log lines that reference the template rather
 * than the concrete URL.
 */
export interface IGatewayRouteContext {
  /** HTTP method that matched this invocation. */
  readonly method: HttpMethod;

  /**
   * Original registered path template, e.g. `/users/:id`.
   * @remarks
   * The exact string passed to `@GatewayRoute({ path })`. Stable across
   * requests, safe to use as a metric label (low cardinality).
   */
  readonly path: string;

  /**
   * Concrete request URL path, e.g. `/users/abc-123`.
   * @remarks
   * High-cardinality value — do not use as a metric label. Prefer `path`
   * (the template) for aggregation and reserve `matchedPath` for audit
   * logs and request tracing.
   */
  readonly matchedPath: string;
}
