// force timezone to UTC to allow tests to work regardless of local timezone
// generally used by snapshots, but can affect specific tests
process.env.TZ = 'UTC';

const { grafanaESModules, nodeModulesToTransform } = require('./.config/jest/utils');
// @grafana/llm ships ESM dependencies (e.g. the MCP SDK) that Jest must
// transform; the package exports the required module list for convenience.
const { grafanaLLMESModules } = require('@grafana/llm/jest');

module.exports = {
  // Jest configuration provided by Grafana scaffolding
  ...require('./.config/jest.config'),
  transformIgnorePatterns: [nodeModulesToTransform([...grafanaESModules, ...grafanaLLMESModules])],
};
