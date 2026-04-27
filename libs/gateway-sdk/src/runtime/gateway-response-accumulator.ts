import { serializeCookie } from '../normalization/cookie-serializer';

import type { ICookieOptions } from '../types/cookie-options.interface';
import type { IGatewayResponse } from '../types/gateway-response.interface';

/**
 * Default status for `redirect()` when no status is explicitly passed. 302
 * Found matches Express / Fastify behavior.
 */
const DEFAULT_REDIRECT_STATUS = 302;

/**
 * Unix-epoch date used by `clearCookie()` as the `Expires` attribute so
 * ancient clients that ignore `Max-Age` still delete the cookie.
 * Pre-allocated once at module load instead of constructing a new Date
 * per call.
 */
const EPOCH = new Date(0);

/**
 * Mutable write buffer for the success-path reply shape.
 * @remarks
 * Implements `IGatewayResponse` as a plain class — every method mutates
 * `this` and returns `this` for chainable DX. Plain class (not Proxy) is
 * a deliberate perf choice: method invocation + self-return is ~3ns in
 * V8, strictly cheaper than a Proxy trap.
 *
 * Object lifecycle is managed by `gateway-response-pool`:
 * `acquireAccumulator` returns a fresh or recycled instance,
 * `releaseAccumulator` calls `reset()` and returns the instance to the
 * free list. Handler code never calls `reset()` directly.
 *
 * **Express-convention throw semantics.** Accumulator state is only
 * consumed on the success path by `GatewayResponseInterceptor`. If the
 * handler throws, `finalize` releases the accumulator and its state is
 * discarded — the exception filter builds its reply purely from the
 * thrown exception. This matches Express / Fastify throw behavior and is
 * the documented contract.
 */
export class GatewayResponseAccumulator implements IGatewayResponse {
  /**
   * Override status for the success path. `undefined` means "no override,
   * let the status resolver decide".
   */
  public statusCode: number | undefined;

  /**
   * Response headers as a multi-value map, with lowercase keys. The
   * object identity is preserved across resets so the pool's consumers
   * can rely on it.
   */
  public readonly headers: Record<string, string[]> = {};

  /**
   * Cookie attribute defaults from
   * `GatewayModule.forRoot({ defaults: { cookies } })`. Merged under
   * per-cookie `options` in `cookie()` so that module-level settings
   * (e.g. `{ httpOnly: true, secure: true, path: '/' }`) are applied
   * without repeating them on every `res.cookie()` call. Reset to an
   * empty object by `reset()` so that pooled instances do not leak
   * defaults across requests.
   */
  public cookieDefaults: Partial<ICookieOptions> = {};

  public status(code: number): this {
    this.statusCode = code;

    return this;
  }

  public header(name: string, value: string): this {
    this.headers[name.toLowerCase()] = [value];

    return this;
  }

  public appendHeader(name: string, value: string): this {
    const key = name.toLowerCase();
    const existing = this.headers[key];

    if (existing === undefined) {
      this.headers[key] = [value];
    } else {
      existing.push(value);
    }

    return this;
  }

  public removeHeader(name: string): this {
    Reflect.deleteProperty(this.headers, name.toLowerCase());

    return this;
  }

  public cookie(name: string, value: string, options?: ICookieOptions): this {
    return this.appendHeader(
      'set-cookie',
      serializeCookie(name, value, options, this.cookieDefaults),
    );
  }

  public clearCookie(name: string, options?: Pick<ICookieOptions, 'path' | 'domain'>): this {
    const clearOptions: ICookieOptions = {
      maxAge: 0,
      expires: EPOCH,
      ...(options?.path !== undefined ? { path: options.path } : {}),
      ...(options?.domain !== undefined ? { domain: options.domain } : {}),
    };

    return this.cookie(name, '', clearOptions);
  }

  public redirect(
    url: string,
    status: 301 | 302 | 303 | 307 | 308 = DEFAULT_REDIRECT_STATUS,
  ): this {
    return this.status(status).header('location', url);
  }

  /**
   * Clear all accumulated state in place so the instance can be returned
   * to the pool and handed to the next request. Keeps the `headers`
   * object identity intact — callers that capture a reference (for
   * example the interceptor that reads it during `map()`) see a
   * consistent shape across reuses.
   */
  public reset(): void {
    this.statusCode = undefined;
    this.cookieDefaults = {};

    for (const key of Object.keys(this.headers)) {
      Reflect.deleteProperty(this.headers, key);
    }
  }
}
