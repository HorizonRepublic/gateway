const nxPreset = require('@nx/jest/preset').default;

module.exports = {
  ...nxPreset,
  workerIdleMemoryLimit: '512MB',
  testEnvironmentOptions: {
    customExportConditions: ['node', 'node-addons'],
  },
};

// Suppress "ExperimentalWarning: VM Modules" noise from Jest + Node 22+.
// Applied once at preset level so every lib inherits it.
process.env.NODE_NO_WARNINGS = '1';
