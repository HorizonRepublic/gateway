import type { IGatewayDefaults } from '../types';

/**
 * Merge module-level defaults with per-route metadata. Called by the
 * lazy `meta` getter composed by `@GatewayRoute` whenever the metadata
 * is read against a new defaults snapshot.
 * @remarks
 * Merge rules:
 *
 *   - `cors`, `rateLimit` — shallow replace (per-route wins entirely).
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
