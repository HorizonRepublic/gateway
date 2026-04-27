import type { IGatewayRequestMeta } from './gateway-request-meta.interface';
import type { IGatewayRouteContext } from './gateway-route-context.interface';

/**
 * Envelope payload sent by `gateway-server` to a NestJS handler over Core NATS
 * request/reply.
 * @template TBody - Type of the parsed HTTP body. Defaults to `unknown` for
 *                   safety; narrow via the handler signature or param decorators.
 * @template TAuth - Type of the verifier claims injected by the gateway for
 *                   protected routes. Defaults to `unknown`; narrow via
 *                   `@GatewayUser()` parameter typing or an explicit generic
 *                   argument on the envelope itself.
 * @remarks
 * Application code typically does not touch this type directly — param
 * decorators (`@GatewayBody`, `@GatewayParam`, `@GatewayUser`, etc.) extract
 * the fields individually. Use `@Payload()` with this type when you need
 * the raw envelope.
 */
export interface IGatewayRequest<TBody = unknown, TAuth = unknown> {
  /** Routing context describing which registered route matched this request. */
  readonly route: IGatewayRouteContext;

  /**
   * Path parameters extracted from `:placeholders` in the route template.
   * @remarks
   * Always present; an empty object when the matched route template has no
   * placeholders. Handlers can read keys unconditionally without null checks.
   * Values are always `string` — numeric coercion is the handler's
   * responsibility (e.g., via a NestJS pipe or explicit `Number()`).
   */
  readonly params: Readonly<Record<string, string>>;

  /**
   * Parsed query string.
   * @remarks
   * Always present; an empty object when the incoming URL has no query.
   * Repeated keys arrive as `readonly string[]`; single-value keys arrive as
   * `string`. Handlers should `Array.isArray` to discriminate when a single
   * key may or may not be repeated.
   */
  readonly query: Readonly<Record<string, string | readonly string[]>>;

  /**
   * Request headers, lowercased.
   * @remarks
   * Always present; an empty object on requests that somehow carry no headers.
   * Multi-value headers are joined into a single string in MVP; `headersRaw`
   * will be added as a non-breaking extension when multi-value support lands.
   * Hop-by-hop headers (`Connection`, `Transfer-Encoding`, etc.) are stripped
   * by the gateway before forwarding — handlers only see end-to-end headers.
   */
  readonly headers: Readonly<Record<string, string>>;

  /**
   * Parsed HTTP body.
   * @remarks
   * For `application/json` requests the gateway parses the body before
   * publishing; for empty-body methods (typically `GET`, `DELETE`) the field
   * holds the JSON `null` literal. Handlers that need to tolerate missing
   * bodies should narrow the generic (`IGatewayRequest<MyDto | null>`) rather
   * than relying on runtime presence checks.
   */
  readonly body: TBody;

  /** Gateway-generated metadata: request id, trace context, deadlines. */
  readonly meta: IGatewayRequestMeta;

  /**
   * Verifier claims injected by the gateway when the matched route declared
   * an `auth` block. `undefined` for unprotected routes and for optional-auth
   * routes when no credential was presented. Route handlers read this via
   * `@GatewayUser()`.
   */
  readonly auth?: TAuth;
}
