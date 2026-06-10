import { describe, expect, it } from '@jest/globals';

import { getDefaultsSnapshot, setDefaultsSnapshot } from '../../runtime/defaults-snapshot';

import type { IGatewayDefaults } from '../../types';

/**
 * Case order matters: the pristine-module assertion must run first —
 * Jest evaluates the module registry once per file, and later cases
 * install snapshots.
 */
describe('defaults-snapshot', () => {
  it('returns an empty object before any snapshot is installed', () => {
    expect(getDefaultsSnapshot()).toEqual({});
  });

  it('returns the exact object passed to setDefaultsSnapshot', () => {
    const defaults: IGatewayDefaults = Object.freeze({ timeout: 5000 });

    setDefaultsSnapshot(defaults);

    expect(getDefaultsSnapshot()).toBe(defaults);
  });

  it('last write wins across repeated installs', () => {
    const first: IGatewayDefaults = Object.freeze({ timeout: 1000 });
    const second: IGatewayDefaults = Object.freeze({ timeout: 2000 });

    setDefaultsSnapshot(first);
    setDefaultsSnapshot(second);

    expect(getDefaultsSnapshot()).toBe(second);
  });
});
