import type { IGatewayErrorBody } from '../../types/gateway-error-body.interface';
import type { IGatewayReply } from '../../types/gateway-reply.interface';

/**
 * Contract for constructing `IGatewayReply` envelopes for both success and
 * error paths.
 * @remarks
 * **Single source of truth for reply envelope shape.** All wire-level reply
 * construction MUST go through an `IGatewayReplyBuilder` implementation —
 * neither the response interceptor nor the exception filter is allowed to
 * build envelopes inline. Centralizing envelope assembly behind one seam
 * keeps `status`, `headers`, and `body` ordering consistent across code
 * paths and leaves a single file to audit when the wire format evolves.
 *
 * The default implementation (`DefaultGatewayReplyBuilder`) produces JSON
 * envelopes with an empty headers map. Future implementations may add
 * Content-Type negotiation, alternative wire formats (CBOR, MessagePack),
 * or domain-specific reply enrichment (correlation headers, cache hints) —
 * all swappable via DI by binding a custom class to the
 * `GATEWAY_REPLY_BUILDER` token.
 *
 * Keeping this surface as an interface rather than an abstract class means
 * consumers can adapt existing helper objects without inheritance
 * gymnastics, and the SDK stays framework-agnostic at the type level.
 */
export interface IGatewayReplyBuilder {
  /**
   * Build an envelope for a successful handler return.
   * @template TBody - Type of the handler's return value; propagated to
   *                   `IGatewayReply<TBody>` so downstream consumers that
   *                   narrow the generic in their own signatures keep
   *                   type-level visibility into the payload shape.
   * @param status - Resolved HTTP status code, typically produced by an
   *                 `IStatusResolver` implementation.
   * @param body - Handler return value, or `null` for void/204 responses.
   *               Implementations must not mutate or deep-clone the value;
   *               the envelope holds the reference verbatim.
   * @param headers - Optional multi-value headers map. When omitted
   *                  implementations emit an empty object so the wire
   *                  shape stays stable; each value is an array so
   *                  multi-value headers such as `Set-Cookie` reach the
   *                  Go gateway verbatim.
   */
  success<TBody>(
    status: number,
    body: TBody | null,
    headers?: Readonly<Record<string, readonly string[]>>,
  ): IGatewayReply<TBody>;

  /**
   * Build an envelope for a thrown exception, after it has been translated
   * into an `IGatewayErrorBody` by an `IErrorBodyFactory`.
   * @param status - HTTP status code to return. The default factory sources
   *                 it from `HttpException.getStatus()`; custom factories
   *                 apply whatever logic their error taxonomy requires.
   * @param body - Serialized error body produced by an `IErrorBodyFactory`.
   *               Implementations must forward the body verbatim without
   *               rewriting fields so error semantics stay lossless between
   *               the factory and the wire.
   * @param headers - Optional multi-value headers map. Same contract as the
   *                  success variant; defaults to an empty object.
   */
  error(
    status: number,
    body: IGatewayErrorBody,
    headers?: Readonly<Record<string, readonly string[]>>,
  ): IGatewayReply<IGatewayErrorBody>;
}
