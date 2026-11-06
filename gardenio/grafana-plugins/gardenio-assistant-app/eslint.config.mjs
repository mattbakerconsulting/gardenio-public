import { defineConfig } from 'eslint/config';
import baseConfig from './.config/eslint.config.mjs';

export default defineConfig([
  {
    ignores: [
      '**/logs',
      '**/*.log',
      '**/npm-debug.log*',
      '**/yarn-debug.log*',
      '**/yarn-error.log*',
      '**/.pnpm-debug.log*',
      '**/node_modules/',
      '.yarn/cache',
      '.yarn/unplugged',
      '.yarn/build-state.yml',
      '.yarn/install-state.gz',
      '**/.pnp.*',
      '**/pids',
      '**/*.pid',
      '**/*.seed',
      '**/*.pid.lock',
      '**/lib-cov',
      '**/coverage',
      '**/dist/',
      '**/artifacts/',
      '**/work/',
      '**/ci/',
      'test-results/',
      'playwright-report/',
      'blob-report/',
      'playwright/.cache/',
      'playwright/.auth/',
      '**/.idea',
      '**/.eslintcache',
    ],
  },
  ...baseConfig,
  // --- Egress-containment guard (ka-0000000846, item 13) ---
  // The chat engine + tool surface MUST keep all LLM/MCP traffic inside the
  // cluster: every request goes through @grafana/llm (-> grafana-llm-app ->
  // in-cluster LiteLLM/Bedrock) and the curated read-only mcp-grafana tools.
  // There is no legitimate reason for code under src/components/Chat or
  // src/tools to reach the network directly. Ban the direct-egress primitives
  // here so a future change that smuggles in fetch/axios/raw http/WebSocket
  // fails lint (this is the load-bearing security property — see readOnlyTools.ts).
  {
    files: ['src/components/Chat/**/*.{ts,tsx}', 'src/tools/**/*.{ts,tsx}'],
    rules: {
      'no-restricted-imports': [
        'error',
        {
          paths: [
            { name: 'axios', message: 'Off-cluster HTTP is forbidden in the chat/tools egress envelope — route through @grafana/llm.' },
            { name: 'http', message: 'No raw http in the chat/tools egress envelope.' },
            { name: 'https', message: 'No raw https in the chat/tools egress envelope.' },
            { name: 'node:http', message: 'No raw http in the chat/tools egress envelope.' },
            { name: 'node:https', message: 'No raw https in the chat/tools egress envelope.' },
            { name: 'got', message: 'No HTTP client in the chat/tools egress envelope.' },
            { name: 'node-fetch', message: 'No HTTP client in the chat/tools egress envelope.' },
          ],
          patterns: [
            { group: ['axios', 'axios/*'], message: 'No axios in the chat/tools egress envelope.' },
          ],
        },
      ],
      'no-restricted-globals': [
        'error',
        { name: 'fetch', message: 'No direct fetch in the chat/tools egress envelope — all traffic goes through @grafana/llm.' },
        { name: 'XMLHttpRequest', message: 'No XMLHttpRequest in the chat/tools egress envelope.' },
        { name: 'WebSocket', message: 'No WebSocket in the chat/tools egress envelope.' },
        { name: 'EventSource', message: 'No EventSource in the chat/tools egress envelope.' },
      ],
    },
  },
]);
