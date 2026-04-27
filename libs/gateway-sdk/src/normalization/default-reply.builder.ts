import { Injectable } from '@nestjs/common';

import type { IGatewayReplyBuilder } from './contracts/reply-builder.interface';
import type { IGatewayErrorBody } from '../types/gateway-error-body.interface';
import type { IGatewayReply } from '../types/gateway-reply.interface';

/**
 * Default `IGatewayReplyBuilder` implementation. Produces plain JSON
 * envelopes with an empty headers map on both the success and error paths.
 * @remarks
 * This class is the only place in the SDK allowed to construct
 * `IGatewayReply` values. All other code paths (response interceptor,
 * exception filter) delegate here via the `IGatewayReplyBuilder` interface
 * so that envelope shape stays a single-source-of-truth concern. Whenever
 * the wire format evolves, this is the single file to audit.
 *
 * Both success and error replies ship with an empty headers map by default.
 * The Go gateway transport layer stamps `Content-Type: application/json`
 * and `X-Request-Id` as part of its own response-writing pass, so any
 * headers this builder set would be overwritten anyway — keeping the
 * default map empty avoids the illusion of control over transport-layer
 * concerns and matches NestJS's own `BaseExceptionFilter`, which emits
 * errors with the same `application/json` content type as successful
 * responses.
 *
 * Bind a custom implementation against the `GATEWAY_REPLY_BUILDER` token
 * when you need alternative wire formats, content negotiation, or
 * cross-cutting reply enrichment (e.g. an always-appended `X-Api-Version`
 * header).
 * @example
 * ```ts
 * import { GatewayModule } from '@horizon-republic/gateway-sdk';
 * import { MyReplyBuilder } from './my-reply.builder';
 *
 * GatewayModule.forRoot({ replyBuilder: MyReplyBuilder });
 * ```
 */
@Injectable()
export class DefaultGatewayReplyBuilder implements IGatewayReplyBuilder {
  public success<TBody>(
    status: number,
    body: TBody | null,
    headers?: Readonly<Record<string, readonly string[]>>,
  ): IGatewayReply<TBody> {
    return {
      status,
      headers: headers ?? {},
      // Coerce `undefined` to `null` so the wire envelope shape stays
      // deterministic across void and explicit-null handler returns.
      // `JSON.stringify` omits `undefined` fields entirely, which would
      // produce different byte shapes for semantically identical 204
      // responses — a real bug for the Go gateway that decodes the reply
      // into a fixed struct.
      body: body ?? null,
    };
  }

  public error(
    status: number,
    body: IGatewayErrorBody,
    headers?: Readonly<Record<string, readonly string[]>>,
  ): IGatewayReply<IGatewayErrorBody> {
    return {
      status,
      headers: headers ?? {},
      body,
    };
  }
}
