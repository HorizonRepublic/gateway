import 'reflect-metadata';

import { PATTERN_EXTRAS_METADATA } from '@nestjs/microservices/constants';

import { afterEach, describe, expect, it } from '@jest/globals';

import { GatewayRoute, normalizeRouteAuth } from '../../decorators/gateway-route.decorator';
import { setDefaultsSnapshot } from '../../runtime/defaults-snapshot';

import type { IGatewayRouteOptions } from '../../types/gateway-route-options.interface';

describe(normalizeRouteAuth.name, () => {
  describe('happy path', () => {
    it('returns undefined for an unprotected route', () => {
      expect(normalizeRouteAuth(undefined)).toBeUndefined();
    });

    it('translates `auth: true` to the wire-shape default-verifier object', () => {
      // Given: handler declared `auth: true`
      // When / Then: normalize emits { verifier: '', optional: false }
      expect(normalizeRouteAuth(true)).toEqual({ verifier: '', optional: false });
    });

    it('passes through structured options with explicit verifier id', () => {
      const result = normalizeRouteAuth({ verifier: 'jwt', optional: true });

      expect(result).toEqual({ verifier: 'jwt', optional: true });
    });

    it('fills in defaults when the structured form omits fields', () => {
      // Given: empty object — equivalent to `auth: true` per the contract
      const result = normalizeRouteAuth({});

      // Then: same wire shape as `auth: true`
      expect(result).toEqual({ verifier: '', optional: false });
    });
  });
});

describe(GatewayRoute.name, () => {
  const baseOptions: IGatewayRouteOptions = {
    pattern: 'users.create',
    method: 'POST',
    path: '/users',
  };

  describe('config validation', () => {
    it('throws when CORS combines wildcard origin with credentials: true', () => {
      const options: IGatewayRouteOptions = {
        ...baseOptions,
        cors: { origins: ['*'], credentials: true },
      };

      expect(() => GatewayRoute(options)).toThrow(/cors\.credentials: true cannot be combined/);
    });

    it('throws when rateLimit.rps is invalid', () => {
      const options: IGatewayRouteOptions = {
        ...baseOptions,
        rateLimit: { rps: 0 },
      };

      expect(() => GatewayRoute(options)).toThrow(/rateLimit\.rps must be a positive integer/);
    });

    it('mentions the route source in the validation error', () => {
      const options: IGatewayRouteOptions = {
        ...baseOptions,
        cors: { origins: ['*'], credentials: true },
      };

      expect(() => GatewayRoute(options)).toThrow(/Source: @GatewayRoute\(POST \/users\)/);
    });
  });

  describe('decoration shape', () => {
    it('returns a callable MethodDecorator without throwing for a valid options block', () => {
      // Given: valid options with full surface
      const options: IGatewayRouteOptions = {
        ...baseOptions,
        statusCode: 201,
        auth: true,
        cors: { origins: ['https://app.example.com'] },
        rateLimit: { rps: 100 },
        headers: { 'cache-control': 'no-store' },
        timeout: 5000,
      };

      // When: decorator factory runs
      const decorator = GatewayRoute(options);

      // Then: a function suitable for application to a class method
      expect(typeof decorator).toBe('function');
    });
  });
});

describe('GatewayRoute lazy defaults merge', () => {
  afterEach(() => {
    setDefaultsSnapshot({});
  });

  const decorate = (): object => {
    class Fixture {
      @GatewayRoute({
        pattern: 'users.get',
        method: 'GET',
        path: '/users/:id',
        headers: { 'x-route': 'route' },
      })
      public getUser(): string {
        return 'ok';
      }
    }

    const handler = Fixture.prototype.getUser;

    return Reflect.getMetadata(PATTERN_EXTRAS_METADATA, handler) as object;
  };

  it('merges a snapshot installed AFTER decoration into meta reads', () => {
    const extras = decorate();

    setDefaultsSnapshot(Object.freeze({ timeout: 7000, headers: { 'x-default': 'def' } }));

    const meta = (extras as { meta: Record<string, unknown> }).meta;

    expect(meta['timeout']).toBe(7000);
    expect(meta['headers']).toEqual({ 'x-default': 'def', 'x-route': 'route' });
  });

  it('keeps route-level values over defaults', () => {
    const extras = decorate();

    setDefaultsSnapshot(Object.freeze({ headers: { 'x-route': 'default-loses' } }));

    const meta = (extras as { meta: Record<string, unknown> }).meta;

    expect((meta['headers'] as Record<string, string>)['x-route']).toBe('route');
  });

  it('returns raw meta under an empty snapshot', () => {
    const extras = decorate();

    const meta = (extras as { meta: Record<string, unknown> }).meta;

    expect(meta['timeout']).toBeUndefined();
    expect(meta['http']).toEqual({ method: 'GET', path: '/users/:id' });
  });

  it('memoizes: repeated reads return the same object while the snapshot is unchanged', () => {
    const extras = decorate();

    setDefaultsSnapshot(Object.freeze({ timeout: 7000 }));

    const first = (extras as { meta: object }).meta;
    const second = (extras as { meta: object }).meta;

    expect(second).toBe(first);
  });

  it('recomputes when the snapshot identity changes', () => {
    const extras = decorate();

    setDefaultsSnapshot(Object.freeze({ timeout: 1000 }));
    const first = (extras as { meta: Record<string, unknown> }).meta;

    setDefaultsSnapshot(Object.freeze({ timeout: 2000 }));
    const second = (extras as { meta: Record<string, unknown> }).meta;

    expect(first['timeout']).toBe(1000);
    expect(second['timeout']).toBe(2000);
    expect(second).not.toBe(first);
  });

  it('publishes an explicit CORS headers list when the route omits one (wire-shape guard)', () => {
    class CorsFixture {
      @GatewayRoute({
        pattern: 'orders.create',
        method: 'POST',
        path: '/orders',
        cors: { origins: ['https://app.example.com'] },
      })
      public createOrder(): string {
        return 'ok';
      }
    }

    const extras = Reflect.getMetadata(
      PATTERN_EXTRAS_METADATA,
      CorsFixture.prototype.createOrder,
    ) as { meta: Record<string, unknown> };

    // The registry entry must carry the documented default explicitly —
    // the Go side adds no implicit Access-Control-Allow-Headers values.
    expect(extras.meta['cors']).toEqual({
      origins: ['https://app.example.com'],
      headers: ['Content-Type', 'Authorization', 'X-Request-Id'],
    });
  });

  it('throws on assignment to extras.meta', () => {
    const extras = decorate();

    expect(() => {
      (extras as { meta: unknown }).meta = {};
    }).toThrow(TypeError);
  });

  it('meta survives JSON.stringify (enumerable getter)', () => {
    const extras = decorate();

    setDefaultsSnapshot(Object.freeze({ timeout: 7000 }));

    const parsed = JSON.parse(JSON.stringify(extras)) as { meta: { timeout: number } };

    expect(parsed.meta.timeout).toBe(7000);
  });
});
