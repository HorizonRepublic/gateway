import { NestFactory } from '@nestjs/core';
import { FastifyAdapter } from '@nestjs/platform-fastify';

import { describe, expect, it } from '@jest/globals';

import { AppModule } from './app.module';

/**
 * Smoke test: the full application DI graph resolves with the same
 * adapter the production bootstrap in `main.ts` uses.
 *
 * `NestFactory.create` instantiates every provider in the graph but runs
 * no lifecycle hooks, so no NATS connection is attempted.
 * `@nestjs/testing`'s `Test.createTestingModule` cannot be used here:
 * in `@nestjs/testing@12.0.0-alpha.2` `compile()` preloads
 * `@nestjs/platform-express` via `loadPackage`, which terminates the
 * process when the package is absent — and this app is fastify-only.
 */
describe(AppModule, () => {
  it('compiles and resolves the DI graph', async () => {
    // abortOnError:false makes DI failures reject instead of exiting
    // the jest worker process.
    const app = await NestFactory.create(AppModule, new FastifyAdapter(), {
      logger: false,
      abortOnError: false,
    });

    expect(app).toBeDefined();

    await app.close();
  });
});
