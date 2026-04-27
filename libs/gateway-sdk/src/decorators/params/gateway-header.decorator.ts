import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that returns a single request-header value by name.
 * Returns `undefined` when the header is absent.
 * @param name - Header name. Lowercased at read time so handler code may
 *               pass any casing — on-envelope keys are already lowercase
 *               because Fastify normalizes request headers before they
 *               reach the gateway.
 * @remarks
 * Mirrors NestJS's own `@Headers('name')` split: prefer this decorator
 * over `@GatewayHeaders()` (full map) when you only need one or two named
 * headers — it sidesteps the full-object read and composes cleanly with
 * NestJS pipes for validation and transformation.
 *
 * **Pipe integration.** NestJS's `createParamDecorator` pipeline passes the
 * returned value through any pipes supplied alongside the decorator, so
 * `@GatewayHeader('x-count', ParseIntPipe)` yields a `number`. Missing
 * headers become `undefined` and can be paired with `DefaultValuePipe` for
 * a fallback.
 * @example
 * ```ts
 * @GatewayRoute({ pattern: 'users.get', method: 'GET', path: '/users/:id' })
 * public getUser(
 *   @GatewayParam('id') id: string,
 *   @GatewayHeader('x-tenant-id') tenant: string | undefined,
 * ): IUser {
 *   return this.users.findById(id, tenant);
 * }
 * ```
 */
export const GatewayHeader = createParamDecorator(
  (name: string, context: ExecutionContext): string | undefined =>
    readGatewayEnvelope(context).headers[name.toLowerCase()],
);
