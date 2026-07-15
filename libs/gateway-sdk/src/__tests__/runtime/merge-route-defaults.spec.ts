import { describe, expect, it } from '@jest/globals';

import { mergeRouteDefaults } from '../../runtime/merge-route-defaults';

import type { IGatewayDefaults } from '../../types';

describe(mergeRouteDefaults.name, () => {
  describe('happy path — defaults fill in missing route fields', () => {
    it('returns the route untouched when defaults are empty', () => {
      const route = { method: 'GET', path: '/x' };

      expect(mergeRouteDefaults({}, route)).toEqual({ method: 'GET', path: '/x' });
    });

    it('fills in cors from defaults when the route has none', () => {
      const defaults: IGatewayDefaults = { cors: { origins: ['*'] } };

      const merged = mergeRouteDefaults(defaults, { method: 'GET' });

      expect(merged['cors']).toEqual({
        origins: ['*'],
        headers: ['Content-Type', 'Authorization', 'X-Request-Id'],
      });
    });

    it('fills in rateLimit from defaults when the route has none', () => {
      const defaults: IGatewayDefaults = { rateLimit: { rps: 100 } };

      const merged = mergeRouteDefaults(defaults, {});

      expect(merged['rateLimit']).toEqual({ rps: 100 });
    });

    it('fills in timeout from defaults when the route has none', () => {
      const defaults: IGatewayDefaults = { timeout: 30000 };

      const merged = mergeRouteDefaults(defaults, {});

      expect(merged['timeout']).toBe(30000);
    });
  });

  describe('happy path — per-route wins', () => {
    it('keeps the route cors and ignores defaults.cors (shallow replace)', () => {
      const defaults: IGatewayDefaults = { cors: { origins: ['*'] } };

      const merged = mergeRouteDefaults(defaults, {
        cors: { origins: ['https://app.example.com'] },
      });

      expect(merged['cors']).toEqual({
        origins: ['https://app.example.com'],
        headers: ['Content-Type', 'Authorization', 'X-Request-Id'],
      });
    });

    it('keeps the route rateLimit and ignores defaults.rateLimit', () => {
      const defaults: IGatewayDefaults = { rateLimit: { rps: 100 } };

      const merged = mergeRouteDefaults(defaults, { rateLimit: { rps: 5 } });

      expect(merged['rateLimit']).toEqual({ rps: 5 });
    });

    it('keeps the route timeout and ignores defaults.timeout', () => {
      const defaults: IGatewayDefaults = { timeout: 30000 };

      const merged = mergeRouteDefaults(defaults, { timeout: 5000 });

      expect(merged['timeout']).toBe(5000);
    });
  });

  describe('headers — deep merge per key', () => {
    it('overlays route headers on top of defaults so keys deep-merge', () => {
      const defaults: IGatewayDefaults = {
        headers: { 'cache-control': 'no-store', 'x-content-type-options': 'nosniff' },
      };

      const merged = mergeRouteDefaults(defaults, {
        headers: { 'cache-control': 'public, max-age=60', 'x-route-marker': 'on' },
      });

      expect(merged['headers']).toEqual({
        'cache-control': 'public, max-age=60',
        'x-content-type-options': 'nosniff',
        'x-route-marker': 'on',
      });
    });

    it('returns the route headers untouched when defaults have none', () => {
      const merged = mergeRouteDefaults({}, { headers: { 'x-foo': 'bar' } });

      expect(merged['headers']).toEqual({ 'x-foo': 'bar' });
    });

    it('returns the defaults headers untouched when the route has none', () => {
      const defaults: IGatewayDefaults = { headers: { 'cache-control': 'no-store' } };

      const merged = mergeRouteDefaults(defaults, {});

      expect(merged['headers']).toEqual({ 'cache-control': 'no-store' });
    });
  });

  describe('cors — explicit headers default on the wire', () => {
    it('writes the documented headers default into a route cors block that omits headers', () => {
      const merged = mergeRouteDefaults({}, { cors: { origins: ['https://app.example.com'] } });

      expect(merged['cors']).toEqual({
        origins: ['https://app.example.com'],
        headers: ['Content-Type', 'Authorization', 'X-Request-Id'],
      });
    });

    it('keeps an explicit headers list untouched', () => {
      const merged = mergeRouteDefaults(
        {},
        { cors: { origins: ['https://app.example.com'], headers: ['X-Custom'] } },
      );

      expect(merged['cors']).toEqual({
        origins: ['https://app.example.com'],
        headers: ['X-Custom'],
      });
    });

    it('keeps an explicit empty headers list (opt-out of Access-Control-Allow-Headers)', () => {
      const merged = mergeRouteDefaults(
        {},
        { cors: { origins: ['https://app.example.com'], headers: [] } },
      );

      expect(merged['cors']).toEqual({
        origins: ['https://app.example.com'],
        headers: [],
      });
    });

    it('leaves the cors slot absent when neither side declares one', () => {
      const merged = mergeRouteDefaults({}, { method: 'GET' });

      expect('cors' in merged).toBe(false);
    });
  });

  describe('edge cases', () => {
    it('does not write a `headers` slot when neither side has one', () => {
      // Given: neither defaults nor route declare headers
      const merged = mergeRouteDefaults({}, { method: 'GET' });

      // Then: the slot stays absent (no `'headers': undefined` noise on the wire)
      expect('headers' in merged).toBe(false);
    });

    it('ignores defaults.cookies — SDK-only, never written to KV', () => {
      const defaults: IGatewayDefaults = { cookies: { httpOnly: true } };

      const merged = mergeRouteDefaults(defaults, { method: 'GET' });

      expect('cookies' in merged).toBe(false);
    });

    it('does not mutate the input route object', () => {
      const route = { method: 'GET' as const, headers: { 'x-foo': 'bar' } };

      mergeRouteDefaults({ headers: { 'x-bar': 'baz' } }, route);

      expect(route.headers).toEqual({ 'x-foo': 'bar' });
    });
  });
});
