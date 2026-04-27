import type { IErrorBodyBuildResult } from './error-body-build-result.interface';
import type { IGatewayRequest } from '../../types/gateway-request.interface';

/**
 * Contract for translating an arbitrary thrown value into an HTTP status
 * code and a wire-ready response body.
 * @remarks
 * The SDK takes no position on what an error body should look like — this
 * contract is the single seam where applications decide. The default
 * implementation (`DefaultErrorBodyFactory`) matches NestJS's built-in
 * `BaseExceptionFilter` output byte-for-byte so the gateway is transparent:
 * for any `HttpException` subclass it forwards `getStatus()` +
 * `getResponse()`, and for anything else it emits the same generic
 * `{ statusCode: 500, message: 'Internal server error' }` shape NestJS uses
 * for unrecognised throws.
 *
 * Applications that need a richer error format (correlation fields, RFC 9457
 * (formerly RFC 7807) problem+json, i18n codes, domain-specific metadata)
 * provide their own `IErrorBodyFactory` implementation and bind it against
 * the `GATEWAY_ERROR_BODY_FACTORY` token. The factory returns whatever body
 * shape the project's HTTP contract requires — the gateway transports the
 * result verbatim.
 *
 * Implementations should be stateless. DI-injectable state (logger, config,
 * metrics) is acceptable, but per-request state is not — the same factory
 * instance is reused for every error across every concurrent invocation.
 */
export interface IErrorBodyFactory {
  /**
   * Translate a thrown value into a `{ status, body }` pair.
   * @param error - The value thrown by the handler. Typed as `unknown`
   *                because JavaScript allows any value to be thrown —
   *                strings, numbers, plain objects, or `null` — and the
   *                factory is the single layer responsible for deciding
   *                which of those become user-facing HTTP responses.
   * @param request - The inbound request envelope. Ignored by the default
   *                  factory but available for implementations that want to
   *                  weave request-specific context into the error body
   *                  (user-agent-aware formatting, trace id propagation,
   *                  locale-aware messages, etc.).
   */
  build(error: unknown, request: IGatewayRequest): IErrorBodyBuildResult;
}
