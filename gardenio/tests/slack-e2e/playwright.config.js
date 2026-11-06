// Playwright config for the real-user Slack E2E suite (task ka-0000004549,
// epic ka-0000006991). Sibling to gardenio/tests/beads-viewer-e2e/, but with a
// different execution model:
//
//   - beads-viewer-e2e auto-boots the app it tests (webServer:) and seeds a
//     local daemon. This suite does NOT: it drives the PUBLIC app.slack.com web
//     client authenticated as a dedicated test human user, against a dedicated
//     test workspace/app wired to an already-running ephemeral slack-bridge.
//     There is nothing for Playwright to boot — the bridge, workspace, app, and
//     captured session are all provisioned out of band (operator runbook
//     gardenio/docs/slack-e2e-provisioning.md) and handed in via env by the
//     module runner gardenio/tests/e2e/scripts/test-slack-ui.sh.
//
//   - Auth is a pre-captured browser storageState (Slack `d`/`d-s` session
//     cookies), injected via `use.storageState`, NOT a scripted login (Slack
//     login is MFA/email-code gated and bot-hostile). The runner extracts it
//     from the ESO-synced K8s Secret into a private temp file and exports the
//     path as SLACK_E2E_STORAGE_STATE.
//
//   - chromium-only, serialized (workers: 1, fullyParallel: false). Real Slack +
//     real network is flaky and, per the plan (§Risks 4), multiple bridges
//     attached to one test workspace fan Socket Mode events out to ALL of them —
//     so the slack-ui module is pinned to a single shard and run non-parallel.
//     Reuses the beads-viewer-e2e chromium base image + e2e-system-deps lock.
//
// Env contract (exported by test-slack-ui.sh; see tests/_slack.js):
//   SLACK_E2E_STORAGE_STATE  captured test-user storageState path (auth)
//   SLACK_E2E_BRIDGE_URL     ephemeral slack-bridge base URL
//   SLACK_E2E_CHANNEL        C0… test channel id
//   BEADS_HTTP_URL           beads daemon HTTP base URL
//   BEADS_AUTH_TOKEN         daemon bearer (optional)

import { defineConfig, devices } from '@playwright/test';

// Applied via `use.storageState`. Empty (undefined) when unset so `playwright
// test --list` and lint-style dry runs work without a captured session; the
// `workspace` fixture / globalSetup still gate real runs on its presence.
const storageState = process.env.SLACK_E2E_STORAGE_STATE || undefined;

export default defineConfig({
  testDir: './tests',
  // Real Slack + real network — generous per-test budget vs the local
  // beads-viewer suite. Still bounded so a hung selector fails loudly.
  timeout: 90 * 1000,
  expect: { timeout: 15 * 1000 },

  // Serialize: one test app per shard, Socket Mode fan-out safety (plan §Risks 4).
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,

  // Pre-run gate: assert the env contract + session liveness ONCE before any
  // spec, so a mis-provisioned rig fails with a runbook pointer up front.
  globalSetup: './globalSetup.mjs',

  reporter: [
    ['html', { open: 'never', outputFolder: 'test-results/slack-e2e-report' }],
    ['list'],
  ],

  outputDir: 'test-results/slack-e2e-artifacts',

  use: {
    // The captured test-user browser session. Skips the MFA-gated login form.
    storageState,
    ignoreHTTPSErrors: true,
    screenshot: 'only-on-failure',
    // Traces/video are worth the cost here — real-Slack flakes are hard to
    // reproduce, so capture on first retry for post-mortem.
    trace: 'on-first-retry',
    video: 'retain-on-failure',
    locale: 'en-US',
    timezoneId: 'UTC',
    actionTimeout: 20 * 1000,
    navigationTimeout: 45 * 1000,
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
