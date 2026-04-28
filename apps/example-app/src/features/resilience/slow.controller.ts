import { Controller } from '@nestjs/common';

import { GatewayRoute } from '@horizon-republic/gateway-sdk';

@Controller()
export class SlowController {
  @GatewayRoute({
    method: 'GET',
    path: '/__res/slow',
    pattern: 'res.slow',
  })
  public async slow(): Promise<{ done: boolean }> {
    await new Promise((resolve) => setTimeout(resolve, 500));

    return { done: true };
  }
}
