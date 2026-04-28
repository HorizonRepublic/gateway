import { Module } from '@nestjs/common';

import { AuthFlowController } from './auth-flow.controller';

@Module({
  controllers: [AuthFlowController],
})
export class ResponseModule {}
