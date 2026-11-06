# edge-auth operator runbook

Operator-facing guide to the `gardenio-edge-auth` non-AVA edge — the auth
shim that lets teammates drive `kd` and `coop` from a laptop with a
GitLab PAT instead of an AVA browser session.

Plan: [`gardenio/plans/stage3-ava-cli-eb-rollout.md`](../plans/stage3-ava-cli-eb-rollout.md).
Epic: [ka-0000003384](https://beads.gardenio.e2e.example.com/beads/ka-0000003384).
Sibling: `gb-login-runbook.md` (browser/AVA path; works in parallel and is
unchanged by edge-auth).

## TL;DR

```sh
# one-time
#   1. join the gardenio GitLab group
#   2. create a GitLab PAT (read_api), save it somewhere your shell can read
export GARDENIO_TOKEN="glpat-xxxxxxxxxxxxxxxxxxxx"

# day-to-day
kd list   --http-url https://cli.gardenio.example.com/kd
coop list --http-url https://cli.gardenio.example.com/mux
```

The CLIs read `GARDENIO_TOKEN` automatically and stamp it as
`Authorization: Bearer …`. No VPN. No `gb login`. No browser.

## Prerequisites

- **GitLab.com account** in the `gardenio` group on
  [`gitlab.com/example-org/gardenio`](https://gitlab.com/example-org/gardenio).
  If you can see the gardenio project in the GitLab UI, you are in the
  group; if not, ask in `#gardenio-ops` for an invite.
- **No VPN required** — `cli.gardenio.<env>` is a public hostname (rate-
  limited and audited). The AVA-fronted `gardenio.<env>` host is still
  VPN-fronted and untouched by this runbook.
- **Compatible CLI**: `kd` and `coop` from the calver release that
  shipped `--token` (W-B-13 / W-B-14). `kd --help | grep token` should
  show the flag.

## Provision a GitLab PAT

1. Open <https://gitlab.com/-/user_settings/personal_access_tokens>.
2. **Name**: pick something self-describing, e.g. `gardenio-cli-laptop`.
3. **Expiration**: at most 90 days. Shorter is better; longer is rejected
   by group policy.
4. **Scopes**: tick **`read_api`** only. Nothing else.
   - `api` works but is over-scoped — do not use it.
   - `read_user` alone is **not** sufficient; edge-auth needs `read_api`
     to call `/api/v4/groups/.../members/all/{user_id}`.
5. Click **Create personal access token**, copy the `glpat-…` value
   shown once.
6. Store it. Recommended:
   - macOS: `security add-generic-password -a "$USER" -s gardenio-token -w glpat-…`
     then export from your shell rc:
     `export GARDENIO_TOKEN="$(security find-generic-password -a "$USER" -s gardenio-token -w)"`.
   - Linux: a `pass` entry, an env-managed file under `~/.config/gardenio/`
     mode `0600`, or your team's existing secret manager.
   - **Never** commit it. **Never** paste it into Slack or a ticket.

## Discover the `cli.gardenio.<env>` URL

The hostname pattern is **`cli.` + the env's AVA hostname**, on the same
Traefik ACM cert chain. Per env:

| Env | CLI base | Status |
| --- | --- | --- |
| `gardenio-rwx` (staging) | `https://cli.gardenio-rwx.e2e.example.com` | enabled per rollout-15 |
| `gardenio` (prod) | `https://cli.gardenio.example.com` | live (133 prod edge, ka-0000002359) |
| `canonical-prod` | `https://cli.gardenio-customer-poc.e2e.example.com` | follows prod |
| long-lived dev (operator-owned) | `https://cli.gardenio-<env>.e2e.example.com` | per-env opt-in |

`Helm` values that govern this: `edgeAuth.enabled` (master gate, default
`false`) and `edgeAuth.host` (required when enabled). If a CLI call gets
`NXDOMAIN` or `connection refused`, the env has not flipped
`edgeAuth.enabled=true` yet — check rollout status in
[ka-0000004915](https://beads.gardenio.e2e.example.com/beads/ka-0000004915).

Path fanout (matches the unified-host layout, see plan §Workstream E):

| Upstream | Path |
| --- | --- |
| `kbeads` HTTP API | `/kd/…` |
| `coopmux` | `/mux/…` |
| health (no auth) | `/healthz`, `/readyz` |
| self-test (auth, no proxy) | `/auth/check` |

`mezmo-viewer` and `jira-bridge` are AVA-only at v1 and are not exposed
through edge-auth.

## Configure the CLI

`kd` and `coop` resolve the bearer token from environment variables in
priority order (`kbeads/cmd/kd/main.go` → `defaultToken`):

1. `--token <value>` flag (explicit override).
2. `$GARDENIO_TOKEN` (preferred).
3. `$KBEADS_TOKEN` (legacy alias).
4. `$BEADS_AUTH_TOKEN` (direct-CLI canonical).
5. `$BEADS_DAEMON_TOKEN` (agent-pod path; do not set on laptops).

> **Humans: prefer `gb login` over exporting a token.** For a laptop /
> daily-driver CLI, run `gb login` (or `kd login`): a Keycloak device-grant
> sign-in whose token both CLIs send and refresh, so you never copy
> `BEADS_DAEMON_TOKEN` (or any daemon bearer) out of a pod. The explicit-token
> variables above remain for in-cluster agents and CI. Full flow:
> [`gb-login-runbook.md`](gb-login-runbook.md).

The CLI sends `Authorization: Bearer $GARDENIO_TOKEN` on every outbound
request. A `glpat-*` token is validated against `/api/v4/user` and the
configured Gardenio group membership. A Gardenio-audience Entra JWT acquired
through PiID is validated against the configured Entra issuer, tenant,
audience, delegated scope, authorized client, and app roles. Both paths stamp
the authenticated identity into an HMAC envelope and reverse-proxy to the
in-cluster Service.

The role-aware v2 envelope is used by exactly two paths: a direct
PiID/Microsoft Entra delegated bearer token, and an oauth2-proxy cookie whose
provider is explicitly configured with `roleSource=entra-app-roles` (the
private Ziti browser handoff). Both map exact bare Entra app-role values
(`gardenio-viewer`, `gardenio-operator`, `gardenio-admin`) to Gardenio roles;
path-like values such as `personal-project/gardenio-admin` remain unrecognized,
so a similarly named group in an unrelated path grants no role.

The public AVA path and the default `provider-groups` cookie source do **not**
carry v2 roles — they sign a v1 envelope. KBeads still verifies v1, but since
gd-AvSKOUyqRz (which retired operator decision ka-0000001641, "additive") a
verified v1 envelope is non-role-bearing, like the shared static token:
operator-level routes admit it, while RoleAdmin routes (bead delete/restore,
config delete, taints, bridge/fleet tokens, operator alias admin) and admin-only
fields refuse it. Paths that need admin authority must sign v2 with roles.
A v1 envelope cannot approve an escalation either: only a verified v2 envelope
with at least `gardenio-operator` is an escalation approver (gd-dMof1PW1oJ).

v1 and v2 envelopes can be signed with separate keys (gd-dMof1PW1oJ), so a
holder of the v1 key alone (beads-viewer signs v1 only) cannot mint a v2 admin
envelope. The v2 key is `EDGE_AUTH_HMAC_KEY_V2` on edge-auth,
`SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY_V2` on the slack-bridge, and
`DAEMON_HMAC_KEY_V2` on KBeads and Coopmux. Each falls back to the shared key
while unset, so the split is inert until the second secret is wired. Never
give the v2 key to beads-viewer. Wire every v2 signer and verifier in one
upgrade: a verifier with the v2 key rejects v2 envelopes from a signer still on
the shared key.

In the chart this is one value, `edgeAuth.hmacKey.secretKeyV2` (e.g.
`hmac-key-v2`): setting it wires all four env vars from that key of the
edge-auth HMAC Secret in one upgrade (gd-h2tQrOwjGJ). The in-box self-seeded
Secret gains the key; on the ExternalSecrets path also set
`externalSecrets.edgeAuthHMACKeyV2.remoteRef` (the template fails without it);
an operator-supplied `edgeAuth.hmacKey.secretName` Secret must already carry
the key before the upgrade, since the daemon mounts are required unless
`edgeAuth.dryRun`.

Edge-auth recognizes oauth2-proxy's base session cookie and its numbered
`_<n>` chunks, but never parses or trusts their contents. It forwards the full
Cookie header to `/oauth2/auth`, and accepts identity and roles only from that
endpoint's successful authenticated response.

Edge-auth then signs the operator, auth mode, canonical roles, audit ID, HTTP
method, upstream path, and timestamp in
`X-Edge-Auth-Sig-V2`. The v1 `X-Edge-Auth-Sig` remains alongside it during
rolling upgrades, but updated daemons treat v1 as read-only. KBeads and
Coopmux echo the trusted `X-Auth-Audit-Id` on responses and include it in
structured mutation logs.

For rollout, deploy the KBeads and Coopmux verifier/authorization changes
before relying on Edge v2 for writes. The dual signature prevents read
outages while old pods drain, but production write clients must stay disabled
until every serving daemon pod is on the v2-aware image and representative
viewer/operator/admin checks pass in a non-production environment.

## Entra CLI and Ziti browser handoff

The PiID CLI path and the private Ziti browser path both use Acme Entra
identity and the same Gardenio app-role vocabulary, but they present different
credentials to edge-auth:

| Surface | Credential at edge-auth | Authentication authority | Role source |
| --- | --- | --- | --- |
| PiID CLI | Gardenio-audience Entra access token in `Authorization: Bearer` | edge-auth validates issuer, tenant, audience, delegated scope, authorized `azp`, expiry, email, and roles | exact bare Entra app roles |
| Ziti browser | `_gardenio_ziti_oauth2` session cookie, possibly split into numbered `_<n>` chunks | the configured oauth2-proxy validates the complete cookie through `/oauth2/auth` | exact bare Entra app roles because this proxy uses `--oidc-groups-claim=roles` |

After those provider-specific checks, both paths share the same authorization
pipeline:

1. Filter to exact `gardenio-viewer`, `gardenio-operator`, and
   `gardenio-admin` values and canonicalize their order.
2. Resolve the authenticated email, auth mode, and audit ID.
3. Sign the same method- and path-bound v2 HMAC identity envelope.
4. Let KBeads and Coopmux enforce the same role hierarchy and endpoint policy.

The intentional trust boundaries are:

- Ziti proves private network reachability; it is not itself a Gardenio bearer
  token or application role.
- The browser does not need to acquire a PiID-style API token. Its session
  remains opaque to edge-auth and is trusted only after `/oauth2/auth`
  successfully validates it.
- Edge-auth never trusts client-supplied `X-Auth-Request-*` headers. Browser
  email and roles come only from the authenticated oauth2-proxy response.
- Cookie providers default to `roleSource=provider-groups`, which carries no v2
  roles at all — a default cookie deployment signs a v1 envelope and keeps its
  pre-MR authority. Roles (v2) are produced only for a provider explicitly set
  to `roleSource=entra-app-roles`.
- `entra-app-roles` performs exact bare-value matching. A value such as
  `personal-project/gardenio-admin` does not become `gardenio-admin`.

The shared post-authentication pipeline could later be made more explicit with
an internal principal such as:

```go
type authenticatedPrincipal struct {
    Email    string
    Roles    []string
    AuthMode string
}
```

The direct bearer verifier and oauth2-proxy verifier would be adapters that
produce this type. Envelope signing, auditing, and authorization would remain
shared. Do not merge the credential validators, parse oauth2-proxy cookies in
edge-auth, or enable bare-role matching globally; those separations are the
security boundary, not duplication to remove.

### Reviewer checklist

- Numbered cookies are recognized only to select cookie authentication; the
  entire `Cookie` header is still forwarded unchanged to `/oauth2/auth`.
- Base, `_0`, `_1`, and later numeric chunks are recognized; lookalike
  non-numeric suffixes are not.
- `provider-groups` remains the chart and runtime default, and it produces no
  v2 roles — a default cookie deployment signs a v1 envelope.
- The canonical prod Ziti overlay explicitly sets
  `edgeAuth.cookieAuth.roleSource: entra-app-roles` because its oauth2-proxy
  explicitly maps the Entra `roles` claim; this is the only cookie source that
  yields a v2 envelope.
- Unknown runtime role-source values grant no canonical role (fail closed),
  as does the default `provider-groups` source.
- Regression coverage for the `entra-app-roles` v2 path combines a split
  cookie, exact bare `gardenio-operator`, and unrelated
  `personal-project/gardenio-admin`; the resulting v2 envelope contains only
  `gardenio-operator`.

### Deployment and verification

1. Build and promote one release containing the daemon v2 verifier/RBAC
   changes, edge-auth changes, and chart changes.
2. Roll KBeads and Coopmux before relying on browser or CLI mutations.
3. Roll edge-auth with the `edgeAuth` block of the production values,
   `deploy-values/gardenio-k3s.yaml`.
4. Confirm the rendered/live edge-auth deployment contains:

   ```text
   EDGE_AUTH_OAUTH2_PROXY_COOKIE_NAME=_gardenio_ziti_oauth2
   EDGE_AUTH_OAUTH2_PROXY_ROLE_SOURCE=entra-app-roles
   ```

5. With Ziti connected, sign into `https://gardenio.example.com` and verify
   `/kd` and `/mux` no longer return `{"error":"missing_token"}`. A large Entra
   session should work when oauth2-proxy splits it into multiple cookies.
6. Exercise representative viewer and operator reads on both the PiID bearer
   and Ziti browser paths. In a non-production environment, verify an operator
   mutation succeeds and a viewer mutation receives `403`.
7. Correlate each request by `X-Auth-Audit-Id`; do not log or paste bearer
   tokens or raw cookies.

Smoke-test the round trip before doing anything destructive:

```sh
# PiID / Entra path. Keep shell tracing off so the bearer is never printed.
set +x
export GARDENIO_TOKEN="$(
  piid auth token \
    --scope api://acme.example/gardenio/user_impersonation \
    --silent --json |
  jq -r .access_token
)"

# Just the auth surface, no upstream call:
curl -sS -H "Authorization: Bearer $GARDENIO_TOKEN" \
  https://cli.gardenio.example.com/auth/check
# -> {"ok":true,"email":"alice@example.com",...}

# Hit kbeads:
kd list --http-url https://cli.gardenio.example.com/kd

# Attach coop:
coop list --http-url https://cli.gardenio.example.com/mux

unset GARDENIO_TOKEN
```

If PiID has no cached account, omit `--silent` once and complete the normal
interactive sign-in. PiID reuses the cached MSAL account, not an access token
for another audience; it must acquire a separate Gardenio-audience token.

If you want a persistent default, add a `kd remote` entry instead of
typing `--http-url` every time:

```sh
kd remote add prod \
  --http-url https://cli.gardenio.example.com/kd
kd remote use prod
```

The remote stores the URL; the token continues to come from
`$GARDENIO_TOKEN`. The shim does not give the CLI any longer-lived credential.
It validates Entra JWTs on every request; GitLab PAT validation retains its
existing positive and negative caches.

## Audit logs

Every authn outcome and every proxied request emits one OCSF Access
Activity JSON record from `gardenio-edge-auth`. One line per event,
shape (excerpt):

```json
{
  "class": "Access Activity: Access",
  "time": "<iso8601>",
  "actor": { "user": { "email_addr": "alice@example.com" } },
  "src_endpoint": { "ip": "203.0.113.7" },
  "http_request": { "url": { "path": "/kd/v1/beads" }, "http_method": "GET" },
  "metadata": {
    "audit_id": "01J...",
    "upstream_service": "kbeads",
    "shim_version": "...",
    "endpoint": "proxy",
    "dry_run": false
  },
  "status_id": 1,
  "status_detail": "ok"
}
```

Where to find them:

| Sink | How to query |
| --- | --- |
| `stdout` of the `gardenio-edge-auth` pod | `kubectl -n <ns> logs deploy/gardenio-edge-auth -f` (in-cluster) |
| Mezmo (recommended) | filter `app:gardenio-edge-auth class:"Access Activity: Access"` |
| CloudWatch Logs | log group `gardenio-edge-auth/<env>`; filter pattern `{ $.class = "Access Activity: Access" }` |

Useful filters when investigating a specific event:

```
# Every deny in the last hour, by reason:
mezmo: app:gardenio-edge-auth status_id:2

# A specific operator (only resolvable when PAT was valid):
mezmo: app:gardenio-edge-auth actor.user.email_addr:alice@example.com

# A specific call by audit id (returned to the caller in X-Auth-Audit-Id):
mezmo: app:gardenio-edge-auth metadata.audit_id:01J...
```

The full set of `status_detail` values is defined in
`gardenio/controller/cmd/edge-auth/audit.go`:

| `status_detail` | Meaning |
| --- | --- |
| `ok` | request authenticated and proxied (or dry-run shorted) |
| `deny:missing-token` | no `Authorization: Bearer …` header on the request |
| `deny:invalid-pat` | GitLab returned 401 for the PAT (revoked / wrong-scope / typo) |
| `deny:not-in-gardenio` | PAT is valid; user is not in the configured group |
| `deny:gitlab-unavailable` | GitLab API returned non-200 / timed out — caller sees 503 |
| `deny:unknown-upstream` | path prefix did not match `/kd` or `/mux` |
| `deny:validator-not-configured` | shim started before secrets landed |
| `deny:proxy-not-configured` | upstream URL or HMAC key missing |

## Rotating your PAT

1. In GitLab, create a new PAT with the same scope (`read_api`).
2. Update wherever `GARDENIO_TOKEN` is sourced from (keychain, dotfile,
   secret manager) — do **not** edit a committed file.
3. New PAT works immediately; the shim's positive cache is keyed by
   token hash, so a new value bypasses the cache.
4. **Then** revoke the old PAT in GitLab. The shim caches positive
   results for ~5 min; the old token will stop working at the next
   re-validation (so within 5 min, regardless of GitLab cache).
5. If a token may have leaked, revoke it first and treat the gap as
   normal — the shim's negative cache (~30 s) means revoked tokens stop
   working within seconds. Audit logs (`status_detail:deny:invalid-pat`
   for the old token) will mark the cutover.

Group policy caps PAT lifetime at 90 days; calendar a rotation for ~80
days. The CLI will refuse the token only when GitLab itself rejects
it — no edge-auth-side TTL.

## Troubleshooting

### `401 Unauthorized` from the CLI

Most common. Check, in order:

1. **Token is set**: `echo "${GARDENIO_TOKEN:0:6}…"` should print
   `glpat-…`. If empty, your shell never exported it.
2. **Token is the one you think**: hit `/auth/check` (see above). If it
   returns `{"ok":true,"email":"…"}` the PAT is fine and the failing
   call is something else.
3. **PAT scope is `read_api`**: in GitLab → Settings → Access Tokens.
   `read_user` alone won't pass; `api` works but is over-scoped.
4. **You are in the gardenio group**: check
   <https://gitlab.com/example-org/gardenio/-/group_members>.
   Inherited membership counts; the shim uses `/members/all`.
5. **PAT is not expired/revoked**: GitLab will show it as revoked or
   expired in the same Access Tokens page.

The audit line will tell you which one — pull it by `X-Auth-Audit-Id`
from the failing response.

### `503 Service Unavailable`

- **`deny:gitlab-unavailable`** (in the audit line) — GitLab.com is
  down or the shim can't reach it. Re-try; if it persists, escalate
  in `#gardenio-ops` and link the audit line.
- **`/readyz` reports `gitlab validator not configured`** — the shim
  started before its config (`GITLAB_BASE_URL` + `EDGE_AUTH_GITLAB_GROUP`)
  was populated. Operator-side fix, not user-side.

### `502 Bad Gateway` from Traefik

The shim Pod is unhealthy. In-cluster traffic (agents talking to kbeads
via the cluster Service) is unaffected — only the CLI path goes through
the shim. Check `kubectl -n <ns> get deploy gardenio-edge-auth`.

### Viewer bounces to sign-in; forwardauth logs `cookie signature not valid`

Symptom (gd-C2Sl6ClxAY): with a viewer tab open, every request starts failing
with `Error loading cookied session: cookie signature not valid, removing
session` -> 401 -> `/oauth2/start`, and a comment POSTed in that window is lost.
Cause: the default oauth2-proxy `cookie` session store keeps the whole session in
~4KB cookie chunks, and two concurrent refreshes each `Set-Cookie` their own
chunks, so the browser ends up with a mix. Fix: set
`keycloak.forwardAuth.sessionStore.type: redis` (renders
`<release>-forwardauth-redis` and `--session-store-type=redis`). The cookie then
holds only a ticket. The Redis has no persistence: after a restart users silently
re-sign-in through SSO. The viewer also now shows a "session expired, your draft
is kept" banner instead of dropping a write when a sign-in lapses; that needs the
`viewer-authproxy` nginx ConfigMap (`helm/gardenio-ziti/viewer-authproxy.yaml`)
re-applied and the nginx pods restarted, since it answers scripted requests with
a 401 rather than a redirect.

### macOS `Keychain Not Found` for `__gardenio_probe__`

Older `clicreds` clients tested Keychain availability by writing and deleting a
sentinel credential named `__gardenio_probe__`. The Darwin implementation of
`go-keyring` performs writes through interactive `security -i`, so that
availability check could raise a GUI dialog even though the operator was not
storing a new credential.

The current client uses the read-only user default-Keychain lookup on macOS and
keeps the active write/delete probe only on platforms where it is needed to
distinguish a real secret service from a file fallback. Cancel an already-open
sentinel dialog and rebuild or upgrade the client; this dialog is unrelated to
Entra token validity or Ziti connectivity. A later real credential write may
still request normal Keychain interaction.

### Request "works" but returns dry-run

If responses come back with `X-Edge-Auth-Dry-Run: true` and synthetic
content, the env has `edgeAuth.dryRun=true` set as a safe-deploy stance
(ka-0000002544). Auth is being validated; the proxy is short-circuited.
This is intentional during the rollout-15 soak — track when the env
moves off dry-run via [ka-0000004915](https://beads.gardenio.e2e.example.com/beads/ka-0000004915).

### "It worked yesterday, doesn't today"

PAT may have expired (90-day cap); group membership may have been
removed; the env may be in a kill-switch window (`edgeAuth.enabled=false`
or `edgeAuth.dryRun=true`). The `/auth/check` self-test isolates the
first two from the third.

## See also

- [`gardenio/plans/stage3-ava-cli-eb-rollout.md`](../plans/stage3-ava-cli-eb-rollout.md)
  — design and rollout sequence.
- [`gardenio/docs/gb-login-runbook.md`](gb-login-runbook.md) — browser-session
  login behavior for the public AVA path; the private Ziti Entra path is
  described above.
- `gardenio/controller/cmd/edge-auth/` — service source.
- `gardenio/controller/internal/edgeauth/` — PAT validator + HMAC verify
  used by both the shim and daemons.
- [ka-0000002544](https://beads.gardenio.e2e.example.com/beads/ka-0000002544)
  — safe-deploy flags, kill switches, monitoring.
- [ka-0000004915](https://beads.gardenio.e2e.example.com/beads/ka-0000004915)
  — per-env rollout state.
