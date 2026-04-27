import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { parseCookies } from '../../normalization/cookie-parser';
import { PARSED_COOKIES_KEY } from '../../runtime/parsed-cookies-symbol';

import { readGatewayEnvelope } from './envelope-accessor';

import type { IGatewayRequest } from '../../types/gateway-request.interface';

/**
 * Runtime view of the RPC envelope with the parsed-cookie cache slot
 * exposed. Module-private — user-land types only see `IGatewayRequest`,
 * the `Symbol` slot is invisible outside this file.
 */
type IEnvelopeWithCookieCache = IGatewayRequest & {
  [PARSED_COOKIES_KEY]?: Record<string, string>;
};

/**
 * Parameter decorator that returns the value of a single request cookie by
 * name. Parses the raw `Cookie:` header once per request and caches the
 * result on the envelope under a module-private `Symbol` so multiple
 * `@GatewayCookie` parameters on one handler share the parse cost.
 * @param name - Exact cookie name to look up. Unlike header names, cookie
 *               names are case-sensitive per RFC 6265 §4.1.1, so
 *               `@GatewayCookie('Sid')` does NOT match a cookie named `sid`.
 * @remarks
 * Mirrors NestJS request-decorator ergonomics: `@GatewayCookie` is the
 * cookies analogue of `@GatewayHeader(name)`. Prefer it over parsing
 * `@GatewayHeaders().cookie` manually — the `Symbol`-based cache means N
 * decorators cost one parse.
 *
 * Returns `undefined` when the cookie is absent or the request has no
 * `Cookie:` header at all. Pairs cleanly with `DefaultValuePipe` and
 * validation pipes (`ParseUUIDPipe`, typia-based pipes) through the
 * standard NestJS pipe pipeline.
 *
 * The parsed cache is stored on the envelope itself under a module-private
 * `Symbol`, so the envelope type visible to handler code is unchanged —
 * the slot is invisible outside of this file.
 * @example
 * ```ts
 * @GatewayRoute({
 *   pattern: 'users.me',
 *   method: 'GET',
 *   path: '/me',
 *   auth: true,
 * })
 * public me(
 *   @GatewayCookie('sid') sid: string | undefined,
 *   @GatewayUser() user: IUser,
 * ): IUser {
 *   return user;
 * }
 * ```
 */
/**
 * Factory function used by both `createParamDecorator` and the spec.
 * Exported so the cache-once invariant is tested against the same code
 * path production runs through, not a duplicated copy.
 */
export const extractGatewayCookie = (
  name: string,
  context: ExecutionContext,
): string | undefined => {
  const envelope = readGatewayEnvelope(context) as IEnvelopeWithCookieCache;

  let parsed = envelope[PARSED_COOKIES_KEY];

  if (parsed === undefined) {
    parsed = parseCookies(envelope.headers['cookie'] ?? '');
    envelope[PARSED_COOKIES_KEY] = parsed;
  }

  return parsed[name];
};

export const GatewayCookie = createParamDecorator(extractGatewayCookie);
