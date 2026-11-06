// Playwright config for the beads-viewer screenshot harness.
// Modeled on beads3d/playwright.config.js; captures canonical pages at two
// viewports so MRs can post before/after visuals for eyeball review.
//
// The harness assumes a beads-viewer is already reachable at BEADS_VIEWER_URL
// (default http://localhost:8095). The local dev boot path is owned by
// ka-0000007138 and documented in gardenio/docs/beads-viewer-dev.md. Once that
// lands, this config can grow a `webServer:` block that boots the viewer
// automatically; for now we rely on an externally-started process so the
// harness can iterate in parallel.

import { defineConfig, devices } from '@playwright/test';

const baseURL = process.env.BEADS_VIEWER_URL || 'http://localhost:8095';

// When set, every navigation carries `Authorization: Bearer <token>`. Set it
// (e.g. to a bearer minted via lib-keycloak-client-creds.sh) to drive the
// capture against a real OIDC-protected beads-viewer; the RWX info-density
// audit workflow did this. Local dev runs
// against BEADS_VIEWER_DEV_AUTH_BYPASS=1 leave it unset.
const bearer = process.env.BEADS_VIEWER_BEARER || '';
const extraHTTPHeaders = bearer ? { Authorization: `Bearer ${bearer}` } : undefined;

// When set, launch Chromium from an explicit executable path instead of the
// version-pinned bundled browser. Agent pods run against a pre-cached
// /ms-playwright browser set whose revision may not match the harness's
// @playwright/test pin; pointing at the cached chrome binary lets the capture
// run without a network browser download. Unset in local/CI runs so the
// bundled browser is used (default behavior unchanged).
const chromeExecutablePath = process.env.BEADS_VIEWER_CHROME_PATH || undefined;
const launchOptions = chromeExecutablePath
  ? { executablePath: chromeExecutablePath }
  : undefined;

export default defineConfig({
  testDir: './tests',
  timeout: 60000,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,

  reporter: [
    ['html', { open: 'never', outputFolder: 'test-results/beads-viewer-screenshots-report' }],
    ['list'],
  ],

  outputDir: 'test-results/beads-viewer-screenshots-artifacts',

  use: {
    baseURL,
    ignoreHTTPSErrors: true,
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
    video: 'off',
    extraHTTPHeaders,
    // Deterministic rendering: freeze time, disable animations.
    reducedMotion: 'reduce',
    colorScheme: 'light',
    locale: 'en-US',
    timezoneId: 'UTC',
  },

  projects: [
    {
      name: 'desktop',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1280, height: 800 },
        deviceScaleFactor: 1,
        launchOptions,
      },
    },
    {
      name: 'narrow',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 480, height: 800 },
        deviceScaleFactor: 1,
        launchOptions,
      },
    },
  ],
});
