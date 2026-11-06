// Playwright config for the Keycloak login-theme preview harness (plan
// gd-QXq1fjYuRD, step S1b). It does NOT boot anything: run.sh starts the
// Keycloak container and exports KC_BASE_URL; this file only drives it.
//
// Four projects, one per (form factor x colour scheme). `metadata` carries the
// two halves so specs can name screenshots <screen>-<form>-<scheme>.png. Six
// more run only screens.spec.js for the phone sweep (Lands mobile M0.1,
// gd-0B2drDTgFw): an iPhone 12 (Safari's user agent, 390 px) upright and on its
// side, and the 360 px phone on its side, light and dark.
const { defineConfig, devices } = require('@playwright/test');

const KC_BASE_URL = process.env.KC_BASE_URL || 'http://localhost:8080';

const desktop = { viewport: { width: 1280, height: 800 } };
const mobile = {
  viewport: { width: 360, height: 740 },
  isMobile: true,
  hasTouch: true,
  deviceScaleFactor: 2,
};

function project(form, scheme, size) {
  return {
    name: `${form}-${scheme}`,
    metadata: { form, scheme },
    use: { ...devices['Desktop Chrome'], ...size, colorScheme: scheme },
    // Passkeys do not depend on form factor or scheme: run them once. The
    // passkey browser-flow spec needs the chart's flow bound to the realm, which
    // run.sh does for its own second pass (KC_FLOW=passkey), after this one.
    testIgnore:
      form === 'desktop' && scheme === 'light'
        ? process.env.KC_FLOW === 'passkey' ? [] : /passkey-flow\.spec\.js/
        : /passkey(-flow)?\.spec\.js/,
  };
}

// The descriptors' own viewports (on the iPhone, what Safari leaves once its bars
// are drawn); Chromium either way, so a user agent and size, not WebKit.
function phone(name) {
  const { defaultBrowserType, ...d } = devices[name];
  return { ...d, deviceScaleFactor: 2 };
}

function sweep(form, scheme, size) {
  return {
    name: `${form}-${scheme}`,
    metadata: { form, scheme },
    use: { ...size, colorScheme: scheme },
    testMatch: /(^|\/)screens\.spec\.js$/,
  };
}

module.exports = defineConfig({
  testDir: './tests',
  // Starts the mail sink Keycloak sends to and prepares the passkey-only test client.
  globalSetup: require.resolve('./tests/lib/global-setup.js'),
  // The flow pass (KC_FLOW=passkey) runs after the first one and must not wipe its results.
  outputDir: process.env.KC_FLOW ? './test-results-flow' : './test-results',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 2,
  retries: process.env.CI ? 1 : 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: process.env.KC_FLOW ? 'playwright-report-flow' : 'playwright-report' }]],
  use: {
    baseURL: KC_BASE_URL,
    trace: 'retain-on-failure',
  },
  projects: [
    project('desktop', 'light', desktop),
    project('desktop', 'dark', desktop),
    project('mobile', 'light', mobile),
    project('mobile', 'dark', mobile),
    ...['light', 'dark'].flatMap((scheme) => [
      sweep('ios390', scheme, phone('iPhone 12')),
      sweep('ios390land', scheme, phone('iPhone 12 landscape')),
      sweep('and360land', scheme, phone('Galaxy S8 landscape')),
    ]),
  ],
});
