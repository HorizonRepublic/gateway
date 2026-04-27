import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator sugar that extracts only `meta.requestId` from the
 * envelope — the single most commonly needed meta field.
 * @remarks
 * Equivalent to `@GatewayMeta()` followed by `.requestId`, but avoids
 * dragging the full meta object into handler signatures that only need the
 * correlation id for logging.
 * @example
 * ```ts
 * handle(@GatewayRequestId() requestId: string) {
 *   this.logger.info({ requestId }, 'handling request');
 * }
 * ```
 */
export const GatewayRequestId = createParamDecorator(
  (_: unknown, context: ExecutionContext) => readGatewayEnvelope(context).meta.requestId,
);
