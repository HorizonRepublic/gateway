/**
 * Route-level declaration that an endpoint is protected by a verifier.
 * @remarks
 * Two forms are supported in the `@GatewayRoute({ auth })` slot:
 *
 *   - `true` — shorthand for "required auth via the default verifier"
 *   - `IGatewayRouteAuthOptions` object — explicit control over which
 *     verifier to call and whether the verification is required or optional
 *     for the route
 */
export type GatewayRouteAuth = true | IGatewayRouteAuthOptions;

/**
 * Structured form of the `auth` slot on `@GatewayRoute`.
 * @remarks
 * Passing an empty object (`auth: {}`) is equivalent to `auth: true` —
 * both resolve to the default verifier, whichever one was registered with
 * `default: true` via `@GatewayAuthVerifier`.
 */
export interface IGatewayRouteAuthOptions {
  /**
   * Verifier id this route delegates to. Omitting the field (or passing
   * `true` / `{}`) uses the default verifier. A route with no explicit
   * verifier AND no default verifier in KV is dropped from the routing
   * table at build time on the gateway side, and the endpoint returns
   * 404 until the configuration is fixed.
   */
  readonly verifier?: string;

  /**
   * When true, the verifier is still called with whatever request context
   * arrived, but a thrown `UnauthorizedException` is treated as "anonymous,
   * proceed" rather than a 401. `ForbiddenException` is still honored.
   * Route handlers MUST annotate `@GatewayUser()` as `T | undefined` to opt
   * into the possibility of no claims. Default: `false` (required auth).
   */
  readonly optional?: boolean;
}
