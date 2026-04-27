import {
  Inject,
  Injectable,
  Optional,
  type CallHandler,
  type ExecutionContext,
  type NestInterceptor,
} from '@nestjs/common';
import { Reflector } from '@nestjs/core';
import { PATTERN_EXTRAS_METADATA } from '@nestjs/microservices/constants';

import { finalize, map, type Observable } from 'rxjs';

import { acquireAccumulator, releaseAccumulator } from '../runtime/gateway-response-pool';
import { RESPONSE_ACCUMULATOR_KEY } from '../runtime/response-accumulator-symbol';
import {
  GATEWAY_DEFAULTS,
  GATEWAY_REPLY_BUILDER,
  GATEWAY_STATUS_RESOLVER,
} from '../tokens/gateway-tokens.constant';

import type { IGatewayReplyBuilder } from '../normalization/contracts/reply-builder.interface';
import type { IStatusResolver } from '../normalization/contracts/status-resolver.interface';
import type { GatewayResponseAccumulator } from '../runtime/gateway-response-accumulator';
import type { IGatewayDefaults } from '../types/gateway-defaults.interface';
import type { IGatewayHttpMeta } from '../types/gateway-http-meta.interface';

/**
 * Shape that `@MessagePattern`'s `extras.meta` is expected to hold for a
 * gateway-exposed handler.
 * @remarks
 * File-private, non-exported. Duck-types the `extras` record stored at
 * `PATTERN_EXTRAS_METADATA` by `@nestjs/microservices` so the interceptor
 * does not have to import the framework's internal `PatternMetadata`
 * descriptor and stays tolerant of additive schema changes.
 *
 * The two top-level keys are mutually exclusive in practice:
 *
 *   - `http` — regular `@GatewayRoute` handler. The value populates routing
 *     metadata on the Go side and drives status resolution.
 *   - `verifier` — `@GatewayAuthVerifier` handler. Its presence alone tells
 *     the interceptor to wrap the return in a 200 reply; the fields of the
 *     object itself are only read on the gateway side.
 */
interface IExtrasWithGatewayMeta {
  readonly meta?: {
    readonly http?: IGatewayHttpMeta;
    readonly verifier?: object;
  };
}

/**
 * Runtime view of the RPC envelope limited to its `Symbol`-keyed
 * accumulator slot. Module-private — the full envelope shape is exposed to
 * user code through `IGatewayRequest`, but the accumulator slot is shared
 * only with the `@GatewayResponse()` decorator.
 */
type IEnvelopeWithAccumulatorSlot = Record<symbol, unknown>;

/**
 * HTTP status used for every successful verifier reply. Verifiers that
 * need to signal anything other than success throw an `HttpException`
 * subclass, which `GatewayExceptionFilter` converts into the appropriate
 * error envelope — so the success path is always 200, unambiguously.
 */
const VERIFIER_SUCCESS_STATUS = 200;

/**
 * Frozen sentinel returned by `snapshotHeaders` when no accumulator exists
 * on the envelope. Module-level constant so the zero-header fast path
 * avoids allocating a fresh empty object per request. The freeze guards
 * against accidental mutation by downstream reply builders.
 */
const EMPTY_HEADERS: Readonly<Record<string, readonly string[]>> = Object.freeze({});

/**
 * Build an immutable, request-owned copy of the accumulator's headers map
 * for the reply builder.
 * @remarks
 * The accumulator recycles its `headers` object identity across pool
 * resets so later acquirers observe a consistent shape. Passing that same
 * reference into `replyBuilder.success` is unsafe: the interceptor's
 * `finalize` hook releases the accumulator (resetting the underlying
 * headers) after `map` emits, which could otherwise clear the reply's
 * headers out from under downstream transport code synchronously before
 * the envelope reaches the gateway.
 *
 * Shallow-cloning the outer map AND copying each value array keeps the
 * reply envelope self-sufficient without deep-cloning cookie strings. The
 * returned object is a plain record but is typed as `Readonly<Record<…>>`
 * to signal intent at the call site — the reply builder treats its
 * `headers` parameter as `readonly`.
 */
const snapshotHeaders = (
  acc: GatewayResponseAccumulator | undefined,
): Readonly<Record<string, readonly string[]>> => {
  if (acc === undefined) {
    return EMPTY_HEADERS;
  }

  const source = acc.headers;
  const keys = Object.keys(source);

  if (keys.length === 0) {
    return EMPTY_HEADERS;
  }

  const snapshot: Record<string, readonly string[]> = {};

  for (const key of keys) {
    const values = source[key];

    if (values !== undefined) {
      snapshot[key] = [...values];
    }
  }

  return snapshot;
};

/**
 * Read the accumulator stashed on the envelope by a prior
 * `@GatewayResponse()` injection, or `undefined` if the handler never
 * injected one. One property lookup on a hidden-class slot — ~2-3ns in
 * V8 — is the entire fast-path overhead this interceptor pays per
 * request.
 */
const readAccumulator = (
  envelope: IEnvelopeWithAccumulatorSlot,
): GatewayResponseAccumulator | undefined => {
  const stashed = envelope[RESPONSE_ACCUMULATOR_KEY];

  return stashed as GatewayResponseAccumulator | undefined;
};

/**
 * Wraps the return value of an `@GatewayRoute`-decorated handler into an
 * `IGatewayReply` envelope, applying configured status-resolution rules.
 * @remarks
 * **Locally attached** via `@UseInterceptors(GatewayResponseInterceptor)`
 * inside the `@GatewayRoute` decorator — never registered globally.
 * Because it is bound only to gateway-exposed handlers, the interceptor
 * never has to discriminate between gateway and non-gateway calls: its
 * mere presence on the execution stack is proof of the former.
 *
 * HTTP metadata is read from `PATTERN_EXTRAS_METADATA` — the same NestJS
 * reflection key that `@MessagePattern(pattern, { meta: { http } })`
 * writes to. No custom reflection key is introduced, so HTTP routing
 * metadata has a single source of truth per handler.
 *
 * All normalization policy is delegated to injected contracts:
 *
 *   - `IGatewayReplyBuilder` via `GATEWAY_REPLY_BUILDER`
 *   - `IStatusResolver` via `GATEWAY_STATUS_RESOLVER`
 *
 * **Response mutation integration.** Handlers that inject
 * `@GatewayResponse()` mutate a pooled `GatewayResponseAccumulator`
 * stashed on the envelope under a module-private `Symbol` key. During
 * `map()` this interceptor reads the accumulator and merges its
 * `statusCode` / `headers` into the outgoing reply — `statusCode` wins
 * over `IStatusResolver` when set, and `headers` is forwarded verbatim to
 * `replyBuilder.success`. A `finalize` operator releases the accumulator
 * back to the pool on both success and error paths so throw/complete are
 * symmetric from the pool's perspective (the Express-convention throw
 * semantics then let the exception filter ignore the accumulated STATE
 * while the pool still reclaims the OBJECT).
 *
 * **Zero-overhead fast path.** Handlers that do NOT inject
 * `@GatewayResponse()` pay a single `envelope[SYMBOL] === undefined`
 * check (hidden-class lookup, ~2-3ns) and fall through to the
 * pre-mutation behavior with no allocation, no pool touch, and the
 * `finalize` callback short-circuits.
 *
 * The defensive `meta.http` guard covers the unsupported edge case in
 * which a consumer manually attaches this interceptor to a non-gateway
 * handler; rather than throwing at runtime, the interceptor passes the
 * handler output through untouched.
 * @example
 * ```ts
 * @GatewayRoute({ pattern: 'users.create', method: 'POST', path: '/users' })
 * createUser(@GatewayBody() dto: CreateUserDto) {
 *   return this.usersService.create(dto);
 * }
 * ```
 */
@Injectable()
export class GatewayResponseInterceptor implements NestInterceptor {
  public constructor(
    private readonly reflector: Reflector,
    @Inject(GATEWAY_REPLY_BUILDER)
    private readonly replyBuilder: IGatewayReplyBuilder,
    @Inject(GATEWAY_STATUS_RESOLVER)
    private readonly statusResolver: IStatusResolver,
    @Optional()
    @Inject(GATEWAY_DEFAULTS)
    private readonly gatewayDefaults: IGatewayDefaults | undefined,
  ) {}

  public intercept(context: ExecutionContext, next: CallHandler): Observable<unknown> {
    const extras = this.reflector.get<IExtrasWithGatewayMeta | undefined>(
      PATTERN_EXTRAS_METADATA,
      context.getHandler(),
    );
    const httpMeta = extras?.meta?.http;
    const isVerifier = extras?.meta?.verifier !== undefined;

    if (httpMeta === undefined && !isVerifier) {
      return next.handle();
    }

    const envelope = context.switchToRpc().getData<IEnvelopeWithAccumulatorSlot>();

    this.preAcquireAccumulator(envelope);

    if (httpMeta !== undefined) {
      return next.handle().pipe(
        map((value: unknown) => {
          const acc = readAccumulator(envelope);
          const status = acc?.statusCode ?? this.statusResolver.resolveSuccess(httpMeta, value);
          const headers = snapshotHeaders(acc);

          return this.replyBuilder.success(status, value, headers);
        }),
        finalize(() => {
          this.releaseAccumulatorIfPresent(envelope);
        }),
      );
    }

    return next.handle().pipe(
      map((value: unknown) => {
        const acc = readAccumulator(envelope);
        const headers = snapshotHeaders(acc);

        return this.replyBuilder.success(VERIFIER_SUCCESS_STATUS, value, headers);
      }),
      finalize(() => {
        this.releaseAccumulatorIfPresent(envelope);
      }),
    );
  }

  /**
   * Eagerly checkout an accumulator from the pool and stash it on the
   * envelope before the handler runs. Setting `cookieDefaults` here
   * ensures that the per-request defaults from
   * `GatewayModule.forRoot({ defaults: { cookies } })` are in place
   * before the handler's first `res.cookie()` call. The
   * `@GatewayResponse()` decorator detects the pre-stashed instance and
   * returns it directly without a second pool checkout. Handlers that do
   * not inject `@GatewayResponse()` still complete correctly: the
   * `finalize` operator releases the accumulator back to the pool so the
   * allocation cost is bounded to one object per request on the gateway
   * path.
   */
  private preAcquireAccumulator(envelope: IEnvelopeWithAccumulatorSlot): void {
    const existing = readAccumulator(envelope);

    if (existing !== undefined) {
      existing.cookieDefaults = this.gatewayDefaults?.cookies ?? {};

      return;
    }

    const acc = acquireAccumulator();

    acc.cookieDefaults = this.gatewayDefaults?.cookies ?? {};
    envelope[RESPONSE_ACCUMULATOR_KEY] = acc;
  }

  /**
   * Release the accumulator stashed on the envelope, if any, and clear
   * the `Symbol` slot. Runs on both success and error paths via the RxJS
   * `finalize` operator so every handler invocation is guaranteed to
   * return its accumulator to the pool exactly once. Clearing the slot
   * prevents a second interceptor pass on the same envelope object from
   * observing stale state — an invariant that matters for fixture-heavy
   * unit tests and for any future handler-retry strategy layered on top.
   */
  private releaseAccumulatorIfPresent(envelope: IEnvelopeWithAccumulatorSlot): void {
    const acc = readAccumulator(envelope);

    if (acc === undefined) {
      return;
    }

    Reflect.deleteProperty(envelope, RESPONSE_ACCUMULATOR_KEY);
    releaseAccumulator(acc);
  }
}
