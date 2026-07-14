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

describe('serializeCookie — Partitioned Secure policy', () => {
  let warnSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    resetSameSiteWarnDedupeForTests();
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => undefined);
  });

  afterEach(() => {
    warnSpy.mockRestore();
  });

  it('auto-promotes Secure when partitioned is set without secure', () => {
    const result = serializeCookie('sid', 'v', { partitioned: true });

    expect(result).toContain('; Secure');
    expect(result).toContain('; Partitioned');
    expect(warnSpy).toHaveBeenCalledTimes(1);
  });

  it('honours explicit secure:false with a loud warning (browser will ignore the cookie)', () => {
    const result = serializeCookie('sid', 'v', { partitioned: true, secure: false });

    expect(result).not.toContain('; Secure');
    expect(result).toContain('; Partitioned');
    expect(warnSpy).toHaveBeenCalledTimes(1);
  });

  it('takes no action and emits no warning when secure is already true', () => {
    const result = serializeCookie('sid', 'v', { partitioned: true, secure: true });

    expect(result).toContain('; Secure');
    expect(result).toContain('; Partitioned');
    expect(warnSpy).not.toHaveBeenCalled();
  });
});

describe('serializeCookie — cookie name prefixes', () => {
  let warnSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    resetSameSiteWarnDedupeForTests();
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => undefined);
  });

  afterEach(() => {
    warnSpy.mockRestore();
  });

  it('__Host- auto-fills Secure and Path=/ when absent', () => {
    const result = serializeCookie('__Host-sid', 'v');

    expect(result).toBe('__Host-sid=v; Path=/; Secure');
  });

  it('__Host- with a Domain attribute emits a loud warning (UA rejects the cookie)', () => {
    serializeCookie('__Host-sid', 'v', { domain: '.example.com' });

    expect(warnSpy).toHaveBeenCalledTimes(1);
    expect(String(warnSpy.mock.calls[0]?.[0])).toContain('__Host-');
  });

  it('__Host- with an explicit non-root Path emits a loud warning', () => {
    serializeCookie('__Host-sid', 'v', { path: '/api' });

    expect(warnSpy).toHaveBeenCalledTimes(1);
  });

  it('__Secure- auto-promotes Secure when absent', () => {
    const result = serializeCookie('__Secure-sid', 'v');

    expect(result).toBe('__Secure-sid=v; Secure');
  });

  it('detects prefixes case-insensitively (UA validation is case-insensitive per rfc6265bis §5.4)', () => {
    const result = serializeCookie('__HOST-sid', 'v');

    expect(result).toBe('__HOST-sid=v; Path=/; Secure');
  });
});

describe('serializeCookie — name token encoding', () => {
  it('percent-encodes parentheses in cookie names (not legal token octets)', () => {
    expect(serializeCookie('a(b)', 'v')).toBe('a%28b%29=v');
  });
});

describe('serializeCookie — size awareness', () => {
  let warnSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    resetSameSiteWarnDedupeForTests();
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => undefined);
  });

  afterEach(() => {
    warnSpy.mockRestore();
  });

  it('warns once when name+value exceed the 4096-octet UA ignore threshold', () => {
    const big = 'x'.repeat(5000);

    serializeCookie('big', big);
    serializeCookie('big', big);

    expect(warnSpy).toHaveBeenCalledTimes(1);
  });

  it('does not warn below the threshold', () => {
    serializeCookie('ok', 'x'.repeat(100));

    expect(warnSpy).not.toHaveBeenCalled();
  });
});

describe('serializeCookie — attribute-injection guards (rfc6265bis §4.1.1 grammar)', () => {
  it('throws when path contains a `;` (attribute-injection vector)', () => {
    expect(() =>
      serializeCookie('return_to', 'v', { path: '/; Domain=.example.com; SameSite=None' }),
    ).toThrow(/Path/);
  });

  it('throws when path contains control characters', () => {
    expect(() => serializeCookie('sid', 'v', { path: '/api\r\nSet-Cookie: evil=1' })).toThrow(
      /Path/,
    );
    expect(() => serializeCookie('sid', 'v', { path: '/api\u0000' })).toThrow(/Path/);
  });

  it('throws when domain contains a `;`', () => {
    expect(() =>
      serializeCookie('sid', 'v', { domain: 'example.com; SameSite=None; Secure' }),
    ).toThrow(/Domain/);
  });

  it('throws when domain contains CR/LF', () => {
    expect(() => serializeCookie('sid', 'v', { domain: 'example.com\r\nX-Evil: 1' })).toThrow(
      /Domain/,
    );
  });

  it('throws when domain is not an RFC 1123 host name', () => {
    expect(() => serializeCookie('sid', 'v', { domain: 'exa mple.com' })).toThrow(/Domain/);
    expect(() => serializeCookie('sid', 'v', { domain: '-bad.example.com' })).toThrow(/Domain/);
    expect(() => serializeCookie('sid', 'v', { domain: '' })).toThrow(/Domain/);
  });

  it('accepts a leading-dot domain (UAs ignore the dot)', () => {
    expect(serializeCookie('sid', 'v', { domain: '.example.com' })).toBe(
      'sid=v; Domain=.example.com',
    );
  });

  it('accepts every av-octet in path (printable US-ASCII minus `;`)', () => {
    expect(serializeCookie('sid', 'v', { path: '/a b/c?d=e&f=<g>~' })).toBe(
      'sid=v; Path=/a b/c?d=e&f=<g>~',
    );
  });

  it('validates values arriving through module-level defaults too', () => {
    expect(() => serializeCookie('sid', 'v', {}, { path: '/; Secure' })).toThrow(/Path/);
  });
});

describe('serializeCookie — lifetime attribute validation', () => {
  it('throws on NaN maxAge instead of shipping Max-Age=NaN', () => {
    expect(() => serializeCookie('sid', 'v', { maxAge: Number.NaN })).toThrow(/Max-Age/);
  });

  it('throws on non-finite maxAge', () => {
    expect(() => serializeCookie('sid', 'v', { maxAge: Number.POSITIVE_INFINITY })).toThrow(
      /Max-Age/,
    );
    expect(() => serializeCookie('sid', 'v', { maxAge: Number.NEGATIVE_INFINITY })).toThrow(
      /Max-Age/,
    );
  });

  it('accepts maxAge 0 (the clearCookie form)', () => {
    expect(serializeCookie('sid', '', { maxAge: 0 })).toBe('sid=; Max-Age=0');
  });

  it('throws on an invalid Date instead of shipping Expires=Invalid Date', () => {
    expect(() => serializeCookie('sid', 'v', { expires: new Date('bogus') })).toThrow(/Expires/);
  });
});
