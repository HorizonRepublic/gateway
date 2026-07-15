import { withDefaultCorsHeaders } from '../normalization/cors-defaults';

import type { IGatewayDefaults } from '../types';
import type { IGatewayCorsConfig } from '../types/gateway-cors-config.interface';

/**
 * Merge module-level defaults with per-route metadata. Called by the
 * lazy `meta` getter composed by `@GatewayRoute` whenever the metadata
 * is read against a new defaults snapshot.
 * @remarks
 * Merge rules:
 *
 *   - `cors`, `rateLimit` — shallow replace (per-route wins entirely).
 *     The winning `cors` block then gets the documented `headers`
 *     default materialized when it omits the field, so the registry
 *     entry always carries an explicit allow-list.
 *   - `headers` — deep merge per key (per-route adds / overrides).
 *   - `timeout` — simple override.
 *   - `cookies` — excluded; SDK-only, never written to KV.
 */
export const mergeRouteDefaults = (
  defaults: IGatewayDefaults,
  route: Record<string, unknown>,
): Record<string, unknown> => {
  const merged: Record<string, unknown> = { ...route };

  if (merged['cors'] === undefined && defaults.cors !== undefined) {
    merged['cors'] = defaults.cors;
  }

  const cors = merged['cors'] as IGatewayCorsConfig | undefined;

  if (cors !== undefined) {
    merged['cors'] = withDefaultCorsHeaders(cors);
  }

  if (merged['rateLimit'] === undefined && defaults.rateLimit !== undefined) {
    merged['rateLimit'] = defaults.rateLimit;
  }

  const defaultHeaders = defaults.headers;
  const routeHeaders = merged['headers'] as Readonly<Record<string, string>> | undefined;

  if (defaultHeaders !== undefined || routeHeaders !== undefined) {
    merged['headers'] = { ...defaultHeaders, ...routeHeaders };
  }

  if (merged['timeout'] === undefined && defaults.timeout !== undefined) {
    merged['timeout'] = defaults.timeout;
  }

  return merged;
};
