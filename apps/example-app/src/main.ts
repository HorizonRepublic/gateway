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
