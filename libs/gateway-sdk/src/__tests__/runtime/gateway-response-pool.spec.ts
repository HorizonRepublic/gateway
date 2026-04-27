import { beforeEach, describe, expect, it } from '@jest/globals';

import { GatewayResponseAccumulator } from '../../runtime/gateway-response-accumulator';
import {
  acquireAccumulator,
  drainPoolForTesting,
  getPoolSizeForTesting,
  releaseAccumulator,
} from '../../runtime/gateway-response-pool';

describe('gateway-response-pool', () => {
  beforeEach(() => {
    drainPoolForTesting();
  });

  describe('happy path', () => {
    it('allocates a fresh accumulator when the pool is empty', () => {
      // Given: empty pool
      // When: acquire
      const acc = acquireAccumulator();

      // Then: a fresh instance with clean state
      expect(acc).toBeInstanceOf(GatewayResponseAccumulator);
      expect(acc.statusCode).toBeUndefined();
      expect(Object.keys(acc.headers)).toEqual([]);
    });

    it('reuses the most-recently-released instance (LIFO)', () => {
      // Given: two instances released in order
      const first = new GatewayResponseAccumulator();
      const second = new GatewayResponseAccumulator();

      releaseAccumulator(first);
      releaseAccumulator(second);

      // When: two consecutive acquires
      // Then: LIFO order — second comes back first (warm in CPU cache)
      expect(acquireAccumulator()).toBe(second);
      expect(acquireAccumulator()).toBe(first);
    });

    it('resets the instance on release so the next acquirer observes a clean slate', () => {
      // Given: an instance with state
      const acc = new GatewayResponseAccumulator();

      acc.status(201).header('x-foo', 'bar');

      // When: released and re-acquired
      releaseAccumulator(acc);
      const recycled = acquireAccumulator();

      // Then: the same instance, now clean
      expect(recycled).toBe(acc);
      expect(recycled.statusCode).toBeUndefined();
      expect(Object.keys(recycled.headers)).toEqual([]);
    });
  });

  describe('edge cases', () => {
    it('caps the free list at POOL_MAX (1024) — releases beyond that are dropped on the floor', () => {
      // Given: 1025 release attempts (one over the cap)
      for (let i = 0; i < 1025; i++) {
        releaseAccumulator(new GatewayResponseAccumulator());
      }

      // Then: pool size is exactly 1024 — the 1025th instance was eligible for GC
      expect(getPoolSizeForTesting()).toBe(1024);
    });
  });
});
