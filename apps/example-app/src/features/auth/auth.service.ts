import { Injectable } from '@nestjs/common';

import type { IClaims } from './auth.types';

const TOKENS: ReadonlyMap<string, IClaims | 'banned'> = new Map<string, IClaims | 'banned'>([
  ['demo-alice', { sub: 'alice', roles: ['user'] }],
  ['demo-admin', { sub: 'admin', roles: ['admin', 'user'] }],
  ['demo-banned', 'banned'],
]);

export type LookupResult =
  | { kind: 'allow'; claims: IClaims }
  | { kind: 'banned' }
  | { kind: 'unknown' };

@Injectable()
export class AuthService {
  public lookup(token: string | undefined): LookupResult {
    if (token === undefined || token === '') {
      return { kind: 'unknown' };
    }

    const found = TOKENS.get(token);

    if (found === undefined) {
      return { kind: 'unknown' };
    }

    if (found === 'banned') {
      return { kind: 'banned' };
    }

    return { kind: 'allow', claims: found };
  }
}
