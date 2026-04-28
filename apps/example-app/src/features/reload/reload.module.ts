import { Module } from '@nestjs/common';

import { ShadowController } from './shadow.controller';

@Module({
  controllers: [ShadowController],
})
export class ReloadModule {}
