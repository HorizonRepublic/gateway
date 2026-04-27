import { Inject, Injectable, type OnModuleInit } from '@nestjs/common';
import { DiscoveryService, MetadataScanner } from '@nestjs/core';
import { PATTERN_EXTRAS_METADATA } from '@nestjs/microservices/constants';

import { GATEWAY_DEFAULTS } from '../tokens';

import { mergeRouteDefaults } from './merge-route-defaults';

import type { IGatewayDefaults } from '../types';

/**
 * Enriches `@GatewayRoute` handler metadata with module-level defaults at
 * module initialization time.
 * @remarks
 * Runs during `onModuleInit` — after `GatewayModule.forRoot` has provided
 * `GATEWAY_DEFAULTS`, but before `connectMicroservice` triggers handler
 * scanning by `nestjs-jetstream`. This ensures the strategy sees
 * already-merged metadata when writing to KV.
 *
 * Uses NestJS `DiscoveryService` to enumerate controllers and
 * `MetadataScanner` to find decorated methods. Only methods whose extras
 * contain `meta.http` (i.e., `@GatewayRoute`-decorated handlers) are
 * enriched — plain `@MessagePattern` handlers are left untouched.
 */
@Injectable()
export class GatewayMetadataEnricher implements OnModuleInit {
  public constructor(
    @Inject(GATEWAY_DEFAULTS)
    private readonly defaults: IGatewayDefaults,
    private readonly discovery: DiscoveryService,
    private readonly scanner: MetadataScanner,
  ) {}

  public onModuleInit(): void {
    const controllers = this.discovery.getControllers();

    for (const wrapper of controllers) {
      const instance = wrapper.instance;

      if (!instance?.constructor) {
        continue;
      }

      const prototype = Object.getPrototypeOf(instance) as Record<string, unknown>;
      const methods = this.scanner.getAllMethodNames(prototype);

      for (const method of methods) {
        this.enrichMethod(prototype, method);
      }
    }
  }

  private enrichMethod(prototype: Record<string, unknown>, method: string): void {
    const handler = prototype[method];

    if (typeof handler !== 'function') {
      return;
    }

    const extras = Reflect.getMetadata(PATTERN_EXTRAS_METADATA, handler) as
      | { meta?: Record<string, unknown> }
      | undefined;

    if (!extras?.meta || !('http' in extras.meta)) {
      return;
    }

    extras.meta = mergeRouteDefaults(this.defaults, extras.meta);
  }
}
