import { beforeEach, describe, expect, it } from '@jest/globals';

import { DefaultGatewayReplyBuilder } from '../../normalization/default-reply.builder';

import type { IGatewayErrorBody } from '../../types/gateway-error-body.interface';

describe(DefaultGatewayReplyBuilder, () => {
  let sut: DefaultGatewayReplyBuilder;

  beforeEach(() => {
    sut = new DefaultGatewayReplyBuilder();
  });

  describe(DefaultGatewayReplyBuilder.prototype.success.name, () => {
    describe('happy path', () => {
      it('wraps a body into a reply with empty headers when none are provided', () => {
        // Given: a status and a body
        // When: success builds
        const reply = sut.success(200, { id: 1 });

        // Then: full envelope with empty headers
        expect(reply).toEqual({ status: 200, headers: {}, body: { id: 1 } });
      });

      it('returns the provided status verbatim', () => {
        expect(sut.success(418, 'teapot').status).toBe(418);
      });

      it('forwards the provided multi-value headers map by reference', () => {
        // Given: a multi-value headers map
        // The Go gateway relies on byte-identity between what the accumulator
        // buffers and what lands on the wire, so a defensive clone here
        // would break the zero-copy contract.
        const headers = {
          'set-cookie': ['sid=a; Path=/', 'theme=dark; Path=/'],
          'x-custom': ['one'],
        } as const;

        // When: success builds
        const reply = sut.success(200, { id: 1 }, headers);

        // Then: same reference, no clone
        expect(reply.headers).toBe(headers);
      });
    });

    describe('edge cases', () => {
      it('preserves explicit null body as-is', () => {
        expect(sut.success(204, null)).toEqual({
          status: 204,
          headers: {},
          body: null,
        });
      });

      it('coerces undefined body to null for wire-format determinism', () => {
        // Given: an interceptor handed off undefined (handler had a void return)
        // When: success builds
        const reply = sut.success(204, undefined as unknown as null);

        // Then: body is normalized to null so JSON.stringify emits the field
        // explicitly instead of omitting it — the Go gateway decoder reads
        // into a fixed struct and would mis-parse omitted fields.
        expect(reply).toEqual({ status: 204, headers: {}, body: null });
      });
    });
  });

  describe(DefaultGatewayReplyBuilder.prototype.error.name, () => {
    const errorBody: IGatewayErrorBody = {
      statusCode: 404,
      message: 'User 3 not found',
      error: 'Not Found',
    };

    describe('happy path', () => {
      it('returns the error body verbatim', () => {
        // Given: an error body produced by the factory
        // When: error builds
        // Then: same reference (no rewrite)
        expect(sut.error(404, errorBody).body).toBe(errorBody);
      });

      it('returns the provided status', () => {
        expect(sut.error(404, errorBody).status).toBe(404);
      });

      it('emits an empty headers map by default', () => {
        // Given: no headers passed
        // The Go gateway stamps Content-Type / X-Request-Id on its own;
        // setting them here would be overwritten anyway. Empty map matches
        // NestJS BaseExceptionFilter's behaviour for error responses.
        expect(sut.error(404, errorBody).headers).toEqual({});
      });
    });

    describe('edge cases', () => {
      it('forwards the provided multi-value headers map by reference', () => {
        const headers = { 'www-authenticate': ['Bearer realm="api"'] } as const;

        const reply = sut.error(401, errorBody, headers);

        expect(reply.headers).toBe(headers);
      });
    });
  });
});
