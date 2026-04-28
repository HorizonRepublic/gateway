/* eslint-disable @typescript-eslint/no-require-imports */
const { composePlugins, withNx } = require('@nx/webpack');

module.exports = composePlugins(withNx(), (config) => config);
