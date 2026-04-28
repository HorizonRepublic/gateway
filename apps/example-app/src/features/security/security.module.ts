import { Module } from '@nestjs/common';

import { EchoHeadersController } from './echo-headers.controller';

@Module({
  controllers: [EchoHeadersController],
})
export class SecurityModule {}
