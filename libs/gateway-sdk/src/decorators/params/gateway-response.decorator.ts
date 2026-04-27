import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { GatewayResponseAccumulator } from '../../runtime/gateway-response-accumulator';
import { acquireAccumulator } from '../../runtime/gateway-response-pool';
import { RESPONSE_ACCUMULATOR_KEY } from '../../runtime/response-accumulator-symbol';

import type { IGatewayResponse } from '../../types/gateway-response.interface';

/**
 * Runtime shape of the RPC envelope viewed through its `Symbol`-keyed
 * accumulator slot. Local to this module because the slot is a
 * module-private implementation detail shared only with
 * `GatewayResponseInterceptor`.
 */
type IEnvelopeWithAccumulatorSlot = Record<symbol, unknown>;

/**
 * Factory function used by both `createParamDecorator` and the spec.
 * Exported so the idempotent-acquire invariant is tested against the same
 * code path production runs through.
 */
export const extractGatewayResponse = (_: unknown, context: ExecutionContext): IGatewayResponse => {
  const envelope = context.switchToRpc().getData<IEnvelopeWithAccumulatorSlot>();
  const existing = envelope[RESPONSE_ACCUMULATOR_KEY];

  if (existing instanceof GatewayResponseAccumulator) {
    return existing;
  }

  const fresh = acquireAccumulator();

  envelope[RESPONSE_ACCUMULATOR_KEY] = fresh;

  return fresh;
};

/**
 * Parameter decorator that injects a mutable `IGatewayResponse` builder
 * into a `@GatewayRoute` or `@GatewayAuthVerifier` handler.
 * @remarks
 * Handlers use the builder to set cookies, response headers, and a dynamic
 * HTTP status without wrapping their return values in a transport
 * envelope — the handler still returns a pure DTO, and the
 * `GatewayResponseInterceptor` merges the accumulated state into the reply
 * when the handler emits.
 *
 * **Express-convention throw semantics.** When a handler throws after
 * calling any `res.*` method, the accumulator state is discarded. The
 * exception filter ignores the accumulator and builds its reply purely
 * from the thrown exception. This matches Express / Fastify.
 *
 * **Per-request lifecycle.** The first `@GatewayResponse()` injection on a
 * request lazily checks out an accumulator from the pool and stashes it on
 * the envelope under a module-private `Symbol` key. Subsequent injections
 * on the same request return the same instance by reference, so multiple
 * `@GatewayResponse()` parameters in one handler signature share state.
 * After the handler emits (or throws), the interceptor's `finalize`
 * operator releases the instance back to the pool.
 *
 * **Zero-overhead fast path.** Handlers that do NOT inject this decorator
 * pay nothing at all — the interceptor performs a single
 * `envelope[SYMBOL] === undefined` check on its success path and falls
 * through to the pre-accumulator behavior when no injection occurred. No
 * allocation, no pool touch, no extra RxJS operator beyond what the
 * existing pipeline uses.
 * @example
 * ```ts
 * @GatewayRoute({ pattern: 'auth.login', method: 'POST', path: '/login' })
 * public login(
 *   @GatewayBody() dto: ILoginDto,
 *   @GatewayResponse() res: IGatewayResponse,
 * ): IUserDto {
 *   const session = this.auth.signIn(dto);
 *
 *   res
 *     .cookie('sid', session.token, {
 *       httpOnly: true,
 *       secure: true,
 *       sameSite: 'strict',
 *       maxAge: 3600,
 *     })
 *     .status(201);
 *
 *   return session.user;
 * }
 * ```
 */
export const GatewayResponse = createParamDecorator(extractGatewayResponse);
