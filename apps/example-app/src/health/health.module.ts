import { Module } from '@nestjs/common';

import { HealthStateModule } from './health.bootstrap';
import { HealthController } from './health.controller';
import { ResetController } from './reset.controller';

@Module({
  controllers: [HealthController, ResetController],
  imports: [HealthStateModule],
})
export class HealthModule {}
