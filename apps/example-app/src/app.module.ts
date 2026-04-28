import { Module } from '@nestjs/common';

import { GatewayModule } from '@horizon-republic/gateway-sdk';
import { JetstreamModule } from '@horizon-republic/nestjs-jetstream';

import { AuthModule } from './features/auth/auth.module';
import { CoreModule } from './features/core/core.module';
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
      },
    }),
    HealthStateModule,
    HealthModule,
    CoreModule,
    AuthModule,
    ResponseModule,
  ],
})
export class AppModule {}
