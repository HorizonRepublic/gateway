import { Module } from '@nestjs/common';

import { AuthFlowController } from './auth-flow.controller';
import { CookieReaderController } from './cookie-reader.controller';

@Module({
  controllers: [AuthFlowController, CookieReaderController],
})
export class ResponseModule {}
