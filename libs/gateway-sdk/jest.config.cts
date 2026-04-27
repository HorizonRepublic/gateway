/**
 * Jest configuration for `@horizon-republic/gateway-sdk`.
 *
 * Runs in ESM mode because specs need to import real
 * `@nestjs/common@12.0.0-alpha.x`, `@nestjs/core@12.0.0-alpha.x`, and
 * `@nestjs/microservices@12.0.0-alpha.x` — all three ship as pure ESM
 * (`"type": "module"`, ESM-only `exports` map).
 *
 * Spec files MUST `import { jest } from '@jest/globals'` because the `jest`
 * global is only injected in CommonJS mode. `@jest/globals` is a workspace-root
 * devDependency to ensure it is hoisted.
 */
module.exports = {
  displayName: 'gateway-sdk',
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
  coverageDirectory: '../../coverage/libs/gateway-sdk',
  passWithNoTests: true,
};
