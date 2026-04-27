import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that extracts path parameters from `IGatewayRequest`.
 * @remarks
 * When called without a `key`, returns the entire `params` map. When called
 * with a `key`, returns the value for that specific path parameter. Values
 * are always `string` — handlers are responsible for numeric coercion (via
 * a NestJS pipe, typia, or explicit `Number()`).
 * @example
 * ```ts
 * @GatewayRoute({ pattern: 'users.get', method: 'GET', path: '/users/:id' })
 * getUser(@GatewayParam('id') id: string) {
 *   return this.usersService.findById(id);
 * }
 * ```
 */
export const GatewayParam = createParamDecorator(
  (key: string | undefined, context: ExecutionContext): unknown => {
    const params = readGatewayEnvelope(context).params;

    return key === undefined ? params : params[key];
  },
);
