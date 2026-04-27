/**
 * Default HTTP status for successful handler returns that produce a response
 * body.
 * @remarks
 * Applied by `DefaultStatusResolver` whenever `@GatewayRoute` omits an
 * explicit `statusCode` and the handler returns a non-null, non-undefined
 * value. Kept as a standalone literal (rather than reused from
 * `@nestjs/common`'s `HttpStatus`) so that the gateway transport and the
 * SDK share a single source of truth without introducing a runtime
 * dependency on `@nestjs/common` in hot paths.
 */
export const DEFAULT_STATUS_OK = 200 as const;

/**
 * Default HTTP status for handler returns that carry no response body.
 * @remarks
 * Applied by `DefaultStatusResolver` whenever a handler returns `null`,
 * `undefined`, or is declared `void`, and `@GatewayRoute` does not override
 * `statusCode`. `204` is the canonical "success with no body" status per
 * RFC 7231 §6.3.5 and permits the transport to skip body encoding entirely.
 */
export const DEFAULT_STATUS_NO_CONTENT = 204 as const;

/**
 * Default HTTP status returned for unhandled throws that are not
 * `HttpException` instances.
 * @remarks
 * Used by `DefaultErrorBodyFactory` as the last-resort fallback when an
 * uncaught error reaches the gateway's exception filter. `HttpException`
 * subclasses carry their own status via `exception.getStatus()`; everything
 * else (native `Error`, third-party throws, unknown payloads) is normalized
 * to `500` to guarantee the filter never emits a response without a status.
 */
export const DEFAULT_STATUS_INTERNAL_ERROR = 500 as const;
