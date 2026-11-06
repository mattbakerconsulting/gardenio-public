// Screenshot every reachable Keycloak login page, once per project
// (desktop/mobile x light/dark), and run axe on each one.
//
// Output:
//   screenshots/<screen>-<form>-<scheme>.png   one per screen per project
//   axe-report/<screen>-<form>-<scheme>.json   axe violations for that page
//
// Axe is a HARD GATE (plan step S1c): any serious or critical violation on any
// screen, in any of the four projects (desktop/mobile x light/dark), fails the
// run. Contrast (AA) is part of that: axe's color-contrast rule is serious.
// AXE_ENFORCE=0 turns it back into a report, for diagnosing a stock-theme page.
const { test, expect } = require('@playwright/test');
const { realmUrl, signIn, openSigningIn, CREDS } = require('./lib/kc');

const { capture, SETTLE_MS } = require('./lib/capture');
const { expectAccountThemed } = require('./lib/account');

// Open the login page by asking for the account console while signed out.
async function openLogin(page) {
  await page.goto(realmUrl('/account'));
  await expect(page.locator('#username')).toBeVisible();
}

async function startDeviceFlow(request) {
  const res = await request.post(realmUrl('/protocol/openid-connect/auth/device'), {
    form: { client_id: 'gardenio-cli' },
  });
  expect(res.ok(), 'device authorization request').toBeTruthy();
  return res.json();
}

test.describe('login screens', () => {
  test('login', async ({ page }, testInfo) => {
    await openLogin(page);
    await page.waitForLoadState('networkidle').catch(() => {});
    await capture(page, 'login', testInfo);
    // 44px minimum touch targets on the controls a thumb has to hit. The text
    // inputs are measured on their bordered wrapper span.
    const targets = {
      '#kc-login': page.locator('#kc-login'),
      '#username': page.locator('#username').locator('..'),
      '#password': page.locator('#password').locator('xpath=ancestor::span[contains(@class,"pf-v5-c-form-control")][1]'),
    };
    for (const [name, loc] of Object.entries(targets)) {
      const box = await loc.boundingBox();
      expect(box.height, `${name} height`).toBeGreaterThanOrEqual(44);
    }
    // Keyboard: Tab from the password field reaches the show-password button,
    // which draws a visible focus ring.
    await page.locator('#password').focus();
    await page.keyboard.press('Tab');
    const focused = page.locator(':focus-visible');
    await expect(focused).toHaveCount(1);
    expect(await focused.evaluate((el) => getComputedStyle(el).outlineStyle), 'focus ring').not.toBe('none');
  });

  test('login-error (bad password)', async ({ page }, testInfo) => {
    await openLogin(page);
    await page.locator('#username').fill(CREDS.user.username);
    await page.locator('#password').fill('not-the-password');
    await page.locator('#kc-login').click();
    await expect(page.locator('body')).toContainText(/not right|invalid/i);
    await capture(page, 'login-error', testInfo);
  });

  test('forgot-password', async ({ page }, testInfo) => {
    await openLogin(page);
    await page.getByRole('link', { name: /forgot/i }).click();
    await expect(page.locator('#username')).toBeVisible();
    await capture(page, 'forgot-password', testInfo);
  });

  test('login-page-expired', async ({ page }, testInfo) => {
    await openLogin(page);
    // Remember this form's step, move the flow on (forgot password), then
    // come back to the old step: Keycloak answers with the "page expired" page.
    const staleStep = await page.locator('#kc-form-login').getAttribute('action');
    await page.getByRole('link', { name: /forgot/i }).click();
    await expect(page.locator('#username')).toBeVisible();
    await page.goto(new URL(staleStep, page.url()).toString());
    await page.waitForLoadState('domcontentloaded');
    const heading = await page.locator('h1, #kc-page-title').first().innerText().catch(() => '');
    testInfo.annotations.push({
      type: 'page:login-page-expired',
      description: `title="${await page.title()}" heading="${heading}"`,
    });
    await expect(page.locator('body')).toContainText(/expired|restart/i);
    await capture(page, 'login-page-expired', testInfo);
  });

  test('account console: signing in page', async ({ page }, testInfo) => {
    await openLogin(page);
    await signIn(page, 'user');
    await openSigningIn(page);
    // The account console is its own React app with its own theme (S1e,
    // account/ in the theme tree): held to its own checks (lib/account.js), not
    // the login gate. Its full axe report is recorded but not enforced; the
    // rules it is held to are in lib/account.js.
    await page.waitForLoadState('networkidle', { timeout: SETTLE_MS }).catch(() => {});
    await expectAccountThemed(page, testInfo);
    await capture(page, 'account-signingin', testInfo, { themed: false, enforce: false });
  });

  test('logout confirmation', async ({ page }, testInfo) => {
    await openLogin(page);
    await signIn(page, 'user');
    await expect(page.getByRole('heading', { name: /personal info/i }).first()).toBeVisible();
    // No id_token_hint: Keycloak asks the signed-in user to confirm.
    await page.goto(realmUrl('/protocol/openid-connect/logout'));
    await expect(page.locator('#kc-logout, input[name="confirmLogout"]').first()).toBeVisible();
    await capture(page, 'logout-confirm', testInfo);
  });

  test('error page (bad redirect_uri)', async ({ page }, testInfo) => {
    const url = new URL(realmUrl('/protocol/openid-connect/auth'));
    url.search = new URLSearchParams({
      client_id: 'gardenio-consent-test',
      redirect_uri: 'http://not-registered.invalid/cb',
      response_type: 'code',
      scope: 'openid',
    }).toString();
    await page.goto(url.toString());
    await expect(page.locator('#kc-error-message')).toBeVisible();
    await capture(page, 'error', testInfo);
  });

  test('required action: update password', async ({ page }, testInfo) => {
    await openLogin(page);
    await page.locator('#username').fill(CREDS.pwchange.username);
    await page.locator('#password').fill(CREDS.pwchange.password);
    await page.locator('#kc-login').click();
    await expect(page.locator('#password-new')).toBeVisible();
    await capture(page, 'login-update-password', testInfo);
  });

  test('after sign-out', async ({ page }, testInfo) => {
    await openLogin(page);
    await signIn(page, 'user');
    await expect(page.getByRole('heading', { name: /personal info/i }).first()).toBeVisible();
    await page.goto(realmUrl('/protocol/openid-connect/logout'));
    await page.locator('#kc-logout, input[name="confirmLogout"]').first().click();
    await page.waitForLoadState('domcontentloaded');
    // Keycloak shows an info page or the login page, depending on version.
    const heading = await page.locator('h1, #kc-page-title').first().innerText().catch(() => '');
    testInfo.annotations.push({
      type: 'page:logout-done',
      description: `title="${await page.title()}" heading="${heading}"`,
    });
    await capture(page, 'logout-done', testInfo);
  });

  test('device verification: code entry, grant, done', async ({ page, request }, testInfo) => {
    const dev = await startDeviceFlow(request);
    await page.goto(realmUrl('/device'));
    await expect(page.locator('input[name="device_user_code"], #device-user-code')).toBeVisible();
    // S1d: the field says where the code comes from, and is large and monospaced.
    await expect(page.getByLabel('Enter the code shown in your terminal')).toBeVisible();
    const codeStyle = await page.locator('#device_user_code').evaluate((el) => {
      const cs = getComputedStyle(el);
      return { size: parseFloat(cs.fontSize), family: cs.fontFamily, transform: cs.textTransform };
    });
    expect(codeStyle.size, 'code field font size').toBeGreaterThanOrEqual(24);
    expect(codeStyle.family, 'code field font').toMatch(/mono|menlo|consolas|courier/i);
    expect(codeStyle.transform, 'code field capitals').toBe('uppercase');
    await capture(page, 'device-code', testInfo);

    await page.locator('input[name="device_user_code"], #device-user-code').fill(dev.user_code);
    await page.locator('input[type="submit"], button[type="submit"]').first().click();
    await expect(page.locator('#username')).toBeVisible();
    await signIn(page, 'user');

    // gardenio-cli has consentRequired, so this is the device grant page.
    await expect(page.locator('#kc-login')).toBeVisible();
    await capture(page, 'device-grant', testInfo);

    await page.locator('#kc-login').click();
    await page.waitForLoadState('domcontentloaded');
    // S1d: the page says what happened and that the tab can be closed.
    await expect(page.locator('#kc-page-title')).toHaveText(/terminal is connected/i);
    await expect(page.locator('body')).toContainText(/close this tab/i);
    await capture(page, 'device-success', testInfo);
  });

  test('oauth grant page', async ({ page }, testInfo) => {
    const url = new URL(realmUrl('/protocol/openid-connect/auth'));
    url.search = new URLSearchParams({
      client_id: 'gardenio-consent-test',
      redirect_uri: 'http://localhost:3000/cb',
      response_type: 'code',
      scope: 'openid profile email',
      code_challenge: 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM',
      code_challenge_method: 'S256',
    }).toString();
    await page.goto(url.toString());
    await signIn(page, 'user');
    await expect(page.locator('#kc-login')).toBeVisible();
    await expect(page.locator('body')).toContainText(/gardenio consent test app|grant|access/i);
    await capture(page, 'oauth-grant', testInfo);
  });
  // S1d: a wrong code gets a plain message and the field again.
  test('device verification: wrong code', async ({ page }, testInfo) => {
    await page.goto(realmUrl('/device'));
    await page.locator('#device_user_code').fill('ZZZZ-ZZZZ');
    await page.locator('input[type="submit"], button[type="submit"]').first().click();
    await expect(page.locator('body')).toContainText(/that code is not right/i);
    await expect(page.locator('#device_user_code')).toBeVisible();
    await capture(page, 'device-code-invalid', testInfo);
  });

  // The CSS shows the code in capitals; what is typed is submitted as typed.
  // This proves Keycloak takes a lower-case code, so the display never lies.
  test('device verification: a lower-case code is accepted', async ({ page, request }) => {
    const dev = await startDeviceFlow(request);
    await page.goto(realmUrl('/device'));
    await page.locator('#device_user_code').fill(dev.user_code.toLowerCase());
    await page.locator('input[type="submit"], button[type="submit"]').first().click();
    await expect(page.locator('#username')).toBeVisible();
  });
});
