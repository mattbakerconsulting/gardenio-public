// Helpers for the passkey browser-flow scenarios (plan gd-QXq1fjYuRD, S4a).
// Each scenario creates its own throwaway user, so no scenario depends on a
// passkey another one left behind. Credential ids are never read or logged:
// only counts and sign counts are.
const { expect } = require('@playwright/test');
const { BASE, realmUrl, openSigningIn } = require('./kc');

let tokenCache = null;

async function adminToken(request) {
  if (tokenCache && tokenCache.exp > Date.now() + 10_000) return tokenCache.token;
  const res = await request.post(`${BASE}/auth/realms/master/protocol/openid-connect/token`, {
    form: {
      grant_type: 'password',
      client_id: 'admin-cli',
      username: process.env.KC_ADMIN_USER,
      password: process.env.KC_ADMIN_PASSWORD,
    },
  });
  expect(res.ok(), 'master-realm admin token (KC_ADMIN_USER / KC_ADMIN_PASSWORD from run.sh)').toBeTruthy();
  const { access_token, expires_in } = await res.json();
  tokenCache = { token: access_token, exp: Date.now() + expires_in * 1000 };
  return access_token;
}

// A TEST-ONLY user in realm gardenio. `admin` grants the gardenio-admin realm
// role, the target of the flow's conditional-user-role.
async function createUser(request, name, { admin }) {
  const headers = { Authorization: `Bearer ${await adminToken(request)}` };
  const username = `flow-${name}-${Date.now().toString(36)}`;
  const password = 'test-only-fake-flow-pw';
  const res = await request.post(`${BASE}/auth/admin/realms/gardenio/users`, {
    headers,
    data: {
      username,
      email: `${username}@example.invalid`,
      firstName: 'Flow',
      lastName: name,
      enabled: true,
      emailVerified: true,
      credentials: [{ type: 'password', value: password, temporary: false }],
    },
  });
  expect(res.status(), 'create user').toBe(201);
  const user = { username, password, id: res.headers().location.split('/').pop() };
  if (admin) await grantAdmin(request, user);
  return user;
}

async function adminRole(request) {
  const headers = { Authorization: `Bearer ${await adminToken(request)}` };
  return (await request.get(`${BASE}/auth/admin/realms/gardenio/roles/gardenio-admin`, { headers })).json();
}

async function grantAdmin(request, user) {
  const headers = { Authorization: `Bearer ${await adminToken(request)}` };
  const map = await request.post(`${BASE}/auth/admin/realms/gardenio/users/${user.id}/role-mappings/realm`, {
    headers,
    data: [await adminRole(request)],
  });
  expect(map.ok(), 'grant gardenio-admin').toBeTruthy();
}

// What the break-glass recipe does before a user with no passkey re-enrols.
async function revokeAdmin(request, user) {
  const headers = { Authorization: `Bearer ${await adminToken(request)}` };
  const res = await request.delete(`${BASE}/auth/admin/realms/gardenio/users/${user.id}/role-mappings/realm`, {
    headers,
    data: [await adminRole(request)],
  });
  expect(res.ok(), 'revoke gardenio-admin').toBeTruthy();
}

// Counts per credential type on the server, from the admin API.
async function serverCredentialTypes(request, user) {
  const headers = { Authorization: `Bearer ${await adminToken(request)}` };
  const res = await request.get(`${BASE}/auth/admin/realms/gardenio/users/${user.id}/credentials`, { headers });
  const types = {};
  for (const c of await res.json()) types[c.type] = (types[c.type] || 0) + 1;
  return types;
}

// Chromium virtual authenticator over CDP (ctap2, internal, resident key, user
// verification). Presence is automatic by default, which also answers the
// login page's conditional-mediation request at once.
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

const presence = (va, enabled) =>
  va.cdp.send('WebAuthn.setAutomaticPresenceSimulation', { authenticatorId: va.authenticatorId, enabled });

// Sum of sign counts: every assertion the authenticator answered bumps it by
// one, so a delta says how many passkey prompts a sign-in went through.
async function assertions(va) {
  const { credentials } = await va.cdp.send('WebAuthn.getCredentials', { authenticatorId: va.authenticatorId });
  return credentials.reduce((n, c) => n + c.signCount, 0);
}

async function credentialCount(va) {
  const { credentials } = await va.cdp.send('WebAuthn.getCredentials', { authenticatorId: va.authenticatorId });
  return credentials.length;
}

const accountHeading = (page) => page.getByRole('heading', { name: /personal info/i }).first();

async function typeCredentials(page, user, password = user.password) {
  await page.locator('#username').fill(user.username);
  await page.locator('#password').fill(password);
  await page.locator('#kc-login').click();
}

// Sign out, then drop every cookie so the next visit is a fresh browser (a
// leftover auth-session cookie shows the previous user's re-authentication
// form instead of the username field). The virtual authenticator lives in CDP,
// not in cookies, so the passkey survives.
async function signOut(page) {
  await page.goto(realmUrl('/protocol/openid-connect/logout'));
  await page.locator('#kc-logout, input[name="confirmLogout"]').first().click();
  await page.waitForLoadState('domcontentloaded');
  await page.context().clearCookies();
}

// The register-passkey page (Keycloak's webauthn-register-passwordless required
// action): Register button, answered by the virtual authenticator.
const registerButton = (page) => page.locator('#registerWebAuthn, button:has-text("Register")').first();

// Password sign-in, then "Set up a Passkey" on the account console. This is how
// a passkey gets enrolled: the flow itself cannot do it (Keycloak's passwordless
// authenticator does not offer enrolment, see the README), so it happens while
// the user does NOT hold the admin role. Leaves the user signed in with one
// passkey in `va`.
async function enrolPasskey(page, va, user) {
  await page.goto(realmUrl('/account'));
  await typeCredentials(page, user);
  await openSigningIn(page);
  const setup = page.getByRole('button', { name: /passkey/i }).first();
  await expect(setup).toBeVisible();
  await setup.click();
  await expect(registerButton(page)).toBeVisible({ timeout: 20_000 });
  await registerButton(page).click();
  await page.waitForURL(/\/account/, { timeout: 20_000 });
  expect(await credentialCount(va), 'passkeys held by the virtual authenticator').toBe(1);
}

module.exports = {
  createUser, grantAdmin, revokeAdmin, serverCredentialTypes, virtualAuthenticator, presence, assertions, credentialCount,
  accountHeading, typeCredentials, signOut, registerButton, enrolPasskey,
};
