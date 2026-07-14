/**
 * Jest configuration for the example-app smoke tests.
 *
 * Runs in ESM mode for the same reason as `libs/gateway-sdk/jest.config.cts`:
 * specs import real `@nestjs/*@12.0.0-alpha.x` packages, which ship as pure
 * ESM (`"type": "module"`, ESM-only `exports` map).
 *
 * Spec files MUST import from `@jest/globals` because the `jest` global is
 * only injected in CommonJS mode.
 */
module.exports = {
  displayName: 'example-app',
  preset: '../../jest.preset.js',
  testEnvironment: 'node',
  extensionsToTreatAsEsm: ['.ts'],
  transform: {
    '^.+\\.[tj]s$': [
      'ts-jest',
      {
        tsconfig: '<rootDir>/tsconfig.spec.json',
        useESM: true,
      },
    ],
  },
  moduleFileExtensions: ['ts', 'js', 'html'],
  moduleNameMapper: {
    '^(\\.{1,2}/.*)\\.js$': '$1',
  },
  coverageDirectory: '../../coverage/apps/example-app',
};
