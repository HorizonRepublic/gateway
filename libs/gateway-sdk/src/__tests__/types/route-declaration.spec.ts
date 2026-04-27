import { describe, expect, it } from '@jest/globals';

import type {
  GatewayRouteAuth,
  IGatewayHttpMeta,
  IGatewayRouteAuthOptions,
  IGatewayRouteOptions,
} from '../../types';

describe('route declaration types', () => {
  describe('IGatewayHttpMeta', () => {
    it('compiles a minimal meta with method and path', () => {
      const meta: IGatewayHttpMeta = { method: 'GET', path: '/users/:id' };

      expect(meta.method).toBe('GET');
      expect(meta.statusCode).toBeUndefined();
    });

    it('accepts an explicit statusCode override', () => {
      const meta: IGatewayHttpMeta = { method: 'POST', path: '/users', statusCode: 201 };

      expect(meta.statusCode).toBe(201);
    });
  });

  describe('GatewayRouteAuth', () => {
    it('accepts the `true` shorthand', () => {
      const auth: GatewayRouteAuth = true;

      expect(auth).toBe(true);
    });

    it('accepts the structured form with verifier and optional', () => {
      const auth: IGatewayRouteAuthOptions = { verifier: 'jwt-default', optional: true };

      expect(auth.verifier).toBe('jwt-default');
      expect(auth.optional).toBe(true);
    });

    it('accepts the empty object form (defaults to required + default verifier)', () => {
      const auth: IGatewayRouteAuthOptions = {};

      expect(auth.verifier).toBeUndefined();
    });
  });

  describe('IGatewayRouteOptions', () => {
    it('compiles a minimal options object with the four required fields', () => {
      const options: IGatewayRouteOptions = {
        pattern: 'user.get',
        method: 'GET',
        path: '/users/:id',
      };

      expect(options.pattern).toBe('user.get');
    });

    it('compiles a full options object with policies and overrides', () => {
      const options: IGatewayRouteOptions = {
        pattern: 'user.create',
        method: 'POST',
        path: '/users',
        statusCode: 201,
        auth: { verifier: 'jwt' },
        cors: { origins: ['https://app.example.com'] },
        rateLimit: { rps: 100, store: 'nats-kv' },
        headers: { 'cache-control': 'no-store' },
        timeout: 5000,
      };

      expect(options.cors?.origins).toEqual(['https://app.example.com']);
      expect(options.rateLimit?.store).toBe('nats-kv');
      expect(options.timeout).toBe(5000);
    });
  });
});
