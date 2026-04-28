import { Module } from '@nestjs/common';

import { HealthController } from './health.controller';
import { HealthStateModule } from './health.bootstrap';
import { ResetController } from './reset.controller';

@Module({
  controllers: [HealthController, ResetController],
  imports: [HealthStateModule],
})
export class HealthModule {}
