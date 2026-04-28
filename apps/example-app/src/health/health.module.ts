import { Module } from '@nestjs/common';

import { CoreModule } from '../features/core/core.module';

import { HealthStateModule } from './health.bootstrap';
import { HealthController } from './health.controller';
import { ResetController } from './reset.controller';

@Module({
  controllers: [HealthController, ResetController],
  imports: [HealthStateModule, CoreModule],
})
export class HealthModule {}
