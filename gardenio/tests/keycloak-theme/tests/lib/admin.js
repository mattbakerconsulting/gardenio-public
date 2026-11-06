// Admin REST helpers for the harness (bootstrap admin from run.sh; the
// container is a throwaway). Node's global fetch, no extra dependency.
const { BASE, REALM } = require('./kc');

async function adminToken() {
  const user = process.env.KC_ADMIN_USER;
  const pass = process.env.KC_ADMIN_PASSWORD;
  if (!user || !pass) throw new Error('KC_ADMIN_USER / KC_ADMIN_PASSWORD are not set (run.sh exports them)');
  const res = await fetch(`${BASE}/auth/realms/master/protocol/openid-connect/token`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ grant_type: 'password', client_id: 'admin-cli', username: user, password: pass }),
  });
  if (!res.ok) throw new Error(`admin token: HTTP ${res.status}`);
  return (await res.json()).access_token;
}

async function api(token, method, path, body) {
  const res = await fetch(`${BASE}/auth/admin/realms/${REALM}${path}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok && res.status !== 409) {
    throw new Error(`${method} ${path}: HTTP ${res.status} ${await res.text().catch(() => '')}`);
  }
  return res;
}

const PASSKEY_ONLY_FLOW = 'gd-passkey-only';
const PASSKEY_ONLY_CLIENT = 'gardenio-passkey-test';

// A browser flow whose only step is the passwordless passkey authenticator
// (what the chart's passkey flow, S4a, ends with), bound to one test client
// through authenticationFlowBindingOverrides so the realm's own browser flow,
// and every other spec, stay as they are. Idempotent.
async function ensurePasskeyOnlyClient() {
  const token = await adminToken();
  let flows = await (await api(token, 'GET', '/authentication/flows')).json();
  let flow = flows.find((f) => f.alias === PASSKEY_ONLY_FLOW);
  if (!flow) {
    await api(token, 'POST', '/authentication/flows', {
      alias: PASSKEY_ONLY_FLOW,
      description: 'S1d test flow: passkey only',
      providerId: 'basic-flow',
      topLevel: true,
      builtIn: false,
    });
    flows = await (await api(token, 'GET', '/authentication/flows')).json();
    flow = flows.find((f) => f.alias === PASSKEY_ONLY_FLOW);
    await api(token, 'POST', `/authentication/flows/${PASSKEY_ONLY_FLOW}/executions/execution`, {
      provider: 'webauthn-authenticator-passwordless',
    });
    const execs = await (await api(token, 'GET', `/authentication/flows/${PASSKEY_ONLY_FLOW}/executions`)).json();
    for (const e of execs) {
      await api(token, 'PUT', `/authentication/flows/${PASSKEY_ONLY_FLOW}/executions`, { ...e, requirement: 'REQUIRED' });
    }
  }
  const found = await (await api(token, 'GET', `/clients?clientId=${PASSKEY_ONLY_CLIENT}`)).json();
  if (!found.length) {
    await api(token, 'POST', '/clients', {
      clientId: PASSKEY_ONLY_CLIENT,
      name: 'gardenio passkey test app',
      enabled: true,
      publicClient: true,
      standardFlowEnabled: true,
      redirectUris: ['http://localhost:3000/*'],
      attributes: { 'pkce.code.challenge.method': 'S256' },
      authenticationFlowBindingOverrides: { browser: flow.id },
    });
  }
  return { flowId: flow.id, clientId: PASSKEY_ONLY_CLIENT };
}

// Ask Keycloak to email `username` the "execute actions" link (the message an
// administrator triggers), through the admin API. Needs the user to have an email.
async function sendExecuteActionsEmail(username, actions) {
  const token = await adminToken();
  const users = await (await api(token, 'GET', `/users?username=${encodeURIComponent(username)}&exact=true`)).json();
  if (!users.length) throw new Error(`no such user ${username}`);
  await api(token, 'PUT', `/users/${users[0].id}/execute-actions-email?lifespan=43200`, actions);
}

module.exports = { adminToken, ensurePasskeyOnlyClient, sendExecuteActionsEmail, PASSKEY_ONLY_CLIENT };
