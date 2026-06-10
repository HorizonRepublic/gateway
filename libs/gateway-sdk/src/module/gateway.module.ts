import { Global, Logger, Module, type DynamicModule, type Provider } from '@nestjs/common';

import { GatewayExceptionFilter } from '../filters/gateway-exception.filter';
import { GatewayResponseInterceptor } from '../interceptors/gateway-response.interceptor';
import { assertCorsCredentialsNotWildcard } from '../normalization/cors-validator';
import { DefaultErrorBodyFactory } from '../normalization/default-error-body.factory';
import { DefaultGatewayReplyBuilder } from '../normalization/default-reply.builder';
import { DefaultStatusResolver } from '../normalization/default-status.resolver';
import { assertRateLimitConfig } from '../normalization/rate-limit-validator';
import { setDefaultsSnapshot } from '../runtime/defaults-snapshot';
import {
  GATEWAY_DEFAULTS,
  GATEWAY_ERROR_BODY_FACTORY,
  GATEWAY_REPLY_BUILDER,
  GATEWAY_STATUS_RESOLVER,
} from '../tokens/gateway-tokens.constant';

import type {
  IGatewayModuleAsyncOptions,
  IGatewayModuleOptions,
} from './gateway-module-options.interface';
import type { IGatewayDefaults } from '../types/gateway-defaults.interface';

/**
 * Recursively freeze a defaults object, its nested objects, and arrays.
 * @remarks
 * A shallow `Object.freeze` is not enough for the defaults snapshot:
 * `mergeRouteDefaults` copies `cors` / `rateLimit` / `headers` into route
 * metadata BY REFERENCE, and the lazy `meta` getter memoizes per snapshot
 * identity. A nested mutation after boot would neither invalidate the
 * memo nor be visible as a new snapshot — it would silently diverge what
 * the transport re-serialises to the `handler_registry` KV on heartbeat.
 * Deep-freezing turns that class of bug into a loud `TypeError` at the
 * mutation site. Runs once per `forRoot` / factory resolution.
 */
const deepFreeze = (value: unknown): void => {
  if (typeof value !== 'object' || value === null || Object.isFrozen(value)) {
    return;
  }

  for (const key of Object.keys(value)) {
    deepFreeze((value as Record<string, unknown>)[key]);
  }

  Object.freeze(value);
};

/**
 * Light cross-field semantic checks on module-level defaults that cannot
 * be expressed by the `IGatewayDefaults` type alone. CORS
 * wildcard+credentials and rps/burst-shape errors throw — these defaults
 * represent legal-but-suspicious topologies, so we WARN via the NestJS
 * logger and let boot continue.
 *
 * Currently warns on:
 *
 *   - `rateLimit.burst < rateLimit.rps`. Legal — every GCRA reading of
 *     `(rps, burst)` is well-defined for any non-negative pair — but
 *     unusual. Most production rate-limit policies want a burst ceiling
 *     that is at least equal to the sustained rate so a single second of
 *     normal traffic doesn't immediately wedge the bucket.
 *   - `rateLimit.keyBy` includes a `'user:'` prefix while no `auth` is
 *     required at the global level. The Go gateway falls back to IP for
 *     anonymous traffic when a `user:` claim cannot resolve; this is
 *     correct behaviour but can surprise operators who expected per-user
 *     isolation across the board. The warn line gives them a loud signal
 *     at boot.
 *
 * Pure function modulo the logger side effect.
 */
const validateDefaults = (defaults: IGatewayDefaults, logger: Logger): void => {
  const { rateLimit } = defaults;

  if (rateLimit === undefined) {
    return;
  }

  if (rateLimit.burst !== undefined && rateLimit.burst < rateLimit.rps) {
    logger.warn(
      `defaults.rateLimit.burst (${String(rateLimit.burst)}) is less than ` +
        `defaults.rateLimit.rps (${String(rateLimit.rps)}). This is legal but ` +
        `unusual: a sub-rps burst causes the bucket to wedge after a single ` +
        `second of sustained traffic. Confirm the policy is intentional.`,
    );
  }

  if (rateLimit.keyBy !== undefined) {
    const userKeyPresent = rateLimit.keyBy.some((k) => k.startsWith('user:'));

    if (userKeyPresent) {
      logger.warn(
        `defaults.rateLimit.keyBy includes a 'user:' prefix. Anonymous ` +
          `requests have no claims to extract from, so the gateway will fall ` +
          `back to client IP for those — per-user isolation only applies to ` +
          `authenticated traffic. Make auth required at the route level for ` +
          `any endpoint where claim-based isolation is essential.`,
      );
    }
  }
};

/**
 * Global NestJS module that wires the gateway SDK building blocks into
 * the application's DI container.
 * @remarks
 * Marked `@Global()` so that `GatewayResponseInterceptor` and
 * `GatewayExceptionFilter` — which are referenced by class in the
 * `@GatewayRoute` decorator via `@UseInterceptors` / `@UseFilters` — are
 * resolvable anywhere in the application without re-importing the module
 * inside every feature module.
 *
 * Override any of the three normalization contracts via `forRoot()`
 * options. All three slots have production-ready defaults; pass `{}` (or
 * nothing) to accept all defaults.
 * @example
 * ```ts
 * @Module({
 *   imports: [GatewayModule.forRoot({})],
 * })
 * export class AppModule {}
 * ```
 */
@Global()
@Module({})
export class GatewayModule {
  /**
   * Build a dynamic module descriptor with the provided options.
   * @param options - Configuration for the gateway SDK. Override any of
   *                  the three normalization contracts by passing a class
   *                  reference; the remaining slots use their defaults.
   *                  Pass `defaults` to apply module-level endpoint
   *                  defaults merged into every `@GatewayRoute` handler's
   *                  metadata at read time (no lifecycle-hook ordering
   *                  dependency).
   * @remarks
   * Validation pass over `options.defaults`:
   *
   *   - **Throws** when `defaults.cors` combines `credentials: true` with
   *     a wildcard origin (browsers silently reject that combination per
   *     the Fetch Living Standard).
   *   - **Throws** when `defaults.rateLimit.rps` is `<= 0` or
   *     `defaults.rateLimit.burst` is negative — the Go gateway would
   *     interpret those as "no limit" / undefined behaviour.
   *   - **Warns (NestJS Logger)** when `defaults.rateLimit.burst` is less
   *     than `defaults.rateLimit.rps`, or when `defaults.rateLimit.keyBy`
   *     includes a `'user:'` prefix (anonymous traffic falls back to
   *     client IP). These are legal-but-suspicious shapes; boot continues.
   */
  public static forRoot(options: IGatewayModuleOptions = {}): DynamicModule {
    assertCorsCredentialsNotWildcard(options.defaults?.cors, 'GatewayModule.forRoot');
    assertRateLimitConfig(options.defaults?.rateLimit, 'GatewayModule.forRoot');

    if (options.defaults !== undefined) {
      validateDefaults(options.defaults, new Logger(GatewayModule.name));
    }

    const replyBuilderProvider: Provider = {
      provide: GATEWAY_REPLY_BUILDER,
      useClass: options.replyBuilder ?? DefaultGatewayReplyBuilder,
    };

    const statusResolverProvider: Provider = {
      provide: GATEWAY_STATUS_RESOLVER,
      useClass: options.statusResolver ?? DefaultStatusResolver,
    };

    const errorBodyFactoryProvider: Provider = {
      provide: GATEWAY_ERROR_BODY_FACTORY,
      useClass: options.errorBodyFactory ?? DefaultErrorBodyFactory,
    };

    const frozenDefaults = options.defaults ?? {};

    deepFreeze(frozenDefaults);

    setDefaultsSnapshot(frozenDefaults);

    const defaultsProvider: Provider = {
      provide: GATEWAY_DEFAULTS,
      useValue: frozenDefaults,
    };

    return {
      module: GatewayModule,
      global: true,
      imports: [],
      providers: [
        defaultsProvider,
        replyBuilderProvider,
        statusResolverProvider,
        errorBodyFactoryProvider,
        GatewayResponseInterceptor,
        GatewayExceptionFilter,
      ],
      exports: [
        GATEWAY_DEFAULTS,
        GATEWAY_REPLY_BUILDER,
        GATEWAY_STATUS_RESOLVER,
        GATEWAY_ERROR_BODY_FACTORY,
        GatewayResponseInterceptor,
        GatewayExceptionFilter,
      ],
    };
  }

  /**
   * Build a dynamic module descriptor from async configuration.
   * @param asyncOptions - Async options including an optional `imports`
   *                       array, an optional `inject` array, and a
   *                       `useFactory` function that returns
   *                       `IGatewayModuleOptions` or a `Promise` of it.
   * @remarks
   * Use this variant when options depend on providers that must be
   * resolved by NestJS DI at startup — for example when timeout or CORS
   * origins come from a config service backed by environment variables.
   *
   * Note: the three normalization contract slots (`replyBuilder`,
   * `statusResolver`, `errorBodyFactory`) always use their default
   * implementations in the async variant. Only `defaults` is resolved
   * asynchronously.
   * @example
   * ```ts
   * GatewayModule.forRootAsync({
   *   imports: [ConfigModule],
   *   inject: [APP_CONFIG],
   *   useFactory: (config: IAppConfig) => ({
   *     defaults: {
   *       cors: { origins: config.corsOrigins },
   *       timeout: config.requestTimeout,
   *     },
   *   }),
   * })
   * ```
   */
  public static forRootAsync(asyncOptions: IGatewayModuleAsyncOptions): DynamicModule {
    const defaultsProvider: Provider = {
      provide: GATEWAY_DEFAULTS,
      useFactory: async (...args: unknown[]) => {
        const resolved = await asyncOptions.useFactory(...args);

        assertCorsCredentialsNotWildcard(resolved.defaults?.cors, 'GatewayModule.forRootAsync');
        assertRateLimitConfig(resolved.defaults?.rateLimit, 'GatewayModule.forRootAsync');

        if (resolved.defaults !== undefined) {
          validateDefaults(resolved.defaults, new Logger(GatewayModule.name));
        }

        const frozenDefaults = resolved.defaults ?? {};

        deepFreeze(frozenDefaults);

        setDefaultsSnapshot(frozenDefaults);

        return frozenDefaults;
      },
      inject: asyncOptions.inject ?? [],
    };

    return {
      module: GatewayModule,
      global: true,
      imports: [...(asyncOptions.imports ?? [])],
      providers: [
        defaultsProvider,
        {
          provide: GATEWAY_REPLY_BUILDER,
          useClass: DefaultGatewayReplyBuilder,
        },
        {
          provide: GATEWAY_STATUS_RESOLVER,
          useClass: DefaultStatusResolver,
        },
        {
          provide: GATEWAY_ERROR_BODY_FACTORY,
          useClass: DefaultErrorBodyFactory,
        },
        GatewayResponseInterceptor,
        GatewayExceptionFilter,
      ],
      exports: [
        GATEWAY_DEFAULTS,
        GATEWAY_REPLY_BUILDER,
        GATEWAY_STATUS_RESOLVER,
        GATEWAY_ERROR_BODY_FACTORY,
        GatewayResponseInterceptor,
        GatewayExceptionFilter,
      ],
    };
  }
}
