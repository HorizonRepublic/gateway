import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that returns the full `IGatewayRequestMeta` object —
 * request id, trace context, remote client IP, timing, and deadline.
 * @remarks
 * Prefer `@GatewayRequestId()` when you only need the request id. Use this
 * decorator when you need multiple meta fields at once or you are passing
 * the full metadata structure down into a logger or tracing helper.
 * @example
 * ```ts
 * handle(@GatewayMeta() meta: IGatewayRequestMeta) {
 *   this.logger.debug({ requestId: meta.requestId, remoteAddr: meta.remoteAddr });
 * }
 * ```
 */
export const GatewayMeta = createParamDecorator(
  (_: unknown, context: ExecutionContext) => readGatewayEnvelope(context).meta,
);
