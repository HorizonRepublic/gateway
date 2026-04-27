/**
 * Pattern prefix used by `@GatewayAuthVerifier` when registering a verifier
 * handler via `@MessagePattern`.
 * @remarks
 * The decorator appends the verifier `id` to this prefix to form the full
 * pattern passed to `nestjs-jetstream`, which in turn produces:
 *
 *   - NATS subject: `<service>__microservice.cmd.auth.verifier.<id>`
 *   - KV key:       `<service>.cmd.auth.verifier.<id>`
 *
 * Both names are built by `nestjs-jetstream` through its existing subject
 * construction helpers, so the gateway-side `registry.SubjectFromKey()`
 * reconstructs the subject from the KV key with zero new parsing logic.
 *
 * COMPATIBILITY NOTE: this string is part of the wire contract between
 * `@horizon-republic/gateway-sdk` and `gateway-server`. Changing it is a
 * breaking change for the entire gateway auth feature — both sides must
 * update in lockstep. The Go counterpart lives alongside
 * `apps/gateway-server/internal/registry/subject.go`.
 */
export const AUTH_VERIFIER_PATTERN_PREFIX = 'auth.verifier.' as const;
