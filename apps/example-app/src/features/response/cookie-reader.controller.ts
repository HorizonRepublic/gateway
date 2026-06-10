import { Controller } from '@nestjs/common';

import { GatewayCookie, GatewayRoute } from '@horizon-republic/gateway-sdk';

/**
 * Request-side cookie fixture for the `@GatewayCookie` e2e pack. The
 * response-builder routes exercise cookie WRITES (`res.cookie` /
 * `res.clearCookie`); this route is the read direction — a real wire
 * round-trip through the gateway's header forwarding and the SDK's
 * RFC 6265 parser (semicolon split, first-wins on duplicate names,
 * percent-decoding).
 */
@Controller()
export class CookieReaderController {
  @GatewayRoute({ method: 'GET', path: '/cookies/session', pattern: 'cookies.session' })
  public readSession(@GatewayCookie('session') session: string | undefined): {
    value: string | null;
  } {
    return { value: session ?? null };
  }
}
