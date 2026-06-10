import type { IGatewayCorsConfig } from './gateway-cors-config.interface';
import type { IGatewayRateLimitConfig } from './gateway-rate-limit-config.interface';
import type { GatewayRouteAuth } from './gateway-route-auth-options.interface';
import type { HttpMethod } from './http-method.type';

/**
 * Options accepted by the `@GatewayRoute` decorator.
 * @remarks
 * These options are composed into two separate metadata targets at
 * decoration time: the `pattern` is forwarded to `@MessagePattern`, while
 * `method`, `path`, and `statusCode` are written under `extras.meta.http`
 * so that `nestjs-jetstream` persists them to the `handler_registry` KV
 * bucket.
 */
export interface IGatewayRouteOptions {
  /**
   * The message pattern — same semantics as `@MessagePattern(pattern)`.
   * @remarks
   * Determines the NATS subject the handler subscribes to, using the
   * `nestjs-jetstream` convention: `{service}__microservice.cmd.{pattern}`.
   *
   * Consumers may still invoke this handler directly as an RPC from another
   * NestJS service using this same pattern — the HTTP exposure is additive.
   */
  readonly pattern: string;

  /**
   * HTTP method the gateway should accept.
   * @remarks
   * Persisted verbatim into `IGatewayHttpMeta.method` (see that type for
   * the on-wire shape) and becomes part of the gateway's routing trie key.
   */
  readonly method: HttpMethod;

  /**
   * URL path template with `:param` placeholders, e.g. `/users/:id`.
   * @remarks
   * Placeholder names become the keys of `IGatewayRequest.params` and MUST
   * match the string passed to `@GatewayParam('name')` inside the handler.
   * The template is also used verbatim as a metric/log label — keep
   * cardinality bounded by using `:params` for dynamic segments instead of
   * embedding ids in the literal path.
   */
  readonly path: string;

  /**
   * HTTP status returned on successful responses.
   * @remarks
   * When omitted, the gateway applies the default rules: `200` for non-null
   * returns, `204` for `null` / `undefined` / `void` returns. Provide an
   * explicit value (e.g. `201` for `POST /users`) when the default is wrong.
   */
  readonly statusCode?: number;

  /**
   * Declares the route is protected by an auth verifier.
   * @remarks
   * Omit the field entirely for public routes. Short form `auth: true` is
   * equivalent to `auth: {}` — both resolve to the default verifier. See
   * `IGatewayRouteAuthOptions` for the full decision matrix.
   */
  readonly auth?: GatewayRouteAuth;

  /**
   * CORS policy for this route. Overrides `forRoot` defaults (shallow
   * replace).
   * @remarks
   * When provided, the gateway applies this policy instead of the global
   * CORS configuration. Omit to inherit the application-wide defaults.
   */
  readonly cors?: IGatewayCorsConfig;

  /**
   * Rate-limit policy for this route. Overrides `forRoot` defaults (shallow
   * replace).
   * @remarks
   * When provided, the gateway enforces this limit instead of the global
   * rate-limit configuration. Omit to inherit the application-wide defaults.
   */
  readonly rateLimit?: IGatewayRateLimitConfig;

  /**
   * Static response headers. Deep-merged with `forRoot` defaults per-key.
   * @remarks
   * Keys defined here take precedence over same-named keys from the global
   * header defaults. Use this to set `cache-control`,
   * `x-content-type-options`, and similar per-route overrides.
   */
  readonly headers?: Readonly<Record<string, string>>;

  /**
   * Per-route request timeout in milliseconds. Overrides global timeout.
   * @remarks
   * When omitted, the gateway applies the application-wide timeout. Must be
   * a positive integer — `timeout: 0` is rejected at decoration time
   * because the gateway treats a non-positive route timeout as "inherit
   * the gateway-wide value", never as "no timeout". There is no per-route
   * disable switch.
   */
  readonly timeout?: number;
}
