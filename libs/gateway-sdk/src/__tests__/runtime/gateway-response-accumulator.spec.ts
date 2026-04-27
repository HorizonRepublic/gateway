import { afterEach, beforeEach, describe, expect, it, jest } from '@jest/globals';

import { GatewayResponseAccumulator } from '../../runtime/gateway-response-accumulator';

describe(GatewayResponseAccumulator, () => {
  let sut: GatewayResponseAccumulator;
  let warnSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    sut = new GatewayResponseAccumulator();
    warnSpy = jest.spyOn(console, 'warn').mockImplementation(() => undefined);
  });

  afterEach(() => {
    warnSpy.mockRestore();
  });

  describe(GatewayResponseAccumulator.prototype.status.name, () => {
    it('records the status and returns this for chaining', () => {
      const result = sut.status(201);

      expect(result).toBe(sut);
      expect(sut.statusCode).toBe(201);
    });

    it('overwrites a previously-set status', () => {
      sut.status(201).status(204);

      expect(sut.statusCode).toBe(204);
    });
  });

  describe(GatewayResponseAccumulator.prototype.header.name, () => {
    it('lowercases the header name', () => {
      sut.header('X-Trace-Id', 'abc');

      expect(sut.headers['x-trace-id']).toEqual(['abc']);
    });

    it('replaces a prior value (set semantics, not append)', () => {
      sut.header('x-foo', 'first').header('X-FOO', 'second');

      expect(sut.headers['x-foo']).toEqual(['second']);
    });
  });

  describe(GatewayResponseAccumulator.prototype.appendHeader.name, () => {
    it('creates the slot when the header is unset', () => {
      sut.appendHeader('vary', 'Accept');

      expect(sut.headers['vary']).toEqual(['Accept']);
    });

    it('appends to the existing slot for multi-value headers', () => {
      sut.appendHeader('vary', 'Accept').appendHeader('Vary', 'Accept-Language');

      expect(sut.headers['vary']).toEqual(['Accept', 'Accept-Language']);
    });
  });

  describe(GatewayResponseAccumulator.prototype.removeHeader.name, () => {
    it('removes the named header', () => {
      sut.header('x-foo', 'bar').removeHeader('X-FOO');

      expect(sut.headers['x-foo']).toBeUndefined();
    });

    it('is a no-op for a header that was never set', () => {
      expect(() => sut.removeHeader('x-absent')).not.toThrow();
    });
  });

  describe(GatewayResponseAccumulator.prototype.cookie.name, () => {
    it('appends a Set-Cookie line for each call', () => {
      sut.cookie('a', '1').cookie('b', '2');

      expect(sut.headers['set-cookie']).toEqual(['a=1', 'b=2']);
    });

    it('honours module-level cookieDefaults via per-key merge', () => {
      sut.cookieDefaults = { httpOnly: true, secure: true, path: '/' };
      sut.cookie('sid', 'abc');

      expect(sut.headers['set-cookie']?.[0]).toBe('sid=abc; Path=/; HttpOnly; Secure');
    });

    it('lets per-cookie options override the defaults', () => {
      sut.cookieDefaults = { secure: true, sameSite: 'lax' };
      sut.cookie('sid', 'abc', { sameSite: 'strict' });

      expect(sut.headers['set-cookie']?.[0]).toBe('sid=abc; Secure; SameSite=Strict');
    });
  });

  describe(GatewayResponseAccumulator.prototype.clearCookie.name, () => {
    it('emits a cookie with Max-Age=0 and Expires at the epoch', () => {
      sut.clearCookie('sid');

      expect(sut.headers['set-cookie']?.[0]).toBe(
        'sid=; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0',
      );
    });

    it('forwards path and domain so the client matches the cookie to delete', () => {
      sut.clearCookie('sid', { path: '/api', domain: '.example.com' });

      expect(sut.headers['set-cookie']?.[0]).toBe(
        'sid=; Domain=.example.com; Path=/api; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0',
      );
    });
  });

  describe(GatewayResponseAccumulator.prototype.redirect.name, () => {
    it('defaults the redirect status to 302 and stamps Location', () => {
      sut.redirect('/login');

      expect(sut.statusCode).toBe(302);
      expect(sut.headers['location']).toEqual(['/login']);
    });

    it('honours an explicit redirect status', () => {
      sut.redirect('/permanent', 301);

      expect(sut.statusCode).toBe(301);
    });
  });

  describe(GatewayResponseAccumulator.prototype.reset.name, () => {
    it('clears statusCode, cookieDefaults, and every header', () => {
      // Given: a fully-populated accumulator
      sut.status(201).header('x-foo', 'bar').appendHeader('vary', 'Accept');
      sut.cookieDefaults = { secure: true };

      // When: reset runs
      sut.reset();

      // Then: state cleared
      expect(sut.statusCode).toBeUndefined();
      expect(Object.keys(sut.headers)).toEqual([]);
      expect(sut.cookieDefaults).toEqual({});
    });

    it('preserves headers object identity across resets so consumers can cache the reference', () => {
      const headersRef = sut.headers;

      sut.header('x-foo', 'bar');
      sut.reset();

      expect(sut.headers).toBe(headersRef);
    });
  });
});
