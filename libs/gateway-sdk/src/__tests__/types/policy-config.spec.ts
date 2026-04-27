import { describe, expect, it } from '@jest/globals';

import type {
  IGatewayCorsConfig,
  IGatewayRateLimitConfig,
  RateLimitKey,
  RateLimitStore,
} from '../../types';

describe('policy config types', () => {
  describe('IGatewayCorsConfig', () => {
    it('accepts the minimum policy with origins only', () => {
      const cors: IGatewayCorsConfig = { origins: ['https://app.example.com'] };

      expect(cors.origins).toHaveLength(1);
    });

    it('accepts a wildcard origin and the full optional surface', () => {
      const cors: IGatewayCorsConfig = {
        origins: ['*'],
        methods: ['GET', 'POST'],
        headers: ['Content-Type', 'Authorization'],
        credentials: true,
        maxAge: 86400,
        exposeHeaders: ['X-Request-Id', 'X-RateLimit-Remaining'],
      };

      expect(cors.maxAge).toBe(86400);
      expect(cors.exposeHeaders).toEqual(['X-Request-Id', 'X-RateLimit-Remaining']);
    });
  });

  describe('IGatewayRateLimitConfig', () => {
    it('accepts the minimum policy with rps only', () => {
      const limit: IGatewayRateLimitConfig = { rps: 100 };

      expect(limit.rps).toBe(100);
    });

    it('accepts the full surface with keyBy chain and explicit store', () => {
      const limit: IGatewayRateLimitConfig = {
        rps: 50,
        burst: 100,
        keyBy: ['user:sub', 'header:x-api-key', 'ip'],
        store: 'nats-kv',
      };

      expect(limit.burst).toBe(100);
      expect(limit.keyBy).toHaveLength(3);
      expect(limit.store).toBe('nats-kv');
    });
  });

  describe('RateLimitKey union', () => {
    it('accepts the four canonical key sources', () => {
      const keys: RateLimitKey[] = ['ip', 'header:x-api-key', 'cookie:session', 'user:sub'];

      expect(keys).toHaveLength(4);
    });
  });

  describe('RateLimitStore union', () => {
    it('exposes memory, nats-kv, and redis selectors', () => {
      const stores: RateLimitStore[] = ['memory', 'nats-kv', 'redis'];

      expect(stores).toEqual(['memory', 'nats-kv', 'redis']);
    });
  });
});
