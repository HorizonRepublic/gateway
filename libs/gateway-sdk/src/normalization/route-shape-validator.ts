import type { IGatewayRouteOptions } from '../types/gateway-route-options.interface';

/**
 * Canonical verb set mirrored from the `HttpMethod` union. Runtime
 * twin of the static type for JS callers and hand-built configs that
 * bypass the type checker — a lowercase verb registers an unreachable
 * route on the gateway side (route keys are exact-match uppercase).
 */
const HTTP_METHODS = new Set(['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']);

/**
 * One NATS subject token: the conservative charset NATS documentation
 * recommends per token. Dots are token separators and are validated
 * structurally (no empty tokens), not as part of a token.
 */
const SUBJECT_TOKEN = /^[A-Za-z0-9_-]+$/;

/**
 * Runtime guard for the wire-shape invariants of `@GatewayRoute`
 * options that the static type system cannot enforce (or that JS
 * callers bypass entirely).
 * @param options - The route options to check.
 * @param context - Human-readable origin used in error messages,
 *                  e.g. `'@GatewayRoute(GET /users)'`.
 * @throws Error when:
 *   - `method` is not one of the canonical uppercase verbs — the Go
 *     gateway's route keys are exact-match, so `get` would register an
 *     unreachable route.
 *   - `pattern` is empty, contains a non-token-safe character, a NATS
 *     wildcard (`*`, `>`), whitespace, or an empty dot-separated
 *     token — the pattern becomes part of the NATS subject AND the
 *     `handler_registry` KV key; an unpublishable subject fails only
 *     at request time.
 *   - `timeout` is not a positive integer. `timeout: 0` is rejected
 *     explicitly: the Go gateway treats a non-positive route timeout
 *     as "inherit the gateway-wide value", so 0 is never a disable
 *     switch — omit the field to inherit.
 *   - `statusCode` is not an integer in `[100, 599]` — the gateway
 *     rejects out-of-range statuses at reply time with 502; failing at
 *     decoration time is strictly earlier and names the route.
 *   - `cors.maxAge` is not a non-negative integer.
 * @remarks
 * A non-integer JSON number in any of these fields would fail the Go
 * side's strict `*int` unmarshal and silently drop the WHOLE registry
 * entry (route 404s with no signal). Pure function; no side effects
 * beyond `throw`.
 */
export const assertRouteWireShape = (options: IGatewayRouteOptions, context: string): void => {
  const method: string = options.method;

  if (!HTTP_METHODS.has(method)) {
    throw new Error(
      `gateway: method must be one of ${[...HTTP_METHODS].join(', ')} (exact uppercase); ` +
        `got ${JSON.stringify(method)}. Source: ${context}.`,
    );
  }

  assertPattern(options.pattern, context);

  if (options.timeout !== undefined) {
    if (options.timeout === 0) {
      throw new Error(
        `gateway: timeout: 0 is not a disable switch — the gateway treats it as ` +
          `"inherit the gateway-wide value". Omit timeout to inherit the gateway-wide value. ` +
          `Source: ${context}.`,
      );
    }

    if (!Number.isInteger(options.timeout) || options.timeout < 1) {
      throw new Error(
        `gateway: timeout must be a positive integer (milliseconds); ` +
          `got ${String(options.timeout)}. Source: ${context}.`,
      );
    }
  }

  if (
    options.statusCode !== undefined &&
    (!Number.isInteger(options.statusCode) || options.statusCode < 100 || options.statusCode > 599)
  ) {
    throw new Error(
      `gateway: statusCode must be an integer in [100, 599]; ` +
        `got ${String(options.statusCode)}. Source: ${context}.`,
    );
  }

  const maxAge = options.cors?.maxAge;

  if (maxAge !== undefined && (!Number.isInteger(maxAge) || maxAge < 0)) {
    throw new Error(
      `gateway: cors.maxAge must be a non-negative integer (seconds); ` +
        `got ${String(maxAge)}. Source: ${context}.`,
    );
  }
};

/**
 * Validate the message pattern against NATS subject constraints: the
 * pattern is embedded verbatim into both the NATS subject and the
 * `handler_registry` KV key, so wildcards, whitespace, and empty
 * tokens produce subjects that are unpublishable or routable to the
 * wrong handler — failures that otherwise surface only at request
 * time.
 */
const assertPattern = (pattern: string, context: string): void => {
  if (pattern.length === 0) {
    throw new Error(`gateway: pattern must not be empty. Source: ${context}.`);
  }

  for (const token of pattern.split('.')) {
    if (!SUBJECT_TOKEN.test(token)) {
      throw new Error(
        `gateway: pattern ${JSON.stringify(pattern)} is not a valid NATS subject — every ` +
          `dot-separated token must match [A-Za-z0-9_-]+ (no wildcards, no whitespace, ` +
          `no empty tokens). Source: ${context}.`,
      );
    }
  }
};
