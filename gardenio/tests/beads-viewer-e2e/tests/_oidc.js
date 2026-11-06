// OIDC sessions for the specs that need a human principal (gd-ZTu68qjfEC).
//
// The default viewer runs dev-auth-bypass and reaches the daemon on the
// harness fleet token, which kbeads does not let answer a decision or write
// gardener budget/counter fields. When the harness OIDC issuer is running
// (oidc-stub/server.mjs), playwright.config.js also boots a second viewer in
// OIDC mode at OIDC_VIEWER_URL. A spec opts in per describe block:
//
//   test.describe('...', () => {
//     requireOIDCStub(test);
//     test.use(oidcSession('someone@example.test', ['gardenio-operator']));
//     ...
//   });
//
// Each request then carries Authorization: Bearer <minted token>, the viewer
// verifies it like a Keycloak token, and forwards it to the daemon as the
// write credential (BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1), which is the
// production write path.

import { randomUUID } from 'node:crypto';

import { mintBearer, mintMachineBearer, readState, TOKEN_MINT_CLIENT } from '../oidc-stub/mint.mjs';

export const OIDC_VIEWER_PORT = process.env.BEADS_VIEWER_OIDC_PORT || '8096';
export const OIDC_VIEWER_URL = `http://localhost:${OIDC_VIEWER_PORT}`;

export const oidcStub = readState();

// requireOIDCStub skips the enclosing describe when no stub is running, which
// is the normal local setup (a port-forwarded daemon cannot trust the stub). In
// CI it never skips: a missing stub there must fail loudly, not hide the specs.
export function requireOIDCStub(test) {
  test.skip(!oidcStub && !process.env.CI, 'needs the harness OIDC issuer (oidc-stub/server.mjs) and a daemon that trusts it; see README');
}

// oidcSession returns test.use() options pointing the browser at the OIDC
// viewer with a bearer for email carrying the given realm roles.
export function oidcSession(email, roles) {
  if (!oidcStub) return {};
  return {
    baseURL: OIDC_VIEWER_URL,
    extraHTTPHeaders: { Authorization: `Bearer ${mintBearer({ email, roles })}` },
  };
}

// mintAgentToken registers a per-agent fleet token for the agent bead agentId
// and returns it. A write on it is authored as agentName: the daemon takes the
// author from the credential and honours no X-Actor override (gd-ke6PC7bRto).
// It mints the way the controller does in prod (agenttoken.go): a token-mint
// machine principal POSTs /v1/bridge-tokens with bridge agent-<bead id> and
// subject <agent name>. Needs the OIDC stub and a daemon with TOKEN_MINT_CLIENT
// in BEADS_MACHINE_CLIENTS (e2e.yml phase 2).
export async function mintAgentToken(agentId, agentName) {
  const base = process.env.BEADS_HTTP_ADDR || 'http://localhost:8080';
  const token = `e2e-agent-${randomUUID()}`;
  const machine = mintMachineBearer({ clientId: TOKEN_MINT_CLIENT, roles: ['gardenio-machine-token-mint'] });
  const res = await fetch(`${base}/v1/bridge-tokens`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${machine}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ token, bridge: `agent-${agentId}`, subject: agentName, ttl: '2h' }),
  });
  if (!res.ok) {
    throw new Error(`mint agent token for ${agentId}: HTTP ${res.status} ${await res.text()}`);
  }
  return token;
}
