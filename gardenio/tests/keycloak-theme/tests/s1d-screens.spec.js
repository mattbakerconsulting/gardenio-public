// S1d screens: passkeys, set-up and account pages, and the email-driven pages.
// Same rules as screens.spec.js (four projects, themed checks, axe hard gate);
// the device pages for `kd login` are in screens.spec.js.
//
//   webauthn-authenticate, webauthn-error   a client bound to a passkey-only flow
//                                           (tests/lib/admin.js, made in global-setup)
//   webauthn-register                       user test-passkey: required action
//   login-config-totp                       user test-totp: required action
//   login-verify-email, info-confirm, info  user test-verify-<form>-<scheme>; the mail
//                                           goes to the sink in tests/lib/mail.js
//   login-reset-password's emailed follow-up is the login page with an info alert
//   (captured as forgot-password-sent).
const { test, expect } = require('@playwright/test');
const { realmUrl, signIn, CREDS } = require('./lib/kc');
const { capture } = require('./lib/capture');
const { PASSKEY_ONLY_CLIENT } = require('./lib/admin');
const { latestActionLink } = require('./lib/mail');

async function openLogin(page) {
  await page.goto(realmUrl('/account'));
  await expect(page.locator('#username')).toBeVisible();
}

function passkeyOnlyUrl() {
  const url = new URL(realmUrl('/protocol/openid-connect/auth'));
  url.search = new URLSearchParams({
    client_id: PASSKEY_ONLY_CLIENT,
    redirect_uri: 'http://localhost:3000/cb',
    response_type: 'code',
    scope: 'openid',
    code_challenge: 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM',
    code_challenge_method: 'S256',
  }).toString();
  return url.toString();
}

async function emptyVirtualAuthenticator(page) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
}

test.describe('passkey screens', () => {
  test('use your passkey, and the error when it fails', async ({ page }, testInfo) => {
    await page.goto(passkeyOnlyUrl());
    const button = page.locator('#authenticateWebAuthnButton');
    await expect(button).toBeVisible();
    await expect(page.locator('#kc-page-title')).toHaveText('Use your passkey');
    await expect(button).toHaveText(/use a passkey/i);
    await capture(page, 'webauthn-authenticate', testInfo);

    // The hint sits next to the button; it is CSS generated text.
    const hint = await page.locator('#kc-form-webauthn').evaluate((el) => getComputedStyle(el, '::before').content);
    expect(hint, 'passkey hint').toContain('fingerprint, face, PIN or security key');

    // An authenticator with no passkey for this site answers "not allowed" at
    // once; the page posts the failure and Keycloak shows its error page.
    await emptyVirtualAuthenticator(page);
    await button.click();
    await expect(page.locator('#kc-try-again')).toBeVisible({ timeout: 20_000 });
    await expect(page.locator('#kc-page-title')).toHaveText('That passkey did not work');
    await expect(page.locator('body')).toContainText(/try again/i);
    // The page offers only Try again (no password step at this point of the
    // flow), so the copy must not promise one (gd-aYmqhMKH05).
    await expect(page.locator('.kc-feedback-text')).not.toContainText(/password/i);
    await capture(page, 'webauthn-error', testInfo);
  });

  test('add a passkey', async ({ page }, testInfo) => {
    await openLogin(page);
    await signIn(page, 'passkey');
    const register = page.locator('#registerWebAuthn');
    await expect(register).toBeVisible();
    await expect(page.locator('#kc-page-title')).toHaveText('Add a passkey');
    await expect(register).toHaveText('Add a passkey');
    const hint = await page.locator('form#register').evaluate((el) => getComputedStyle(el, '::before').content);
    expect(hint, 'passkey hint').toContain('fingerprint, face, PIN or security key');
    await capture(page, 'webauthn-register', testInfo);
  });
});

test.describe('account set-up screens', () => {
  test('authenticator app set-up', async ({ page }, testInfo) => {
    await openLogin(page);
    await signIn(page, 'totp');
    await expect(page.locator('#kc-totp-settings')).toBeVisible();
    // Both inputs have a name a screen reader can announce (the parent's labels
    // pointed at an id that does not exist).
    await expect(page.getByLabel('One-time code')).toBeVisible();
    await expect(page.getByLabel('Device name')).toBeVisible();
    await capture(page, 'login-config-totp', testInfo);
  });

  test('forgot password: the "check your email" step', async ({ page }, testInfo) => {
    await openLogin(page);
    await page.getByRole('link', { name: /forgot/i }).click();
    await page.locator('#username').fill(CREDS.user.username);
    await page.locator('input[type="submit"], button[type="submit"]').first().click();
    await expect(page.locator('body')).toContainText(/check your email/i);
    await capture(page, 'forgot-password-sent', testInfo);
  });

  test('verify your email, then the "verified" info page', async ({ page, browser, contextOptions }, testInfo) => {
    const { form, scheme } = testInfo.project.metadata;
    const who = CREDS.verify(form, scheme);
    await openLogin(page);
    await signIn(page, who);
    await expect(page.locator('#kc-page-title')).toHaveText('Verify your email');
    await expect(page.locator('body')).toContainText(/we sent a message to/i);
    // The page's one action is a 44px button that names itself, not a "Click
    // here" link in running text (gd-KiRStJd56I).
    const resend = page.getByRole('link', { name: 'Send the message again' });
    await expect(resend).toBeVisible();
    expect((await resend.boundingBox()).height, 'resend height').toBeGreaterThanOrEqual(44);
    await capture(page, 'login-verify-email', testInfo);

    // Follow the emailed link in a fresh browser context, so no login session
    // is shared and Keycloak ends on its info page rather than continuing.
    const link = await latestActionLink(process.env.KC_MAIL_DIR, `${who.username}@example.invalid`);
    const ctx = await browser.newContext(contextOptions);
    try {
      const other = await ctx.newPage();
      await other.goto(link);
      // Keycloak first asks to confirm (an info page with one link), then verifies.
      await expect(other.locator('#kc-page-title')).toHaveText(/confirm your email address/i);
      await capture(other, 'info-confirm', testInfo);
      await other.getByRole('link', { name: /continue/i }).click();
      await expect(other.locator('#kc-page-title')).toHaveText(/email address verified/i);
      await expect(other.locator('body')).toContainText(/close this tab/i);
      await capture(other, 'info', testInfo);
    } finally {
      await ctx.close();
    }
  });
});
