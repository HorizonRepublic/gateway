/**
 * Module-private `Symbol` used to stash the per-request response
 * accumulator on the NestJS RPC envelope.
 * @remarks
 * Stored under a `Symbol` key (not a string) so the slot stays invisible
 * to user-land TypeScript types — the envelope still looks like a plain
 * `IGatewayRequest` to handler code while the interceptor and the
 * `@GatewayResponse()` decorator share a reliable mutation target at
 * runtime.
 *
 * Exported ONLY to the two collaborators that need it — the
 * `GatewayResponse` parameter decorator and the
 * `GatewayResponseInterceptor`. Do NOT re-export it from the package
 * barrel: user code must go through `@GatewayResponse()` to touch the
 * accumulator.
 *
 * Performance notes:
 *
 *   - Module-level constant: one allocation at SDK load, zero per-request
 *     cost.
 *   - `Symbol` property access on an object is a direct hidden-class
 *     lookup in V8 (~2-3ns), strictly cheaper than `WeakMap.get`
 *     (~50-100ns) or `AsyncLocalStorage.getStore()` (~20-50ns with async
 *     chain overhead).
 */
export const RESPONSE_ACCUMULATOR_KEY = Symbol('gateway-sdk:response-accumulator');
