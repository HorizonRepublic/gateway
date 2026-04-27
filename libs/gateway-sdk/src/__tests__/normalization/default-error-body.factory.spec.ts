import {
  BadRequestException,
  HttpException,
  HttpStatus,
  NotFoundException,
  UnauthorizedException,
} from '@nestjs/common';

import { beforeEach, describe, expect, it } from '@jest/globals';

import { DefaultErrorBodyFactory } from '../../normalization/default-error-body.factory';

import type { IGatewayRequest } from '../../types/gateway-request.interface';

describe(DefaultErrorBodyFactory, () => {
  let sut: DefaultErrorBodyFactory;

  const request: IGatewayRequest = {
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
  };

  beforeEach(() => {
    sut = new DefaultErrorBodyFactory();
  });

  describe(DefaultErrorBodyFactory.prototype.build.name, () => {
    describe('happy path — NestJS HttpException', () => {
      it('extracts status from NotFoundException via getStatus()', () => {
        // Given: a NestJS-built-in NotFoundException
        const error = new NotFoundException('User 3 not found');

        // When: factory builds
        const result = sut.build(error, request);

        // Then: status mirrors NestJS HttpStatus.NOT_FOUND
        expect(result.status).toBe(HttpStatus.NOT_FOUND);
      });

      it('forwards Nest native body shape verbatim for NotFoundException', () => {
        const result = sut.build(new NotFoundException('User 3 not found'), request);

        expect(result.body).toEqual({
          statusCode: HttpStatus.NOT_FOUND,
          message: 'User 3 not found',
          error: 'Not Found',
        });
      });

      it('forwards BadRequestException with array message (ValidationPipe shape)', () => {
        // Given: a BadRequestException whose message is an array of validation issues
        const error = new BadRequestException(['email must be an email', 'age must be a number']);

        // When: factory builds
        const result = sut.build(error, request);

        // Then: full body round-trips verbatim
        expect(result.status).toBe(HttpStatus.BAD_REQUEST);
        expect(result.body).toEqual({
          statusCode: HttpStatus.BAD_REQUEST,
          message: ['email must be an email', 'age must be a number'],
          error: 'Bad Request',
        });
      });

      it('forwards UnauthorizedException', () => {
        const result = sut.build(new UnauthorizedException('token expired'), request);

        expect(result.status).toBe(HttpStatus.UNAUTHORIZED);
        expect(result.body).toEqual({
          statusCode: HttpStatus.UNAUTHORIZED,
          message: 'token expired',
          error: 'Unauthorized',
        });
      });
    });

    describe('edge cases — HttpException corner forms', () => {
      it('wraps a plain-string HttpException response into { statusCode, message }', () => {
        // Given: the rare new HttpException('literal', status) form
        const error = new HttpException('raw string body', 418);

        // When: factory builds
        const result = sut.build(error, request);

        // Then: wrapped to match NestJS BaseExceptionFilter shape
        expect(result.status).toBe(418);
        expect(result.body).toEqual({ statusCode: 418, message: 'raw string body' });
      });

      it('forwards custom subclass structured response verbatim', () => {
        // Given: a custom HttpException subclass whose constructor passes a
        // structured object to the base. Every field round-trips untouched —
        // the factory never normalizes, re-keys, or strips. This is the
        // extensibility path for projects that want a richer error shape
        // without writing a full IErrorBodyFactory.
        class TeapotException extends HttpException {
          public constructor() {
            super(
              {
                statusCode: 418,
                message: 'short and stout',
                error: "I'm a Teapot",
                teapotId: 'tp-42',
                brew: 'earl-grey',
              },
              418,
            );
          }
        }

        // When: factory builds
        const result = sut.build(new TeapotException(), request);

        // Then: every custom field survives
        expect(result.status).toBe(418);
        expect(result.body).toEqual({
          statusCode: 418,
          message: 'short and stout',
          error: "I'm a Teapot",
          teapotId: 'tp-42',
          brew: 'earl-grey',
        });
      });
    });

    describe('error cases — unrecognized throws', () => {
      it('returns the generic 500 envelope for a plain Error', () => {
        // Given: a thrown native Error (not HttpException)
        const error = new Error('raw internal detail');

        // When: factory builds
        const result = sut.build(error, request);

        // Then: generic 500, no original message leak
        expect(result.status).toBe(HttpStatus.INTERNAL_SERVER_ERROR);
        expect(result.body).toEqual({
          statusCode: HttpStatus.INTERNAL_SERVER_ERROR,
          message: 'Internal server error',
        });
      });

      it('never leaks the original Error message into the body', () => {
        // Given: a thrown Error whose message contains internal infra detail
        const error = new Error('postgres connection to db.internal:5432 failed');

        // When: factory builds
        const result = sut.build(error, request);

        // Then: body contains nothing from the original error
        expect(JSON.stringify(result.body)).not.toContain('postgres');
        expect(JSON.stringify(result.body)).not.toContain('db.internal');
      });

      it.each([
        ['string', 'some string'],
        ['null', null],
        ['number', 42],
        ['plain object', { foo: 'bar' }],
      ])('handles non-Error throws (%s) with the generic 500 envelope', (_label, thrown) => {
        const result = sut.build(thrown, request);

        expect(result.status).toBe(HttpStatus.INTERNAL_SERVER_ERROR);
        expect(result.body).toEqual({
          statusCode: HttpStatus.INTERNAL_SERVER_ERROR,
          message: 'Internal server error',
        });
      });
    });
  });
});
