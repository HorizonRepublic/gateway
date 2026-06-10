import { Controller } from '@nestjs/common';
import { MessagePattern, Payload } from '@nestjs/microservices';

interface IGatewayReply {
  status: number;
  headers: Record<string, string[]>;
  body: unknown;
}

interface IEnvelopeWithAuth {
  auth?: { sub?: string };
}

const claimsReplyOf = (sub: 'shadow-a1' | 'shadow-a2'): IGatewayReply => ({
  status: 200,
  headers: { 'content-type': ['application/json'] },
  body: { sub, roles: [] },
});

/**
 * Shadow auth fixtures for the verifier hot-reload e2e pack. Plain
 * `@MessagePattern` handlers — no `@GatewayAuthVerifier` / `@GatewayRoute`
 * decoration, so the transport publishes NO registry entries for them:
 * the NATS handlers exist, but every KV entry that points at them is
 * synthetic, owned by the test, and immune to the metadata heartbeat.
 *
 * The `a1` / `a2` pattern names sort lexicographically BEFORE the real
 * `auth.verifier.jwt` key on purpose: the gateway resolves a `default:
 * true` collision by keeping the first lexicographic key, so a synthetic
 * default can win the slot without mutating the heartbeat-owned jwt entry.
 *
 * Both verifiers are allow-all and distinguishable only by the `sub`
 * claim they return — `reload.whoami` echoes that claim back so the e2e
 * suite can observe ON THE WIRE which verifier the gateway consulted.
 */
@Controller()
export class ShadowAuthController {
  @MessagePattern('auth.verifier.a1')
  public verifierA1(): IGatewayReply {
    return claimsReplyOf('shadow-a1');
  }

  @MessagePattern('auth.verifier.a2')
  public verifierA2(): IGatewayReply {
    return claimsReplyOf('shadow-a2');
  }

  @MessagePattern('reload.whoami')
  public whoami(@Payload() envelope: IEnvelopeWithAuth): IGatewayReply {
    return {
      status: 200,
      headers: { 'content-type': ['application/json'] },
      body: { sub: envelope.auth?.sub ?? null },
    };
  }
}
