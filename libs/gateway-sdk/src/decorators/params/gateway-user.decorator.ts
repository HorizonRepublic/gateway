import { createParamDecorator, type ExecutionContext } from '@nestjs/common';

import { readGatewayEnvelope } from './envelope-accessor';

/**
 * Parameter decorator that returns the verifier's claims for a protected
 * `@GatewayRoute` handler. The return type is whatever the verifier
 * produced — the gateway forwards it verbatim through `envelope.auth`.
 * @template TAuth - Type of the claims object returned by the verifier.
 *                   Defaults to `unknown` for safety; narrow via the handler
 *                   parameter type annotation.
 * @remarks
 * On an unprotected route this returns `undefined`, which causes the
 * handler to crash on first access — surfacing the misuse at test time
 * instead of silently forwarding nullish claims. On `optional: true` auth
 * routes the value is `undefined` when no credential was presented; such
 * handlers MUST annotate the parameter as `TAuth | undefined` and guard
 * the access.
 * @example
 * ```ts
 * @GatewayRoute({
 *   pattern: 'users.me',
 *   method: 'GET',
 *   path: '/users/me',
 *   auth: true,
 * })
 * public me(@GatewayUser() user: IMyUser): IMyUser {
 *   return user;
 * }
 * ```
 */
export const GatewayUser = createParamDecorator(
  (_: unknown, context: ExecutionContext): unknown => readGatewayEnvelope<unknown>(context).auth,
);
