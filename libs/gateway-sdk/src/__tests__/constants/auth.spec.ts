import { describe, expect, it } from '@jest/globals';

import { AUTH_VERIFIER_PATTERN_PREFIX } from '../../constants';

describe('auth verifier pattern prefix', () => {
  it('is the wire-stable string the gateway-server reconstructs from KV keys', () => {
    expect(AUTH_VERIFIER_PATTERN_PREFIX).toBe('auth.verifier.');
  });

  it('ends with a dot so the verifier id appends cleanly', () => {
    expect(AUTH_VERIFIER_PATTERN_PREFIX.endsWith('.')).toBe(true);
  });
});
