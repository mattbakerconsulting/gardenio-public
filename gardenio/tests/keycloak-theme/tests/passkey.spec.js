// Passkeys end to end with Chromium's virtual authenticator (CDP WebAuthn
// domain): sign in with a password, register a passkey from the account
// console, sign out, then sign back in with the passkey.
//
//   test 1: conditional UI (the passkey is offered from the username field)
//   test 2: the autofill hook the theme must keep (autocomplete="username webauthn")
//   test 3: modal UI (the "Sign in with Passkey" button)
//
// Passkeys are supported from Keycloak 26.4; all tests SKIP with a message on
// an older server. Credential ids are never logged: only counts are.
const { test, expect } = require('@playwright/test');
const {
  realmUrl, signIn, openSigningIn, serverVersion, atLeast, screenshot, holdConditionalUI, conditionalUIHeld,
} = require('./lib/kc');

// No retries: a failed attempt may already have stored a passkey for the user
// on the server, and a second registration then fails with "Device already
// exists with the same name", hiding the real error.
test.describe.configure({ retries: 0 });

async function requirePasskeys(request) {
  const v = await serverVersion(request);
  test.skip(!v, 'Cannot determine the Keycloak version (no KC_ADMIN_* and no KC_TAG): skipping passkey tests');
  test.skip(
    !atLeast(v, 26, 4),
    `Passkeys (conditional + modal UI) need Keycloak >= 26.4; this server is ${v && v.raw}`,
  );
}

async function virtualAuthenticator(page) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return { cdp, authenticatorId };
}

async function credentialCount({ cdp, authenticatorId }) {
  const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
  return credentials.length;
}

// Password sign-in, then "Set up a Passkey" on the account console.
async function registerPasskey(page, who, va, testInfo) {
  await page.goto(realmUrl('/account'));
  await signIn(page, who);
  await openSigningIn(page);
  const setup = page.getByRole('button', { name: /passkey/i }).first();
  await expect(setup).toBeVisible();
  await setup.click();
  // Keycloak's webauthn-register page; the virtual authenticator answers.
  const register = page.locator('#registerWebAuthn, button:has-text("Register")').first();
  await expect(register).toBeVisible();
  await screenshot(page, 'passkey-register', testInfo).catch(() => {});
  // The parent script asks for a name with a browser prompt; Playwright would
  // dismiss it unseen. Record the wording (S1d: plain, no "WebAuthn") and answer.
  let prompted = null;
  page.once('dialog', async (dialog) => {
    prompted = { type: dialog.type(), message: dialog.message() };
    await dialog.accept('Laptop');
  });
  await register.click();
  await page.waitForURL(/\/account/, { timeout: 20_000 });
  expect(prompted, 'the passkey name prompt').not.toBeNull();
  expect(prompted.message).toBe('Name this passkey (for example Laptop or Phone)');
  expect(await credentialCount(va), 'passkeys held by the virtual authenticator').toBe(1);
}

// Sign out, then drop every cookie so the next visit is a fresh browser: a
// leftover auth-session cookie makes Keycloak show the previous user's
// re-authentication form (username pre-filled and disabled) instead of the
// username field the passkey is offered from. The virtual authenticator, and
// the passkey in it, lives in CDP, not in cookies, so it survives.
async function signOut(page) {
  await page.goto(realmUrl('/protocol/openid-connect/logout'));
  await page.locator('#kc-logout, input[name="confirmLogout"]').first().click();
  await page.waitForLoadState('domcontentloaded');
  await page.context().clearCookies();
}

test.describe('passkeys', () => {
  test('conditional UI: sign in from the username field', async ({ page, request }, testInfo) => {
    await requirePasskeys(request);
    const va = await virtualAuthenticator(page);
    await registerPasskey(page, 'admin', va, testInfo);
    await signOut(page);

    // Nothing is typed or clicked: with a virtual authenticator holding a
    // resident credential, Chromium resolves the conditional-mediation request
    // the login page starts on load, so the user lands signed in.
    await page.goto(realmUrl('/account'));
    await expect(page.getByRole('heading', { name: /personal info/i }).first()).toBeVisible({
      timeout: 20_000,
    });
    await expect(page.locator('input[name="username"]')).toHaveValue('test-admin');
  });

  test('conditional UI hook: the username input carries the webauthn autofill token', async ({
    page,
    request,
  }) => {
    await requirePasskeys(request);
    // No virtual authenticator here, so nothing signs in behind our back.
    await page.goto(realmUrl('/account'));
    await expect(page.locator('#username')).toHaveAttribute('autocomplete', /webauthn/);
  });

  test('modal UI: "Sign in with Passkey" button', async ({ page, request }, testInfo) => {
    await requirePasskeys(request);
    const va = await virtualAuthenticator(page);
    await registerPasskey(page, 'user', va, testInfo);
    await signOut(page);

    // The login page starts a conditional request on load, which the virtual
    // authenticator would answer at once and sign us in before the button can
    // be used. Keep it in the page; the button's click aborts it.
    await holdConditionalUI(page);
    await page.goto(realmUrl('/account'));
    // An <a href="#"> in 26.5, a button in some earlier lines: match either.
    // The gardenio theme words it "Use a passkey" (S1d); the stock text was
    // "Sign in with Passkey".
    const button = page.getByRole('link', { name: /use a passkey|sign in with passkey/i }).or(
      page.getByRole('button', { name: /use a passkey|sign in with passkey/i }),
    );
    await expect(button).toBeVisible();
    await screenshot(page, 'passkey-signin-button', testInfo).catch(() => {});
    await conditionalUIHeld(page);
    await button.click();
    await expect(page.getByRole('heading', { name: /personal info/i }).first()).toBeVisible({
      timeout: 20_000,
    });
  });
});
