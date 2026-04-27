import type { HttpMethod } from './http-method.type';

/**
 * HTTP-routing metadata stored under `meta.http` in the `handler_registry`
 * NATS KV bucket.
 * @remarks
 * Written by the `@GatewayRoute` decorator (via `@MessagePattern`'s
 * `extras.meta` passthrough in `nestjs-jetstream`) and read by
 * `gateway-server` to build its HTTP routing table.
 *
 * **Extensibility policy:** new optional fields may be added without a major
 * version bump. Both the SDK and the gateway MUST tolerate unknown fields
 * gracefully (ignore them). Removing or renaming a field IS a breaking
 * change and requires a synchronized release of both packages.
 *
 * Reserved field names for planned extensions (do not collide):
 *   - `encoding`  — `'json' | 'protobuf'` wire format for the endpoint body
 *   - `schema`    — refs to schema descriptors, e.g. `{ request, response }`
 *   - `auth`      — auth strategy descriptor
 *   - `rateLimit` — per-route rate limit config
 *   - `cors`      — CORS policy for the endpoint
 *   - `version`   — API version marker, e.g. `'v1'`
 */
export interface IGatewayHttpMeta {
  /**
   * HTTP method the gateway will accept for this handler.
   * @remarks
   * Combined with `path` this field forms the dispatch key of the gateway's
   * routing trie. It is what makes `@GatewayRoute` *additive* over the plain
   * `@MessagePattern` contract: the same handler stays callable as a pure
   * RPC while also being reachable through the gateway under the declared
   * HTTP verb.
   */
  readonly method: HttpMethod;

  /**
   * Path template with `:param` placeholders, e.g. `/users/:id`.
   * @remarks
   * The gateway's routing trie extracts positional parameters by placeholder
   * name and passes them to the handler via `IGatewayRequest.params`.
   */
  readonly path: string;

  /**
   * HTTP status returned on a successful handler return. Optional.
   * @remarks
   * If omitted, the default rules apply: `200 OK` for non-null returns,
   * `204 No Content` for `null` / `undefined` / `void` returns.
   */
  readonly statusCode?: number;
}
