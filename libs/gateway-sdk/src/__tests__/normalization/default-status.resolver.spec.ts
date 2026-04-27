import { beforeEach, describe, expect, it } from '@jest/globals';

import { DefaultStatusResolver } from '../../normalization/default-status.resolver';

import type { IGatewayHttpMeta } from '../../types/gateway-http-meta.interface';

describe(DefaultStatusResolver, () => {
  let sut: DefaultStatusResolver;

  const httpMeta: IGatewayHttpMeta = { method: 'POST', path: '/users' };

  beforeEach(() => {
    sut = new DefaultStatusResolver();
  });

  describe(DefaultStatusResolver.prototype.resolveSuccess.name, () => {
    describe('happy path', () => {
      it('returns the explicit statusCode from httpMeta when defined', () => {
        // Given: route declares an explicit success status
        const meta: IGatewayHttpMeta = { ...httpMeta, statusCode: 201 };

        // When: handler returns a body
        const status = sut.resolveSuccess(meta, { id: 1 });

        // Then: explicit value wins over defaults
        expect(status).toBe(201);
      });

      it('returns 200 for a non-null object return without explicit statusCode', () => {
        expect(sut.resolveSuccess(httpMeta, { id: 1 })).toBe(200);
      });

      it('returns 204 for null return value', () => {
        expect(sut.resolveSuccess(httpMeta, null)).toBe(204);
      });

      it('returns 204 for undefined return value', () => {
        expect(sut.resolveSuccess(httpMeta, undefined)).toBe(204);
      });
    });

    describe('edge cases', () => {
      it.each([
        ['zero', 0],
        ['empty string', ''],
        ['false boolean', false],
      ])('returns 200 for falsy-but-not-null/undefined value: %s', (_label, value) => {
        // Given: a handler return that is falsy but represents real content
        // When: resolver inspects it
        // Then: 200, not 204 — distinguishes "empty response" from "response with falsy content"
        expect(sut.resolveSuccess(httpMeta, value)).toBe(200);
      });

      it('honours an explicit statusCode of 0 verbatim', () => {
        // Given: route declares the unusual status 0
        const meta: IGatewayHttpMeta = { ...httpMeta, statusCode: 0 };

        // When: any handler return
        // Then: 0 is returned literally — `statusCode !== undefined` is the precedence rule
        expect(sut.resolveSuccess(meta, { id: 1 })).toBe(0);
      });

      it('explicit statusCode wins over the null-default-204 rule', () => {
        const meta: IGatewayHttpMeta = { ...httpMeta, statusCode: 200 };

        expect(sut.resolveSuccess(meta, null)).toBe(200);
      });
    });
  });
});
