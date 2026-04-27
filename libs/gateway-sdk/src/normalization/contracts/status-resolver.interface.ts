import type { IGatewayHttpMeta } from '../../types/gateway-http-meta.interface';

/**
 * Contract for resolving the HTTP status code of a successful handler
 * return.
 * @remarks
 * The default implementation (`DefaultStatusResolver`) applies the standard
 * rules, in precedence order:
 *
 *   1. Explicit `statusCode` from `@GatewayRoute` options
 *      (`IGatewayHttpMeta.statusCode`).
 *   2. `DEFAULT_STATUS_NO_CONTENT` (204) for `null` / `undefined` returns.
 *   3. `DEFAULT_STATUS_OK` (200) otherwise.
 *
 * Custom implementations may layer domain-specific rules on top — for
 * example "all handlers whose pattern ends in `.create` default to 201", or
 * "handlers tagged with a custom metadata marker default to 202". Override
 * by binding a custom class to the `GATEWAY_STATUS_RESOLVER` token.
 *
 * This contract is deliberately one method only: single responsibility
 * means easier testing, simpler substitution, and no temptation to leak
 * unrelated concerns (body shaping, error handling, logging, header
 * injection) into the resolver. Anything that is not "pick a status code"
 * belongs in a different seam.
 */
export interface IStatusResolver {
  /**
   * Decide which HTTP status to return for a successful handler invocation.
   * @param httpMeta - HTTP metadata from the handler's `@GatewayRoute`
   *                   decorator, read from `PATTERN_EXTRAS_METADATA` at
   *                   interceptor time.
   * @param returnValue - Raw value returned by the handler method, before
   *                      envelope wrapping. Typed as `unknown` so
   *                      implementations can inspect it (e.g., for the
   *                      `null` / `undefined` → 204 rule) without forcing a
   *                      cast at the call site.
   */
  resolveSuccess(httpMeta: IGatewayHttpMeta, returnValue: unknown): number;
}
