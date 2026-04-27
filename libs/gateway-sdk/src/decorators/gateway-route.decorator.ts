import { applyDecorators, UseFilters, UseInterceptors } from '@nestjs/common';
import { MessagePattern } from '@nestjs/microservices';

import { GatewayExceptionFilter } from '../filters/gateway-exception.filter';
import { GatewayResponseInterceptor } from '../interceptors/gateway-response.interceptor';
import { assertCorsCredentialsNotWildcard } from '../normalization/cors-validator';
import { assertRateLimitConfig } from '../normalization/rate-limit-validator';

import type { IGatewayHttpMeta } from '../types/gateway-http-meta.interface';
import type { IGatewayRouteOptions } from '../types/gateway-route-options.interface';

/**
 * Wire shape of the `auth` block persisted into the `handler_registry` KV
 * bucket under `meta.auth`. Mirrors the Go-side `RouteAuthMeta` exactly —
 * changing this type requires a synchronized update to
 * `apps/gateway-server/internal/registry/entry.go`.
 * @remarks
 * Always a plain object on the wire, even when the user wrote `auth: true`
 * at the decorator call site. An empty `verifier` string means "use the
 * default verifier".
 */
interface IGatewayRouteAuthWire {
  verifier: string;
  optional: boolean;
}

/**
 * Normalise the decorator's `auth` option into the wire-compatible object
 * shape. Handles the three legal forms:
 *
 *   - `undefined` → returns `undefined` (public route, no auth block).
 *   - `true` → `{ verifier: '', optional: false }` (default verifier,
 *     required auth).
 *   - `IGatewayRouteAuthOptions` → a plain object with the fields from the
 *     decorator, filling `verifier = ''` and `optional = false` defaults
 *     where omitted.
 *
 * Kept as a pure helper so the decorator body stays a flat
 * `applyDecorators` call and the normalisation rules are testable in
 * isolation.
 */
export const normalizeRouteAuth = (
  auth: IGatewayRouteOptions['auth'],
): IGatewayRouteAuthWire | undefined => {
  if (auth === undefined) {
    return undefined;
  }

  // eslint-disable-next-line security/detect-possible-timing-attacks
  if (auth === true) {
    return { verifier: '', optional: false };
  }

  return {
    verifier: auth.verifier ?? '',
    optional: auth.optional ?? false,
  };
};

/**
 * Exposes a NATS message handler as an HTTP endpoint via `gateway-server`.
 * @param options - Routing metadata for the handler.
 * @remarks
 * Composes three separate decorations in one call:
 *
 *   1. `@MessagePattern(pattern, { meta: { http } })` — registers the
 *      handler with `nestjs-jetstream` and writes HTTP routing metadata to
 *      the `handler_registry` NATS KV bucket via the existing
 *      `extras.meta` passthrough.
 *
 *   2. `@UseInterceptors(GatewayResponseInterceptor)` — locally attaches
 *      the success-path interceptor, so it fires only for gateway-exposed
 *      handlers with no runtime "is this a gateway handler?" check.
 *
 *   3. `@UseFilters(GatewayExceptionFilter)` — locally attaches the
 *      error-path filter. Because filters run after NestJS pipes and
 *      guards, validation errors from pipes are correctly serialized into
 *      structured HTTP responses rather than surfacing as raw 500s on the
 *      client side.
 *
 * The handler remains callable as a pure RPC from other NestJS services
 * via the same pattern — the HTTP exposure is additive, not exclusive.
 *
 * The `statusCode` field is spread conditionally because the workspace
 * enables TypeScript's `exactOptionalPropertyTypes`: assigning a possibly
 * `undefined` value to an optional key is rejected, so the ternary
 * explicitly omits the key when no override was provided.
 * @example
 * ```ts
 * @Controller()
 * export class UsersController {
 *   @GatewayRoute({
 *     pattern: 'users.create',
 *     method: 'POST',
 *     path: '/users',
 *     statusCode: 201,
 *   })
 *   createUser(@GatewayBody() dto: CreateUserDto) {
 *     return this.usersService.create(dto);
 *   }
 * }
 * ```
 */
export const GatewayRoute = (options: IGatewayRouteOptions): MethodDecorator => {
  const source = `@GatewayRoute(${options.method} ${options.path})`;

  assertCorsCredentialsNotWildcard(options.cors, source);
  assertRateLimitConfig(options.rateLimit, source);

  const http: IGatewayHttpMeta =
    options.statusCode === undefined
      ? { method: options.method, path: options.path }
      : {
          method: options.method,
          path: options.path,
          statusCode: options.statusCode,
        };

  const auth = normalizeRouteAuth(options.auth);

  const meta: Record<string, unknown> = { http };

  if (auth !== undefined) meta['auth'] = auth;
  if (options.cors !== undefined) meta['cors'] = options.cors;
  if (options.rateLimit !== undefined) meta['rateLimit'] = options.rateLimit;
  if (options.headers !== undefined) meta['headers'] = options.headers;
  if (options.timeout !== undefined) meta['timeout'] = options.timeout;

  return applyDecorators(
    MessagePattern(options.pattern, { meta }),
    UseInterceptors(GatewayResponseInterceptor),
    UseFilters(GatewayExceptionFilter),
  );
};
