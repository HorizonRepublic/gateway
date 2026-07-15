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
 * Resets the per-name dedupe set used by the serializer's warning
 * policies (SameSite=None, Partitioned, name prefixes, size). Test-only
 * helper exposed for spec isolation; production code never calls this.
 */
export const resetSameSiteWarnDedupeForTests = (): void => {
  sameSiteNoneInsecureWarned.clear();
};

/**
 * `Partitioned` requires `Secure` — the CHIPS algorithm makes a UA
 * ignore a Partitioned cookie entirely when the secure flag is absent.
 * Same anti-footgun shape as the SameSite=None policy: auto-promote
 * when `secure` is undefined (with a one-time WARN), honour an explicit
 * `secure: false` with a loud WARN (the cookie will never reach a
 * browser jar). Note CHIPS is browser-shipped (Chrome 118+) but still
 * pre-standard — an expired individual draft with the WG successor in
 * `draft-ietf-httpbis-layered-cookies`; the attribute itself rides
 * along verbatim.
 */
const applyPartitionedSecurePolicy = (name: string, merged: ICookieOptions): ICookieOptions => {
  if (merged.partitioned !== true || merged.secure === true) {
    return merged;
  }

  if (merged.secure === false) {
    warnOnce(
      `${name}::partitioned-insecure`,
      `gateway: cookie ${name} ships with Partitioned and explicit Secure=false — ` +
        `browsers ignore a Partitioned cookie without Secure entirely. Override honoured ` +
        `for local-dev compatibility, but the cookie will not reach a real browser.`,
    );

    return merged;
  }

  warnOnce(
    `${name}::partitioned-promoted`,
    `gateway: cookie ${name} with Partitioned auto-promoted to Secure ` +
      `(production-default policy — browsers ignore Partitioned cookies without Secure).`,
  );

  return { ...merged, secure: true };
};

/**
 * Cookie name prefix rules per rfc6265bis §4.1.3: `__Secure-` requires
 * Secure; `__Host-` requires Secure + `Path=/` + no Domain. UAs match
 * the prefixes case-insensitively (§5.4) and silently reject violating
 * cookies — the silent-drop incident class this serializer's policies
 * exist to prevent. Absent attributes are auto-filled (Secure, Path);
 * explicit violations (Secure=false, non-root Path, any Domain) are
 * honoured with a loud one-time WARN because no conformant browser
 * will store the result.
 */
const applyPrefixPolicy = (name: string, merged: ICookieOptions): ICookieOptions => {
  const lower = name.toLowerCase();
  const isHost = lower.startsWith('__host-');
  const isSecure = lower.startsWith('__secure-');

  if (!isHost && !isSecure) {
    return merged;
  }

  let effective = merged;

  if (effective.secure === undefined) {
    effective = { ...effective, secure: true };
  }

  if (isHost && effective.path === undefined && effective.domain === undefined) {
    effective = { ...effective, path: '/' };
  }

  const violations: string[] = [];

  if (effective.secure === false) {
    violations.push('Secure=false');
  }

  if (isHost && effective.domain !== undefined) {
    violations.push(`Domain=${effective.domain}`);
  }

  if (isHost && effective.path !== undefined && effective.path !== '/') {
    violations.push(`Path=${effective.path}`);
  }

  if (violations.length > 0) {
    const prefix = isHost ? '__Host-' : '__Secure-';

    warnOnce(
      `${name}::prefix-violation`,
      `gateway: cookie ${name} violates the ${prefix} prefix rules (${violations.join(', ')}) — ` +
        `browsers validate prefixes case-insensitively and will reject this cookie entirely.`,
    );
  }

  return effective;
};

/**
 * One-time `console.warn` keyed on the same dedupe set as the
 * SameSite=None policy so every serializer warning shares the
 * once-per-(name, outcome) contract.
 */
const warnOnce = (dedupeKey: string, message: string): void => {
  if (sameSiteNoneInsecureWarned.has(dedupeKey)) {
    return;
  }

  sameSiteNoneInsecureWarned.add(dedupeKey);
  console.warn(message);
};

/**
 * Encode a cookie NAME for the wire. `encodeURIComponent` leaves the
 * RFC 3986 sub-delims `(` and `)` unencoded, but cookie-name is an
 * HTTP `token` and parentheses are separators — a name like `a(b)`
 * would otherwise ship as an illegal token. Values do not need this:
 * `(` and `)` are legal cookie-octets in a value.
 */
const encodeCookieToken = (name: string): string => {
  const encoded = TOKEN_SAFE.test(name) ? name : encodeURIComponent(name);

  return encoded.replaceAll('(', '%28').replaceAll(')', '%29');
};

/**
 * rfc6265bis §4.1.1 `path-value = *av-octet` where
 * `av-octet = %x20-3A / %x3C-7E` — every printable US-ASCII octet
 * except `;`. Anything outside this alphabet inside a Path attribute
 * would terminate the attribute early and let the remainder of the
 * string masquerade as further cookie attributes.
 */
const PATH_VALUE = /^[ -:<-~]*$/;

/**
 * One RFC 1034 §3.5 label as refined by RFC 1123 §2.1: letters,
 * digits, and interior hyphens — a single alphanumeric, or an
 * alphanumeric-delimited run. The 63-octet cap is checked separately
 * in {@link isDomainValue}; spelling the one-char case as an
 * alternation keeps every quantifier un-nested (star height 1), so
 * the match is backtracking-safe.
 */
const DOMAIN_LABEL = /^(?:[a-z0-9]|[a-z0-9][a-z0-9-]*[a-z0-9])$/i;

/**
 * rfc6265bis §4.1.1 `domain-value = <subdomain>` per RFC 1034 §3.5 as
 * refined by RFC 1123 §2.1: dot-separated labels of letters, digits,
 * and interior hyphens, 1–63 octets each. A single leading `.` is
 * tolerated because rfc6265bis instructs UAs to ignore it. Validated
 * label-by-label — one anchored regex per label is linear-time, where
 * a whole-host regex would need backtracking-prone nested groups.
 */
const isDomainValue = (domain: string): boolean => {
  const host = domain.startsWith('.') ? domain.slice(1) : domain;

  if (host.length === 0) {
    return false;
  }

  return host.split('.').every((label) => label.length <= 63 && DOMAIN_LABEL.test(label));
};

/**
 * Fail-closed validation of the attributes that are interpolated into
 * the `Set-Cookie` line verbatim. Name and value are percent-encoded
 * on the way out, but `domain` / `path` must stay readable and are
 * therefore validated against the rfc6265bis grammar instead: a `;`
 * or control character in either would inject attacker-chosen cookie
 * attributes (session-fixation scope widening). `expires` / `maxAge`
 * are checked here too — `Max-Age=NaN` / `Expires=Invalid Date` are
 * ignored by UAs, silently downgrading the cookie to browser-session
 * lifetime.
 *
 * Throwing (rather than warn-and-drop) matches the module's
 * registration-time fail-fast posture: every branch below is a caller
 * bug, not a runtime condition to paper over.
 */
const assertWireSafeAttributes = (name: string, merged: ICookieOptions): void => {
  if (merged.domain !== undefined && !isDomainValue(merged.domain)) {
    throw new Error(
      `gateway: cookie ${name} has an invalid Domain attribute ` +
        `(${JSON.stringify(merged.domain)}) — must be a host name per RFC 1123. ` +
        `Domain is interpolated into the Set-Cookie line verbatim; rejecting ` +
        `prevents cookie-attribute injection.`,
    );
  }

  if (merged.path !== undefined && !PATH_VALUE.test(merged.path)) {
    throw new Error(
      `gateway: cookie ${name} has an invalid Path attribute ` +
        `(${JSON.stringify(merged.path)}) — only printable US-ASCII except ';' is ` +
        `allowed (rfc6265bis path-value grammar). Path is interpolated into the ` +
        `Set-Cookie line verbatim; rejecting prevents cookie-attribute injection.`,
    );
  }

  if (merged.expires !== undefined && Number.isNaN(merged.expires.getTime())) {
    throw new Error(
      `gateway: cookie ${name} has an invalid expires Date — serializing would ` +
        `ship "Expires=Invalid Date", which UAs ignore, silently downgrading the ` +
        `cookie to browser-session lifetime.`,
    );
  }

  if (merged.maxAge !== undefined && !Number.isFinite(merged.maxAge)) {
    throw new Error(
      `gateway: cookie ${name} has a non-finite maxAge (${String(merged.maxAge)}) — ` +
        `serializing would ship a non-numeric Max-Age, which UAs ignore, silently ` +
        `downgrading the cookie to browser-session lifetime.`,
    );
  }
};

/**
 * rfc6265bis §5.6: a UA ignores the whole cookie when name + value
 * exceed 4096 octets. Another silent-drop class — surface it once per
 * cookie name instead of letting the operator chase a phantom session
 * bug.
 */
const warnOversizedCookie = (name: string, encodedPair: string): void => {
  if (Buffer.byteLength(encodedPair) <= 4096) {
    return;
  }

  warnOnce(
    `${name}::oversized`,
    `gateway: cookie ${name} name+value exceed 4096 octets — browsers ignore the ` +
      `cookie entirely at this size. Move the payload server-side or shrink the value.`,
  );
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
 * @throws Error when `domain` or `path` violate the rfc6265bis attribute
 *         grammar (cookie-attribute injection guard), when `expires` is an
 *         invalid `Date`, or when `maxAge` is not finite. All four are
 *         caller bugs that would otherwise ship a header the browser
 *         either misparses or silently downgrades.
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
  const { effective: afterSameSite, outcome } = applySameSiteNoneSecurePolicy(initial);

  warnSameSiteNonePolicy(name, outcome);

  const merged = applyPrefixPolicy(name, applyPartitionedSecurePolicy(name, afterSameSite));

  assertWireSafeAttributes(name, merged);

  const encodedName = encodeCookieToken(name);
  const encodedValue = TOKEN_SAFE.test(value) ? value : encodeURIComponent(value);

  warnOversizedCookie(name, `${encodedName}=${encodedValue}`);

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
