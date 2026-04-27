import type { IGatewayRequest } from '../../types/gateway-request.interface';
import type { ExecutionContext } from '@nestjs/common';

/**
 * Extracts the `IGatewayRequest` envelope from a NestJS `ExecutionContext`
 * produced by a Core NATS RPC call.
 * @template TBody - Optional narrowing for `body` typing; defaults to
 *                   `unknown` so untyped call sites still compile without
 *                   explicit generic arguments.
 * @remarks
 * Centralizing this read here means:
 *
 *   1. All parameter decorators share one extraction strategy (DRY).
 *   2. If the envelope storage location ever changes (e.g., wrapped in an
 *      async context or Pino-bound scope), only this function needs
 *      updating.
 *   3. Unit tests for parameter decorators can stub this helper alone
 *      rather than mocking the full NestJS execution context shape.
 *
 * This helper is *the* single source of truth for how the gateway envelope
 * is read from an RPC context. No other file in
 * `@horizon-republic/gateway-sdk` is allowed to call
 * `ctx.switchToRpc().getData()` directly.
 * @param context - The NestJS execution context for the current RPC call.
 * @returns The gateway request envelope with `TBody` applied to `body`.
 * @example
 * ```ts
 * export const GatewayBody = createParamDecorator(
 *   (_: unknown, context: ExecutionContext) =>
 *     readGatewayEnvelope(context).body,
 * );
 * ```
 */
export const readGatewayEnvelope = <TBody = unknown>(
  context: ExecutionContext,
): IGatewayRequest<TBody> => context.switchToRpc().getData<IGatewayRequest<TBody>>();
