import { Module } from '@nestjs/common';

import { SlowController } from './slow.controller';

@Module({
  controllers: [SlowController],
})
export class ResilienceModule {}
