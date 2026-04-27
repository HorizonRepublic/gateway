import { afterEach, beforeEach, describe, expect, it, jest } from '@jest/globals';

import {
  resetSameSiteWarnDedupeForTests,
  serializeCookie,
} from '../../normalization/cookie-serializer';

describe(serializeCookie.name, () => {
  let warnSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    resetSameSiteWarnDedupeForTests();
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => undefined);
  });

  afterEach(() => {
    warnSpy.mockRestore();
  });

  describe('happy path — attribute serialization', () => {
    it('emits the bare name=value for an empty options object', () => {
      expect(serializeCookie('sid', 'abc')).toBe('sid=abc');
    });

    it('emits attributes in the documented order: Domain → Path → Expires → Max-Age → HttpOnly → Secure → SameSite → Partitioned', () => {
      const result = serializeCookie('sid', 'abc', {
        domain: '.example.com',
        path: '/api',
        expires: new Date(Date.UTC(2026, 0, 1, 0, 0, 0)),
        maxAge: 3600,
        httpOnly: true,
        secure: true,
        sameSite: 'lax',
        partitioned: true,
      });

      expect(result).toBe(
        'sid=abc; Domain=.example.com; Path=/api; Expires=Thu, 01 Jan 2026 00:00:00 GMT; ' +
          'Max-Age=3600; HttpOnly; Secure; SameSite=Lax; Partitioned',
      );
    });

    it('omits flags when set to false', () => {
      const result = serializeCookie('sid', 'abc', { httpOnly: false, secure: false });

      expect(result).toBe('sid=abc');
    });

    it('floors fractional Max-Age', () => {
      expect(serializeCookie('sid', 'abc', { maxAge: 3600.9 })).toBe('sid=abc; Max-Age=3600');
    });

    it('capitalizes SameSite labels: strict → Strict, lax → Lax, none + secure → None', () => {
      expect(serializeCookie('a', '1', { sameSite: 'strict' })).toBe('a=1; SameSite=Strict');
      expect(serializeCookie('a', '1', { sameSite: 'lax' })).toBe('a=1; SameSite=Lax');
      expect(serializeCookie('a', '1', { sameSite: 'none', secure: true })).toBe(
        'a=1; Secure; SameSite=None',
      );
    });
  });

  describe('happy path — defaults merge', () => {
    it('applies module defaults when per-cookie options are absent', () => {
      const result = serializeCookie('sid', 'abc', {}, { secure: true, sameSite: 'lax' });

      expect(result).toBe('sid=abc; Secure; SameSite=Lax');
    });

    it('lets per-cookie options override defaults on a key-by-key basis', () => {
      const result = serializeCookie(
        'sid',
        'abc',
        { sameSite: 'strict' },
        { secure: true, sameSite: 'lax' },
      );

      expect(result).toBe('sid=abc; Secure; SameSite=Strict');
    });
  });

  describe('edge cases — encoding', () => {
    it('passes the RFC 3986 unreserved set through verbatim', () => {
      expect(serializeCookie('sid_abc-1.0~x', 'A1.b-c_d~e')).toBe('sid_abc-1.0~x=A1.b-c_d~e');
    });

    it('percent-encodes name and value with disallowed characters', () => {
      expect(serializeCookie('na me', 'a b')).toBe('na%20me=a%20b');
    });
  });

  describe('SameSite=None policy', () => {
    it('auto-promotes secure when sameSite is none and secure is undefined', () => {
      const result = serializeCookie('sid', 'abc', { sameSite: 'none' });

      expect(result).toBe('sid=abc; Secure; SameSite=None');
      expect(warnSpy).toHaveBeenCalledTimes(1);
      expect(warnSpy.mock.calls[0]?.[0]).toMatch(/auto-promoted to Secure/);
    });

    it('honours explicit secure=false and emits a loud warning instead', () => {
      const result = serializeCookie('sid', 'abc', { sameSite: 'none', secure: false });

      expect(result).toBe('sid=abc; SameSite=None');
      expect(warnSpy).toHaveBeenCalledTimes(1);
      expect(warnSpy.mock.calls[0]?.[0]).toMatch(/WILL reject this cookie/);
    });

    it('takes no action and emits no warning when secure is already true', () => {
      const result = serializeCookie('sid', 'abc', { sameSite: 'none', secure: true });

      expect(result).toBe('sid=abc; Secure; SameSite=None');
      expect(warnSpy).not.toHaveBeenCalled();
    });

    it('takes no action when sameSite is strict or lax', () => {
      serializeCookie('sid', 'abc', { sameSite: 'lax' });
      serializeCookie('sid', 'abc', { sameSite: 'strict' });

      expect(warnSpy).not.toHaveBeenCalled();
    });

    it('dedupes the auto-promote warning per cookie name', () => {
      // Given: two emissions of the same cookie name in the same outcome shape
      serializeCookie('sid', 'abc', { sameSite: 'none' });
      serializeCookie('sid', 'def', { sameSite: 'none' });

      // Then: only one WARN reaches stderr — the second is silenced
      expect(warnSpy).toHaveBeenCalledTimes(1);
    });

    it('emits a separate warning when the same cookie later hits a different outcome', () => {
      // Given: cookie first auto-promoted, then later an explicit override
      serializeCookie('sid', 'abc', { sameSite: 'none' });
      serializeCookie('sid', 'abc', { sameSite: 'none', secure: false });

      // Then: both outcomes warn, dedupe is per (name, outcome) pair
      expect(warnSpy).toHaveBeenCalledTimes(2);
    });
  });
});
