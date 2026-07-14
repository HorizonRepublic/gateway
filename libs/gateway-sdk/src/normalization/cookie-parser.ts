/**
 * Parses the value of an HTTP `Cookie:` request header into a name-to-value
 * map per RFC 6265 §5.4 plus RFC 6265bis relaxations.
 * @param header - Raw value of the `Cookie:` request header, or an empty
 *                 string when the header is absent.
 * @returns A map of cookie names to values, allocated fresh per call with a
 *          `null` prototype so cookie names that collide with
 *          `Object.prototype` members (`toString`, `__proto__`,
 *          `constructor`, …) are stored and read as ordinary own
 *          properties. Use `Object.hasOwn(map, name)` for existence checks —
 *          the map itself carries no `hasOwnProperty`.
 * @remarks
 * Pure function, no side effects, no external dependencies. Zero allocation
 * for empty input is intentionally NOT attempted — the caller owns the
 * returned map and may mutate it (e.g. to memoize on a request envelope),
 * so a shared frozen sentinel would be unsafe.
 *
 * Parsing rules:
 *
 *   - Split on `;`, trim each cookie pair.
 *   - Split each pair on the FIRST `=` — the value may contain `=`
 *     characters (common in base64-encoded session tokens).
 *   - A pair without `=` (or with nothing before it) is skipped.
 *     rfc6265bis §5.6 reads such a pair as a NAMELESS cookie — empty
 *     name carrying the whole string as value — which can never be
 *     addressed by name. The Go gateway's extractor and Express /
 *     npm-`cookie` skip these pairs identically, so SDK⇄gateway pair
 *     consistency and spec semantics agree here.
 *   - Names and values are trimmed of surrounding whitespace
 *     (rfc6265bis §5.6 step 4), so `sid =abc` resolves as `sid` on
 *     both sides of the wire.
 *   - Duplicate names resolve to the first occurrence. rfc6265bis
 *     §5.8.3 orders the UA's serialization longest-path-first (older
 *     creation-time breaking ties) and §4.2.2 tells servers not to rely
 *     on the order; first-wins therefore reads the most-path-specific
 *     cookie under conformant UA ordering.
 *   - Values wrapped in double quotes have the quotes stripped.
 *     Conscious divergence: rfc6265bis §4.1.1 keeps DQUOTEs as part of
 *     the value; stripping matches Go `net/http` and the JS ecosystem,
 *     and the Go gateway strips identically.
 *   - Percent-encoded name / value segments are decoded; malformed percent
 *     sequences fall back to the raw string because `decodeURIComponent`
 *     would otherwise throw and lose the entire parse.
 *
 * This parser is intentionally decoupled from any specific cookie cache
 * layer — the `@GatewayCookie()` decorator wraps it with `Symbol`-based
 * per-request caching separately.
 * @example
 * ```ts
 * parseCookies('sid=abc; theme=dark');
 * // → { sid: 'abc', theme: 'dark' }
 * ```
 */
export const parseCookies = (header: string): Record<string, string> => {
  // Null prototype: a default-prototype object silently swallows cookies
  // named after Object.prototype members (`out['toString'] ??= v` sees the
  // inherited function as non-nullish and skips the write) and leaks
  // inherited functions to readers of absent names.
  const out: Record<string, string> = Object.create(null) as Record<string, string>;

  if (header.length === 0) {
    return out;
  }

  const pairs = header.split(';');

  for (const rawPair of pairs) {
    const pair = rawPair.trim();

    if (pair.length === 0) {
      continue;
    }

    const eqIndex = pair.indexOf('=');

    // No `=` at all, or nothing before it: a nameless cookie per
    // rfc6265bis §5.6 — skip (see the parsing rules in the TSDoc above).
    if (eqIndex < 1) {
      continue;
    }

    const name = pair.slice(0, eqIndex).trim();
    let value = pair.slice(eqIndex + 1).trim();

    if (
      value.length >= 2 &&
      value.charCodeAt(0) === 0x22 &&
      value.charCodeAt(value.length - 1) === 0x22
    ) {
      value = value.slice(1, -1);
    }

    const decodedName = safeDecode(name);
    const decodedValue = safeDecode(value);

    out[decodedName] ??= decodedValue;
  }

  return out;
};

/**
 * `decodeURIComponent` throws on malformed percent sequences. Real-world
 * `Cookie:` headers sometimes contain raw `%` characters that are not
 * followed by valid hex pairs, and losing the entire parse over one bad
 * cookie is worse than surfacing the raw bytes.
 */
const safeDecode = (input: string): string => {
  if (input.indexOf('%') === -1) {
    return input;
  }

  try {
    return decodeURIComponent(input);
  } catch {
    return input;
  }
};
