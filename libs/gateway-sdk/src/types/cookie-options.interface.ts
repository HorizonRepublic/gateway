/**
 * RFC 6265 §4.1.1 cookie attributes accepted by `IGatewayResponse.cookie()`
 * and `.clearCookie()`.
 * @remarks
 * All fields are optional — omitting a field omits the corresponding attribute
 * from the serialized `Set-Cookie` header value. The gateway SDK serializes
 * these locally via a small helper; no `cookie` npm dependency is required.
 * The helper expects values to already be wire-safe (no control characters
 * in `value`, no CRLF in `domain` / `path`); callers that accept
 * user-controlled input must sanitize upstream.
 */
export interface ICookieOptions {
  /**
   * Host the cookie is valid for, e.g. `.example.com`. Omitting binds the
   * cookie to the exact host that returned it.
   */
  readonly domain?: string;

  /**
   * URL path prefix the cookie is scoped to, e.g. `/api`. Omitting defaults
   * to `/` per RFC 6265 §5.1.4.
   */
  readonly path?: string;

  /**
   * Absolute expiry time. Serialized via `Date.toUTCString()` into the
   * `Expires` attribute. Prefer `maxAge` for relative lifetimes — it is more
   * robust against client-side clock skew.
   */
  readonly expires?: Date;

  /**
   * Relative lifetime in seconds. Serialized as `Max-Age=<n>`. `0` expires
   * the cookie immediately (used by `clearCookie`).
   */
  readonly maxAge?: number;

  /**
   * When true, emits the `HttpOnly` flag so client-side JavaScript cannot
   * read the cookie via `document.cookie`. Essential for session tokens.
   */
  readonly httpOnly?: boolean;

  /**
   * When true, emits the `Secure` flag so the cookie is only sent over HTTPS.
   * Required by modern browsers for cookies with `SameSite=None`.
   */
  readonly secure?: boolean;

  /**
   * Cross-site request semantics. `'strict'` blocks all cross-site requests;
   * `'lax'` allows top-level navigation; `'none'` allows all cross-site
   * requests (requires `secure: true`).
   */
  readonly sameSite?: 'strict' | 'lax' | 'none';

  /**
   * Emits the `Partitioned` attribute (CHIPS — Cookies Having Independent
   * Partitioned State) so the cookie is scoped to the embedding top-level
   * site.
   * @remarks
   * Chrome 118+ requires `Partitioned` on third-party cookies used in
   * cross-site contexts; without it the cookie is blocked by the
   * third-party cookie deprecation. Partitioned cookies require
   * `Secure: true` — browsers ignore the cookie entirely otherwise; the
   * serializer auto-promotes `secure` when it is left undefined (same
   * policy as SameSite=None) and loud-warns on an explicit
   * `secure: false`. No SameSite value is mandated by CHIPS itself.
   * Standardisation note: CHIPS is browser-shipped but pre-standard —
   * the original individual draft expired and the active successor is
   * the httpbis layered-cookies work; the attribute name is stable in
   * practice.
   */
  readonly partitioned?: boolean;
}
