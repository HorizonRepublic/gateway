import { Controller } from '@nestjs/common';

import { GatewayHeaders, GatewayRoute } from '@horizon-republic/gateway-sdk';

@Controller()
export class EchoHeadersController {
  @GatewayRoute({
    method: 'GET',
    path: '/__sec/headers',
    pattern: 'sec.headers.echo',
  })
  public echoHeaders(@GatewayHeaders() headers: Record<string, string>): {
    headers: Record<string, string>;
  } {
    return { headers };
  }
}
