import { describe, expect, it } from '@jest/globals';

import type {
  HttpMethod,
  IGatewayRequest,
  IGatewayRequestMeta,
  IGatewayRouteContext,
} from '../../types';

describe('wire request envelope types', () => {
  it('compiles a structurally valid IGatewayRequest', () => {
    const meta: IGatewayRequestMeta = {
      requestId: '01HX7K8M9P3QRSTUVWXYZ12345',
      remoteAddr: '127.0.0.1',
      receivedAt: 1700000000000,
      timeoutMs: 30000,
    };
    const route: IGatewayRouteContext = {
      method: 'GET' satisfies HttpMethod,
      path: '/users/:id',
      matchedPath: '/users/abc-123',
    };
    const request: IGatewayRequest<{ id: string }> = {
      route,
      params: { id: 'abc-123' },
      query: {},
      headers: { 'user-agent': 'jest' },
      body: { id: 'abc-123' },
      meta,
    };

    expect(request.meta.requestId).toBe('01HX7K8M9P3QRSTUVWXYZ12345');
    expect(request.route.method).toBe('GET');
    expect(request.params['id']).toBe('abc-123');
  });

  it('accepts optional traceparent and auth claims', () => {
    const meta: IGatewayRequestMeta = {
      requestId: '01HX...',
      remoteAddr: '127.0.0.1',
      receivedAt: 0,
      timeoutMs: 1000,
      traceparent: '00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01',
    };
    const request: IGatewayRequest<unknown, { sub: string }> = {
      route: { method: 'POST', path: '/x', matchedPath: '/x' },
      params: {},
      query: {},
      headers: {},
      body: null,
      meta,
      auth: { sub: 'user-1' },
    };

    expect(request.meta.traceparent).toBeDefined();
    expect(request.auth?.sub).toBe('user-1');
  });

  it('exposes the canonical HttpMethod union', () => {
    const allMethods: HttpMethod[] = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'];

    expect(allMethods).toHaveLength(7);
  });
});
