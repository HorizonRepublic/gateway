import { Injectable } from '@nestjs/common';

import { DEFAULT_STATUS_NO_CONTENT, DEFAULT_STATUS_OK } from '../constants/defaults.constant';

import type { IStatusResolver } from './contracts/status-resolver.interface';
import type { IGatewayHttpMeta } from '../types/gateway-http-meta.interface';

/**
 * Default `IStatusResolver` implementation applying the standard resolution
 * rules.
 * @remarks
 * Rules, in precedence order:
 *
 *   1. `httpMeta.statusCode` — if defined (including `0`), return it verbatim.
 *   2. Return value is `null` or `undefined` → `DEFAULT_STATUS_NO_CONTENT` (204).
 *   3. Otherwise → `DEFAULT_STATUS_OK` (200).
 *
 * Falsy non-null values (`0`, `''`, `false`) are treated as valid response
 * bodies and receive `200 OK`, not `204`. This mirrors the HTTP semantic
 * distinction between "empty response" and "response with falsy content" —
 * `null` / `undefined` mean "no body to send", while `0` or `''` mean "the
 * handler genuinely computed this value and the client should see it".
 *
 * Bind a custom implementation against the `GATEWAY_STATUS_RESOLVER` token
 * to override these rules with project-specific logic (e.g. "all `*.create`
 * handlers default to 201", or "handlers tagged with a custom metadata
 * marker default to 202").
 * @example
 * ```ts
 * import { GatewayModule } from '@horizon-republic/gateway-sdk';
 * import { MyStatusResolver } from './my-status.resolver';
 *
 * GatewayModule.forRoot({ statusResolver: MyStatusResolver });
 * ```
 */
@Injectable()
export class DefaultStatusResolver implements IStatusResolver {
  public resolveSuccess(httpMeta: IGatewayHttpMeta, returnValue: unknown): number {
    if (httpMeta.statusCode !== undefined) {
      return httpMeta.statusCode;
    }

    if (returnValue === null || returnValue === undefined) {
      return DEFAULT_STATUS_NO_CONTENT;
    }

    return DEFAULT_STATUS_OK;
  }
}
