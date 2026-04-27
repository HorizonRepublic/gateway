import { describe, expect, it } from '@jest/globals';

import type { IGatewayAuthVerifierOptions, IGatewayDefaults } from '../../types';

describe('module-level defaults and verifier options', () => {
  describe('IGatewayDefaults', () => {
    it('compiles an empty defaults object', () => {
      const defaults: IGatewayDefaults = {};

      expect(defaults).toEqual({});
    });

    it('compiles a defaults object with every optional slot populated', () => {
      const defaults: IGatewayDefaults = {
        cors: { origins: ['https://app.example.com'] },
        rateLimit: { rps: 1000 },
        headers: { 'cache-control': 'no-store', 'x-content-type-options': 'nosniff' },
        cookies: { secure: true, sameSite: 'lax' },
        timeout: 30000,
      };

      expect(defaults.cors?.origins).toEqual(['https://app.example.com']);
      expect(defaults.cookies?.secure).toBe(true);
      expect(defaults.timeout).toBe(30000);
    });
  });

  describe('IGatewayAuthVerifierOptions', () => {
    it('compiles the minimal verifier options with id only', () => {
      const options: IGatewayAuthVerifierOptions = { id: 'jwt-default' };

      expect(options.id).toBe('jwt-default');
      expect(options.default).toBeUndefined();
    });

    it('compiles a verifier options object marked as default', () => {
      const options: IGatewayAuthVerifierOptions = { id: 'jwt-default', default: true };

      expect(options.default).toBe(true);
    });
  });
});
