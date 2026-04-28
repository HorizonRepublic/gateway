import { Module } from '@nestjs/common';

import { ContractController } from './contract.controller';

@Module({
  controllers: [ContractController],
})
export class ContractModule {}
