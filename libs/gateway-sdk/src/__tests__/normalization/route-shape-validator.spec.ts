import { describe, expect, it } from '@jest/globals';

import { assertRouteWireShape } from '../../normalization/route-shape-validator';

import type { IGatewayRouteOptions } from '../../types/gateway-route-options.interface';

const context = '@GatewayRoute(GET /users)';

const base: IGatewayRouteOptions = {
  pattern: 'users.list',
  method: 'GET',
  path: '/users',
};

describe(assertRouteWireShape.name, () => {
  describe('happy path', () => {
    it('accepts the minimal valid shape', () => {
      expect(() => {
        assertRouteWireShape(base, context);
      }).not.toThrow();
    });

    it('accepts the full integer surface', () => {
      expect(() => {
        assertRouteWireShape(
          {
            ...base,
            statusCode: 201,
            timeout: 5000,
            cors: { origins: ['https://a.example'], maxAge: 600 },
          },
          context,
        );
      }).not.toThrow();
    });

    it('accepts dotted patterns with token-safe segments', () => {
      expect(() => {
        assertRouteWireShape({ ...base, pattern: 'auth.verifier.a-1_b' }, context);
      }).not.toThrow();
    });
  });

  describe('method invariants', () => {
    it.each(['get', 'Get', 'TRACE', ''])('rejects %j', (method) => {
      expect(() => {
        assertRouteWireShape(
          { ...base, method: method as IGatewayRouteOptions['method'] },
          context,
        );
      }).toThrow(/method must be one of/);
    });
  });

  describe('pattern invariants (NATS subject charset)', () => {
    it.each([
      'users list',
      'users.*',
      'users.>',
      'users..list',
      '.users',
      'users.',
      '',
      'кир.лиця',
    ])('rejects %j', (pattern) => {
      expect(() => {
        assertRouteWireShape({ ...base, pattern }, context);
      }).toThrow(/pattern/);
    });
  });

  describe('integer invariants', () => {
    it('rejects timeout: 0 with the omit-to-inherit escape hatch', () => {
      expect(() => {
        assertRouteWireShape({ ...base, timeout: 0 }, context);
      }).toThrow(/Omit timeout to inherit the gateway-wide value/);
    });

    it.each([1.5, -1, NaN, Infinity])('rejects timeout %p', (timeout) => {
      expect(() => {
        assertRouteWireShape({ ...base, timeout }, context);
      }).toThrow(/timeout/);
    });

    it.each([99, 600, 200.5, NaN])('rejects statusCode %p', (statusCode) => {
      expect(() => {
        assertRouteWireShape({ ...base, statusCode }, context);
      }).toThrow(/statusCode/);
    });

    it.each([-1, 1.5, NaN])('rejects cors.maxAge %p', (maxAge) => {
      expect(() => {
        assertRouteWireShape(
          { ...base, cors: { origins: ['https://a.example'], maxAge } },
          context,
        );
      }).toThrow(/maxAge/);
    });
  });

  describe('context propagation', () => {
    it('quotes the source in every error', () => {
      expect(() => {
        assertRouteWireShape({ ...base, timeout: 0 }, context);
      }).toThrow(/Source: @GatewayRoute\(GET \/users\)\./);
    });
  });
});
