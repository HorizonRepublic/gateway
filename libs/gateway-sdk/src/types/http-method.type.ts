/**
 * HTTP methods supported by Horizon Gateway.
 * @remarks
 * Canonical list of verbs that `@GatewayRoute` accepts and that `gateway-server`
 * dispatches. Any extension (e.g. custom verbs) requires a breaking change and
 * a synchronized release of `@horizon-republic/gateway-sdk` and `gateway-server`.
 */
export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE' | 'HEAD' | 'OPTIONS';
