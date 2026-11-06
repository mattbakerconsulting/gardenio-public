// Test-harness OIDC issuer for the viewer-e2e lane (gd-ZTu68qjfEC).
//
// The lane's viewer runs dev-auth-bypass and talks to the daemon on the shared
// static token, so it has no human principal: kbeads refuses a decision answer
// from that token (decision_resolver_authz.go) and gardener budget/counter
// writes need an OIDC gardenio-admin (autonomous_field_authz.go). Production
// humans write with their own Keycloak bearer. This stub stands in for Keycloak
// so the lane can take that same path, with nothing changed in the shipping
// binaries or the chart:
//
//   - serves OIDC discovery + a JWKS over HTTPS (beads-viewer's
//     oidcauth.Validate refuses a non-https issuer), Keycloak-shaped paths;
//   - writes its RS256 signing key and a CA bundle into a state dir, so
//     mint.mjs can sign Keycloak-shaped access tokens in-process for any email
//     and realm roles a spec needs.
//
// It mints nothing over HTTP: there is no token endpoint. The private key never
// leaves the state dir on the runner.
//
//   node oidc-stub/server.mjs            # foreground; Ctrl-C to stop
//
// Env:
//   OIDC_STUB_PORT  listen port on 127.0.0.1 (default 8098)
//   OIDC_STUB_DIR   state dir (default oidc-stub/.state next to this file)
//
// Writes into OIDC_STUB_DIR:
//   state.json      { issuer, audience, kid, signingKeyFile, caBundleFile }
//   signing-key.pem RS256 private key (PKCS#8)
//   tls.crt/.key    self-signed localhost cert for the HTTPS listener
//   ca-bundle.pem   system roots + tls.crt; point SSL_CERT_FILE at it for the
//                   Go processes that must trust the stub (viewer, daemon)
// state.json is written last, after the listener is up, so its presence means
// "ready".

import { execFileSync } from 'node:child_process';
import { createHash, generateKeyPairSync } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:https';
import { join, resolve } from 'node:path';

import { AUDIENCE, REALM_PATH, stateDir } from './mint.mjs';

const port = Number(process.env.OIDC_STUB_PORT || '8098');
const dir = resolve(stateDir());
const issuer = `https://localhost:${port}${REALM_PATH}`;

mkdirSync(dir, { recursive: true });
// A stale state.json from an earlier run would read as "ready" before this
// listener is up.
rmSync(join(dir, 'state.json'), { force: true });

// TLS: node:crypto cannot build an X.509 certificate, so shell out to openssl.
const tlsKey = join(dir, 'tls.key');
const tlsCrt = join(dir, 'tls.crt');
execFileSync(
  'openssl',
  [
    'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
    '-keyout', tlsKey, '-out', tlsCrt,
    '-days', '2', '-subj', '/CN=localhost',
    '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1',
  ],
  { stdio: ['ignore', 'ignore', 'pipe'] },
);

// SSL_CERT_FILE replaces Go's system root pool rather than adding to it, so
// the bundle carries the system roots too.
const systemRoots = ['/etc/ssl/certs/ca-certificates.crt', '/etc/pki/tls/certs/ca-bundle.crt', '/etc/ssl/cert.pem']
  .find((p) => existsSync(p));
const caBundle = join(dir, 'ca-bundle.pem');
writeFileSync(caBundle, (systemRoots ? readFileSync(systemRoots, 'utf8') + '\n' : '') + readFileSync(tlsCrt, 'utf8'));

// Signing key: fresh per start, so a token minted against an earlier run's key
// fails verification instead of silently passing.
const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const jwk = publicKey.export({ format: 'jwk' });
const kid = createHash('sha256').update(jwk.n).digest('base64url').slice(0, 16);
const signingKeyFile = join(dir, 'signing-key.pem');
writeFileSync(signingKeyFile, privateKey.export({ format: 'pem', type: 'pkcs8' }), { mode: 0o600 });

const discovery = {
  issuer,
  jwks_uri: `${issuer}/protocol/openid-connect/certs`,
  // Never served; present because a Keycloak discovery document has them.
  authorization_endpoint: `${issuer}/protocol/openid-connect/auth`,
  token_endpoint: `${issuer}/protocol/openid-connect/token`,
  response_types_supported: ['code'],
  subject_types_supported: ['public'],
  id_token_signing_alg_values_supported: ['RS256'],
};
const jwks = { keys: [{ ...jwk, kid, alg: 'RS256', use: 'sig' }] };

const routes = {
  [`${REALM_PATH}/.well-known/openid-configuration`]: discovery,
  [`${REALM_PATH}/protocol/openid-connect/certs`]: jwks,
};

const server = createServer({ key: readFileSync(tlsKey), cert: readFileSync(tlsCrt) }, (req, res) => {
  const body = req.method === 'GET' ? routes[new URL(req.url, issuer).pathname] : undefined;
  if (!body) {
    res.writeHead(404, { 'Content-Type': 'text/plain' }).end('not found\n');
    return;
  }
  res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify(body));
});

server.listen(port, '127.0.0.1', () => {
  writeFileSync(
    join(dir, 'state.json'),
    JSON.stringify({ issuer, audience: AUDIENCE, kid, signingKeyFile, caBundleFile: caBundle }, null, 2) + '\n',
  );
  console.log(`[oidc-stub] issuer ${issuer} (kid ${kid}); state in ${dir}`);
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => server.close(() => process.exit(0)));
}
