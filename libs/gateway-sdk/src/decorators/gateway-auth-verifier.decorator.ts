import { applyDecorators, UseFilters, UseInterceptors } from '@nestjs/common';
import { MessagePattern } from '@nestjs/microservices';

import { AUTH_VERIFIER_PATTERN_PREFIX } from '../constants/auth.constant';
import { GatewayExceptionFilter } from '../filters/gateway-exception.filter';
import { GatewayResponseInterceptor } from '../interceptors/gateway-response.interceptor';

import type { IGatewayAuthVerifierOptions } from '../types/gateway-auth-verifier-options.interface';

/**
 * Valid verifier id shape: URL-safe alphanumerics plus `-` and `_`,
 * bounded to 1–63 characters. Enforced at decoration time so typos and
 * oversized ids never reach the `handler_registry` NATS KV bucket.
 */
const VERIFIER_ID_PATTERN = /^[A-Za-z0-9_-]{1,63}$/;

/**
 * Registers a NestJS handler as an auth verifier for `gateway-server`.
 * @param options - Verifier id and optional default flag. See
 *                  `IGatewayAuthVerifierOptions` for field semantics.
 * @remarks
 * Composes three separate decorations in one call so consumers need a
 * single annotation per verifier method:
 *
 *   1. `@MessagePattern('auth.verifier.<id>', { meta: { verifier } })` —
 *      registers the handler with `nestjs-jetstream`, which writes the
 *      `extras.meta` payload verbatim into the `handler_registry` NATS KV
 *      bucket keyed by `<service>.cmd.auth.verifier.<id>`. The gateway
 *      then reads the entry as a verifier record during routing-table
 *      build, resolving each route's verifier once at build time so
 *      request-time paths read a pre-resolved subject off the matched
 *      route.
 *
 *   2. `@UseInterceptors(GatewayResponseInterceptor)` — wraps the
 *      verifier's return value in the same `IGatewayReply` envelope the
 *      route-side interceptor uses, so the gateway observes a uniform
 *      reply shape on both the route and the verifier sub-request paths.
 *
 *   3. `@UseFilters(GatewayExceptionFilter)` — routes thrown
 *      `HttpException` instances through the same error path as route
 *      handlers, producing an envelope with the correct status code and
 *      optional headers rather than a raw 500.
 *
 * Invalid ids (empty, longer than 63 characters, or containing any
 * character outside `[A-Za-z0-9_-]`) throw synchronously when the
 * decorator is applied so misconfiguration surfaces at app boot rather
 * than on the first request.
 *
 * The `default` field is spread conditionally because the workspace
 * enables TypeScript's `exactOptionalPropertyTypes`: assigning a possibly
 * `undefined` value to an optional key is rejected, so the ternary
 * explicitly omits the key when the caller did not pass `default: true`.
 * @example
 * ```ts
 * @Controller()
 * export class AuthController {
 *   @GatewayAuthVerifier({ id: 'jwt', default: true })
 *   public async verify(
 *     @GatewayHeader('authorization') auth: string | undefined,
 *   ): Promise<IMyUser> {
 *     const token = auth?.replace(/^Bearer /, '');
 *     if (!token) throw new UnauthorizedException();
 *     return this.jwt.verify(token);
 *   }
 * }
 * ```
 */
export const GatewayAuthVerifier = (options: IGatewayAuthVerifierOptions): MethodDecorator => {
  if (!VERIFIER_ID_PATTERN.test(options.id)) {
    throw new Error(
      `GatewayAuthVerifier: invalid verifier id ${JSON.stringify(options.id)} ` +
        `— must match ${VERIFIER_ID_PATTERN}`,
    );
  }

  const verifier =
    options.default === true ? { id: options.id, default: true } : { id: options.id };

  return applyDecorators(
    MessagePattern(`${AUTH_VERIFIER_PATTERN_PREFIX}${options.id}`, {
      meta: { verifier },
    }),
    UseInterceptors(GatewayResponseInterceptor),
    UseFilters(GatewayExceptionFilter),
  );
};
