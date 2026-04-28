import { Module } from '@nestjs/common';

import { GatewayModule } from '@horizon-republic/gateway-sdk';
import { JetstreamModule } from '@horizon-republic/nestjs-jetstream';

import { AuthModule } from './features/auth/auth.module';
import { CoreModule } from './features/core/core.module';
import { HealthStateModule } from './health/health.bootstrap';
import { HealthModule } from './health/health.module';

@Module({
  imports: [
    JetstreamModule.forRoot({
      name: 'example-app',
      servers: [process.env['NATS_URL'] ?? 'nats://localhost:4222'],
    }),
    GatewayModule.forRoot(),
    HealthStateModule,
    HealthModule,
    CoreModule,
    AuthModule,
  ],
})
export class AppModule {}
