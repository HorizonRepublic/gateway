import { describe, expect, it } from '@jest/globals';

import type { ICookieOptions, IGatewayErrorBody, IGatewayReply, IGatewayResponse } from './index';

describe('wire reply envelope types', () => {
  it('compiles a structurally valid IGatewayReply success envelope', () => {
    const reply: IGatewayReply<{ id: string }> = {
      status: 200,
      headers: {
        'content-type': ['application/json'],
        'x-trace-id': ['01HX...'],
      },
      body: { id: 'abc-123' },
    };

    expect(reply.status).toBe(200);
    expect(reply.headers['content-type']).toEqual(['application/json']);
    expect(reply.body).toEqual({ id: 'abc-123' });
  });

  it('accepts a null body for void / 204 responses', () => {
    const reply: IGatewayReply = {
      status: 204,
      headers: {},
      body: null,
    };

    expect(reply.body).toBeNull();
  });

  it('IGatewayErrorBody accepts arbitrary keys with unknown values', () => {
    const body: IGatewayErrorBody = {
      statusCode: 400,
      message: 'Bad Request',
      error: 'Bad Request',
      detail: { field: 'email', reason: 'invalid' },
    };

    expect(body['statusCode']).toBe(400);
    expect(body['detail']).toBeDefined();
  });

  it('ICookieOptions exposes the RFC 6265 attribute set', () => {
    const options: ICookieOptions = {
      domain: '.example.com',
      path: '/api',
      maxAge: 3600,
      httpOnly: true,
      secure: true,
      sameSite: 'lax',
      partitioned: false,
    };

    expect(options.maxAge).toBe(3600);
    expect(options.sameSite).toBe('lax');
  });

  it('IGatewayResponse builder methods return this for chainability (type-level check)', () => {
    const stub: IGatewayResponse = {
      status: () => stub,
      header: () => stub,
      appendHeader: () => stub,
      removeHeader: () => stub,
      cookie: () => stub,
      clearCookie: () => stub,
      redirect: () => stub,
    };
    const result = stub.status(201).header('x-foo', 'bar').cookie('s', 'v');

    expect(result).toBe(stub);
  });
});
