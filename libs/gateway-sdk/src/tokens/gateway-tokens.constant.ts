/**
 * Dependency injection token for `IGatewayReplyBuilder` implementations.
 * @remarks
 * Override the default builder by providing your own class against this token
 * in `GatewayModule.forRoot({ replyBuilder: MyCustomBuilder })`, which wires
 * it under `{ provide: GATEWAY_REPLY_BUILDER, useClass: MyCustomBuilder }`.
 *
 * A `Symbol` is preferred over a string constant because symbols are
 * guaranteed unique across duplicated module graphs (common in monorepo
 * workspaces that end up with two copies of `@horizon-republic/gateway-sdk`
 * in the dependency tree) and cannot collide with consumer-land provider
 * tokens.
 */
export const GATEWAY_REPLY_BUILDER = Symbol('gateway-reply-builder');

/**
 * Dependency injection token for `IStatusResolver` implementations.
 * @remarks
 * Bind a custom resolver to implement project-specific rules on top of the
 * baseline `@GatewayRoute({ statusCode })` override, for example "all handlers
 * whose pattern ends in `.create` default to `201`" or "all handlers tagged
 * with a custom metadata marker default to `202`". The resolver is consulted
 * once per request after the handler resolves and before the reply builder
 * writes the response.
 */
export const GATEWAY_STATUS_RESOLVER = Symbol('gateway-status-resolver');

/**
 * Dependency injection token for `IErrorBodyFactory` implementations.
 * @remarks
 * Bind a custom factory to shape error responses — for example to integrate
 * with a non-Nest error hierarchy, to apply project-specific error-code
 * mapping (`ProblemDetails`, internal taxonomies), or to strip sensitive
 * fields from third-party errors before they reach the wire. The default
 * implementation recognizes Nest `HttpException` via `instanceof`, forwards
 * `exception.getStatus()` and `exception.getResponse()` verbatim, and falls
 * back to `DEFAULT_STATUS_INTERNAL_ERROR` for everything else.
 */
export const GATEWAY_ERROR_BODY_FACTORY = Symbol('gateway-error-body-factory');

/**
 * Dependency injection token for module-level endpoint defaults configured
 * via `GatewayModule.forRoot({ defaults })`.
 * @remarks
 * The value behind this token is a frozen `IGatewayDefaults` object —
 * the same object installed as the module-scoped defaults snapshot that
 * `@GatewayRoute`'s lazy `meta` getter merges into route metadata at
 * read time. Via DI it is consumed by `GatewayResponseInterceptor` for
 * response-side defaults (for example `cookies`).
 */
export const GATEWAY_DEFAULTS: unique symbol = Symbol('gateway-defaults');
