import { HttpException, Injectable } from '@nestjs/common';

import { DEFAULT_STATUS_INTERNAL_ERROR } from '../constants/defaults.constant';

import type { IErrorBodyBuildResult } from './contracts/error-body-build-result.interface';
import type { IErrorBodyFactory } from './contracts/error-body-factory.interface';
import type { IGatewayErrorBody } from '../types/gateway-error-body.interface';
import type { IGatewayRequest } from '../types/gateway-request.interface';

/**
 * Default `IErrorBodyFactory` implementation — a thin pass-through layer
 * that mirrors NestJS's `BaseExceptionFilter` behaviour so the gateway's
 * error envelope is byte-for-byte indistinguishable from what a direct
 * NestJS HTTP controller would produce for the same exception.
 * @remarks
 * The factory recognizes exactly one base contract: `HttpException` from
 * `@nestjs/common`. Every NestJS built-in (`NotFoundException`,
 * `BadRequestException`, `UnauthorizedException`, …) and every user
 * subclass of `HttpException` flows through the same passthrough path:
 *
 *   1. Status comes from `exception.getStatus()`.
 *   2. Body comes from `exception.getResponse()` verbatim — no
 *      normalization, no re-keying, no projection. If the response is a
 *      plain string (the rare `new HttpException('literal', status)`
 *      case), the factory wraps it into `{ statusCode, message }` to
 *      match NestJS's own `BaseExceptionFilter` behaviour.
 *
 * Anything that is NOT an `HttpException` instance — plain `Error`,
 * thrown strings, thrown numbers, thrown `null`, values from broken
 * third-party libraries — produces the same generic fallback NestJS emits
 * for unrecognised throws: status `500`, body
 * `{ statusCode: 500, message: 'Internal server error' }`. The original
 * error's `.message`, `.name`, `.stack`, and any other field are NEVER
 * included in the response body. Operators recover the original error
 * via server-side logs correlated by `requestId`, not via HTTP response
 * inspection.
 *
 * There is deliberately no magic-marker path (`isDomainException`, etc.)
 * and no duck-typed recognition. Users who want a domain-specific error
 * hierarchy either:
 *
 *   a. Extend `HttpException` directly (the NestJS-idiomatic shortest
 *      path) — the default factory handles them automatically; or
 *   b. Bind a custom `IErrorBodyFactory` via `GATEWAY_ERROR_BODY_FACTORY`
 *      that understands their specific exception shape.
 *
 * The gateway itself never prescribes an error taxonomy — it is simply
 * the transport.
 * @example
 * ```ts
 * import { NotFoundException } from '@nestjs/common';
 *
 * // Inside any @GatewayRoute-decorated handler:
 * throw new NotFoundException(`User ${id} not found`);
 * // → gateway emits HTTP 404 with body:
 * // { "statusCode": 404, "message": "User 3 not found", "error": "Not Found" }
 * ```
 */
@Injectable()
export class DefaultErrorBodyFactory implements IErrorBodyFactory {
  /**
   * Translate a thrown value into an HTTP status + response body.
   * @param error The thrown value. Only `HttpException` subclasses are
   *              recognized; everything else falls through to the generic
   *              500 path.
   * @param _request Ignored. Present for interface compatibility;
   *                 applications that need request-aware error shapes
   *                 should bind a custom factory.
   */
  public build(error: unknown, _request: IGatewayRequest): IErrorBodyBuildResult {
    if (error instanceof HttpException) {
      return {
        status: error.getStatus(),
        body: toHttpExceptionBody(error),
      };
    }

    return {
      status: DEFAULT_STATUS_INTERNAL_ERROR,
      body: {
        statusCode: DEFAULT_STATUS_INTERNAL_ERROR,
        message: 'Internal server error',
      },
    };
  }
}

/**
 * Reproduce NestJS `BaseExceptionFilter`'s HttpException body assembly
 * without copying its implementation: if `getResponse()` returns a plain
 * string the filter wraps it into `{ statusCode, message }`; otherwise
 * the response object is passed through verbatim so subclass-specific
 * fields (`error`, custom codes, structured details) survive on the wire.
 *
 * Kept file-private because it has no use outside the default factory —
 * callers that need alternative HttpException shaping should implement
 * their own `IErrorBodyFactory` rather than reuse this helper.
 */
const toHttpExceptionBody = (error: HttpException): IGatewayErrorBody => {
  const response = error.getResponse();

  if (typeof response === 'string') {
    return {
      statusCode: error.getStatus(),
      message: response,
    };
  }

  return response as IGatewayErrorBody;
};
