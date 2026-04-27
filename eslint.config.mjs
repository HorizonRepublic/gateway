import nx from '@nx/eslint-plugin';
import eslintConfigPrettier from 'eslint-config-prettier';
import importPlugin from 'eslint-plugin-import-x';
import jsdoc from 'eslint-plugin-jsdoc';
import noSecrets from 'eslint-plugin-no-secrets';
// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-expect-error — plugin lacks types
import preferArrowPlugin from 'eslint-plugin-prefer-arrow';
import eslintPluginPrettier from 'eslint-plugin-prettier';
import rxjsX from 'eslint-plugin-rxjs-x';
// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-expect-error — plugin lacks types
import security from 'eslint-plugin-security';
import sonarjs from 'eslint-plugin-sonarjs';
import unicorn from 'eslint-plugin-unicorn';
import unusedImports from 'eslint-plugin-unused-imports';
import jsoncParser from 'jsonc-eslint-parser';
import tseslint from 'typescript-eslint';

const tsconfigRootDir = import.meta.dirname;

export default [
  // Nx flat presets
  ...nx.configs['flat/base'],
  ...nx.configs['flat/typescript'],
  ...nx.configs['flat/javascript'],

  // typescript-eslint strict preset
  ...tseslint.configs.strict,

  // SonarJS recommended (scoped to JS/TS)
  {
    ...sonarjs.configs.recommended,
    files: ['**/*.{js,jsx,ts,tsx}'],
    rules: {
      ...sonarjs.configs.recommended.rules,
      'sonarjs/todo-tag': 'off',
    },
  },

  // Project-wide ignores
  {
    ignores: [
      '**/node_modules/**',
      '**/dist/**',
      '**/.nx/**',
      '**/tmp/**',
      '**/coverage/**',
      '**/.docusaurus/**',
    ],
  },

  // Base rules for all JS/TS files
  {
    files: ['**/*.ts', '**/*.tsx', '**/*.js', '**/*.jsx'],
    plugins: {
      'prefer-arrow': preferArrowPlugin,
      prettier: eslintPluginPrettier,
      'unused-imports': unusedImports,
      'import-x': importPlugin,
      unicorn,
      security,
      'no-secrets': noSecrets,
    },
    rules: {
      // Naming conventions
      camelcase: ['error', { ignoreDestructuring: false, properties: 'never' }],

      // Import ordering and deduplication
      'import-x/newline-after-import': ['error', { count: 1 }],
      'import-x/no-duplicates': 'error',
      'import-x/order': [
        'error',
        {
          groups: ['builtin', 'external', 'internal', 'parent', 'sibling', 'index', 'type'],
          pathGroups: [
            { pattern: '@nestjs/**', group: 'external', position: 'before' },
            { pattern: '@nestia/**', group: 'external', position: 'before' },
            { pattern: '@horizon-republic/**', group: 'internal', position: 'before' },
          ],
          pathGroupsExcludedImportTypes: ['builtin', 'type'],
          'newlines-between': 'always',
          alphabetize: { order: 'asc', caseInsensitive: true },
        },
      ],

      // Class member spacing
      'lines-between-class-members': [
        'error',
        {
          enforce: [
            { blankLine: 'always', next: 'method', prev: 'method' },
            { blankLine: 'always', next: 'method', prev: 'field' },
          ],
        },
        { exceptAfterSingleLine: true },
      ],

      'max-depth': ['warn', 4],

      // General code quality
      'no-alert': 'error',
      'no-console': ['warn', { allow: ['warn', 'error'] }],
      'no-debugger': 'error',
      'no-duplicate-imports': 'off',
      'no-useless-constructor': 'error',
      'no-useless-return': 'error',
      'no-var': 'error',

      // Statement padding
      'padding-line-between-statements': [
        'error',
        { blankLine: 'always', next: '*', prev: ['const', 'let', 'var'] },
        { blankLine: 'any', next: ['const', 'let', 'var'], prev: ['const', 'let', 'var'] },
        { blankLine: 'always', next: '*', prev: 'import' },
        { blankLine: 'any', next: 'import', prev: 'import' },
        { blankLine: 'always', next: 'function', prev: '*' },
        { blankLine: 'always', next: 'class', prev: '*' },
        { blankLine: 'always', next: 'export', prev: '*' },
        { blankLine: 'always', next: '*', prev: 'block-like' },
      ],

      'prefer-arrow-callback': 'error',
      'prefer-arrow/prefer-arrow-functions': [
        'error',
        {
          classPropertiesAllowed: false,
          disallowPrototype: true,
          singleReturnOnly: false,
        },
      ],

      'prefer-const': 'error',
      'prefer-template': 'error',

      // Prettier integration
      'prettier/prettier': 'error',

      // Node.js — prefer explicit node: protocol for builtins
      'unicorn/prefer-node-protocol': 'error',

      // Error handling quality
      'unicorn/error-message': 'error',
      'unicorn/throw-new-error': 'error',
      'unicorn/prefer-optional-catch-binding': 'error',

      // Cleaner code patterns
      'unicorn/no-typeof-undefined': 'error',
      'unicorn/no-useless-promise-resolve-reject': 'error',
      'unicorn/prefer-export-from': ['error', { ignoreUsedVariables: true }],

      // Security — catch common backend vulnerabilities
      'security/detect-unsafe-regex': 'error',
      'security/detect-eval-with-expression': 'error',
      'security/detect-bidi-characters': 'error',
      'security/detect-new-buffer': 'warn',
      'security/detect-buffer-noassert': 'warn',
      'security/detect-possible-timing-attacks': 'warn',
      'security/detect-pseudoRandomBytes': 'warn',
      // Too noisy / not applicable for TypeScript projects
      'security/detect-object-injection': 'off',
      'security/detect-non-literal-fs-filename': 'off',
      'security/detect-non-literal-require': 'off',
      'security/detect-non-literal-regexp': 'off',
      'security/detect-no-csrf-before-method-override': 'off',
      'security/detect-child-process': 'off',

      // Prevent accidentally committed secrets (API keys, tokens, etc.)
      'no-secrets/no-secrets': ['error', { tolerance: 4.5 }],
    },
  },

  // Type-aware TypeScript rules
  {
    files: ['**/*.ts', '**/*.tsx'],
    languageOptions: {
      parser: tseslint.parser,
      parserOptions: {
        project: './tsconfig.base.json',
        tsconfigRootDir,
      },
    },
    plugins: {
      'rxjs-x': rxjsX,
      'unused-imports': unusedImports,
    },
    rules: {
      '@typescript-eslint/array-type': ['error', { default: 'array' }],
      '@typescript-eslint/consistent-type-definitions': ['error', 'interface'],
      '@typescript-eslint/explicit-function-return-type': [
        'error',
        {
          allowConciseArrowFunctionExpressionsStartingWithVoid: false,
          allowDirectConstAssertionInArrowFunctions: true,
          allowExpressions: false,
          allowHigherOrderFunctions: true,
          allowTypedFunctionExpressions: true,
        },
      ],
      '@typescript-eslint/explicit-member-accessibility': [
        'error',
        {
          accessibility: 'explicit',
          overrides: {
            accessors: 'explicit',
            constructors: 'explicit',
            methods: 'explicit',
            parameterProperties: 'off',
            properties: 'explicit',
          },
        },
      ],
      '@typescript-eslint/explicit-module-boundary-types': 'error',
      '@typescript-eslint/member-ordering': [
        'error',
        {
          default: [
            'public-static-field',
            'protected-static-field',
            'private-static-field',
            'public-instance-field',
            'protected-instance-field',
            'private-instance-field',
            'public-abstract-field',
            'protected-abstract-field',
            'public-constructor',
            'protected-constructor',
            'private-constructor',
            'public-static-method',
            'protected-static-method',
            'private-static-method',
            'public-instance-method',
            'protected-instance-method',
            'private-instance-method',
            'public-abstract-method',
            'protected-abstract-method',
          ],
        },
      ],
      '@typescript-eslint/method-signature-style': ['error', 'method'],
      '@typescript-eslint/naming-convention': [
        'error',
        // 1. Constants (UPPER_CASE), standard variables (camelCase), or "Enum-as-Const" (PascalCase)
        {
          selector: 'variable',
          modifiers: ['const'],
          format: ['UPPER_CASE', 'camelCase', 'PascalCase'],
          filter: {
            match: true,
            regex: '^[A-Z][A-Z0-9_]*(_TOKEN|_KEY|_CONFIG)?$|^[a-z][a-zA-Z0-9]*$|^[A-Z][a-zA-Z0-9]*$',
          },
        },
        { selector: 'variable', format: ['camelCase'] },
        { selector: 'function', format: ['camelCase'] },
        { selector: 'parameter', format: ['camelCase'], leadingUnderscore: 'allow' },
        { selector: 'typeLike', format: ['PascalCase'] },
        { selector: 'enumMember', format: ['PascalCase', 'UPPER_CASE'] },
        { selector: 'interface', format: ['PascalCase'], prefix: ['I'] },
        { selector: 'objectLiteralProperty', format: null },
        {
          selector: 'memberLike',
          modifiers: ['private'],
          format: ['camelCase'],
          leadingUnderscore: 'allow',
        },
        { selector: 'memberLike', format: ['camelCase'] },
      ],
      'max-params': 'off',

      '@typescript-eslint/no-confusing-void-expression': 'error',
      '@typescript-eslint/no-deprecated': 'warn',
      '@typescript-eslint/no-explicit-any': 'error',
      '@typescript-eslint/no-floating-promises': ['error', { ignoreVoid: true, ignoreIIFE: true }],
      '@typescript-eslint/no-misused-promises': 'error',
      '@typescript-eslint/no-namespace': 'off',
      '@typescript-eslint/no-non-null-assertion': 'error',
      '@typescript-eslint/no-redundant-type-constituents': 'error',
      '@typescript-eslint/no-unnecessary-type-assertion': 'error',
      '@typescript-eslint/no-unnecessary-condition': 'error',
      '@typescript-eslint/no-unused-vars': 'off',
      '@typescript-eslint/no-useless-constructor': 'error',
      '@typescript-eslint/no-useless-empty-export': 'error',
      '@typescript-eslint/prefer-nullish-coalescing': 'error',
      '@typescript-eslint/prefer-optional-chain': 'error',
      '@typescript-eslint/prefer-readonly': 'error',
      '@typescript-eslint/prefer-string-starts-ends-with': 'error',
      '@typescript-eslint/return-await': ['error', 'in-try-catch'],
      '@typescript-eslint/require-await': 'error',
      '@typescript-eslint/switch-exhaustiveness-check': 'error',
      '@typescript-eslint/no-empty-function': 'off',
      '@typescript-eslint/no-invalid-void-type': 'off',
      '@typescript-eslint/no-extraneous-class': [
        'error',
        { allowEmpty: true, allowWithDecorator: true, allowStaticOnly: true },
      ],
      '@typescript-eslint/use-unknown-in-catch-callback-variable': 'error',

      // Disable base rules replaced by TS-aware equivalents
      camelcase: 'off',
      'no-duplicate-imports': 'off',
      'no-shadow': 'off',
      'no-unused-vars': 'off',
      'no-useless-constructor': 'off',

      '@typescript-eslint/no-shadow': 'error',
      '@typescript-eslint/prefer-promise-reject-errors': 'error',

      'unused-imports/no-unused-imports': 'error',
      'unused-imports/no-unused-vars': [
        'error',
        {
          args: 'after-used',
          argsIgnorePattern: '^_',
          ignoreRestSiblings: true,
          vars: 'all',
          varsIgnorePattern: '^_',
        },
      ],

      // RxJS best practices
      'rxjs-x/no-async-subscribe': 'error',
      'rxjs-x/no-nested-subscribe': 'error',
      'rxjs-x/no-unsafe-takeuntil': 'error',
      'rxjs-x/no-internal': 'error',
      'rxjs-x/no-ignored-error': 'warn',
      'rxjs-x/no-floating-observables': 'warn',
    },
  },

  // JSDoc validation for TypeScript files (formatting only — JSDoc not required everywhere)
  {
    ...jsdoc.configs['flat/recommended-typescript'],
    files: ['**/*.ts', '**/*.tsx'],
    rules: {
      ...jsdoc.configs['flat/recommended-typescript'].rules,

      // Formatting & correctness
      'jsdoc/check-access': 'warn',
      'jsdoc/check-alignment': 'warn',
      'jsdoc/check-param-names': 'warn',
      'jsdoc/check-tag-names': ['warn', { definedTags: ['final'] }],
      'jsdoc/empty-tags': 'warn',
      'jsdoc/multiline-blocks': 'warn',
      'jsdoc/no-multi-asterisks': 'warn',
      'jsdoc/tag-lines': 'warn',

      // TypeScript handles types — never duplicate them in JSDoc
      'jsdoc/no-types': 'error',
      'jsdoc/no-defaults': 'warn',

      // JSDoc encouraged, not mandatory
      'jsdoc/require-jsdoc': 'off',
      'jsdoc/require-param': 'off',
      'jsdoc/require-param-description': 'off',
      'jsdoc/require-returns': 'off',
      'jsdoc/require-returns-description': 'off',
      'jsdoc/require-property': 'off',
      'jsdoc/require-property-description': 'off',
      'jsdoc/require-yields': 'off',
      'jsdoc/require-yields-check': 'off',
      'jsdoc/require-yields-type': 'off',
      'jsdoc/require-throws-type': 'off',
      'jsdoc/require-next-type': 'off',
      'jsdoc/check-types': 'off',
      'jsdoc/check-values': 'off',
      'jsdoc/valid-types': 'off',

      // TypeScript type-system rules handled by @typescript-eslint
      'jsdoc/ts-no-empty-object-type': 'off',
      'jsdoc/reject-any-type': 'off',
      'jsdoc/reject-function-type': 'off',
    },
  },

  // Jest config files: relax naming
  {
    files: ['jest.config.*'],
    rules: {
      '@typescript-eslint/naming-convention': 'off',
      camelcase: 'off',
    },
  },

  // JSON files — Nx dependency-checks
  {
    files: ['**/*.json'],
    languageOptions: {
      parser: jsoncParser,
    },
    plugins: {
      '@nx': nx,
    },
    rules: {
      // typescript-eslint/recommended enables this globally; jsonc-eslint-parser
      // wraps JSON root in an ExpressionStatement, so suppress for JSON.
      '@typescript-eslint/no-unused-expressions': 'off',
      '@nx/dependency-checks': [
        'error',
        {
          ignoredDependencies: [
            'typia',
            '@horizon-republic/*',
            '@jest/globals',
            '@types/jest',
            '@types/node',
            'jest',
            'ts-jest',
          ],
          buildTargets: ['build'],
          checkMissingDependencies: true,
          checkObsoleteDependencies: true,
          checkVersionMismatches: true,
          ignoredFiles: ['{projectRoot}/eslint.config.{js,cjs,mjs,ts,cts,mts}'],
          useLocalPathsForWorkspaceDependencies: true,
          peerDepsVersionStrategy: 'workspace',
        },
      ],
    },
  },

  // Module boundaries
  {
    files: ['**/*.ts', '**/*.tsx', '**/*.js', '**/*.jsx'],
    plugins: {
      '@nx': nx,
    },
    rules: {
      '@nx/enforce-module-boundaries': [
        'error',
        {
          allow: ['^.*/eslint(\\.base)?\\.config\\.[cm]?[jt]s$'],
          depConstraints: [
            {
              onlyDependOnLibsWithTags: ['*'],
              sourceTag: '*',
            },
          ],
          enforceBuildableLibDependency: true,
        },
      ],
    },
  },

  // Must be last — disables ESLint rules that conflict with Prettier formatting
  eslintConfigPrettier,
];
