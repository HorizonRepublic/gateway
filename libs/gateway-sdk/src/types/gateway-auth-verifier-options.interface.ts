/**
 * Options accepted by the `@GatewayAuthVerifier` decorator.
 * @remarks
 * The verifier id is the logical handle that routes reference via
 * `auth: { verifier: '<id>' }`. It must be unique within one
 * `handler_registry` KV bucket; collisions are a build-time `WARN` on the
 * gateway side and resolve deterministically to the entry under the
 * lexicographically-smallest KV key.
 */
export interface IGatewayAuthVerifierOptions {
  /**
   * Unique identifier referenced by routes.
   * @remarks
   * Must be URL-safe and shorter than 64 characters. Validated by the
   * decorator at decoration time so typos never reach the KV bucket.
   */
  readonly id: string;

  /**
   * When true, routes that declare `auth: true` or `auth: {}` without an
   * explicit `verifier` field resolve to this verifier.
   * @remarks
   * At most one verifier in the entire KV bucket may set this flag;
   * collisions are logged as `ERROR` at routing-table build time by the
   * gateway, which then picks the first-seen verifier deterministically.
   */
  readonly default?: boolean;
}
