/**
 * Parses the value of an HTTP `Cookie:` request header into a name-to-value
 * map per RFC 6265 §5.4 plus RFC 6265bis relaxations.
 * @param header - Raw value of the `Cookie:` request header, or an empty
 *                 string when the header is absent.
 * @returns A plain object mapping cookie names to values. Always a freshly
 *          allocated map so the caller can cache it without worrying about
 *          sharing.
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
 *   - A pair without `=` is treated as a flag cookie with an empty value.
 *     Conscious divergence: rfc6265bis §5.6 reads a no-`=` pair as an
 *     EMPTY NAME carrying that value, and Express / npm-`cookie` skip
 *     the pair entirely; the flag-cookie reading is kept because the Go
 *     gateway's extractor agrees with it — SDK⇄gateway pair consistency
 *     outweighs either alternative.
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
  const out: Record<string, string> = {};

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

    let name: string;
    let value: string;

    if (eqIndex === -1) {
      name = pair;
      value = '';
    } else {
      name = pair.slice(0, eqIndex).trim();
      value = pair.slice(eqIndex + 1).trim();
    }

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
