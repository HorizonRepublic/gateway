import type { IGatewayRateLimitConfig } from '../types/gateway-rate-limit-config.interface';

/**
 * Runtime guard for `IGatewayRateLimitConfig` semantics that the static
 * type system alone cannot enforce.
 * @param rateLimit - The rate-limit policy to check. Accepts `undefined`
 *                    for callers that want to validate conditionally
 *                    without a prior null check.
 * @param context - Human-readable origin of the config used in the error
 *                  message. Examples: `'GatewayModule.forRoot'`,
 *                  `'@GatewayRoute(POST /users)'`.
 * @throws Error when:
 *   - `rps` is not an integer in the range `[1, 2^32 - 1]`. `rps: 0` is
 *     rejected at build time because the Go gateway treats `RPS <= 0` as
 *     "no limit" — a developer who wrote `rps: 0` almost certainly meant
 *     the opposite. Operators who want "no limit" must omit the
 *     `rateLimit` block entirely.
 *   - `burst` is provided and is not an integer in the range `[0, 2^32 -
 *     1]`. Negative `burst` would land in the Go gateway's
 *     undefined-behaviour branch (the GCRA divisor wraps); fail-fast at
 *     registration time is strictly better than a silent runtime drift
 *     that only shows up in 429-rate metrics.
 * @remarks
 * Called at two seams:
 *
 *   - `@GatewayRoute` decorator for per-route `rateLimit` blocks.
 *   - `GatewayModule.forRoot` / `forRootAsync` for module-level
 *     `defaults.rateLimit` blocks.
 *
 * Pure function; no DI, no side effects beyond `throw`.
 * @example
 * ```ts
 * assertRateLimitConfig(
 *   { rps: 0 },
 *   '@GatewayRoute(POST /users)',
 * );
 * // throws: "gateway: rateLimit.rps must be a positive integer..."
 * ```
 */
export const assertRateLimitConfig = (
  rateLimit: IGatewayRateLimitConfig | undefined,
  context: string,
): void => {
  if (rateLimit === undefined) {
    return;
  }

  if (!Number.isInteger(rateLimit.rps) || rateLimit.rps < 1 || rateLimit.rps > 0xff_ff_ff_ff) {
    throw new Error(
      `gateway: rateLimit.rps must be a positive integer in [1, 2^32 - 1]; ` +
        `got ${String(rateLimit.rps)}. ` +
        `To disable rate limiting, omit the rateLimit block entirely. ` +
        `Source: ${context}.`,
    );
  }

  if (rateLimit.burst !== undefined) {
    if (
      !Number.isInteger(rateLimit.burst) ||
      rateLimit.burst < 0 ||
      rateLimit.burst > 0xff_ff_ff_ff
    ) {
      throw new Error(
        `gateway: rateLimit.burst must be a non-negative integer in [0, 2^32 - 1]; ` +
          `got ${String(rateLimit.burst)}. ` +
          `Omit burst to default to 2 * rps. ` +
          `Source: ${context}.`,
      );
    }
  }

  // Widened to string on purpose: the static type already constrains the
  // value, so this guard only ever fires for inputs that bypassed the
  // type checker (JS callers, hand-built configs, schema drift).
  const failPolicy: string | undefined = rateLimit.failPolicy;

  if (failPolicy !== undefined && failPolicy !== 'open' && failPolicy !== 'closed') {
    throw new Error(
      `gateway: rateLimit.failPolicy must be 'open' or 'closed'; ` +
        `got ${String(failPolicy)}. ` +
        `Omit failPolicy to inherit the gateway-wide policy. ` +
        `Source: ${context}.`,
    );
  }
};
