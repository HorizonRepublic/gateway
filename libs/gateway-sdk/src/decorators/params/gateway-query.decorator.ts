import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that extracts query string parameters from
 * `IGatewayRequest`.
 * @remarks
 * When called without a `key`, returns the entire `query` map. When called
 * with a `key`, returns the value for that key.
 *
 * Repeated keys (`?tag=a&tag=b`) arrive as `readonly string[]`; single-value
 * keys arrive as `string`. Handlers should `Array.isArray` to discriminate
 * when a key may or may not be repeated.
 * @example
 * ```ts
 * handle(@GatewayQuery('tag') tag: string | readonly string[] | undefined) {
 *   const tags = Array.isArray(tag) ? tag : tag ? [tag] : [];
 * }
 * ```
 */
export const GatewayQuery = createParamDecorator(
  (key: string | undefined, context: ExecutionContext): unknown => {
    const query = readGatewayEnvelope(context).query;

    return key === undefined ? query : query[key];
  },
);
