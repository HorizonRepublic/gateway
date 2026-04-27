import { describe, expect, it } from '@jest/globals';

import { GatewayRoute, normalizeRouteAuth } from '../../decorators/gateway-route.decorator';

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
