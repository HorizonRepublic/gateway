import { Controller } from '@nestjs/common';

import { GatewayRoute, GatewayUser } from '@horizon-republic/gateway-sdk';

import type { IClaims } from '../auth/auth.types';

@Controller()
export class RateLimitController {
  @GatewayRoute({
    method: 'GET',
    path: '/rl/burst',
    pattern: 'rl.burst',
    rateLimit: { rps: 1, burst: 1 },
  })
  public burst(): { ok: boolean } {
    return { ok: true };
  }

  /**
   * Mirrors `/rl/burst` with a per-route `failPolicy: 'closed'`. The e2e
   * suite saturates the memory store's entry cap on a replica whose
   * gateway-wide policy is open, then asserts this route rejects (503)
   * while `/rl/burst` (inherit) allows — both fail-policy branches and
   * the route-over-env precedence pinned on one replica.
   */
  @GatewayRoute({
    method: 'GET',
    path: '/rl/burst-closed',
    pattern: 'rl.burst.closed',
    rateLimit: { rps: 1, burst: 1, failPolicy: 'closed' },
  })
  public burstClosed(): { ok: boolean } {
    return { ok: true };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/rl/by-header',
    pattern: 'rl.by.header',
    rateLimit: { rps: 1, burst: 1, keyBy: ['header:x-api-key'] },
  })
  public byHeader(): { ok: boolean } {
    return { ok: true };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/rl/by-user',
    pattern: 'rl.by.user',
    auth: true,
    rateLimit: { rps: 1, burst: 1, keyBy: ['user:sub'] },
  })
  public byUser(@GatewayUser() claims: IClaims): { sub: string } {
    return { sub: claims.sub };
  }

  @GatewayRoute({
    method: 'GET',
    path: '/rl/multi-natskv',
    pattern: 'rl.multi.natskv',
    rateLimit: { rps: 1, burst: 1, store: 'nats-kv' },
  })
  public multiNatsKV(): { ok: boolean } {
    return { ok: true };
  }
}
