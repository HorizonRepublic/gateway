// example-app's package.json carries runtime deps that the Docker
// runtime stage installs via `npm install` against this manifest.
// Several of those packages (tslib, @nestjs/microservices, fastify,
// @opentelemetry/api, rxjs) are pulled transitively at runtime by
// @horizon-republic/nestjs-jetstream and @nestjs/platform-fastify
// rather than being imported directly from src/. The
// @nx/dependency-checks rule does not see those uses; suppress it for
// this project so the runtime manifest can stay accurate.
import baseConfig from '../../eslint.config.mjs';

export default [
  ...baseConfig,
  {
    files: ['**/package.json'],
    rules: {
      '@nx/dependency-checks': 'off',
    },
  },
];
