import type { ICookieOptions } from '../types/cookie-options.interface';

/**
 * Fast-path test: ASCII letters, digits, dash, underscore, dot, and tilde —
 * the RFC 3986 unreserved set — need no percent-encoding in a `Set-Cookie`
 * value or name. Regex is compiled once at module load.
 */
const TOKEN_SAFE = /^[A-Za-z0-9._~-]*$/;

/**
 * Maps the decorator option's lowercase sameSite values to the canonical
 * capitalized form expected in the serialized header.
 */
const SAME_SITE_LABELS = {
  strict: 'Strict',
  lax: 'Lax',
  none: 'None',
} as const;

/**
 * Module-level dedupe set for the `SameSite=None without Secure` warning.
 * Logs once per cookie name + outcome shape so a hot handler does not spam
 * stderr on every request while still surfacing the configuration bug on
 * first emission.
 */
const sameSiteNoneInsecureWarned = new Set<string>();

/**
 * Internal sentinel returned by `applySameSiteNoneSecurePolicy` to
 * communicate whether the caller's options were silently auto-promoted
 * (warn lightly), or contained an explicit `secure: false` override (warn
 * loudly — this is a strict anti-footgun signal).
 */
type SameSiteNoneOutcome = 'auto-promoted' | 'explicit-insecure-override' | 'no-action';

/**
 * Apply the production-default policy for cookies that opt into cross-site
 * delivery via `SameSite=None`:
 *
 * 1. When `sameSite === 'none'` AND `secure` is `undefined`, the serializer
 *    auto-promotes `secure: true` so the cookie reaches the client jar in
 *    every modern browser. Operators who pick `SameSite=None` are
 *    signalling cross-site intent — silently failing because they forgot
 *    the second flag is the wrong default for a production library.
 * 2. When `sameSite === 'none'` AND `secure === false` is explicitly
 *    provided, the serializer respects the override BUT emits a loud
 *    warning. The override exists for local-dev or test contexts where
 *    HTTPS is unavailable; in production it is almost always a footgun.
 * 3. When `secure === true` already, no action and no warning.
 *
 * The policy is documented in TSDoc on `serializeCookie`.
 */
const applySameSiteNoneSecurePolicy = (
  merged: ICookieOptions,
): { effective: ICookieOptions; outcome: SameSiteNoneOutcome } => {
  if (merged.sameSite !== 'none') {
    return { effective: merged, outcome: 'no-action' };
  }

  if (merged.secure === true) {
    return { effective: merged, outcome: 'no-action' };
  }

  if (merged.secure === false) {
    return { effective: merged, outcome: 'explicit-insecure-override' };
  }

  // sameSite === 'none' AND secure === undefined → auto-promote.
  // The serializer's contract is that production defaults must produce a
  // cookie the client jar accepts; doing nothing here would emit
  // `SameSite=None` without `Secure` and the cookie would silently never
  // reach the browser. ICookieOptions is declared `readonly`, so a fresh
  // object is produced rather than mutating the caller's input.
  return {
    effective: { ...merged, secure: true },
    outcome: 'auto-promoted',
  };
};

/**
 * Emit a one-time warning per cookie name when a serialization exercises
 * one of the `SameSite=None` policy branches.
 *
 * The dedupe is per cookie name AND per outcome shape so an operator who
 * first saw the auto-promote nudge during dev and later explicitly
 * overrode `secure: false` for a local test still sees the second (loud)
 * warning instead of having it silenced by the dedupe set.
 */
const warnSameSiteNonePolicy = (name: string, outcome: SameSiteNoneOutcome): void => {
  if (outcome === 'no-action') {
    return;
  }

  const dedupeKey = `${name}::${outcome}`;

  if (sameSiteNoneInsecureWarned.has(dedupeKey)) {
    return;
  }

  sameSiteNoneInsecureWarned.add(dedupeKey);

  if (outcome === 'auto-promoted') {
    console.warn(
      `gateway: cookie ${name} with SameSite=None auto-promoted to Secure ` +
        `(production-default policy — modern browsers reject SameSite=None without Secure).`,
    );

    return;
  }

  // explicit-insecure-override: this is the strict anti-footgun path. The
  // cookie WILL be silently dropped by the browser jar; the loud warning
  // is the only signal an operator gets that their override defeated the
  // contract.
  console.warn(
    `gateway: cookie ${name} ships with SameSite=None and explicit Secure=false — ` +
      `modern browsers WILL reject this cookie. Override is honoured for local-dev ` +
      `compatibility, but the cookie will not reach a real browser.`,
  );
};

/**
 * Resets the per-name dedupe set used by `warnSameSiteNonePolicy`. Test-only
 * helper exposed for spec isolation; production code never calls this.
 */
export const resetSameSiteWarnDedupeForTests = (): void => {
  sameSiteNoneInsecureWarned.clear();
};

/**
 * Serializes a single `Set-Cookie` header value per RFC 6265 §4.1.1. Pure
 * function, no external dependencies.
 * @param name - Cookie name. Percent-encoded if it contains characters
 *               outside the RFC 3986 unreserved set.
 * @param value - Cookie value. Same encoding rule as the name.
 * @param options - Optional RFC 6265 attributes. Missing fields omit the
 *                  corresponding attribute.
 * @param defaults - Module-level cookie defaults from
 *                   `GatewayModule.forRoot({ defaults: { cookies } })`.
 *                   Per-cookie `options` fields take precedence over
 *                   `defaults` on a key-by-key basis.
 * @returns The serialized header value, ready to be stored in a reply
 *          envelope under `set-cookie`.
 * @remarks
 * `SameSite=None` requires `Secure` per the Cookies Living Standard. The
 * serializer enforces this with a production-default policy:
 *
 * - When `sameSite: 'none'` is paired with no `secure` value, the
 *   serializer auto-promotes `secure: true` so the cookie reaches the
 *   client jar in every modern browser. A one-time WARN is emitted per
 *   cookie name to surface the implicit promotion.
 * - When `sameSite: 'none'` is paired with `secure: false`, the override
 *   is honoured (local-dev / HTTP-only test fixtures need it) but a loud
 *   WARN is emitted because the cookie will not reach a real browser.
 *   This is intentional anti-footgun: the silent override path was a
 *   documented production incident vector.
 * - `sameSite: 'strict'` and `sameSite: 'lax'` impose no Secure
 *   requirement and trigger no policy.
 * @example
 * ```ts
 * serializeCookie('sid', 'abc', { httpOnly: true, secure: true, maxAge: 3600 });
 * // → 'sid=abc; Max-Age=3600; HttpOnly; Secure'
 * ```
 * @example
 * ```ts
 * // Auto-promote: secure becomes true, WARN emitted once per name.
 * serializeCookie('sid', 'abc', { sameSite: 'none' });
 * // → 'sid=abc; Secure; SameSite=None'
 * ```
 */
export const serializeCookie = (
  name: string,
  value: string,
  options: ICookieOptions = {},
  defaults: Partial<ICookieOptions> = {},
): string => {
  const initial: ICookieOptions = { ...defaults, ...options };
  const { effective: merged, outcome } = applySameSiteNoneSecurePolicy(initial);

  warnSameSiteNonePolicy(name, outcome);

  const encodedName = TOKEN_SAFE.test(name) ? name : encodeURIComponent(name);
  const encodedValue = TOKEN_SAFE.test(value) ? value : encodeURIComponent(value);

  let out = `${encodedName}=${encodedValue}`;

  if (merged.domain !== undefined) {
    out += `; Domain=${merged.domain}`;
  }

  if (merged.path !== undefined) {
    out += `; Path=${merged.path}`;
  }

  if (merged.expires !== undefined) {
    out += `; Expires=${merged.expires.toUTCString()}`;
  }

  if (merged.maxAge !== undefined) {
    out += `; Max-Age=${Math.floor(merged.maxAge)}`;
  }

  if (merged.httpOnly === true) {
    out += `; HttpOnly`;
  }

  if (merged.secure === true) {
    out += `; Secure`;
  }

  if (merged.sameSite !== undefined) {
    out += `; SameSite=${SAME_SITE_LABELS[merged.sameSite]}`;
  }

  if (merged.partitioned === true) {
    out += `; Partitioned`;
  }

  return out;
};
