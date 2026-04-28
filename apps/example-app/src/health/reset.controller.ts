import { Controller, HttpCode, Post } from '@nestjs/common';

import { UsersService } from '../features/core/users.service';

@Controller('__e2e')
export class ResetController {
  public constructor(private readonly users: UsersService) {}

  @Post('reset')
  @HttpCode(204)
  public reset(): void {
    this.users.reset();
  }
}
