import { Module } from '@nestjs/common';

import { AuthModule } from '../auth/auth.module';

import { RateLimitController } from './ratelimit.controller';

@Module({
  imports: [AuthModule],
  controllers: [RateLimitController],
})
export class RateLimitModule {}
