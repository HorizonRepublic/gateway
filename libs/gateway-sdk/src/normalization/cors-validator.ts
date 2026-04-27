import type { IGatewayCorsConfig } from '../types/gateway-cors-config.interface';

/**
 * Validates a `IGatewayCorsConfig` for the wildcard-origin + credentials
 * collision called out in the Fetch Living Standard.
 * @param cors - The CORS policy to check. Accepts `undefined` for callers
 *               that want to validate conditionally without a prior null
 *               check.
 * @param context - Human-readable origin of the config used in the error
 *                  message. Examples: `'GatewayModule.forRoot'`,
 *                  `'@GatewayRoute(POST /users)'`.
 * @throws Error when `credentials === true` AND `origins` contains `'*'`.
 *         Browsers silently reject the combination per the Fetch Living
 *         Standard, so this configuration is equivalent to "CORS is
 *         broken on this endpoint" — fail-fast at registration time is
 *         strictly better than a silent runtime failure that only shows
 *         up in browser consoles.
 * @remarks
 * Called at two seams:
 *
 *   - `@GatewayRoute` decorator for per-route `cors` blocks.
 *   - `GatewayModule.forRoot` / `forRootAsync` for module-level
 *     `defaults.cors` blocks.
 *
 * Pure function; no DI, no side effects beyond `throw`.
 * @example
 * ```ts
 * assertCorsCredentialsNotWildcard(
 *   { origins: ['*'], credentials: true },
 *   '@GatewayRoute(POST /users)',
 * );
 * // throws: "gateway: cors.credentials: true cannot be combined..."
 * ```
 */
export const assertCorsCredentialsNotWildcard = (
  cors: IGatewayCorsConfig | undefined,
  context: string,
): void => {
  if (cors === undefined) {
    return;
  }

  if (cors.credentials !== true) {
    return;
  }

  if (!cors.origins.includes('*')) {
    return;
  }

  throw new Error(
    `gateway: cors.credentials: true cannot be combined with cors.origins: '*' ` +
      `(browsers reject the combination per Fetch Living Standard). ` +
      `Enumerate explicit origins instead. Source: ${context}.`,
  );
};
