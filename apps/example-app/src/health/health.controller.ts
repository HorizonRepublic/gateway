import { Controller, Get } from '@nestjs/common';

import { HealthState } from './health.bootstrap';

@Controller()
export class HealthController {
  public constructor(private readonly state: HealthState) {}

  @Get('healthz')
  public liveness(): { ok: boolean } {
    return { ok: true };
  }

  @Get('readyz')
  public readiness(): { ok: boolean } {
    return { ok: this.state.isReady() };
  }
}
