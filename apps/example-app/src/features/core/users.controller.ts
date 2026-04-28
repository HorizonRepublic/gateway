import { Controller, NotFoundException } from '@nestjs/common';

import {
  GatewayBody,
  GatewayHeader,
  GatewayParam,
  GatewayQuery,
  GatewayRoute,
} from '@horizon-republic/gateway-sdk';

import { UsersService } from './users.service';

import type { ICreateUserDto, IUpdateUserDto, IUser } from './users.types';

const toRoleArray = (raw: string | string[] | undefined): IUser['role'][] | undefined => {
  if (raw === undefined) return undefined;
  const arr = Array.isArray(raw) ? raw : [raw];

  return arr.filter((r): r is IUser['role'] => r === 'admin' || r === 'user');
};

@Controller()
export class UsersController {
  public constructor(private readonly users: UsersService) {}

  @GatewayRoute({ method: 'GET', path: '/users/:id', pattern: 'users.get' })
  public getById(
    @GatewayParam('id') id: string,
    @GatewayHeader('x-trace-id') traceId: string | undefined,
  ): IUser & { trace?: string } {
    const user = this.users.findById(id);

    if (user === undefined) {
      throw new NotFoundException(`user ${id} not found`);
    }

    return traceId === undefined ? user : { ...user, trace: traceId };
  }

  @GatewayRoute({ method: 'GET', path: '/users', pattern: 'users.list' })
  public list(@GatewayQuery('role') roles: string | string[] | undefined): IUser[] {
    return this.users.list(toRoleArray(roles));
  }

  @GatewayRoute({
    method: 'POST',
    path: '/users',
    pattern: 'users.create',
    statusCode: 201,
  })
  public create(@GatewayBody() dto: ICreateUserDto): IUser {
    return this.users.create(dto);
  }

  @GatewayRoute({ method: 'PATCH', path: '/users/:id', pattern: 'users.patch' })
  public patch(
    @GatewayParam('id') id: string,
    @GatewayBody() dto: IUpdateUserDto,
    @GatewayQuery('reason') reason: string | undefined,
  ): IUser & { reason?: string } {
    const user = this.users.update(id, dto);

    return reason === undefined ? user : { ...user, reason };
  }

  @GatewayRoute({ method: 'DELETE', path: '/users/:id', pattern: 'users.delete' })
  public delete(@GatewayParam('id') id: string): void {
    this.users.delete(id);
  }
}
