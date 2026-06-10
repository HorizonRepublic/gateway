import { describe, expect, it } from '@jest/globals';

import { assertCorsCredentialsNotWildcard } from '../../normalization/cors-validator';

const context = '@GatewayRoute(POST /users)';

describe(assertCorsCredentialsNotWildcard.name, () => {
  describe('happy path', () => {
    it('returns silently when cors is undefined', () => {
      expect(() => {
        assertCorsCredentialsNotWildcard(undefined, context);
      }).not.toThrow();
    });

    it('returns silently when credentials is not enabled', () => {
      expect(() => {
        assertCorsCredentialsNotWildcard({ origins: ['*'], credentials: false }, context);
      }).not.toThrow();
    });

    it('returns silently when origins are explicit and credentials are enabled', () => {
      expect(() => {
        assertCorsCredentialsNotWildcard(
          { origins: ['https://app.example.com'], credentials: true },
          context,
        );
      }).not.toThrow();
    });

    it('returns silently when wildcard origin is used without credentials', () => {
      expect(() => {
        assertCorsCredentialsNotWildcard({ origins: ['*'] }, context);
      }).not.toThrow();
    });
  });

  describe('error cases', () => {
    it('throws when wildcard origin combines with credentials: true', () => {
      // Given: the configuration browsers silently reject per Fetch Living Standard
      const cors = { origins: ['*'], credentials: true };

      // When / Then: validator fails fast at registration time
      expect(() => {
        assertCorsCredentialsNotWildcard(cors, context);
      }).toThrow(/cors\.credentials: true cannot be combined with cors\.origins: '\*'/);
    });

    it('throws when wildcard sits among other origins with credentials: true', () => {
      const cors = { origins: ['https://app.example.com', '*'], credentials: true };

      expect(() => {
        assertCorsCredentialsNotWildcard(cors, context);
      }).toThrow(/cannot be combined/);
    });

    it('includes the caller context in the error message', () => {
      const cors = { origins: ['*'], credentials: true };

      expect(() => {
        assertCorsCredentialsNotWildcard(cors, '@GatewayRoute(POST /users)');
      }).toThrow(/Source: @GatewayRoute\(POST \/users\)\./);
    });
  });
});

describe('assertCorsCredentialsNotWildcard — list-field wildcards with credentials', () => {
  it.each(['methods', 'headers', 'exposeHeaders'] as const)(
    'rejects %s: ["*"] with credentials: true',
    (field) => {
      expect(() => {
        assertCorsCredentialsNotWildcard(
          { origins: ['https://app.example.com'], credentials: true, [field]: ['*'] },
          'GatewayModule.forRoot',
        );
      }).toThrow(new RegExp(`cors\\.${field}`));
    },
  );

  it('accepts list-field wildcards without credentials', () => {
    expect(() => {
      assertCorsCredentialsNotWildcard(
        { origins: ['https://app.example.com'], headers: ['*'] },
        'GatewayModule.forRoot',
      );
    }).not.toThrow();
  });
});
