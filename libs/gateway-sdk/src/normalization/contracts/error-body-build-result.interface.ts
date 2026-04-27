import type { IGatewayErrorBody } from '../../types/gateway-error-body.interface';

/**
 * Result of translating a thrown value into a gateway error payload.
 * @remarks
 * Separated into a named interface rather than an anonymous `{ status, body }`
 * tuple so it can be referenced in documentation, exported as part of the
 * public API, tested in isolation, and mocked cleanly without re-declaring
 * the shape at every call site.
 *
 * Both fields are `readonly` — once an `IErrorBodyFactory` has decided on a
 * status and body, downstream consumers (reply builder, exception filter,
 * diagnostics) must treat the result as an immutable snapshot. Any
 * adjustments require producing a new `IErrorBodyBuildResult`, which keeps
 * the audit trail clear and prevents accidental mutation between the
 * factory and the wire.
 */
export interface IErrorBodyBuildResult {
  /**
   * HTTP status code to attach to the outgoing reply envelope.
   * @remarks
   * Sourced from `HttpException.getStatus()` for the default factory, or
   * from whatever logic a custom factory applies. Must be a valid HTTP
   * status in the 100-599 range — the Go gateway's decoder rejects
   * anything outside that window as a malformed reply.
   */
  readonly status: number;

  /**
   * Serialized error body that will travel inside `IGatewayReply.body`.
   * @remarks
   * Already shaped for wire transmission — no further transformation by
   * the reply builder. The default factory forwards
   * `HttpException.getResponse()` verbatim, matching NestJS's built-in
   * `BaseExceptionFilter`; custom factories may use any JSON-serializable
   * shape they prefer. Sensitive fields (stack traces, internal
   * identifiers) must be stripped by the factory before the body reaches
   * this structure — the downstream layers (reply builder, exception
   * filter, HTTP transport) never rewrite the body.
   */
  readonly body: IGatewayErrorBody;
}
