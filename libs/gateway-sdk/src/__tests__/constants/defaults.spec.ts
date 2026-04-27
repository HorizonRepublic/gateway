import { describe, expect, it } from '@jest/globals';

import {
  DEFAULT_STATUS_INTERNAL_ERROR,
  DEFAULT_STATUS_NO_CONTENT,
  DEFAULT_STATUS_OK,
} from '../../constants';

describe('default HTTP status constants', () => {
  it('matches the RFC 7231 status codes the resolver applies', () => {
    expect(DEFAULT_STATUS_OK).toBe(200);
    expect(DEFAULT_STATUS_NO_CONTENT).toBe(204);
    expect(DEFAULT_STATUS_INTERNAL_ERROR).toBe(500);
  });
});
