import { Logger } from '@nestjs/common';

import { afterEach, beforeEach, describe, expect, it, jest } from '@jest/globals';

import { GatewayModule } from '../../module/gateway.module';
import { DefaultErrorBodyFactory } from '../../normalization/default-error-body.factory';
import { DefaultGatewayReplyBuilder } from '../../normalization/default-reply.builder';
import { DefaultStatusResolver } from '../../normalization/default-status.resolver';
import { getDefaultsSnapshot, setDefaultsSnapshot } from '../../runtime/defaults-snapshot';
import {
  GATEWAY_DEFAULTS,
  GATEWAY_ERROR_BODY_FACTORY,
  GATEWAY_REPLY_BUILDER,
  GATEWAY_STATUS_RESOLVER,
} from '../../tokens/gateway-tokens.constant';

import type { IErrorBodyFactory } from '../../normalization/contracts/error-body-factory.interface';
import type { IGatewayReplyBuilder } from '../../normalization/contracts/reply-builder.interface';
import type { IStatusResolver } from '../../normalization/contracts/status-resolver.interface';
import type { Provider } from '@nestjs/common';

const findProvider = (
  providers: readonly Provider[] | undefined,
  token: symbol,
): Provider | undefined =>
  providers?.find((p) => typeof p === 'object' && 'provide' in p && p.provide === token);

const useClassOf = (provider: Provider | undefined): unknown =>
  provider !== undefined && typeof provider === 'object' && 'useClass' in provider
    ? provider.useClass
    : undefined;

const useValueOf = (provider: Provider | undefined): unknown =>
  provider !== undefined && typeof provider === 'object' && 'useValue' in provider
    ? provider.useValue
    : undefined;

class CustomReplyBuilder implements IGatewayReplyBuilder {
  public success(): never {
    throw new Error('not used in spec');
  }

  public error(): never {
    throw new Error('not used in spec');
  }
}

class CustomStatusResolver implements IStatusResolver {
  public resolveSuccess(): number {
    return 0;
  }
}

class CustomErrorBodyFactory implements IErrorBodyFactory {
  public build(): never {
    throw new Error('not used in spec');
  }
}

describe(GatewayModule, () => {
  let warnSpy: ReturnType<typeof jest.spyOn>;

  beforeEach(() => {
    warnSpy = jest.spyOn(Logger.prototype, 'warn').mockImplementation(() => undefined);
  });

  afterEach(() => {
    warnSpy.mockRestore();
  });

  describe(GatewayModule.forRoot.name, () => {
    describe('happy path', () => {
      it('wires every contract slot to its default class when called with no options', () => {
        const dyn = GatewayModule.forRoot();

        expect(useClassOf(findProvider(dyn.providers, GATEWAY_REPLY_BUILDER))).toBe(
          DefaultGatewayReplyBuilder,
        );
        expect(useClassOf(findProvider(dyn.providers, GATEWAY_STATUS_RESOLVER))).toBe(
          DefaultStatusResolver,
        );
        expect(useClassOf(findProvider(dyn.providers, GATEWAY_ERROR_BODY_FACTORY))).toBe(
          DefaultErrorBodyFactory,
        );
      });

      it('honours explicit overrides for each slot', () => {
        const dyn = GatewayModule.forRoot({
          replyBuilder: CustomReplyBuilder,
          statusResolver: CustomStatusResolver,
          errorBodyFactory: CustomErrorBodyFactory,
        });

        expect(useClassOf(findProvider(dyn.providers, GATEWAY_REPLY_BUILDER))).toBe(
          CustomReplyBuilder,
        );
        expect(useClassOf(findProvider(dyn.providers, GATEWAY_STATUS_RESOLVER))).toBe(
          CustomStatusResolver,
        );
        expect(useClassOf(findProvider(dyn.providers, GATEWAY_ERROR_BODY_FACTORY))).toBe(
          CustomErrorBodyFactory,
        );
      });

      it('freezes the defaults value provider so consumers cannot mutate at runtime', () => {
        const dyn = GatewayModule.forRoot({ defaults: { timeout: 5000 } });
        const defaults = useValueOf(findProvider(dyn.providers, GATEWAY_DEFAULTS));

        expect(Object.isFrozen(defaults)).toBe(true);
      });

      it('marks the dynamic module as global', () => {
        expect(GatewayModule.forRoot().global).toBe(true);
      });
    });

    describe('config validation', () => {
      it('throws when defaults.cors combines wildcard origin with credentials: true', () => {
        expect(() =>
          GatewayModule.forRoot({
            defaults: { cors: { origins: ['*'], credentials: true } },
          }),
        ).toThrow(/cors\.credentials: true cannot be combined/);
      });

      it('throws when defaults.rateLimit.rps is invalid', () => {
        expect(() => GatewayModule.forRoot({ defaults: { rateLimit: { rps: 0 } } })).toThrow(
          /rateLimit\.rps must be a positive integer/,
        );
      });

      it('warns when defaults.rateLimit.burst is below rps (legal but suspicious)', () => {
        GatewayModule.forRoot({ defaults: { rateLimit: { rps: 100, burst: 50 } } });

        expect(warnSpy).toHaveBeenCalledTimes(1);
        expect(warnSpy.mock.calls[0]?.[0]).toMatch(/burst.*less than.*rps/);
      });

      it('warns when defaults.rateLimit.keyBy uses a user: prefix without auth', () => {
        GatewayModule.forRoot({
          defaults: { rateLimit: { rps: 100, keyBy: ['user:sub', 'ip'] } },
        });

        expect(warnSpy).toHaveBeenCalledTimes(1);
        expect(warnSpy.mock.calls[0]?.[0]).toMatch(/'user:' prefix/);
      });
    });
  });

  describe(GatewayModule.forRootAsync.name, () => {
    describe('happy path', () => {
      it('wires the three contract slots to defaults (async path does not honour overrides)', () => {
        const dyn = GatewayModule.forRootAsync({
          useFactory: () => ({ replyBuilder: CustomReplyBuilder }),
        });

        expect(useClassOf(findProvider(dyn.providers, GATEWAY_REPLY_BUILDER))).toBe(
          DefaultGatewayReplyBuilder,
        );
        expect(useClassOf(findProvider(dyn.providers, GATEWAY_STATUS_RESOLVER))).toBe(
          DefaultStatusResolver,
        );
        expect(useClassOf(findProvider(dyn.providers, GATEWAY_ERROR_BODY_FACTORY))).toBe(
          DefaultErrorBodyFactory,
        );
      });

      it('marks the dynamic module as global', () => {
        const dyn = GatewayModule.forRootAsync({ useFactory: () => ({}) });

        expect(dyn.global).toBe(true);
      });
    });

    describe('async-resolved validation', () => {
      const callDefaultsFactory = async (
        asyncOptions: Parameters<typeof GatewayModule.forRootAsync>[0],
      ): Promise<unknown> => {
        const dyn = GatewayModule.forRootAsync(asyncOptions);
        const provider = findProvider(dyn.providers, GATEWAY_DEFAULTS);

        if (provider === undefined || typeof provider !== 'object' || !('useFactory' in provider)) {
          throw new Error('defaults factory provider not present');
        }

        return (provider.useFactory as () => Promise<unknown>)();
      };

      it('throws when the resolved defaults.rateLimit is invalid', async () => {
        await expect(
          callDefaultsFactory({
            useFactory: () => ({ defaults: { rateLimit: { rps: 0 } } }),
          }),
        ).rejects.toThrow(/rateLimit\.rps must be a positive integer/);
      });

      it('freezes the resolved defaults', async () => {
        const value = await callDefaultsFactory({
          useFactory: () => ({ defaults: { timeout: 5000 } }),
        });

        expect(Object.isFrozen(value)).toBe(true);
      });

      it('warns on resolved-but-suspicious shapes (sub-rps burst)', async () => {
        await callDefaultsFactory({
          useFactory: () => ({ defaults: { rateLimit: { rps: 100, burst: 50 } } }),
        });

        expect(warnSpy).toHaveBeenCalledTimes(1);
      });
    });
  });
});

describe('GatewayModule defaults snapshot install', () => {
  afterEach(() => {
    setDefaultsSnapshot({});
  });

  it('forRoot installs a frozen snapshot synchronously', () => {
    GatewayModule.forRoot({ defaults: { timeout: 4000 } });

    expect(getDefaultsSnapshot()).toEqual({ timeout: 4000 });
    expect(Object.isFrozen(getDefaultsSnapshot())).toBe(true);
  });

  it('forRoot without defaults installs an empty frozen snapshot', () => {
    GatewayModule.forRoot();

    expect(getDefaultsSnapshot()).toEqual({});
    expect(Object.isFrozen(getDefaultsSnapshot())).toBe(true);
  });

  it('forRootAsync installs the snapshot when the factory resolves', async () => {
    const dyn = GatewayModule.forRootAsync({
      useFactory: () => ({ defaults: { timeout: 9000 } }),
    });
    const provider = findProvider(dyn.providers, GATEWAY_DEFAULTS);

    if (provider === undefined || typeof provider !== 'object' || !('useFactory' in provider)) {
      throw new Error('defaults factory provider not present');
    }

    await (provider.useFactory as () => Promise<unknown>)();

    expect(getDefaultsSnapshot()).toEqual({ timeout: 9000 });
    expect(Object.isFrozen(getDefaultsSnapshot())).toBe(true);
  });
});

describe('GatewayModule defaults deep-freeze', () => {
  afterEach(() => {
    setDefaultsSnapshot({});
  });

  it('forRoot freezes nested defaults objects, not only the top level', () => {
    GatewayModule.forRoot({
      defaults: {
        headers: { 'x-a': '1' },
        cors: { origins: ['https://app.example.com'] },
        rateLimit: { rps: 100, keyBy: ['ip'] },
      },
    });

    const snap = getDefaultsSnapshot();

    expect(Object.isFrozen(snap.headers)).toBe(true);
    expect(Object.isFrozen(snap.cors)).toBe(true);
    expect(Object.isFrozen(snap.cors?.origins)).toBe(true);
    expect(Object.isFrozen(snap.rateLimit)).toBe(true);
    expect(Object.isFrozen(snap.rateLimit?.keyBy)).toBe(true);
  });

  it('forRootAsync freezes nested defaults objects at factory resolution', async () => {
    const dyn = GatewayModule.forRootAsync({
      useFactory: () => ({ defaults: { headers: { 'x-b': '2' } } }),
    });
    const provider = findProvider(dyn.providers, GATEWAY_DEFAULTS);

    if (provider === undefined || typeof provider !== 'object' || !('useFactory' in provider)) {
      throw new Error('defaults factory provider not present');
    }

    await (provider.useFactory as () => Promise<unknown>)();

    expect(Object.isFrozen(getDefaultsSnapshot().headers)).toBe(true);
  });
});
