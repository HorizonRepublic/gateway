import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that extracts HTTP headers from `IGatewayRequest`.
 * @remarks
 * Header lookup is **case-insensitive**: the decorator lowercases the
 * provided key before lookup, matching how the gateway stores headers.
 * When called without a `key`, returns the entire lowercased header map.
 *
 * Multi-value headers are joined into a single string in MVP. When the
 * `headersRaw` extension field is added in a future SDK version, the
 * key-based lookup here will continue to return a single string — reach
 * for `@GatewayMeta()` or future helpers to access the raw multi-value
 * array.
 * @example
 * ```ts
 * handle(@GatewayHeaders('authorization') auth: string | undefined) {
 *   if (!auth?.startsWith('Bearer ')) throw new UnauthorizedError();
 * }
 * ```
 */
export const GatewayHeaders = createParamDecorator(
  (key: string | undefined, context: ExecutionContext): unknown => {
    const headers = readGatewayEnvelope(context).headers;

    return key === undefined ? headers : headers[key.toLowerCase()];
  },
);
