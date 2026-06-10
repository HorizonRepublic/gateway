import { describe, expect, it } from '@jest/globals';

import { assertRateLimitConfig } from '../../normalization/rate-limit-validator';

const context = '@GatewayRoute(POST /users)';

describe(assertRateLimitConfig.name, () => {
  describe('happy path', () => {
    it('returns silently when rateLimit is undefined', () => {
      expect(() => {
        assertRateLimitConfig(undefined, context);
      }).not.toThrow();
    });

    it('accepts the minimum valid policy with rps only', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 1 }, context);
      }).not.toThrow();
    });

    it('accepts a policy at the upper rps bound (2^32 - 1)', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 0xff_ff_ff_ff }, context);
      }).not.toThrow();
    });

    it('accepts an explicit non-negative burst', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 100, burst: 0 }, context);
      }).not.toThrow();
      expect(() => {
        assertRateLimitConfig({ rps: 100, burst: 250 }, context);
      }).not.toThrow();
    });

    it('accepts the full surface', () => {
      expect(() => {
        assertRateLimitConfig(
          {
            rps: 50,
            burst: 100,
            keyBy: ['user:sub', 'header:x-api-key', 'ip'],
            store: 'nats-kv',
            failPolicy: 'closed',
          },
          context,
        );
      }).not.toThrow();
    });

    it('accepts each legal failPolicy value and its absence', () => {
      for (const failPolicy of ['open', 'closed', undefined] as const) {
        expect(() => {
          assertRateLimitConfig(
            failPolicy === undefined ? { rps: 10 } : { rps: 10, failPolicy },
            context,
          );
        }).not.toThrow();
      }
    });
  });

  describe('error cases — failPolicy invariants', () => {
    it.each(['OPEN', 'Closed', '', 'garbage'])('rejects %j', (failPolicy) => {
      expect(() => {
        assertRateLimitConfig({ rps: 10, failPolicy: failPolicy as 'open' | 'closed' }, context);
      }).toThrow(/rateLimit\.failPolicy must be 'open' or 'closed'/);
    });

    it('includes the caller context in the failPolicy error message', () => {
      expect(() => {
        assertRateLimitConfig(
          { rps: 10, failPolicy: 'garbage' as unknown as 'open' },
          'GatewayModule.forRoot',
        );
      }).toThrow(/Source: GatewayModule\.forRoot\./);
    });

    it('mentions the omit-field escape hatch in the failPolicy error', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 10, failPolicy: 'garbage' as unknown as 'open' }, context);
      }).toThrow(/Omit failPolicy to inherit the gateway-wide policy/);
    });
  });

  describe('error cases — rps invariants', () => {
    it.each([
      ['zero (would be silently treated as no-limit by Go side)', 0],
      ['negative', -1],
      ['fractional', 1.5],
      ['NaN', Number.NaN],
      ['above 2^32 - 1', 0x1_00_00_00_00],
      ['Infinity', Number.POSITIVE_INFINITY],
    ])('throws for rps = %s', (_label, rps) => {
      expect(() => {
        assertRateLimitConfig({ rps }, context);
      }).toThrow(/rateLimit\.rps must be a positive integer in \[1, 2\^32 - 1\]/);
    });

    it('mentions the omit-block escape hatch in the rps error', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 0 }, context);
      }).toThrow(/To disable rate limiting, omit the rateLimit block entirely/);
    });
  });

  describe('error cases — burst invariants', () => {
    it.each([
      ['negative', -1],
      ['fractional', 5.5],
      ['NaN', Number.NaN],
      ['above 2^32 - 1', 0x1_00_00_00_00],
    ])('throws for burst = %s when rps is valid', (_label, burst) => {
      expect(() => {
        assertRateLimitConfig({ rps: 100, burst }, context);
      }).toThrow(/rateLimit\.burst must be a non-negative integer in \[0, 2\^32 - 1\]/);
    });
  });

  describe('error cases — context propagation', () => {
    it('includes the caller context in the rps error message', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 0 }, '@GatewayRoute(POST /users)');
      }).toThrow(/Source: @GatewayRoute\(POST \/users\)\./);
    });

    it('includes the caller context in the burst error message', () => {
      expect(() => {
        assertRateLimitConfig({ rps: 100, burst: -5 }, 'GatewayModule.forRoot');
      }).toThrow(/Source: GatewayModule\.forRoot\./);
    });
  });
});
