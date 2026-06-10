import { applyDecorators, UseFilters, UseInterceptors } from '@nestjs/common';
import { MessagePattern } from '@nestjs/microservices';
import { PATTERN_EXTRAS_METADATA } from '@nestjs/microservices/constants';

import { GatewayExceptionFilter } from '../filters/gateway-exception.filter';
import { GatewayResponseInterceptor } from '../interceptors/gateway-response.interceptor';
import { assertCorsCredentialsNotWildcard } from '../normalization/cors-validator';
import { assertRateLimitConfig } from '../normalization/rate-limit-validator';
import { getDefaultsSnapshot } from '../runtime/defaults-snapshot';
import { mergeRouteDefaults } from '../runtime/merge-route-defaults';

import type { IGatewayDefaults } from '../types';
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
 *   1. `@MessagePattern(pattern, { meta })` plus a follow-up decorator
 *      that redefines `extras.meta` as a lazy getter — registers the
 *      handler with `nestjs-jetstream`; the metadata the transport reads
 *      (and writes to the `handler_registry` NATS KV bucket) is the
 *      route's own options merged with `GatewayModule.forRoot` defaults
 *      at READ time. Read-time merging removes any dependency on
 *      lifecycle-hook ordering: hybrid apps publish correct metadata on
 *      a stock bootstrap. The getter memoizes per defaults-snapshot
 *      identity because the response interceptor re-reads extras on
 *      every request.
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

  const rawMeta: Record<string, unknown> = { http };

  if (auth !== undefined) rawMeta['auth'] = auth;
  if (options.cors !== undefined) rawMeta['cors'] = options.cors;
  if (options.rateLimit !== undefined) rawMeta['rateLimit'] = options.rateLimit;
  if (options.headers !== undefined) rawMeta['headers'] = options.headers;
  if (options.timeout !== undefined) rawMeta['timeout'] = options.timeout;

  let cachedMerge: Record<string, unknown> | undefined;
  let cachedSnapshot: IGatewayDefaults | undefined;

  const readMergedMeta = (): Record<string, unknown> => {
    const snapshot = getDefaultsSnapshot();

    if (cachedMerge === undefined || cachedSnapshot !== snapshot) {
      cachedMerge = mergeRouteDefaults(snapshot, rawMeta);
      cachedSnapshot = snapshot;
    }

    return cachedMerge;
  };

  // `MessagePattern` spreads the extras it receives into the stored
  // metadata object, so a getter on the PASSED object would be evaluated
  // at decoration time — before `GatewayModule.forRoot` runs in
  // module-graph import order. The getter must instead be defined on the
  // object Nest stored, which downstream readers receive by reference.
  const installLazyMeta: MethodDecorator = (_target, _propertyKey, descriptor) => {
    const extras = Reflect.getMetadata(PATTERN_EXTRAS_METADATA, descriptor.value as object) as
      | Record<string, unknown>
      | undefined;

    if (extras !== undefined) {
      Object.defineProperty(extras, 'meta', {
        enumerable: true,
        get: readMergedMeta,
      });
    }

    return descriptor;
  };

  return applyDecorators(
    MessagePattern(options.pattern, { meta: rawMeta }),
    installLazyMeta,
    UseInterceptors(GatewayResponseInterceptor),
    UseFilters(GatewayExceptionFilter),
  );
};
