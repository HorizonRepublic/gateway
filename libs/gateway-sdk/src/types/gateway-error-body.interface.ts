/**
 * Shape returned inside `IGatewayReply.body` when a handler throws.
 * @remarks
 * Deliberately loose — an arbitrary JSON object whose exact fields are
 * determined by whatever `IErrorBodyFactory` the application bound. The
 * SDK's gateway transport layer treats this as an opaque payload and
 * forwards it verbatim to the HTTP client without inspecting or
 * rewriting any field.
 *
 * The default `DefaultErrorBodyFactory` matches NestJS's built-in
 * `BaseExceptionFilter` output byte-for-byte: for any error extending
 * `HttpException` it emits `{ statusCode, message, error, ... }` (the
 * exact shape of `HttpException.getResponse()` for every Nest built-in
 * subclass), and for unknown throws it emits
 * `{ statusCode: 500, message: 'Internal server error' }`. That makes
 * the gateway's error envelope indistinguishable on the wire from a
 * request that went directly to a Nest HTTP controller — the gateway is
 * a transparent proxy for error semantics.
 *
 * Applications that want a custom error shape (extra correlation fields,
 * RFC 9457 (formerly RFC 7807) problem+json, i18n codes, domain-specific
 * metadata) bind their own `IErrorBodyFactory` via
 * `GATEWAY_ERROR_BODY_FACTORY` and return whatever object shape makes
 * sense for their contract. The gateway does not prescribe any fields.
 */
export interface IGatewayErrorBody {
  readonly [key: string]: unknown;
}
