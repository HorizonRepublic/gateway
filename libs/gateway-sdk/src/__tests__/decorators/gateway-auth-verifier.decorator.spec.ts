import { describe, expect, it } from '@jest/globals';

import { GatewayAuthVerifier } from '../../decorators/gateway-auth-verifier.decorator';

describe(GatewayAuthVerifier.name, () => {
  describe('id validation', () => {
    it.each([
      ['empty string', ''],
      ['contains a space', 'jwt default'],
      ['contains a slash', 'jwt/v1'],
      ['contains a dot', 'jwt.v1'],
      ['64 chars (one over limit)', 'a'.repeat(64)],
    ])('throws for an invalid id (%s)', (_label, id) => {
      expect(() => GatewayAuthVerifier({ id })).toThrow(/invalid verifier id/);
    });

    it('mentions the rejected id and the allowed pattern in the error', () => {
      expect(() => GatewayAuthVerifier({ id: 'bad id' })).toThrow(
        /must match \/\^\[A-Za-z0-9_-\]\{1,63\}\$\//,
      );
    });
  });

  describe('happy path', () => {
    it.each([
      ['simple lowercase', 'jwt'],
      ['with hyphen', 'jwt-default'],
      ['with underscore', 'jwt_default'],
      ['mixed case + digits', 'JwtV2'],
      ['exactly 63 chars (boundary)', 'a'.repeat(63)],
    ])('accepts a valid id (%s)', (_label, id) => {
      expect(() => GatewayAuthVerifier({ id })).not.toThrow();
    });

    it('returns a callable MethodDecorator', () => {
      expect(typeof GatewayAuthVerifier({ id: 'jwt' })).toBe('function');
    });

    it('accepts the optional default flag', () => {
      expect(() => GatewayAuthVerifier({ id: 'jwt', default: true })).not.toThrow();
    });
  });
});
