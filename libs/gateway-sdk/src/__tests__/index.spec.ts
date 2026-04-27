import { describe, expect, it } from '@jest/globals';

import * as sdk from '../index';

describe('@horizon-republic/gateway-sdk barrel', () => {
  it('resolves and is an object', () => {
    expect(typeof sdk).toBe('object');
    expect(sdk).not.toBeNull();
  });
});
