import { Module } from '@nestjs/common';

import { EchoIPController } from './echo-ip.controller';

@Module({
  controllers: [EchoIPController],
})
export class TrustedProxyModule {}
