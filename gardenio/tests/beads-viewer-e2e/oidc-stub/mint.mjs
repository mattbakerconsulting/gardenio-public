// Token minting for the test-harness OIDC issuer (server.mjs, gd-ZTu68qjfEC).
//
// Signs Keycloak-shaped RS256 access tokens with the key the running stub wrote
// to its state dir. Synchronous, so a spec can mint at load time inside
// test.use({ extraHTTPHeaders }).
//
// Claim shape follows what the two verifiers read:
//   - beads-viewer oidcauth: aud contains OIDC_CLIENT_ID, email +
//     email_verified=true (required for a non-Entra issuer), realm_access.roles
//     (viewerCanAdmin reads gardenio-admin off it).
//   - kbeads oidc.go: aud contains BEADS_OIDC_AUDIENCE, email_verified, realm
//     roles from realm_access.roles, azp for the approver-client allowlist.

import { createPrivateKey, randomUUID, sign } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));

// The OAuth client the tokens are issued to. One client for both viewer and
// daemon, like prod's keycloak.oidcAudience (the browser SSO client).
export const AUDIENCE = 'gardenio-e2e';
export const REALM_PATH = '/realms/gardenio-e2e';

export function stateDir() {
  return process.env.OIDC_STUB_DIR || join(__dirname, '.state');
}

// readState returns the running stub's state.json, or null when no stub has
// been started (local runs against a port-forwarded daemon).
export function readState() {
  const file = join(stateDir(), 'state.json');
  if (!existsSync(file)) return null;
  return JSON.parse(readFileSync(file, 'utf8'));
}

const b64url = (v) => Buffer.from(typeof v === 'string' ? v : JSON.stringify(v)).toString('base64url');

// mintBearer returns a signed access token for email carrying the given realm
// roles. Throws when the stub is not running.
export function mintBearer({ email, roles = ['gardenio-admin'], ttlSeconds = 2 * 60 * 60 }) {
  const state = readState();
  if (!state) throw new Error(`OIDC stub not running: no state.json in ${stateDir()}`);
  const now = Math.floor(Date.now() / 1000);
  const claims = {
    iss: state.issuer,
    sub: randomUUID(),
    aud: [state.audience, 'account'],
    azp: state.audience,
    typ: 'Bearer',
    iat: now,
    nbf: now - 5,
    exp: now + ttlSeconds,
    scope: 'openid email profile',
    email,
    email_verified: true,
    preferred_username: email,
    realm_access: { roles },
  };
  return signClaims(state, claims);
}

// The OAuth client the token-mint machine principal is issued to. e2e.yml
// registers it in BEADS_MACHINE_CLIENTS for the phase-2 daemon.
export const TOKEN_MINT_CLIENT = 'gardenio-e2e-token-mint';

// mintMachineBearer returns a signed service-account access token for the
// OAuth client clientId carrying the given realm roles: no email, azp=clientId
// and preferred_username service-account-<clientId>, which kbeads classifies
// as a machine principal (machine_identity.go) when clientId is in
// BEADS_MACHINE_CLIENTS. Throws when the stub is not running.
export function mintMachineBearer({ clientId, roles = [], ttlSeconds = 10 * 60 }) {
  const state = readState();
  if (!state) throw new Error(`OIDC stub not running: no state.json in ${stateDir()}`);
  const now = Math.floor(Date.now() / 1000);
  const claims = {
    iss: state.issuer,
    sub: randomUUID(),
    aud: [state.audience, 'account'],
    azp: clientId,
    typ: 'Bearer',
    iat: now,
    nbf: now - 5,
    exp: now + ttlSeconds,
    preferred_username: `service-account-${clientId}`,
    realm_access: { roles },
  };
  return signClaims(state, claims);
}

function signClaims(state, claims) {
  const signingInput = `${b64url({ alg: 'RS256', typ: 'JWT', kid: state.kid })}.${b64url(claims)}`;
  const key = createPrivateKey(readFileSync(state.signingKeyFile));
  return `${signingInput}.${sign('RSA-SHA256', Buffer.from(signingInput), key).toString('base64url')}`;
}
