import { afterEach, beforeEach, describe, expect, it } from '@jest/globals';

import { extractGatewayResponse } from '../../../decorators/params/gateway-response.decorator';
import { GatewayResponseAccumulator } from '../../../runtime/gateway-response-accumulator';
import { drainPoolForTesting, releaseAccumulator } from '../../../runtime/gateway-response-pool';
import { RESPONSE_ACCUMULATOR_KEY } from '../../../runtime/response-accumulator-symbol';

import type { ExecutionContext } from '@nestjs/common';

type IEnvelopeWithSlot = Record<symbol, unknown>;

const buildContext = (envelope: IEnvelopeWithSlot): ExecutionContext =>
  ({
    switchToRpc: () => ({ getData: () => envelope }),
  }) as unknown as ExecutionContext;

describe(extractGatewayResponse.name, () => {
  beforeEach(() => {
    drainPoolForTesting();
  });

  afterEach(() => {
    drainPoolForTesting();
  });

  describe('happy path', () => {
    it('returns a fresh accumulator on first injection and stashes it on the envelope', () => {
      // Given: an envelope with no accumulator slot yet
      const envelope: IEnvelopeWithSlot = {};
      const ctx = buildContext(envelope);

      // When: first injection
      const res = extractGatewayResponse(undefined, ctx);

      // Then: a real accumulator was acquired and stashed under the symbol slot
      expect(res).toBeInstanceOf(GatewayResponseAccumulator);
      expect(envelope[RESPONSE_ACCUMULATOR_KEY]).toBe(res);
    });

    it('returns the same instance on subsequent injections (idempotent acquire)', () => {
      // Given: an envelope where the first injection has already occurred
      const envelope: IEnvelopeWithSlot = {};
      const ctx = buildContext(envelope);

      const first = extractGatewayResponse(undefined, ctx);

      // When: subsequent injections in the same handler signature
      const second = extractGatewayResponse(undefined, ctx);
      const third = extractGatewayResponse(undefined, ctx);

      // Then: every injection returns the same accumulator by reference
      expect(second).toBe(first);
      expect(third).toBe(first);
    });
  });

  describe('integration with the pool', () => {
    it('checks out from the pool when one is available (LIFO reuse)', () => {
      // Given: a previously-released accumulator sitting in the pool
      const recycled = new GatewayResponseAccumulator();

      releaseAccumulator(recycled);

      const envelope: IEnvelopeWithSlot = {};

      // When: first injection
      const res = extractGatewayResponse(undefined, buildContext(envelope));

      // Then: the recycled instance is reused, not a fresh one
      expect(res).toBe(recycled);
    });

    it('does not steal an unrelated stash that is not a GatewayResponseAccumulator', () => {
      // Given: the envelope's accumulator slot already holds a non-accumulator
      // value (e.g. test fixture leak, defensive programming check)
      const envelope: IEnvelopeWithSlot = {
        [RESPONSE_ACCUMULATOR_KEY]: { not: 'an accumulator' },
      };
      const ctx = buildContext(envelope);

      // When: injection runs
      const res = extractGatewayResponse(undefined, ctx);

      // Then: a real accumulator is acquired and the slot is rewritten
      expect(res).toBeInstanceOf(GatewayResponseAccumulator);
      expect(envelope[RESPONSE_ACCUMULATOR_KEY]).toBe(res);
    });
  });
});
