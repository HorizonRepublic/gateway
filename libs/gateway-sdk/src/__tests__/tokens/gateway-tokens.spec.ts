import { describe, expect, it } from '@jest/globals';

import {
  GATEWAY_DEFAULTS,
  GATEWAY_ERROR_BODY_FACTORY,
  GATEWAY_REPLY_BUILDER,
  GATEWAY_STATUS_RESOLVER,
} from '../../tokens';

describe('gateway DI tokens', () => {
  it('exposes four unique Symbol tokens', () => {
    const tokens = [
      GATEWAY_DEFAULTS,
      GATEWAY_ERROR_BODY_FACTORY,
      GATEWAY_REPLY_BUILDER,
      GATEWAY_STATUS_RESOLVER,
    ];

    expect(tokens.every((t) => typeof t === 'symbol')).toBe(true);
    expect(new Set(tokens).size).toBe(tokens.length);
  });

  it('carries human-readable descriptions', () => {
    expect(GATEWAY_REPLY_BUILDER.description).toBe('gateway-reply-builder');
    expect(GATEWAY_STATUS_RESOLVER.description).toBe('gateway-status-resolver');
    expect(GATEWAY_ERROR_BODY_FACTORY.description).toBe('gateway-error-body-factory');
    expect(GATEWAY_DEFAULTS.description).toBe('gateway-defaults');
  });
});
