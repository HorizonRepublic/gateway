import { describe, expect, it } from '@jest/globals';

import { extractGatewayCookie } from '../../../decorators/params/gateway-cookie.decorator';
import { PARSED_COOKIES_KEY } from '../../../runtime/parsed-cookies-symbol';

import type { IGatewayRequest } from '../../../types/gateway-request.interface';
import type { ExecutionContext } from '@nestjs/common';

type IEnvelopeWithCache = IGatewayRequest & {
  [PARSED_COOKIES_KEY]?: Record<string, string>;
};

const buildEnvelope = (cookieHeader?: string): IEnvelopeWithCache => ({
  route: { method: 'GET', path: '/', matchedPath: '/' },
  params: {},
  query: {},
  headers: cookieHeader === undefined ? {} : { cookie: cookieHeader },
  body: null,
  meta: { requestId: 'r', remoteAddr: '127.0.0.1', receivedAt: 0, timeoutMs: 1000 },
});

const buildContext = (envelope: IEnvelopeWithCache): ExecutionContext =>
  ({
    switchToRpc: () => ({ getData: () => envelope }),
  }) as unknown as ExecutionContext;

describe(extractGatewayCookie.name, () => {
  describe('happy path', () => {
    it('returns the value of a single named cookie', () => {
      const ctx = buildContext(buildEnvelope('sid=abc'));

      expect(extractGatewayCookie('sid', ctx)).toBe('abc');
    });

    it('extracts cookies from a multi-cookie header', () => {
      const ctx = buildContext(buildEnvelope('sid=abc; theme=dark; tenant=demo'));

      expect(extractGatewayCookie('sid', ctx)).toBe('abc');
      expect(extractGatewayCookie('theme', ctx)).toBe('dark');
      expect(extractGatewayCookie('tenant', ctx)).toBe('demo');
    });
  });

  describe('edge cases', () => {
    it('returns undefined when the named cookie is absent', () => {
      const ctx = buildContext(buildEnvelope('sid=abc'));

      expect(extractGatewayCookie('theme', ctx)).toBeUndefined();
    });

    it('returns undefined when the request has no Cookie header at all', () => {
      const ctx = buildContext(buildEnvelope());

      expect(extractGatewayCookie('sid', ctx)).toBeUndefined();
    });

    it('keeps caches isolated across separate envelopes (no cross-request bleed)', () => {
      const ctxA = buildContext(buildEnvelope('sid=alice'));
      const ctxB = buildContext(buildEnvelope('sid=bob'));

      expect(extractGatewayCookie('sid', ctxA)).toBe('alice');
      expect(extractGatewayCookie('sid', ctxB)).toBe('bob');
    });
  });

  describe('cache invariant', () => {
    it('parses the Cookie header exactly once per envelope and reuses the cached map on subsequent reads', () => {
      // Given: an envelope with a Cookie header, no cache yet
      const envelope = buildEnvelope('sid=original');
      const ctx = buildContext(envelope);

      // When: first read parses
      expect(extractGatewayCookie('sid', ctx)).toBe('original');

      // Then: the cache slot is populated
      const cached = envelope[PARSED_COOKIES_KEY];

      expect(cached).toBeDefined();

      // When: we tamper with the cached map directly (simulates "second read uses the same map")
      if (cached !== undefined) {
        cached['sid'] = 'tampered';
      }

      // Then: the second read returns the tampered value — proving the parser
      // was not re-run
      expect(extractGatewayCookie('sid', ctx)).toBe('tampered');
    });

    it('caches an empty map even when the Cookie header is missing, so subsequent reads still hit cache', () => {
      const envelope = buildEnvelope();
      const ctx = buildContext(envelope);

      expect(extractGatewayCookie('sid', ctx)).toBeUndefined();

      const cached = envelope[PARSED_COOKIES_KEY];

      expect(cached).toBeDefined();

      // Mutate the cached map: any later read should see it
      if (cached !== undefined) {
        cached['sentinel'] = 'survived';
      }

      expect(extractGatewayCookie('sentinel', ctx)).toBe('survived');
    });
  });
});
