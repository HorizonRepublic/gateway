import { Controller, ForbiddenException, UnauthorizedException } from '@nestjs/common';

import { GatewayAuthVerifier, GatewayHeader } from '@horizon-republic/gateway-sdk';

import { AuthService } from '../auth.service';

import type { IClaims } from '../auth.types';

const parseBearer = (raw: string | undefined): string | undefined => {
  if (raw === undefined) return undefined;
  const trimmed = raw.trim();

  if (!trimmed.toLowerCase().startsWith('bearer ')) return undefined;

  const token = trimmed.slice('bearer '.length).trim();

  return token === '' ? undefined : token;
};

@Controller()
export class JwtVerifier {
  public constructor(private readonly auth: AuthService) {}

  @GatewayAuthVerifier({ id: 'jwt', default: true })
  public verify(@GatewayHeader('authorization') authz: string | undefined): IClaims {
    const token = parseBearer(authz);
    const result = this.auth.lookup(token);

    if (result.kind === 'banned') {
      throw new ForbiddenException('user is banned');
    }

    if (result.kind === 'unknown') {
      throw new UnauthorizedException('invalid or missing token');
    }

    return result.claims;
  }
}
