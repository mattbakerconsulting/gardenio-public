// Playwright config for the beads-viewer behavioral E2E suite.
//
// Sibling to gardenio/tests/beads-viewer-screenshots/ (visual/advisory). This
// suite is behavioral and intended to gate CI: parallelizable, fail-loud,
// chromium-primary to keep wall time tight. A scoped `firefox` project
// runs only `popover_constellation.spec.js` — the one suite where native
// SVG `<title>` tooltip behavior is browser-dependent (per
// docs/design/beads-viewer/constellation-popover.md §Risks). Polish task
// ka-0000006763.
//
// Phase T1 (ka-0000005681) wires up `webServer:` to auto-boot beads-viewer.
// Phase T2 (ka-0000006317) adds `globalSetup` to seed deterministic fixtures
// against that daemon — see ./globalSetup.mjs and ./seed/. The viewer
// needs a beads daemon reachable at BEADS_HTTP_ADDR (default
// http://localhost:8080 — typically a `kubectl port-forward svc/gardenio-beads
// 8080:8080`). See gardenio/plans/beads-viewer-playwright-tests.md.
//
// Auth is forced into dev-bypass mode (BEADS_VIEWER_DEV_AUTH_BYPASS=1) so
// tests do not need an OIDC issuer. The viewer refuses to start with bypass
// active inside a real cluster (presence of K8s SA token mount) — see
// gardenio/controller/cmd/beads-viewer/dev_auth.go.
//
// The exception (gd-ZTu68qjfEC): when the harness OIDC issuer is running
// (oidc-stub/server.mjs, started before the daemon so the daemon trusts it),
// a SECOND viewer boots in OIDC mode with bypass off. Specs that need a human
// principal (answering a decision, gardener admin writes) opt into it per
// describe block via tests/_oidc.js. Without the stub only the bypass viewer
// runs and those specs skip.

import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig, devices } from '@playwright/test';

import { OIDC_VIEWER_PORT, OIDC_VIEWER_URL, oidcStub } from './tests/_oidc.js';

const PORT = process.env.BEADS_VIEWER_PORT || '8095';
const baseURL = process.env.BEADS_VIEWER_URL || `http://localhost:${PORT}`;
const beadsHTTPAddr = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';

// Absolute path from this config file (gardenio/tests/beads-viewer-e2e/) up
// to the monorepo root. Used as cwd for the auto-boot command. Resolved
// from import.meta.url so the value is correct regardless of where
// `playwright test` was invoked from.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const monorepoRoot = path.resolve(__dirname, '../../..');

const viewerCommand = process.env.BEADS_VIEWER_BIN || `go run ./gardenio/controller/cmd/beads-viewer`;

export default defineConfig({
  testDir: './tests',
  timeout: 30 * 1000,
  expect: { timeout: 5 * 1000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,

  // Phase T2 (ka-0000006317) — seed canonical fixtures into the daemon
  // and emit `fixtures/ids.json` before any test runs. Idempotent;
  // skip via BEADS_VIEWER_E2E_SKIP_SEED=1 for tight inner-loop dev.
  globalSetup: './globalSetup.mjs',

  reporter: [
    ['html', { open: 'never', outputFolder: 'test-results/beads-viewer-e2e-report' }],
    ['list'],
  ],

  outputDir: 'test-results/beads-viewer-e2e-artifacts',

  use: {
    baseURL,
    ignoreHTTPSErrors: true,
    screenshot: 'only-on-failure',
    trace: 'on-first-retry',
    video: 'off',
    locale: 'en-US',
    timezoneId: 'UTC',
  },

  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
    // Firefox is scoped to the constellation popover spec only. The
    // native SVG <title> tooltip race is the only behavior we know to
    // be browser-dependent (design doc §Risks); running the full suite
    // on a second engine doubles wall time without proportionate value.
    // Add more spec patterns here if a future risk warrants it.
    {
      name: 'firefox',
      testMatch: /popover_constellation\.spec\.js$/,
      use: { ...devices['Desktop Firefox'] },
    },
  ],

  // Auto-boot the viewer so `npm test` works end-to-end. Skipped if a viewer
  // is already listening (handy for `make run-beads-viewer` + iterating on
  // tests in another shell). When BEADS_VIEWER_BIN is set (CI pre-builds
  // the binary up-front), exec it directly — this dodges go.work / GOWORK
  // resolution inside the playwright spawn, which was unreliable in CI.
  webServer: [{
    command: viewerCommand,
    cwd: monorepoRoot,
    url: `${baseURL}/healthz`,
    timeout: 120 * 1000,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      BEADS_VIEWER_DEV_AUTH_BYPASS: '1',
      // Render the deterministic fixture gardener this suite seeds (label
      // fixture:e2e:gardener, title "[E2E fixture] gardener bead"). Production
      // EXCLUDES fixture gardeners from the live gardeners surfaces (bug
      // ka-0000008054) — but the behavioral suite navigates to /gardener/{id} +
      // /gardeners expecting its own fixture to render. This harness-only opt-in
      // disables that exclusion so gardener_detail.spec.js + gardeners_index.spec.js
      // see a 200 instead of a 404 (bug gd-R8wc4mOHL3). Pairs with the dev-auth
      // bypass above; both are harness-only.
      BEADS_VIEWER_RENDER_FIXTURE_GARDENERS: '1',
      // The deployment's project (production sources this from BOAT_PROJECT).
      // The /claude-config matrix (claude_config.spec.js, ka-0000000682) resolves
      // each role's effective config against `global → project:<this> → role:X`,
      // and the seeded `claude-settings` fixtures carry `project:gardenio` labels
      // to mirror the real fleet shape — so they resolve ONLY when viewerProject
      // is `gardenio` (a project: label is REQUIRED for MatchesSubscriptions).
      // Unset, viewerProject is "" and those project-tagged rows never appear,
      // emptying the matrix. Consumed functionally only by claude_config.go
      // (SetViewerProject) plus a Prometheus label; harmless to every other spec.
      BOAT_PROJECT: 'gardenio',
      BEADS_HTTP_ADDR: beadsHTTPAddr,
      // Bearer the viewer presents to the daemon. In CI (ci.yml step 5) this is
      // the primary static token the daemon also loads as BEADS_AUTH_TOKEN, so
      // viewer→daemon writes authenticate as the primary-static identity — the
      // only auth branch on which the daemon honors the X-Actor override the
      // viewer stamps for comment authorship (ka-0000006262). Passed through
      // explicitly (Playwright merges process.env, but naming it here keeps the
      // token's route to the viewer visible). Empty in local dev → the viewer
      // talks to a bypass/auth-disabled daemon exactly as before.
      BEADS_DAEMON_TOKEN: process.env.BEADS_DAEMON_TOKEN || '',
      LISTEN_ADDR: `:${PORT}`,
      LOG_LEVEL: process.env.LOG_LEVEL || 'info',
      // Pin GOWORK to the monorepo's go.work so the `go run` fallback
      // uses workspace mode for local dev. The CI path uses
      // BEADS_VIEWER_BIN (pre-built) so this env is unused there.
      GOWORK: path.join(monorepoRoot, 'go.work'),
    },
  }, ...(oidcStub ? [oidcViewer(oidcStub)] : [])],
});

// oidcViewer is the second viewer, in OIDC mode against the harness issuer
// (gd-ZTu68qjfEC). Same binary and daemon as the bypass viewer; only auth
// differs, and only through env the shipping binary already reads:
//   - OIDC_ISSUER_URL / OIDC_CLIENT_ID: verify each request's bearer against
//     the stub's JWKS (roles from realm_access.roles, like Keycloak);
//   - SSL_CERT_FILE: the stub's CA bundle, since the viewer requires an https
//     issuer and the stub's cert is self-signed;
//   - BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1: daemon writes go out on the
//     user's bearer, as in prod. Reads stay on the static token.
// Waits on /readyz, not /healthz: /readyz is 503 until OIDC discovery succeeds.
function oidcViewer(stub) {
  return {
    command: viewerCommand,
    cwd: monorepoRoot,
    url: `${OIDC_VIEWER_URL}/readyz`,
    timeout: 120 * 1000,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      BEADS_VIEWER_DEV_AUTH_BYPASS: '0',
      OIDC_ISSUER_URL: stub.issuer,
      OIDC_CLIENT_ID: stub.audience,
      SSL_CERT_FILE: stub.caBundleFile,
      BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES: '1',
      // As for the bypass viewer above: ephemeral gardener fixtures must render.
      BEADS_VIEWER_RENDER_FIXTURE_GARDENERS: '1',
      BOAT_PROJECT: 'gardenio',
      BEADS_HTTP_ADDR: beadsHTTPAddr,
      BEADS_DAEMON_TOKEN: process.env.BEADS_DAEMON_TOKEN || '',
      LISTEN_ADDR: `127.0.0.1:${OIDC_VIEWER_PORT}`,
      LOG_LEVEL: process.env.LOG_LEVEL || 'info',
      GOWORK: path.join(monorepoRoot, 'go.work'),
    },
  };
}
