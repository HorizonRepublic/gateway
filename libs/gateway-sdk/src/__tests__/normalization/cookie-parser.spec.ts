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

    it('treats a pair without `=` as a flag cookie with empty value', () => {
      expect(parseCookies('flag')).toEqual({ flag: '' });
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
});
