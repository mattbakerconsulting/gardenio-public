// Every sign-in path of the chart's passkey browser flow (design doc 4.1) against
// a real Keycloak, with Chromium's virtual authenticator. run.sh builds the flow
// from `helm template` output (build-flow.sh) and binds it as the realm browser
// flow before this spec runs; it is skipped unless KC_FLOW=passkey.
//
//   1  admin with a passkey, conditional UI: no password, no second prompt
//   2  admin with a passkey, the login page's "Use a passkey" button
//   3  admin types username + password: prompted for a passkey, passes it
//   4  admin with a password and NO passkey: refused (no enrolment prompt; see
//      the README "Findings"), then the break-glass re-enrolment works
//   5  non-admin: password only, no prompt
//   6  wrong password: never reaches the second factor
//   7  `kd login` shaped device flow, approved in the browser with a passkey
//
// "No second prompt" is measured, not guessed: every assertion the virtual
// authenticator answers bumps a credential's signCount, so a passkey primary
// sign-in is exactly +1 and a password + passkey sign-in is exactly +1 too (the
// passkey answered once, as the second factor).
const { test, expect } = require('@playwright/test');
const { realmUrl, serverVersion, atLeast, holdConditionalUI, conditionalUIHeld } = require('./lib/kc');
const {
  createUser, grantAdmin, revokeAdmin, serverCredentialTypes, virtualAuthenticator, presence, assertions, credentialCount,
  accountHeading, typeCredentials, signOut, enrolPasskey,
} = require('./lib/flow');

// A failed attempt can leave a passkey on the server; a retry would then fail
// with "Device already exists", hiding the real error.
test.describe.configure({ retries: 0, mode: 'serial' });

test.skip(process.env.KC_FLOW !== 'passkey', 'KC_FLOW=passkey is set by run.sh once the chart flow is bound');

test.beforeEach(async ({ request }) => {
  const v = await serverVersion(request);
  test.skip(!v || !atLeast(v, 26, 4), `Passkeys need Keycloak >= 26.4; this server is ${v && v.raw}`);
});

const loginPage = (page) => page.locator('#username');

// An admin who already holds one passkey, signed out, on a fresh browser. The
// passkey is enrolled BEFORE the admin role is granted: design doc 3.D, the only
// order the flow allows.
async function admin_with_passkey(page, request, name) {
  const user = await createUser(request, name, { admin: false });
  const va = await virtualAuthenticator(page);
  await enrolPasskey(page, va, user);
  await grantAdmin(request, user);
  await signOut(page);
  return { user, va };
}

test('1. admin with a passkey signs in through conditional UI: no password, no second prompt', async ({ page, request }) => {
  const { user, va } = await admin_with_passkey(page, request, 'cond');
  const before = await assertions(va);
  // Nothing is typed or clicked: the login page starts a conditional-mediation
  // request on load and the virtual authenticator answers it.
  await page.goto(realmUrl('/account'));
  await expect(accountHeading(page)).toBeVisible({ timeout: 20_000 });
  await expect(page.locator('input[name="username"]')).toHaveValue(user.username);
  expect(await assertions(va) - before, 'passkey assertions: the primary one only').toBe(1);
});

test('2. admin with a passkey signs in with the "Use a passkey" button', async ({ page, request }) => {
  const { va } = await admin_with_passkey(page, request, 'modal');
  const before = await assertions(va);
  // Keep the conditional request in the page so it cannot sign in before the
  // button is used; the button's click aborts it and makes the only request the
  // authenticator answers.
  await holdConditionalUI(page);
  await page.goto(realmUrl('/account'));
  // Keycloak's login.ftl gives the passkey button a stable id whatever element
  // it is (an <a href="#"> in 26.5, a button in earlier lines). The gardenio
  // theme (S1d) words it "Use a passkey"; the stock copy was "Sign in with Passkey".
  const button = page.locator('#authenticateWebAuthnButton');
  await expect(button).toBeVisible();
  await expect(button).toHaveText(/use a passkey/i);
  await conditionalUIHeld(page);
  await button.click();
  await expect(accountHeading(page)).toBeVisible({ timeout: 20_000 });
  expect(await assertions(va) - before, 'passkey assertions: the primary one only').toBe(1);
});

test('3. admin types username + password: is prompted for a passkey and passes it', async ({ page, request }) => {
  const { user, va } = await admin_with_passkey(page, request, 'pwpk');
  const before = await assertions(va);
  await presence(va, false);
  await page.goto(realmUrl('/account'));
  await typeCredentials(page, user);
  // The password alone does not sign an admin in: the second-factor page is
  // up, waiting on a passkey the authenticator has not been allowed to give.
  await expect(loginPage(page)).toBeHidden({ timeout: 20_000 });
  await expect(accountHeading(page)).toBeHidden();
  expect(await assertions(va) - before, 'no assertion yet').toBe(0);
  // The second-factor page is webauthn-authenticate.ftl, themed by S1d as
  // "Use your passkey" with a "Use a passkey" button (the request it starts on
  // load is still waiting on presence).
  await expect(page.locator('#kc-page-title')).toHaveText('Use your passkey');
  const second = page.locator('#authenticateWebAuthnButton');
  await expect(second).toBeVisible();
  await expect(second).toHaveText(/use a passkey/i);
  await presence(va, true);
  await second.click();
  await expect(accountHeading(page)).toBeVisible({ timeout: 20_000 });
  expect(await assertions(va) - before, 'passkey assertions: the second factor only').toBe(1);
});

test('4. admin with a password but no passkey is refused; re-enrolling as a non-admin fixes it', async ({ page, request }) => {
  const user = await createUser(request, 'enrol', { admin: true });
  const va = await virtualAuthenticator(page);
  expect(await serverCredentialTypes(request, user)).toEqual({ password: 1 });
  await page.goto(realmUrl('/account'));
  await typeCredentials(page, user);
  // A REQUIRED webauthn-authenticator-passwordless with nothing configured does
  // not attach a register action (its factory does not allow user setup): the
  // sign-in stops, closed. The account console is not reached.
  await expect(page.locator('body')).toContainText(/credential setup required/i, { timeout: 20_000 });
  await expect(accountHeading(page)).toBeHidden();
  expect(await credentialCount(va), 'nothing was offered or registered').toBe(0);
  expect(await serverCredentialTypes(request, user)).toEqual({ password: 1 });

  // The recovery (design doc 7): take the admin role away, the user enrols a
  // passkey as a plain user, the role goes back, and passkey sign-in works.
  await page.context().clearCookies();
  await revokeAdmin(request, user);
  await enrolPasskey(page, va, user);
  await grantAdmin(request, user);
  expect(await serverCredentialTypes(request, user)).toEqual({ password: 1, 'webauthn-passwordless': 1 });
  await signOut(page);
  const before = await assertions(va);
  await page.goto(realmUrl('/account'));
  await expect(accountHeading(page)).toBeVisible({ timeout: 20_000 });
  expect(await assertions(va) - before, 'passkey assertions: the primary one only').toBe(1);
});

test('5. non-admin signs in with a password only, no prompt', async ({ page, request }) => {
  const user = await createUser(request, 'plain', { admin: false });
  const va = await virtualAuthenticator(page);
  await page.goto(realmUrl('/account'));
  await typeCredentials(page, user);
  await expect(accountHeading(page)).toBeVisible({ timeout: 20_000 });
  expect(await credentialCount(va), 'no passkey was offered or registered').toBe(0);
  expect(await serverCredentialTypes(request, user)).toEqual({ password: 1 });
});

test('6. wrong password never reaches the second factor', async ({ page, request }) => {
  const { user, va } = await admin_with_passkey(page, request, 'wrong');
  const before = await assertions(va);
  await presence(va, false);
  await page.goto(realmUrl('/account'));
  await typeCredentials(page, user, 'not-the-password');
  await expect(page.locator('body')).toContainText(/(invalid username or password|username or password is not right)/i);
  await expect(loginPage(page)).toBeVisible();
  await expect(accountHeading(page)).toBeHidden();
  // Even with presence back, nothing asks the authenticator.
  await presence(va, true);
  await page.waitForTimeout(2_000);
  await expect(accountHeading(page)).toBeHidden();
  expect(await assertions(va) - before, 'no passkey assertion after a wrong password').toBe(0);
});

test('7. `kd login` shaped device flow: approve the user code in the browser with a passkey', async ({ page, request }) => {
  const { va } = await admin_with_passkey(page, request, 'device');
  const before = await assertions(va);
  const start = await request.post(realmUrl('/protocol/openid-connect/auth/device'), {
    form: { client_id: 'gardenio-cli' },
  });
  expect(start.ok(), 'device authorization request').toBeTruthy();
  const dev = await start.json();

  // Pending until approved.
  const poll = () =>
    request.post(realmUrl('/protocol/openid-connect/token'), {
      form: {
        grant_type: 'urn:ietf:params:oauth:grant-type:device_code',
        client_id: 'gardenio-cli',
        device_code: dev.device_code,
      },
    });
  const pending = await poll();
  expect((await pending.json()).error, 'device code is pending before approval').toBe('authorization_pending');

  await page.goto(realmUrl('/device'));
  await page.locator('input[name="device_user_code"], #device-user-code').fill(dev.user_code);
  await page.locator('input[type="submit"], button[type="submit"]').first().click();
  // The sign-in is the passkey (conditional UI), then the grant page.
  await expect(page.locator('#kc-login')).toBeVisible({ timeout: 20_000 });
  await page.locator('#kc-login').click();
  await page.waitForLoadState('domcontentloaded');
  expect(await assertions(va) - before, 'passkey assertions: the primary one only').toBe(1);

  // The device code now polls to success (interval 5 s in the test realm).
  let ok = null;
  for (let i = 0; i < 12 && !ok; i++) {
    const res = await poll();
    if (res.ok()) ok = await res.json();
    else if ((await res.json()).error === 'slow_down' || (await res.json()).error === 'authorization_pending') await page.waitForTimeout(5_500);
    else break;
  }
  expect(ok && !!ok.access_token, 'the device code polls to an access token').toBe(true);
});
