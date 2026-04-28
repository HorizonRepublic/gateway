import { Controller, HttpCode, Post } from '@nestjs/common';

@Controller('__e2e')
export class ResetController {
  @Post('reset')
  @HttpCode(204)
  public reset(): void {
    // PR 1 has no in-memory state to clear. Feature-pack PRs that add
    // mutable handlers will inject their stores here and call reset().
  }
}
