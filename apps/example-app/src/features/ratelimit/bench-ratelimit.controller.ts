import { Controller } from '@nestjs/common';

import { GatewayRoute } from '@horizon-republic/gateway-sdk';

/**
 * Bench-only fixture. The limit is set high enough that the rate-limit
 * gate evaluates on every request but never rejects under benchmark
 * load — isolating the limiter's per-request hot-path cost.
 */
@Controller()
export class BenchRateLimitController {
  @GatewayRoute({
    method: 'GET',
    path: '/rl/bench',
    pattern: 'rl.bench',
    rateLimit: { rps: 1_000_000, burst: 1_000_000 },
  })
  public bench(): { ok: true } {
    return { ok: true };
  }
}
