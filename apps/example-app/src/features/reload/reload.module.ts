import { Module } from '@nestjs/common';

import { ShadowAuthController } from './shadow-auth.controller';
import { ShadowController } from './shadow.controller';

@Module({
  controllers: [ShadowController, ShadowAuthController],
})
export class ReloadModule {}
