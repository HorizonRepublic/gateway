/**
 * Module-private `Symbol` used to cache the parsed `Cookie:` header on an
 * RPC envelope.
 * @remarks
 * The first `@GatewayCookie()` injection on a request parses the raw
 * `cookie` header once and stashes the result under this key. Subsequent
 * injections on the same request read through the cache, which matters
 * when a handler has multiple `@GatewayCookie('name')` parameters: the
 * parse cost is paid exactly once per request regardless of how many
 * cookie names the handler extracts.
 *
 * The `Symbol` is NOT exported from the package barrel — only the
 * `@GatewayCookie()` parameter decorator touches this slot.
 *
 * Storage choice mirrors `RESPONSE_ACCUMULATOR_KEY`: a module-level
 * `Symbol` constant gives O(1) hidden-class access in V8 (~2-3ns),
 * strictly cheaper than `WeakMap.get` (~50-100ns) or
 * `AsyncLocalStorage.getStore()` (~20-50ns).
 */
export const PARSED_COOKIES_KEY = Symbol('gateway-sdk:parsed-cookies');
