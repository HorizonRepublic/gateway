import { Module } from '@nestjs/common';

import { AuthModule } from '../auth/auth.module';

import { BenchRateLimitController } from './bench-ratelimit.controller';
import { RateLimitController } from './ratelimit.controller';

@Module({
  imports: [AuthModule],
  controllers: [RateLimitController, BenchRateLimitController],
})
export class RateLimitModule {}
