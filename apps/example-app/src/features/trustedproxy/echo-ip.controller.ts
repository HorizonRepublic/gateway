import { Controller } from '@nestjs/common';

import { GatewayMeta, GatewayRoute } from '@horizon-republic/gateway-sdk';

import type { IGatewayRequestMeta } from '@horizon-republic/gateway-sdk';

@Controller()
export class EchoIPController {
  @GatewayRoute({
    method: 'GET',
    path: '/__tp/ip',
    pattern: 'tp.ip',
  })
  public ip(@GatewayMeta() meta: IGatewayRequestMeta): { ip: string } {
    return { ip: meta.remoteAddr };
  }
}
