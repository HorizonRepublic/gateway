import { Controller } from '@nestjs/common';

import { GatewayParam, GatewayRoute, GatewayUser } from '@horizon-republic/gateway-sdk';

import type { IClaims } from './auth.types';

@Controller()
export class ArticlesController {
  @GatewayRoute({
    method: 'GET',
    path: '/articles/:id',
    pattern: 'articles.get',
    auth: { optional: true },
  })
  public byId(
    @GatewayParam('id') id: string,
    @GatewayUser() claims: IClaims | undefined,
  ): { id: string; viewer: IClaims | null } {
    return { id, viewer: claims ?? null };
  }
}
