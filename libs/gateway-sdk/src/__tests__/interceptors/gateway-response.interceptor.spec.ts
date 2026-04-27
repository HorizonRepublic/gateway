import { Reflector } from '@nestjs/core';

import { afterEach, beforeEach, describe, expect, it, jest } from '@jest/globals';
import { firstValueFrom, of, throwError } from 'rxjs';

import { GatewayResponseInterceptor } from '../../interceptors/gateway-response.interceptor';
import { DefaultGatewayReplyBuilder } from '../../normalization/default-reply.builder';
import { DefaultStatusResolver } from '../../normalization/default-status.resolver';
import { GatewayResponseAccumulator } from '../../runtime/gateway-response-accumulator';
import { drainPoolForTesting, getPoolSizeForTesting } from '../../runtime/gateway-response-pool';
import { RESPONSE_ACCUMULATOR_KEY } from '../../runtime/response-accumulator-symbol';

import type { IGatewayDefaults } from '../../types/gateway-defaults.interface';
import type { CallHandler, ExecutionContext } from '@nestjs/common';

type IEnvelopeWithSlot = Record<symbol, unknown>;

const buildContext = (
  envelope: IEnvelopeWithSlot,
  handler: () => unknown = () => undefined,
): ExecutionContext =>
  ({
    switchToRpc: () => ({ getData: () => envelope }),
    getHandler: () => handler,
  }) as unknown as ExecutionContext;

const buildCallHandler = (value: unknown): CallHandler => ({ handle: () => of(value) });

const buildErrorCallHandler = (error: Error): CallHandler => ({
  handle: () => throwError(() => error),
});

describe(GatewayResponseInterceptor, () => {
  let sut: GatewayResponseInterceptor;
  let reflector: Reflector;
  let getMetadata: jest.Mock;
  let defaults: IGatewayDefaults | undefined;

  beforeEach(() => {
    drainPoolForTesting();
    getMetadata = jest.fn();
    reflector = { get: getMetadata } as unknown as Reflector;
    defaults = undefined;
    sut = new GatewayResponseInterceptor(
      reflector,
      new DefaultGatewayReplyBuilder(),
      new DefaultStatusResolver(),
      defaults,
    );
  });

  afterEach(() => {
    drainPoolForTesting();
  });

  describe('happy path — @GatewayRoute handler', () => {
    it('wraps handler return into the wire envelope using the resolver default', async () => {
      // Given: extras carry meta.http; handler returns a body
      getMetadata.mockReturnValue({ meta: { http: { method: 'POST', path: '/users' } } });

      const envelope: IEnvelopeWithSlot = {};
      const ctx = buildContext(envelope);

      // When: interceptor runs
      const result = await firstValueFrom(sut.intercept(ctx, buildCallHandler({ id: 1 })));

      // Then: 200 (resolver default for non-null body), empty headers, body forwarded
      expect(result).toEqual({ status: 200, headers: {}, body: { id: 1 } });
    });

    it('honours an accumulator-set status over the resolver default', async () => {
      // Given: extras with meta.http; envelope already carries an accumulator
      // with status 201 (simulating an earlier @GatewayResponse() injection)
      getMetadata.mockReturnValue({ meta: { http: { method: 'POST', path: '/users' } } });

      const acc = new GatewayResponseAccumulator();

      acc.status(201);
      const envelope: IEnvelopeWithSlot = { [RESPONSE_ACCUMULATOR_KEY]: acc };

      // When: intercept runs over a handler that returns a body
      const result = await firstValueFrom(
        sut.intercept(buildContext(envelope), buildCallHandler({ id: 1 })),
      );

      // Then: 201 (from accumulator) wins over the resolver's 200
      expect((result as { status: number }).status).toBe(201);
    });

    it('snapshots accumulator headers into the reply (not by reference, no mutation bleed)', async () => {
      getMetadata.mockReturnValue({ meta: { http: { method: 'GET', path: '/x' } } });

      // Given: pre-stashed accumulator with single + multi-value headers
      const acc = new GatewayResponseAccumulator();

      acc.header('x-custom', 'one').appendHeader('x-multi', 'a').appendHeader('x-multi', 'b');
      const envelope: IEnvelopeWithSlot = { [RESPONSE_ACCUMULATOR_KEY]: acc };

      // When: intercept runs
      const result = (await firstValueFrom(
        sut.intercept(buildContext(envelope), buildCallHandler(null)),
      )) as { headers: Record<string, readonly string[]> };

      // Then: headers come through verbatim (multi-value preserved)
      expect(result.headers['x-custom']).toEqual(['one']);
      expect(result.headers['x-multi']).toEqual(['a', 'b']);
    });

    it('releases the accumulator back to the pool after success', async () => {
      getMetadata.mockReturnValue({ meta: { http: { method: 'GET', path: '/x' } } });

      const envelope: IEnvelopeWithSlot = {};

      await firstValueFrom(sut.intercept(buildContext(envelope), buildCallHandler({ ok: true })));

      // After finalize, the slot is cleared and the pool has one entry
      expect(envelope[RESPONSE_ACCUMULATOR_KEY]).toBeUndefined();
      expect(getPoolSizeForTesting()).toBe(1);
    });

    it('releases the accumulator on the error path too (symmetric finalize)', async () => {
      getMetadata.mockReturnValue({ meta: { http: { method: 'GET', path: '/x' } } });

      const envelope: IEnvelopeWithSlot = {};
      const error = new Error('boom');

      await expect(
        firstValueFrom(sut.intercept(buildContext(envelope), buildErrorCallHandler(error))),
      ).rejects.toBe(error);

      expect(envelope[RESPONSE_ACCUMULATOR_KEY]).toBeUndefined();
      expect(getPoolSizeForTesting()).toBe(1);
    });
  });

  describe('happy path — @GatewayAuthVerifier handler', () => {
    it('wraps the verifier return in a 200 reply regardless of the resolver', async () => {
      getMetadata.mockReturnValue({ meta: { verifier: { id: 'jwt' } } });

      const result = await firstValueFrom(
        sut.intercept(buildContext({}), buildCallHandler({ sub: 'user-1' })),
      );

      expect(result).toEqual({ status: 200, headers: {}, body: { sub: 'user-1' } });
    });
  });

  describe('edge cases', () => {
    it('passes the handler output through untouched when no gateway metadata is present', async () => {
      // Given: extras lacking both http and verifier (defensive guard)
      getMetadata.mockReturnValue(undefined);

      // When: intercept runs
      const result = await firstValueFrom(
        sut.intercept(buildContext({}), buildCallHandler({ raw: true })),
      );

      // Then: the value is the raw handler output, not an envelope
      expect(result).toEqual({ raw: true });
      // Pool must remain untouched on the no-op path
      expect(getPoolSizeForTesting()).toBe(0);
    });

    it('seeds cookieDefaults from GATEWAY_DEFAULTS so the first res.cookie() honours module config', async () => {
      defaults = { cookies: { httpOnly: true, secure: true, path: '/' } };
      sut = new GatewayResponseInterceptor(
        reflector,
        new DefaultGatewayReplyBuilder(),
        new DefaultStatusResolver(),
        defaults,
      );

      getMetadata.mockReturnValue({ meta: { http: { method: 'GET', path: '/x' } } });

      const envelope: IEnvelopeWithSlot = {};
      const ctx = buildContext(envelope);

      // Pre-acquire is synchronous — assert immediately after intercept returns,
      // then drain the Observable so finalize runs and the pool reclaims the slot.
      const observable = sut.intercept(ctx, buildCallHandler(undefined));
      const accBeforeFinalize = envelope[RESPONSE_ACCUMULATOR_KEY] as GatewayResponseAccumulator;

      expect(accBeforeFinalize.cookieDefaults).toEqual({
        httpOnly: true,
        secure: true,
        path: '/',
      });

      await firstValueFrom(observable);
    });

    it('reuses a pre-stashed accumulator without acquiring a second one', async () => {
      getMetadata.mockReturnValue({ meta: { http: { method: 'GET', path: '/x' } } });

      const preExisting = new GatewayResponseAccumulator();
      const envelope: IEnvelopeWithSlot = { [RESPONSE_ACCUMULATOR_KEY]: preExisting };

      const observable = sut.intercept(buildContext(envelope), buildCallHandler(undefined));

      // Synchronously after intercept: pre-stashed instance still in slot
      expect(envelope[RESPONSE_ACCUMULATOR_KEY]).toBe(preExisting);

      await firstValueFrom(observable);
    });
  });
});
