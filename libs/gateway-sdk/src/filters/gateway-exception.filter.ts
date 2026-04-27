import {
  Catch,
  Inject,
  Injectable,
  Logger,
  type ArgumentsHost,
  type ExceptionFilter,
} from '@nestjs/common';

import { of, type Observable } from 'rxjs';

import {
  GATEWAY_ERROR_BODY_FACTORY,
  GATEWAY_REPLY_BUILDER,
} from '../tokens/gateway-tokens.constant';

import type { IErrorBodyFactory } from '../normalization/contracts/error-body-factory.interface';
import type { IGatewayReplyBuilder } from '../normalization/contracts/reply-builder.interface';
import type { IGatewayErrorBody } from '../types/gateway-error-body.interface';
import type { IGatewayReply } from '../types/gateway-reply.interface';
import type { IGatewayRequest } from '../types/gateway-request.interface';

/**
 * Lowest HTTP status that is logged at `error` level. Anything strictly
 * below this is a deliberate, client-facing signal (404, 401, 422...) and
 * gets emitted silently so legitimate 4xx responses do not flood the logs.
 */
const SERVER_ERROR_THRESHOLD = 500;

/**
 * Catches any exception thrown from an `@GatewayRoute`-decorated handler
 * and serializes it into an `IGatewayReply` envelope with the appropriate
 * HTTP status.
 * @remarks
 * **Locally attached** via `@UseFilters(GatewayExceptionFilter)` inside
 * the `@GatewayRoute` decorator — never registered globally. Because it
 * is bound only to gateway-exposed handlers, every invocation is
 * guaranteed to originate from one, so there is no need to discriminate
 * between gateway and non-gateway exception origins at catch time.
 *
 * Policy is delegated to injected contracts:
 *
 *   - `IErrorBodyFactory` via `GATEWAY_ERROR_BODY_FACTORY` — extracts a
 *     `(status, body)` pair from an arbitrary throw. The default
 *     implementation recognizes NestJS `HttpException` via `instanceof`
 *     and falls through to `500` for anything else.
 *   - `IGatewayReplyBuilder` via `GATEWAY_REPLY_BUILDER` — assembles the
 *     outbound envelope.
 *
 * Pipe and guard exceptions are also caught here: NestJS runs exception
 * filters after pipes and guards throw, so validation errors (e.g. from
 * typia pipes) are correctly serialized into structured HTTP responses
 * rather than surfacing as raw 500s on the client side.
 *
 * Logging policy: exceptions whose resolved status is `>= 500` are
 * logged at `error` level with request context (pattern, matched path,
 * request id, remote addr); anything `< 500` is treated as an expected
 * client-facing signal and emitted silently.
 * @example
 * ```ts
 * // Attached automatically by @GatewayRoute — consumers never reference
 * // this class directly. Throw any NestJS HttpException inside a handler
 * // and the filter produces the matching envelope with status +
 * // getResponse() body.
 * @GatewayRoute({ pattern: 'users.get', method: 'GET', path: '/users/:id' })
 * getUser(@GatewayParam('id') id: string) {
 *   const user = this.users.findById(id);
 *   if (!user) {
 *     throw new NotFoundException('User not found');
 *   }
 *   return user;
 * }
 * ```
 */
@Catch()
@Injectable()
export class GatewayExceptionFilter implements ExceptionFilter {
  private readonly logger = new Logger(GatewayExceptionFilter.name);

  public constructor(
    @Inject(GATEWAY_REPLY_BUILDER)
    private readonly replyBuilder: IGatewayReplyBuilder,
    @Inject(GATEWAY_ERROR_BODY_FACTORY)
    private readonly errorBodyFactory: IErrorBodyFactory,
  ) {}

  /**
   * Serializes the exception into an `IGatewayReply` envelope and emits it
   * as a successful Observable value.
   * @remarks
   * The return type is `Observable<IGatewayReply<IGatewayErrorBody>>` —
   * NOT a bare envelope — because NestJS microservices transports
   * (including `@horizon-republic/nestjs-jetstream`) serialize the value
   * each Observable emits as the RPC reply body. Returning a plain object
   * from an RPC exception filter makes NestJS wrap it in the default
   * transport error envelope (`{err, response, isDisposed}`), which the
   * Go gateway's decoder cannot parse as a `GatewayReply` — it sees
   * `status: 0` and surfaces the whole response as a 502 Bad Gateway.
   *
   * Wrapping the envelope in `of(...)` makes NestJS treat it as a normal
   * reply — exactly mirroring what the response interceptor does on the
   * success path — so the wire envelope shape stays identical regardless
   * of whether the handler returned or threw.
   */
  public catch(
    exception: unknown,
    host: ArgumentsHost,
  ): Observable<IGatewayReply<IGatewayErrorBody>> {
    const request = host.switchToRpc().getData<IGatewayRequest>();
    const { status, body } = this.errorBodyFactory.build(exception, request);

    if (status >= SERVER_ERROR_THRESHOLD) {
      this.logServerError(exception, status, request);
    }

    return of(this.replyBuilder.error(status, body));
  }

  /**
   * Emits a single structured error log line for a 5xx exception.
   * @remarks
   * The NestJS `Logger` contract accepts a context object plus a message
   * string; we attach the raw exception under `err` so `nestjs-pino` (or
   * any other structured logger bridge) can serialize its stack via its
   * own error serializer rather than relying on JSON's default `Error`
   * stringification. Request-side context is pulled from the
   * `IGatewayRequest` envelope: pattern + matched path identify the
   * handler, request id correlates the log with gateway access logs,
   * remote addr helps attribute repeated failures to a misbehaving
   * client.
   *
   * The request envelope is nullable in practice — callers that
   * synthesise a test `ArgumentsHost` may not populate
   * `switchToRpc().getData()` — so every field is read defensively. A
   * missing request produces a log entry with only the error, which is
   * still more useful than silence.
   */
  private logServerError(
    exception: unknown,
    status: number,
    request: IGatewayRequest | undefined,
  ): void {
    this.logger.error({
      msg: 'Gateway Handler Error',
      err: exception,
      status,
      pattern: request?.route.path,
      method: request?.route.method,
      matchedPath: request?.route.matchedPath,
      requestId: request?.meta.requestId,
      remoteAddr: request?.meta.remoteAddr,
    });
  }
}
