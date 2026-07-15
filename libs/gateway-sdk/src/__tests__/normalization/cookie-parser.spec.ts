import { describe, expect, it } from '@jest/globals';

import { parseCookies } from '../../normalization/cookie-parser';

describe(parseCookies.name, () => {
  describe('happy path', () => {
    it('returns an empty map for an empty header', () => {
      expect(parseCookies('')).toEqual({});
    });

    it('parses a single name=value pair', () => {
      expect(parseCookies('sid=abc')).toEqual({ sid: 'abc' });
    });

    it('parses multiple pairs separated by `;`', () => {
      expect(parseCookies('sid=abc; theme=dark; lang=en')).toEqual({
        sid: 'abc',
        theme: 'dark',
        lang: 'en',
      });
    });

    it('returns a fresh map per call so callers can mutate it safely', () => {
      const a = parseCookies('sid=abc');
      const b = parseCookies('sid=abc');

      expect(a).not.toBe(b);
      a['mutated'] = '1';
      expect(b['mutated']).toBeUndefined();
    });
  });

  describe('edge cases — RFC 6265 corner forms', () => {
    it('preserves `=` characters inside the value (base64-encoded session tokens)', () => {
      expect(parseCookies('sid=YWJjPT0=')).toEqual({ sid: 'YWJjPT0=' });
    });

    it('skips a pair without `=` (nameless cookie per rfc6265bis §5.6 — not addressable by name)', () => {
      expect(parseCookies('flag')).toEqual({});
    });

    it('skips a pair with nothing before `=` (empty name)', () => {
      expect(parseCookies('=orphan; sid=abc')).toEqual({ sid: 'abc' });
    });

    it('trims whitespace around the name so `sid =abc` resolves as `sid`', () => {
      expect(parseCookies('sid =abc')).toEqual({ sid: 'abc' });
    });

    it('strips wrapping double quotes per RFC 6265 §4.1.1', () => {
      expect(parseCookies('sid="abc"')).toEqual({ sid: 'abc' });
    });

    it('trims whitespace around each pair', () => {
      expect(parseCookies('  sid=abc ;   theme=dark   ')).toEqual({
        sid: 'abc',
        theme: 'dark',
      });
    });

    it('skips empty segments produced by extra `;`', () => {
      expect(parseCookies('sid=abc; ; theme=dark; ;')).toEqual({
        sid: 'abc',
        theme: 'dark',
      });
    });

    it('decodes percent-encoded names and values', () => {
      expect(parseCookies('na%20me=hello%20world')).toEqual({ 'na me': 'hello world' });
    });

    it('falls back to the raw string for malformed percent sequences', () => {
      // Given: a cookie value with a stray `%` not followed by valid hex
      // When: parser runs
      // Then: the entire parse survives — bad value passes through raw
      expect(parseCookies('sid=ab%c')).toEqual({ sid: 'ab%c' });
    });

    it('keeps the first occurrence on duplicate names (Express convention)', () => {
      expect(parseCookies('sid=first; sid=second')).toEqual({ sid: 'first' });
    });
  });

  describe('edge cases — Object.prototype member names', () => {
    it('stores a cookie named toString as an own property', () => {
      const parsed = parseCookies('toString=abc');

      expect(parsed['toString']).toBe('abc');
    });

    it('stores a cookie named hasOwnProperty as an own property', () => {
      const parsed = parseCookies('hasOwnProperty=v');

      expect(parsed['hasOwnProperty']).toBe('v');
    });

    it('stores a cookie named constructor as an own property', () => {
      const parsed = parseCookies('constructor=ctor');

      expect(parsed['constructor']).toBe('ctor');
    });

    it('stores a cookie named __proto__ without polluting Object.prototype', () => {
      const parsed = parseCookies('__proto__=polluted');

      expect(Object.hasOwn(parsed, '__proto__')).toBe(true);
      expect(parsed['__proto__']).toBe('polluted');
      expect(({} as Record<string, unknown>)['polluted']).toBeUndefined();
      expect(Object.prototype).not.toHaveProperty('polluted');
    });

    it('returns undefined for absent prototype-member names instead of inherited functions', () => {
      const parsed = parseCookies('sid=abc');

      expect(parsed['toString']).toBeUndefined();
      expect(parsed['hasOwnProperty']).toBeUndefined();
    });
  });

  describe('gateway parity — mirrors the Go extractCookie table', () => {
    // Mirrors TestExtractCookie_* in the Go proxy package so both sides
    // of the wire resolve the SAME value for the SAME Cookie header.
    // Keep the two tables in sync when either side changes.
    it.each([
      ['plain value', 'session=abc', 'abc'],
      ['quoted value', 'session="abc"', 'abc'],
      ['leading whitespace after equals', 'session= abc', 'abc'],
      ['quoted with surrounding spaces', 'session= "abc"', 'abc'],
      ['value among siblings', 'theme=dark; session=abc; lang=en', 'abc'],
      ['quoted value among siblings', 'theme=dark; session="abc"; lang=en', 'abc'],
      ['single trailing quote stays attached', 'session=abc"', 'abc"'],
      ['single leading quote stays attached', 'session="abc', '"abc'],
      ['whitespace before equals in name', 'session =abc', 'abc'],
      ['nameless flag pair skipped, later real pair wins', 'session; session=abc', 'abc'],
    ])('%s', (_label, header, want) => {
      expect(parseCookies(header)['session']).toBe(want);
    });

    it.each([
      ['bare flag pair resolves to no cookie', 'session'],
      ['empty-name pair resolves to no cookie', '=abc'],
    ])('%s', (_label, header) => {
      expect(parseCookies(header)['session']).toBeUndefined();
    });
  });
});
