import 'reflect-metadata';

import { PATTERN_EXTRAS_METADATA } from '@nestjs/microservices/constants';

import { beforeEach, describe, expect, it, jest } from '@jest/globals';

import { GatewayMetadataEnricher } from '../../runtime/gateway-metadata-enricher';

import type { IGatewayDefaults } from '../../types';
import type { DiscoveryService, MetadataScanner } from '@nestjs/core';

interface IControllerStubExtras {
  pattern?: string;
  meta?: Record<string, unknown>;
}

const setExtras = (handler: object, extras: IControllerStubExtras): void => {
  Reflect.defineMetadata(PATTERN_EXTRAS_METADATA, extras, handler);
};

const getExtras = (handler: object): IControllerStubExtras | undefined => {
  return Reflect.getMetadata(PATTERN_EXTRAS_METADATA, handler) as IControllerStubExtras | undefined;
};

/**
 * Build a controller-shaped stub with one decorated method whose extras we
 * populate ahead of `onModuleInit`. The enricher walks prototypes (not
 * instances), so the test mirrors that traversal exactly.
 */
const makeController = (
  methodName: string,
  extras: IControllerStubExtras | undefined,
): { wrapper: { instance: object }; handler: object } => {
  const handler = (): void => undefined;

  if (extras !== undefined) {
    setExtras(handler, extras);
  }

  class Controller {}

  Reflect.defineProperty(Controller.prototype, methodName, {
    value: handler,
    writable: false,
    configurable: true,
    enumerable: false,
  });
  const instance = new Controller();
  const wrapper = { instance };

  return { wrapper, handler };
};

describe(GatewayMetadataEnricher, () => {
  let sut: GatewayMetadataEnricher;
  let getControllers: jest.Mock;
  let getAllMethodNames: jest.Mock;
  let defaults: IGatewayDefaults;

  beforeEach(() => {
    defaults = {
      cors: { origins: ['*'] },
      rateLimit: { rps: 100 },
      headers: { 'cache-control': 'no-store' },
      timeout: 30000,
    };

    getControllers = jest.fn().mockReturnValue([]);
    getAllMethodNames = jest.fn().mockReturnValue([]);
    // The enricher reads only `discovery.getControllers()` and
    // `scanner.getAllMethodNames(prototype)`. Minimal structural stubs are
    // sufficient and avoid coupling the test to NestJS's deep `InstanceWrapper`
    // shape (44 declared fields the enricher never touches).
    const discovery = { getControllers } as unknown as DiscoveryService;
    const scanner = { getAllMethodNames } as unknown as MetadataScanner;

    sut = new GatewayMetadataEnricher(defaults, discovery, scanner);
  });

  describe(GatewayMetadataEnricher.prototype.onModuleInit.name, () => {
    describe('happy path', () => {
      it('merges defaults into a @GatewayRoute handler whose extras carry meta.http', () => {
        // Given: a controller with one handler whose extras has meta.http (the @GatewayRoute marker)
        const { wrapper, handler } = makeController('hello', {
          pattern: 'test.hello',
          meta: { http: { method: 'GET', path: '/hello' } },
        });

        getControllers.mockReturnValue([wrapper]);
        getAllMethodNames.mockReturnValue(['hello']);

        // When: enricher runs
        sut.onModuleInit();

        // Then: extras.meta now has cors / rateLimit / headers / timeout merged from defaults
        const enriched = getExtras(handler);

        expect(enriched?.meta).toEqual({
          http: { method: 'GET', path: '/hello' },
          cors: { origins: ['*'] },
          rateLimit: { rps: 100 },
          headers: { 'cache-control': 'no-store' },
          timeout: 30000,
        });
      });

      it('honours per-route values that override defaults (shallow replace)', () => {
        // Given: a route that already declares its own cors
        const { wrapper, handler } = makeController('create', {
          pattern: 'user.create',
          meta: {
            http: { method: 'POST', path: '/users' },
            cors: { origins: ['https://app.example.com'] },
          },
        });

        getControllers.mockReturnValue([wrapper]);
        getAllMethodNames.mockReturnValue(['create']);

        // When: enricher runs
        sut.onModuleInit();

        // Then: per-route cors stays intact, other slots merged from defaults
        const enriched = getExtras(handler);

        expect(enriched?.meta).toMatchObject({
          cors: { origins: ['https://app.example.com'] },
          rateLimit: { rps: 100 },
        });
      });
    });

    describe('edge cases — what the enricher leaves untouched', () => {
      it('leaves a plain @MessagePattern handler (no meta.http) alone', () => {
        // Given: extras without meta.http (plain RPC handler, no @GatewayRoute)
        const { wrapper, handler } = makeController('rpc', {
          pattern: 'service.rpc',
          meta: { other: 'thing' },
        });

        getControllers.mockReturnValue([wrapper]);
        getAllMethodNames.mockReturnValue(['rpc']);

        // When: enricher runs
        sut.onModuleInit();

        // Then: extras.meta is exactly what it was — no defaults bleed
        const after = getExtras(handler);

        expect(after?.meta).toEqual({ other: 'thing' });
      });

      it('skips methods that carry no PATTERN_EXTRAS_METADATA at all', () => {
        // Given: a controller method with zero extras metadata
        const { wrapper, handler } = makeController('plain', undefined);

        getControllers.mockReturnValue([wrapper]);
        getAllMethodNames.mockReturnValue(['plain']);

        // When: enricher runs
        // Then: the method's metadata stays absent (the enricher MUST NOT seed it)
        sut.onModuleInit();
        expect(getExtras(handler)).toBeUndefined();
      });

      it('skips controllers whose instance is null or has no constructor', () => {
        getControllers.mockReturnValue([{ instance: null }]);

        expect(() => {
          sut.onModuleInit();
        }).not.toThrow();
        expect(getAllMethodNames).not.toHaveBeenCalled();
      });
    });
  });
});
