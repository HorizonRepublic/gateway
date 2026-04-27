import { describe, expect, it } from '@jest/globals';

import { readGatewayEnvelope } from '../../../decorators/params/envelope-accessor';

import type { IGatewayRequest } from '../../../types/gateway-request.interface';
import type { ExecutionContext } from '@nestjs/common';

const buildContext = (envelope: IGatewayRequest): ExecutionContext =>
  ({
    switchToRpc: () => ({
      getData: () => envelope,
    }),
  }) as unknown as ExecutionContext;

describe(readGatewayEnvelope.name, () => {
  it('returns the envelope from the RPC context by reference (no clone)', () => {
    // Given: an envelope wrapped in a NestJS RPC ExecutionContext stub
    const envelope: IGatewayRequest<{ email: string }> = {
      route: { method: 'POST', path: '/users', matchedPath: '/users' },
      params: {},
      query: {},
      headers: {},
      body: { email: 'a@b.c' },
      meta: {
        requestId: 'r1',
        remoteAddr: '127.0.0.1',
        receivedAt: 0,
        timeoutMs: 30000,
      },
    };

    // When / Then: helper returns the same reference — downstream decorators
    // assume zero-copy reads on the hot path.
    expect(readGatewayEnvelope(buildContext(envelope))).toBe(envelope);
  });

  it('preserves the generic TBody narrowing through to the consumer', () => {
    interface ITestBody {
      readonly hello: string;
    }
    const envelope: IGatewayRequest<ITestBody> = {
      route: { method: 'GET', path: '/', matchedPath: '/' },
      params: {},
      query: {},
      headers: {},
      body: { hello: 'world' },
      meta: {
        requestId: 'r2',
        remoteAddr: '0.0.0.0',
        receivedAt: 0,
        timeoutMs: 1000,
      },
    };

    const result = readGatewayEnvelope<ITestBody>(buildContext(envelope));

    expect(result.body.hello).toBe('world');
  });
});
