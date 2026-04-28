import 'reflect-metadata';

import { Logger } from '@nestjs/common';
import { NestFactory } from '@nestjs/core';
import { FastifyAdapter, NestFastifyApplication } from '@nestjs/platform-fastify';

import { JetstreamStrategy } from '@horizon-republic/nestjs-jetstream';

import { AppModule } from './app.module';
import { HealthState } from './health/health.bootstrap';

const HEALTH_PORT = Number(process.env['HEALTH_PORT'] ?? 3001);

const bootstrap = async (): Promise<void> => {
  const logger = new Logger('example-app');

  // Hybrid Nest app: Fastify HTTP listener exposes /healthz + /readyz +
  // POST /__e2e/reset on HEALTH_PORT for the Compose healthcheck. None
  // of those endpoints are @GatewayRoute-decorated; the gateway never
  // sees them.
  const app = await NestFactory.create<NestFastifyApplication>(AppModule, new FastifyAdapter(), {
    logger: ['error', 'warn', 'log'],
  });

  // Hybrid-app lifecycle gotcha: NestJS only fires `onModuleInit` hooks
  // when `app.listen()` calls `app.init()` internally. In a hybrid app
  // that pattern means the JetStream strategy publishes handler
  // metadata to KV inside `startAllMicroservices()` BEFORE the SDK's
  // GatewayMetadataEnricher gets a chance to merge `forRoot` defaults
  // into per-route extras. Forcing `app.init()` here drains every
  // module's lifecycle hooks ahead of microservice startup so the
  // enriched metadata is what reaches the wire. `init()` is idempotent;
  // the later `app.listen()` is a no-op for the lifecycle phase.
  await app.init();

  // Microservice runtime: JetstreamStrategy is registered by
  // JetstreamModule.forRoot so we resolve it from the DI graph rather
  // than instantiating it directly (the constructor takes 10+ injected
  // collaborators).
  app.connectMicroservice({ strategy: app.get(JetstreamStrategy) }, { inheritAppConfig: true });

  app.enableShutdownHooks();
  await app.startAllMicroservices();
  await app.listen(HEALTH_PORT, '0.0.0.0');

  app.get(HealthState).markReady();
  logger.log(`example-app ready: http on :${HEALTH_PORT}, microservice on NATS`);
};

bootstrap().catch((err: unknown) => {
  process.stderr.write(`example-app bootstrap failed: ${String(err)}\n`);
  process.exit(1);
});
