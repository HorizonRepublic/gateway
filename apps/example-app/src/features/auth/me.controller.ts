import { Controller } from '@nestjs/common';

import { GatewayRequestId, GatewayRoute, GatewayUser } from '@horizon-republic/gateway-sdk';

import type { IClaims } from './auth.types';

@Controller()
export class MeController {
  @GatewayRoute({
    method: 'GET',
    path: '/me',
    pattern: 'me.get',
    auth: true,
  })
  public me(
    @GatewayUser() claims: IClaims,
    @GatewayRequestId() requestId: string,
  ): { requestId: string; you: IClaims } {
    return { requestId, you: claims };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/me-session',
    pattern: 'me.session.get',
    auth: { verifier: 'session' },
  })
  public meSession(@GatewayUser() claims: IClaims): { you: IClaims } {
    return { you: claims };
  }
}
