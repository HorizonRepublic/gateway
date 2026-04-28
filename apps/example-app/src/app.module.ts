import { Module } from '@nestjs/common';

import { GatewayModule } from '@horizon-republic/gateway-sdk';
import { JetstreamModule } from '@horizon-republic/nestjs-jetstream';

import { AuthModule } from './features/auth/auth.module';
import { ContractModule } from './features/contract/contract.module';
import { CoreModule } from './features/core/core.module';
import { RateLimitModule } from './features/ratelimit/ratelimit.module';
import { ResponseModule } from './features/response/response.module';
import { HealthStateModule } from './health/health.bootstrap';
import { HealthModule } from './health/health.module';

@Module({
  imports: [
    JetstreamModule.forRoot({
      name: 'example-app',
      servers: [process.env['NATS_URL'] ?? 'nats://localhost:4222'],
    }),
    GatewayModule.forRoot({
      defaults: {
        cookies: {
          httpOnly: true,
          secure: true,
          sameSite: 'lax',
          path: '/',
        },
        cors: { origins: ['https://default.example'] },
        headers: {
          'x-default-header': 'forRoot',
          'x-route': 'forRoot-default',
        },
      },
    }),
    HealthStateModule,
    HealthModule,
    CoreModule,
    AuthModule,
    ResponseModule,
    ContractModule,
    RateLimitModule,
  ],
})
export class AppModule {}
