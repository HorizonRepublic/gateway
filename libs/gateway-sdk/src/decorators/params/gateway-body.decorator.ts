import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that extracts the parsed HTTP request body from the
 * `IGatewayRequest` envelope.
 * @remarks
 * The parameter type is asserted by the handler signature; this decorator
 * itself does not perform runtime validation. Combine with typia or a
 * NestJS pipe for schema enforcement when the body shape matters.
 * @example
 * ```ts
 * @GatewayRoute({ pattern: 'users.create', method: 'POST', path: '/users' })
 * createUser(@GatewayBody() dto: CreateUserDto) {
 *   return this.usersService.create(dto);
 * }
 * ```
 */
export const GatewayBody = createParamDecorator(
  (_: unknown, context: ExecutionContext) => readGatewayEnvelope(context).body,
);
