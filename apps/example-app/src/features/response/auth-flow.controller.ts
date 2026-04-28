import { Controller } from '@nestjs/common';

import {
  GatewayBody,
  GatewayHeader,
  GatewayResponse,
  GatewayRoute,
} from '@horizon-republic/gateway-sdk';

import type { IGatewayResponse } from '@horizon-republic/gateway-sdk';

interface ILoginDto {
  name: string;
}

@Controller()
export class AuthFlowController {
  @GatewayRoute({ method: 'POST', path: '/auth/login', pattern: 'auth.login' })
  public login(
    @GatewayBody() dto: ILoginDto,
    @GatewayResponse() res: IGatewayResponse,
  ): { sub: string } {
    res.status(201).cookie('sid', `${dto.name}-token`, { maxAge: 3600 });

    return { sub: dto.name };
  }

  @GatewayRoute({ method: 'POST', path: '/auth/login-strict', pattern: 'auth.login.strict' })
  public loginStrict(
    @GatewayBody() dto: ILoginDto,
    @GatewayResponse() res: IGatewayResponse,
  ): { sub: string } {
    res.status(201).cookie('sid', `${dto.name}-token`, { maxAge: 3600, sameSite: 'strict' });

    return { sub: dto.name };
  }

  @GatewayRoute({ method: 'POST', path: '/auth/logout', pattern: 'auth.logout' })
  public logout(@GatewayResponse() res: IGatewayResponse): { ok: boolean } {
    res.clearCookie('sid');

    return { ok: true };
  }

  @GatewayRoute({ method: 'GET', path: '/auth/google/start', pattern: 'auth.google.start' })
  public googleStart(@GatewayResponse() res: IGatewayResponse): void {
    res.redirect(
      'https://accounts.google.com/o/oauth2/v2/auth?client_id=demo&redirect_uri=https%3A%2F%2Fexample.com%2Fcb',
      302,
    );
  }

  @GatewayRoute({ method: 'GET', path: '/auth/echo-headers', pattern: 'auth.echo.headers' })
  public echoHeaders(
    @GatewayHeader('x-trace-id') traceId: string | undefined,
    @GatewayResponse() res: IGatewayResponse,
  ): Record<string, never> {
    res.header('x-custom', 'horizon').header('x-trace', traceId ?? '');

    return {};
  }
}
