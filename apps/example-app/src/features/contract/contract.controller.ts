import {
  BadRequestException,
  ConflictException,
  Controller,
  ForbiddenException,
  InternalServerErrorException,
} from '@nestjs/common';

import { GatewayParam, GatewayRoute } from '@horizon-republic/gateway-sdk';

interface IProduct {
  id: string;
  name: string;
}

interface ITenant {
  id: string;
}

@Controller()
export class ContractController {
  @GatewayRoute({
    method: 'GET',
    path: '/products/:id',
    pattern: 'contract.products.get',
    cors: { origins: ['https://shop.example'], maxAge: 600 },
  })
  public getProduct(@GatewayParam('id') id: string): IProduct {
    return { id, name: `product-${id}` };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/tenants/:id',
    pattern: 'contract.tenants.get',
    cors: { origins: ['https://tenant.example'], credentials: true },
  })
  public getTenant(@GatewayParam('id') id: string): ITenant {
    return { id };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/shared',
    pattern: 'contract.shared.get',
  })
  public getShared(): { ok: boolean } {
    return { ok: true };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/slow',
    pattern: 'contract.slow.get',
    timeout: 50,
  })
  public async getSlow(): Promise<{ done: boolean }> {
    await new Promise((resolve) => setTimeout(resolve, 250));

    return { done: true };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/headers/static',
    pattern: 'contract.headers.static.get',
    headers: { 'cache-control': 'no-store', 'x-route': 'route-wins' },
  })
  public getStaticHeaders(): { ok: boolean } {
    return { ok: true };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/errors/:kind',
    pattern: 'contract.errors.kind',
  })
  public throwByKind(@GatewayParam('kind') kind: string): { ok: boolean } {
    switch (kind) {
      case 'badrequest':
        throw new BadRequestException('bad-request-kind');
      case 'forbidden':
        throw new ForbiddenException('forbidden-kind');
      case 'conflict':
        throw new ConflictException('conflict-kind');
      case 'internal':
        throw new InternalServerErrorException('internal-kind');
      default:
        return { ok: true };
    }
  }
}
