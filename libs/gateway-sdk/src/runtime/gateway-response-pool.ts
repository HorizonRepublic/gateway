import { GatewayResponseAccumulator } from './gateway-response-accumulator';

/**
 * Upper bound on the free-list size. A value of 1024 covers realistic burst
 * traffic on a single NestJS process without growing unboundedly on a request
 * storm. Tune up if profiling shows cold-pool misses dominating the hot path.
 */
const POOL_MAX = 1024;

/**
 * LIFO free list. Recently-released instances stay warm in CPU cache and
 * benefit from locality on the next acquire.
 */
const freeList: GatewayResponseAccumulator[] = [];

/**
 * Checkout an accumulator from the pool. Returns the most-recently-released
 * instance if the pool is non-empty, otherwise allocates a fresh one. The
 * returned instance is guaranteed to be in a clean state (status undefined,
 * headers empty).
 */
export const acquireAccumulator = (): GatewayResponseAccumulator => {
  const pooled = freeList.pop();

  if (pooled !== undefined) {
    return pooled;
  }

  return new GatewayResponseAccumulator();
};

/**
 * Return an accumulator to the pool. Resets the instance before storing so
 * the next acquirer observes a clean slate. Excess instances beyond
 * `POOL_MAX` are dropped on the floor (eligible for GC) so a transient
 * burst cannot permanently inflate memory usage.
 */
export const releaseAccumulator = (acc: GatewayResponseAccumulator): void => {
  acc.reset();

  if (freeList.length < POOL_MAX) {
    freeList.push(acc);
  }
};

/**
 * Test-only introspection of the free-list size.
 * @remarks
 * Not part of the public API — only the pool's own spec imports this helper
 * to assert the bounded-size invariant and the LIFO free-list ordering.
 */
export const getPoolSizeForTesting = (): number => freeList.length;

/**
 * Test-only helper that drops every pooled instance. Spec uses this to
 * isolate cases that depend on the pool starting empty; production code
 * never calls it.
 */
export const drainPoolForTesting = (): void => {
  freeList.length = 0;
};
