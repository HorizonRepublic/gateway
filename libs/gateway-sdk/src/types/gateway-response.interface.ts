import type { ICookieOptions } from './cookie-options.interface';

/**
 * Mutable response-side builder injected into `@GatewayRoute` and
 * `@GatewayAuthVerifier` handlers via `@GatewayResponse()`.
 * @remarks
 * Methods return `this` for chainable DX (`res.status(201).cookie(...).header(...)`).
 * The builder is backed by a pooled accumulator so repeated request-cycle
 * creation is effectively allocation-free at steady state.
 *
 * **Express-convention throw semantics.** When a handler throws after calling
 * any of these methods, the accumulator state is **entirely discarded** —
 * status, headers, and cookies. The `GatewayExceptionFilter` builds its reply
 * envelope purely from the thrown exception and never reads the accumulator.
 * Handlers that need headers on an error path must throw an `HttpException`
 * whose response object carries a `headers` field.
 */
export interface IGatewayResponse {
  /**
   * Override the HTTP status for the success path. Has no effect if the
   * handler throws — the exception filter owns error status. Later calls
   * overwrite earlier ones.
   */
  status(code: number): this;

  /**
   * Set a single-value response header. Header name is normalized to
   * lowercase at write time. Re-calling with the same name replaces the
   * previous value (set-semantics, not append).
   */
  header(name: string, value: string): this;

  /**
   * Append a value to a multi-value response header. Use for `Vary`, `Link`,
   * `Cache-Control` with multiple directives, and anywhere else RFC permits
   * multi-value. `Set-Cookie` is appended automatically by `.cookie()` —
   * callers should not need to call `appendHeader('set-cookie', ...)` directly.
   */
  appendHeader(name: string, value: string): this;

  /**
   * Remove a previously-set header. No-op if the header was never set on
   * this accumulator (gateway-owned defaults like `x-request-id` are not
   * affected — they are stamped on the Go side after the accumulator is read).
   */
  removeHeader(name: string): this;

  /**
   * Set a response cookie. Serializes to an RFC 6265 `Set-Cookie` header
   * value and appends to the `set-cookie` multi-value header slot. Repeated
   * calls with different cookie names each add a separate entry.
   */
  cookie(name: string, value: string, options?: ICookieOptions): this;

  /**
   * Emit a `Set-Cookie` header that instructs the client to delete the
   * named cookie. Equivalent to
   * `cookie(name, '', { ...options, maxAge: 0, expires: new Date(0) })`.
   * The client MUST see matching `path` / `domain` for the deletion to
   * apply — supply them if the cookie was set with non-default values.
   */
  clearCookie(name: string, options?: Pick<ICookieOptions, 'path' | 'domain'>): this;

  /**
   * Convenience for redirect responses. Sets status and the `location`
   * header atomically. Only the five RFC-permitted redirect codes are
   * accepted.
   */
  redirect(url: string, status?: 301 | 302 | 303 | 307 | 308): this;
}
