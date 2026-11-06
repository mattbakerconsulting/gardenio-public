// Shared helpers for the Keycloak theme preview specs.
const fs = require('fs');
const path = require('path');

const REALM = 'gardenio';
const BASE = process.env.KC_BASE_URL || 'http://localhost:8080';

// TEST-ONLY throwaway credentials. They live in realm/gardenio-test-realm.json
// and protect nothing. The master-realm bootstrap pair is exported by run.sh.
const CREDS = {
  admin: { username: 'test-admin', password: 'test-only-fake-admin-pw' },
  user: { username: 'test-user', password: 'test-only-fake-user-pw' },
  // Has the UPDATE_PASSWORD required action, to reach the update-password page.
  pwchange: { username: 'test-pwchange', password: 'test-only-fake-pwchange-pw' },
  // Required action webauthn-register-passwordless: lands on the "Add a passkey" page.
  passkey: { username: 'test-passkey', password: 'test-only-fake-passkey-pw' },
  // Required action CONFIGURE_TOTP: lands on the authenticator-app page.
  totp: { username: 'test-totp', password: 'test-only-fake-totp-pw' },
  // Required action VERIFY_EMAIL, one user per project because following the
  // emailed link verifies the address for good.
  verify: (form, scheme) => ({
    username: `test-verify-${form}-${scheme}`,
    password: 'test-only-fake-verify-pw',
  }),
};

const SHOT_DIR = process.env.SCREENSHOT_DIR || path.join(__dirname, '..', '..', 'screenshots');
const AXE_DIR = process.env.AXE_DIR || path.join(__dirname, '..', '..', 'axe-report');

const realmUrl = (suffix = '') => `${BASE}/auth/realms/${REALM}${suffix}`;

// <screen>-<form>-<scheme>.png, form/scheme taken from the project metadata.
function shotName(screen, testInfo) {
  const { form, scheme } = testInfo.project.metadata;
  return `${screen}-${form}-${scheme}`;
}

async function screenshot(page, screen, testInfo) {
  fs.mkdirSync(SHOT_DIR, { recursive: true });
  const file = path.join(SHOT_DIR, `${shotName(screen, testInfo)}.png`);
  await page.screenshot({ path: file, fullPage: true });
  return file;
}

async function signIn(page, who) {
  const cred = typeof who === 'string' ? CREDS[who] : who;
  await page.locator('#username').fill(cred.username);
  await page.locator('#password').fill(cred.password);
  await page.locator('#kc-login').click();
}

// Account console, signed in: go to Account security > Signing in by clicking
// through the navigation. Its URL scheme differs between 26.x lines (hash
// routes before 26.5, path routes after), so the click path is the stable one.
async function openSigningIn(page) {
  const { expect } = require('@playwright/test');
  await expect(page.getByRole('heading', { name: /personal info/i }).first()).toBeVisible();
  const security = page.getByRole('button', { name: /account security/i });
  // Narrow viewports collapse the sidebar behind a hamburger.
  if (!(await security.isVisible())) await page.getByRole('button', { name: /^navigation$/i }).click();
  await security.click();
  await page.getByRole('link', { name: /signing in/i }).click();
  await expect(page.getByRole('heading', { name: /signing in/i }).first()).toBeVisible();
}

// Server version from the admin serverinfo endpoint (master-realm bootstrap
// admin from run.sh), falling back to KC_TAG. Returns {major, minor} or null.
async function serverVersion(request) {
  const user = process.env.KC_ADMIN_USER;
  const pass = process.env.KC_ADMIN_PASSWORD;
  if (user && pass) {
    try {
      const tok = await request.post(`${BASE}/auth/realms/master/protocol/openid-connect/token`, {
        form: { grant_type: 'password', client_id: 'admin-cli', username: user, password: pass },
      });
      if (tok.ok()) {
        const { access_token } = await tok.json();
        const info = await request.get(`${BASE}/auth/admin/serverinfo`, {
          headers: { Authorization: `Bearer ${access_token}` },
        });
        if (info.ok()) {
          const v = (await info.json()).systemInfo?.version;
          const m = /^(\d+)\.(\d+)/.exec(v || '');
          if (m) return { major: +m[1], minor: +m[2], raw: v };
        }
      }
    } catch (_) {
      /* fall through to the tag */
    }
  }
  const m = /^(\d+)\.(\d+)/.exec(process.env.KC_TAG || '');
  return m ? { major: +m[1], minor: +m[2], raw: process.env.KC_TAG } : null;
}

// The login page starts a conditional-mediation (passkey autofill) request on
// load. Chromium reads the virtual authenticator's automatic presence only when
// it gets to that request, which can be after a test has turned presence back
// on, so the request signs in by itself and the "Use a passkey" button is gone
// before it is clicked (gd-PrwqK1y4zd). Call before page.goto: every conditional
// request then stays in the page, never reaches the authenticator, and only
// rejects when aborted, as the button's handler does (Keycloak's
// webauthnAuthenticate.js signal()). Presence can stay on throughout.
async function holdConditionalUI(page) {
  await page.addInitScript(() => {
    if (!navigator.credentials) return; // insecure context: no WebAuthn at all
    const get = navigator.credentials.get.bind(navigator.credentials);
    navigator.credentials.get = (options = {}) => {
      if (options.mediation !== 'conditional') return get(options);
      window.__conditionalUI = 'held';
      return new Promise((_, reject) => {
        const { signal } = options;
        if (!signal) return;
        const abort = () => {
          window.__conditionalUI = 'aborted';
          reject(signal.reason);
        };
        if (signal.aborted) abort();
        else signal.addEventListener('abort', abort, { once: true });
      });
    };
  });
}

// Resolves once the page has made its (held) conditional request. Keycloak's
// passkeys.ftl makes it after attaching the button's click listener, so a click
// after this cannot land on a button that does nothing yet.
const conditionalUIHeld = (page) => page.waitForFunction(() => window.__conditionalUI === 'held');

const atLeast = (v, major, minor) =>
  v && (v.major > major || (v.major === major && v.minor >= minor));

module.exports = {
  REALM, BASE, CREDS, SHOT_DIR, AXE_DIR,
  realmUrl, screenshot, signIn, openSigningIn, serverVersion, atLeast, shotName,
  holdConditionalUI, conditionalUIHeld,
};
