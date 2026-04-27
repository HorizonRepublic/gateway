import type { ICookieOptions } from './cookie-options.interface';
import type { IGatewayCorsConfig } from './gateway-cors-config.interface';
import type { IGatewayRateLimitConfig } from './gateway-rate-limit-config.interface';

/**
 * Module-level defaults applied to every `@GatewayRoute` that does not
 * explicitly override a given field. Configured via
 * `GatewayModule.forRoot({ defaults })` or `forRootAsync`.
 * @remarks
 * Merge rules (per-route `@GatewayRoute` overrides module defaults):
 *
 * - `cors`, `rateLimit`, `cookies` — **shallow replace.** Per-route replaces
 *   the entire block.
 * - `headers` — **deep merge per-key.** Per-route adds or overrides
 *   individual keys without losing module-level security headers.
 * - `timeout` — **simple override.** Scalar value.
 */
export interface IGatewayDefaults {
  readonly cors?: IGatewayCorsConfig;
  readonly rateLimit?: IGatewayRateLimitConfig;
  readonly headers?: Readonly<Record<string, string>>;
  /** SDK-only cookie defaults. NOT written to KV. */
  readonly cookies?: Partial<ICookieOptions>;
  /** Per-route request timeout in milliseconds. */
  readonly timeout?: number;
}
