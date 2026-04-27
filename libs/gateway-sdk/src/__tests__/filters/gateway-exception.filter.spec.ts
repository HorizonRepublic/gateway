import { HttpException, NotFoundException, Logger } from '@nestjs/common';

import { afterEach, beforeEach, describe, expect, it, jest } from '@jest/globals';
import { firstValueFrom } from 'rxjs';

import { GatewayExceptionFilter } from '../../filters/gateway-exception.filter';
import { DefaultErrorBodyFactory } from '../../normalization/default-error-body.factory';
import { DefaultGatewayReplyBuilder } from '../../normalization/default-reply.builder';

import type { IErrorBodyFactory } from '../../normalization/contracts/error-body-factory.interface';
import type { IGatewayReplyBuilder } from '../../normalization/contracts/reply-builder.interface';
import type { IGatewayRequest } from '../../types/gateway-request.interface';
import type { ArgumentsHost } from '@nestjs/common';

const buildRequest = (): IGatewayRequest => ({
  route: { method: 'GET', path: '/users/:id', matchedPath: '/users/1' },
  params: { id: '1' },
  query: {},
  headers: {},
  body: null,
  meta: {
    requestId: 'req-abc',
    remoteAddr: '127.0.0.1',
    receivedAt: 0,
    timeoutMs: 30000,
  },
});

const buildHost = (request: IGatewayRequest | undefined): ArgumentsHost =>
  ({
    switchToRpc: () => ({ getData: () => request }),
  }) as unknown as ArgumentsHost;

describe(GatewayExceptionFilter, () => {
  let sut: GatewayExceptionFilter;
  let replyBuilder: IGatewayReplyBuilder;
  let errorBodyFactory: IErrorBodyFactory;
  let errorLogSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    replyBuilder = new DefaultGatewayReplyBuilder();
    errorBodyFactory = new DefaultErrorBodyFactory();
    sut = new GatewayExceptionFilter(replyBuilder, errorBodyFactory);
    errorLogSpy = jest.spyOn(Logger.prototype, 'error').mockImplementation(() => undefined);
  });

  afterEach(() => {
    errorLogSpy.mockRestore();
  });

  describe(GatewayExceptionFilter.prototype.catch.name, () => {
    describe('happy path — HttpException pass-through', () => {
      it('serializes a NotFoundException into a wire-shaped reply envelope', async () => {
        // Given: a thrown 404 inside a gateway handler
        const exception = new NotFoundException('User 1 not found');

        // When: filter catches and emits an Observable
        const result = await firstValueFrom(sut.catch(exception, buildHost(buildRequest())));

        // Then: status + body match what the default factory + builder produce
        expect(result).toEqual({
          status: 404,
          headers: {},
          body: {
            statusCode: 404,
            message: 'User 1 not found',
            error: 'Not Found',
          },
        });
      });

      it('does not log 4xx exceptions (client-facing signal — silent)', async () => {
        // Given: a 4xx exception
        const exception = new NotFoundException('User 1 not found');

        // When: filter catches
        await firstValueFrom(sut.catch(exception, buildHost(buildRequest())));

        // Then: no error log was emitted — 4xx is a deliberate response, not a crash
        expect(errorLogSpy).not.toHaveBeenCalled();
      });
    });

    describe('error cases — 5xx server faults', () => {
      it('logs a structured error line with request context for >= 500 status', async () => {
        // Given: a thrown plain Error (no HttpException → factory falls through to 500)
        const exception = new Error('database connection lost');

        // When: filter catches
        await firstValueFrom(sut.catch(exception, buildHost(buildRequest())));

        // Then: one error log line was emitted with the documented context fields
        expect(errorLogSpy).toHaveBeenCalledTimes(1);
        const entry = errorLogSpy.mock.calls[0]?.[0] as Record<string, unknown>;

        expect(entry).toMatchObject({
          msg: 'Gateway Handler Error',
          err: exception,
          status: 500,
          pattern: '/users/:id',
          method: 'GET',
          matchedPath: '/users/1',
          requestId: 'req-abc',
          remoteAddr: '127.0.0.1',
        });
      });

      it('produces the generic 500 envelope and never leaks the original error message', async () => {
        const exception = new Error('postgres connection to db.internal:5432 failed');

        const result = await firstValueFrom(sut.catch(exception, buildHost(buildRequest())));

        expect(result.status).toBe(500);
        expect(JSON.stringify(result.body)).not.toContain('postgres');
        expect(JSON.stringify(result.body)).not.toContain('db.internal');
      });

      it('logs a 5xx HttpException too (not just plain Errors)', async () => {
        const exception = new HttpException('upstream broke', 503);

        await firstValueFrom(sut.catch(exception, buildHost(buildRequest())));

        expect(errorLogSpy).toHaveBeenCalledTimes(1);
        expect((errorLogSpy.mock.calls[0]?.[0] as Record<string, unknown>)['status']).toBe(503);
      });
    });

    describe('edge cases — defensive request reads', () => {
      it('still emits a reply when the RPC context returns no envelope', async () => {
        // Given: a host whose getData() returns undefined (synthetic test fixture)
        const exception = new Error('boom');

        // When: filter catches with an empty request
        const result = await firstValueFrom(sut.catch(exception, buildHost(undefined)));

        // Then: the envelope is still produced
        expect(result.status).toBe(500);
      });

      it('still logs the 5xx with undefined request fields rather than throwing', async () => {
        const exception = new Error('boom');

        await firstValueFrom(sut.catch(exception, buildHost(undefined)));

        // Even with no request, the log should fire — silence is the worse outcome
        expect(errorLogSpy).toHaveBeenCalledTimes(1);
        const entry = errorLogSpy.mock.calls[0]?.[0] as Record<string, unknown>;

        expect(entry['err']).toBe(exception);
        expect(entry['requestId']).toBeUndefined();
        expect(entry['remoteAddr']).toBeUndefined();
      });
    });
  });
});
