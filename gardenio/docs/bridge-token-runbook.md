# bridge token operator runbook

> **Formerly "bridge fleet token".** Renamed fleet→bridge per decision
> [ka-0000000201](https://beads.gardenio.e2e.example.com/beads/ka-0000000201)
> = DUAL-ACCEPT (ka-0000005788). The noun is now **bridge token**, but this
> is a noun-only rename: the daemon audit stamp stays `fleet:<bridge>`, and
> the legacy `/v1/fleet-tokens` route, `fleet` wire field, `fleet_token:*`
> config rows, and `kd fleet token` CLI alias all keep working through the
> dual-accept compat window (removal = cleanup bead ka-0000004906). This is
> the bridge **credential** surface only — unrelated to the tenancy `fleet`
> concept (fleet DB column, fleet maps/metrics, `kd --fleet` scoping).

Operator-facing guide to the **bridge token** lifecycle: issue, rotate,
revoke, and audit the per-bridge bearer tokens that `slack-bridge`,
`jira-bridge`, and `gitlab-bridge` use to authenticate to the beads daemon.

Sibling docs: [`per-agent-tokens-enablement-plan.md`](per-agent-tokens-enablement-plan.md)
(the per-agent equivalent for `agents.perAgentTokens=true`),
[`edge-auth-operator-runbook.md`](edge-auth-operator-runbook.md) (CLI / PAT
edge), [`secret-rotation.md`](secret-rotation.md) (rotation HA + revocation
mechanics).

Parent epic: [ka-0000003846](https://beads.gardenio.e2e.example.com/beads/ka-0000003846).
Related decision: [ka-0000001054](https://beads.gardenio.e2e.example.com/beads/ka-0000001054).

## Wire compat: bridge-named API (ka-0000000310)

The bridge-credential API noun is being renamed fleet→bridge. During the
dual-accept compat window **both** forms work, with bridge-named as canonical:

| Surface | Canonical | Legacy alias (still accepted) |
|---|---|---|
| Register | `POST /v1/bridge-tokens` | `POST /v1/fleet-tokens` |
| List | `GET /v1/bridge-tokens` | `GET /v1/fleet-tokens` |
| Revoke | `DELETE /v1/bridge-tokens/{bridge}` | `DELETE /v1/fleet-tokens/{fleet}` |
| Register field | `{"bridge":"slack-bridge"}` | `{"fleet":"slack-bridge"}` (`bridge` wins if both set) |
| Response / list fields | `bridge` / `bridges` | `fleet` / `fleets` (mirrored values) |
| Persisted config key | `bridge_token:<name>` | `fleet_token:<name>` (dual-READ fallback) |

New writes persist under `bridge_token:*`; reads prefer it and fall back to a
legacy `fleet_token:*` row when no canonical row exists (no eager backfill —
legacy rows migrate one-shot, tracked in ka-0000004906). The identity value
(e.g. `slack-bridge`) and the `bead.fleet` audit stamp are **unchanged** — only
the API noun differs. The examples below still use the legacy noun; they remain
valid for the compat window. The fleet-named aliases are removed in the cleanup
bead (ka-0000004906).

> **Service identities need the token-mint capability (gd-ua4DGNtZdG).** The
> primary static token is held by every agent pod, so `POST /v1/bridge-tokens`
> with it refuses `privileged:true`, any subject or bridge name containing `:` or
> `@` (`bridge:slack`, an email), and the reserved service names (`slack-bridge`,
> `jira-bridge`, `gitlab-bridge`, `acme-scif`, `controller`, ...) with 403. The
> controller and the rotator mint those through the `gardenio-machine-token-mint`
> machine principal. Hand-minting a bridge token with `$ADMIN_TOKEN` as in the
> examples below now works only for ordinary fleet names and plain subjects; a
> manual bridge mint needs a bearer for a machine client holding that role.

## TL;DR

```sh
# Bearer token for /v1/bridge-tokens (admin-only endpoint; /v1/fleet-tokens is
# a dual-accept alias during the compat window — ka-0000000310).
export ADMIN_TOKEN="$(kubectl -n <ns> get secret <beads-token-secret> -o jsonpath='{.data.token}' | base64 -d)"
export BEADS="http://gardenio-beads.<ns>.svc.cluster.local:8080"

# 1. Issue a fresh slack-bridge token (720h access, with refresh).
NEW=$(openssl rand -hex 32)
curl -sS -X POST "$BEADS/v1/fleet-tokens" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"token\":\"$NEW\",\"fleet\":\"slack-bridge\",\"subject\":\"bridge:slack\",\"ttl\":\"720h\",\"with_refresh\":true}"

# 2. Patch the per-bridge secret + restart the bridge.
kubectl -n <ns> patch secret <bridgeTokenSecret> \
  --type merge -p "{\"stringData\":{\"slack-bridge\":\"$NEW\"}}"
kubectl -n <ns> rollout restart deploy/gardenio-slack-bridge
kubectl -n <ns> rollout status  deploy/gardenio-slack-bridge --timeout=2m

# 3. Revoke the old token.
curl -sS -X DELETE "$BEADS/v1/fleet-tokens/slack-bridge" -H "Authorization: Bearer $ADMIN_TOKEN"
```

## 1. What and why

A **bridge token** is a bearer token registered with the beads daemon under
a *bridge name* (e.g. `slack-bridge`, `jira-bridge`, `gitlab-bridge`) and an
optional *subject* (`bridge:slack`). When the daemon authenticates a write
using a bridge token, the audit pipeline stamps `operated_by =
"bridge:<name>"` on the resulting bead / comment / dep / event, instead of
the opaque `static` attribution used by the primary admin token.

> **Wire compat note.** On the wire, the bridge name is still carried in the
> `fleet` JSON field and persisted as a `fleet_token:<name>` config row, and
> the daemon stamps the `fleet:<bridge>` audit value — none of that changed in
> the rename (it is a noun-only API rename). The canonical route is
> `/v1/bridge-tokens`; `/v1/fleet-tokens` remains an accepted alias during the
> compat window. Examples below still use `/v1/fleet-tokens` + `"fleet"` so
> they work against any daemon version in the fleet.

Pre-bridge-token, every bridge shared the daemon's static admin token —
writes were indistinguishable in the audit trail, and the `operated_by`
resolver fell back to legacy attribution that increasingly triggers
`operator could not be resolved` 400s on the post-cutover write path.

Bridges authenticate exactly the way the static path does: the daemon
reads `Authorization: Bearer …` and resolves the token through
`FleetConfig` (`kbeads/internal/server/fleet.go`). The only difference is
that the bridge's secret carries a bridge-scoped value instead of the
primary admin token. The bridge token grants the same project-scoped
write privileges; it is **not** an admin token and cannot call
`/v1/bridge-tokens` (`/v1/fleet-tokens`) or `/v1/admin/*`.

**Scope of this runbook.** Bridges with per-bridge tokens:
`slack-bridge`, `jira-bridge`, `gitlab-bridge` (all gated on
`.Values.beads.bridgeTokenSecret`, formerly `bridgeFleetTokenSecret`).
Per-agent tokens
(`agents.perAgentTokens=true`) are a separate, deferred enablement; see
[`per-agent-tokens-enablement-plan.md`](per-agent-tokens-enablement-plan.md).

### Privileged service bridges (ownership bypass)

By default a bridge token is subject to the per-bead **ownership gate**
(`kbeads/internal/server/write_authz.go:authorizeOwnership`): only a bead's
`created_by` / `assignee` / `owner` (or an admin-OIDC / primary-static
caller) may mutate it. That is correct for agent and most bridge writes, but
it breaks the **gitlab-bridge**, which performs *system-managed graph writes*
across beads it does not own:

- `AddDependency` for `produces` / `triggered-by` edges (routed through
  `loadBeadForWrite` with `action=update`), and
- `UpdateBeadFields` to **back-fill `mr_url`** onto the originating bead.

Before ka-0000001946 these returned **HTTP 403 ownership denied** on every
cross-owner bead, so the produces/triggered-by graph and MR pills silently
went missing in beads-viewer.

A token registered with `privileged: true` (CLI `--privileged`) bypasses
**only** the ownership gate. Type/project write-policy and token
type/op scopes still apply, and the privilege is snapshotted onto any
companion refresh token so it survives access-token rotation. Reserve it for
bridges that genuinely do cross-owner system-managed writes — currently just
`gitlab-bridge`. `slack-bridge` and `jira-bridge` stay ordinary.

In Helm this is driven by `agents.bridgeTokenRotation.privilegedFleets`
(value key unchanged for compat; default `[gitlab-bridge]`), which the
controller bridge-token rotator passes through when it mints each bridge's
token.

## 2. Issue a new bridge token

Token registration is **admin-only**: the caller must present the
daemon's primary auth token (the value stored in
`.Values.beads.tokenSecret`).

### Preferred path — `kd bridge token register`

Shipped in [ka-0000000937](https://beads.gardenio.e2e.example.com/beads/ka-0000000937)
(as `kd fleet token`); renamed to `kd bridge token` in ka-0000005788. The
deprecated `kd fleet token register` alias still works through the compat
window. Requires a `kd` binary built **on or after** the rename — older
agent images expose only `kd fleet token register`. Verify with
`kd bridge token --help`; if you see "unknown command", use `kd fleet token
register` or fall back to the curl path below.

```sh
# Generate a random token and register it for the slack-bridge bridge,
# 720h access TTL, with a companion refresh token, subject claim
# bridge:slack. --output token prints just the access token to stdout
# (refresh token goes to stderr labelled).
ACCESS=$(kd bridge token register \
  --bridge slack-bridge \
  --subject bridge:slack \
  --ttl 720h \
  --with-refresh \
  --output token \
  --http-url "$BEADS" \
  --token "$ADMIN_TOKEN")
```

Useful flags:

| Flag | Purpose |
|---|---|
| `--bridge <name>` | Bridge name. Required. (Deprecated alias: `--fleet`.) |
| `--token-value <hex>` | Use this token instead of a random one. Default: 64-hex `crypto/rand`. |
| `--subject <s>` | Audit subject (`bridge:slack`). Falls back to bridge name if empty. |
| `--ttl <dur>` | Access-token TTL (Go duration, e.g. `720h`). Mutually exclusive with `--expires-at`. |
| `--expires-at <rfc3339>` | Absolute expiry. |
| `--with-refresh` | Also mint a refresh token. Refresh TTL defaults to 30d. |
| `--refresh-ttl <dur>` | Override refresh-token TTL. Requires `--with-refresh`. |
| `--project <p>` | Restrict token to a single project label. |
| `--allowed-types <list>` | CSV of bead types the token may write. |
| `--allowed-ops <list>` | CSV of operations the token may perform. |
| `--output token|json` | `token` → just the access token to stdout. `json` → full response on stdout (includes token, refresh token, expiries). |
| `--privileged` | Mark as a privileged service-bridge identity that bypasses per-bead ownership for system-managed graph writes (ka-0000001946). Use only for `gitlab-bridge` (produces/triggered-by deps + `mr_url` back-fill). |

### Fallback path — raw HTTP

When the CLI is not available (older agent image, laptop without `kd`),
POST to `/v1/fleet-tokens` directly (the canonical route is
`/v1/bridge-tokens` accepting a `"bridge"` field; the `fleet`-named forms
below are the compat aliases and work against every daemon version):

```sh
NEW=$(openssl rand -hex 32)
curl -sS -X POST "$BEADS/v1/fleet-tokens" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$(cat <<EOF
{
  "token": "$NEW",
  "fleet": "slack-bridge",
  "subject": "bridge:slack",
  "ttl": "720h",
  "with_refresh": true
}
EOF
)"
```

Successful response (HTTP 201):

```json
{
  "fleet": "slack-bridge",
  "subject": "bridge:slack",
  "expires_at": "2026-06-23T17:00:00Z",
  "refresh_token": "rft_…",
  "refresh_token_expires_at": "2026-06-23T17:00:00Z"
}
```

Verify the registration landed:

```sh
curl -sS "$BEADS/v1/fleet-tokens" -H "Authorization: Bearer $ADMIN_TOKEN"
# -> {"fleets":["gitlab-bridge","jira-bridge","slack-bridge"]}
```

The daemon persists the mapping to its `config` store as a
`bridge_token:<name>` row (dual-read: new writes use `bridge_token:*`, with
fallback to legacy `fleet_token:*`) so registrations survive daemon
restarts.

## 3. Rotate a token (manual)

The standard rotation path is the `token-rotator` CronJob
(`gardenio/helm/gardenio/templates/token-rotator/cronjob.yaml`), which
rotates the **primary admin token** weekly and triggers rolling restarts
on a configured deployment list. The CronJob does **not** currently rotate
per-bridge tokens. Use this section for emergency / out-of-band
rotation of a single bridge token, or when the cron is paused.

**Prerequisites.** You need:

- Cluster + namespace access (`kubectl -n <ns>`).
- The daemon's admin token (read from `.Values.beads.tokenSecret`,
  typically `kd-beads-token` key `token`).
- The bridge-token secret name from `.Values.beads.bridgeTokenSecret`
  (formerly `bridgeFleetTokenSecret`; deploy-specific, frequently
  `bridge-tokens` or the legacy `bridge-fleet-tokens`).

### Step-by-step

Replace `<ns>`, `<bridge>` (`slack-bridge` | `jira-bridge` |
`gitlab-bridge`), `<token-secret>`, and `<bridge-token-secret>` with the
values for your environment.

```sh
NS=<ns>
BRIDGE=<bridge>
TOKEN_SECRET=<token-secret>             # e.g. kd-beads-token
BRIDGE_SECRET=<bridge-token-secret>     # e.g. bridge-tokens (legacy: bridge-fleet-tokens)

ADMIN_TOKEN=$(kubectl -n "$NS" get secret "$TOKEN_SECRET" \
  -o jsonpath='{.data.token}' | base64 -d)
BEADS="http://gardenio-beads.$NS.svc.cluster.local:8080"
NEW=$(openssl rand -hex 32)
```

1. **Issue the replacement token** (see §2 — preferred or fallback path).
   Use a TTL appropriate for the fleet (default deploy assumes 720h
   for bridges).

   ```sh
   curl -sS -X POST "$BEADS/v1/fleet-tokens" \
     -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
     -d "{\"token\":\"$NEW\",\"fleet\":\"$BRIDGE\",\"subject\":\"bridge:${BRIDGE%-bridge}\",\"ttl\":\"720h\"}"
   ```

   At this point both the OLD and the NEW token are registered on the
   daemon — the bridge is still using the OLD token, and the daemon
   accepts both. There is no auth gap.

2. **Patch the per-bridge K8s secret.** The Helm chart wires
   `BEADS_DAEMON_TOKEN` from the secret key matching the bridge name
   (`slack-bridge`, `jira-bridge`, `gitlab-bridge`):

   ```sh
   kubectl -n "$NS" patch secret "$BRIDGE_SECRET" \
     --type merge -p "{\"stringData\":{\"$BRIDGE\":\"$NEW\"}}"
   ```

   Also refresh the `fleet-map` key (key name unchanged — it matches the
   daemon `BEADS_FLEET_MAP` env contract) so daemon restarts re-seed the
   correct mapping (see `bridgeTokenSecret` docs in `values.yaml`):

   ```sh
   # Read every per-bridge key, build a CSV of <token>:<fleet> entries.
   FLEET_MAP=""
   for B in slack-bridge jira-bridge gitlab-bridge; do
     T=$(kubectl -n "$NS" get secret "$BRIDGE_SECRET" \
       -o jsonpath="{.data.$B}" 2>/dev/null | base64 -d)
     [ -n "$T" ] && FLEET_MAP="${FLEET_MAP}${FLEET_MAP:+,}${T}:${B}"
   done
   kubectl -n "$NS" patch secret "$BRIDGE_SECRET" \
     --type merge -p "{\"stringData\":{\"fleet-map\":\"$FLEET_MAP\"}}"
   ```

3. **Rolling restart the bridge** so the pod picks up the new
   `BEADS_DAEMON_TOKEN` env value. Secrets are mounted via
   `secretKeyRef`, so the pod must restart for the env to update.

   ```sh
   kubectl -n "$NS" rollout restart deploy/gardenio-"$BRIDGE"
   kubectl -n "$NS" rollout status  deploy/gardenio-"$BRIDGE" --timeout=2m
   ```

4. **Verify the bridge is healthy.** Tail logs and confirm normal write
   traffic — no `401 invalid token`, no `operator could not be resolved`
   (see §6).

   ```sh
   kubectl -n "$NS" logs deploy/gardenio-"$BRIDGE" --tail=200 -f
   ```

5. **Revoke the old token** (§4). Both registrations exist on the
   daemon until you actively remove the old one; leaving it registered
   defeats the purpose of rotating.

### Rollback

If step 3 leaves the bridge crashlooping or failing auth:

```sh
# Put the OLD token back into the secret.
OLD=$(kubectl -n "$NS" get secret "$BRIDGE_SECRET" \
  -o jsonpath="{.data.$BRIDGE-previous}" 2>/dev/null | base64 -d)
kubectl -n "$NS" patch secret "$BRIDGE_SECRET" \
  --type merge -p "{\"stringData\":{\"$BRIDGE\":\"$OLD\"}}"
kubectl -n "$NS" rollout restart deploy/gardenio-"$BRIDGE"
```

Then revoke the new (failed) token via §4 and investigate. If you didn't
stash the old value in `<bridge>-previous` before patching, you can
issue *another* fresh token via §2 and restart — there's no concept of
"current vs previous" on the bridge-token map; both are equally valid
until revoked.

## 4. Revoke a token

Two endpoints, two different things (the canonical revoke route is
`DELETE /v1/bridge-tokens/{bridge}`; `/v1/fleet-tokens/{fleet}` below is the
compat alias and behaves identically):

- `DELETE /v1/fleet-tokens/{fleet}` — removes the bridge→token mapping
  from in-memory `FleetConfig` AND deletes the `fleet_token:<bridge>`
  config row (dual-deletes the new `bridge_token:<bridge>` row too). The
  token still hashes to the same value, but the daemon no longer
  recognises it and serves `401 invalid token`. Idempotent — 404 is
  treated as success by the controller client. **Use this for routine
  revocation.**
- `POST /v1/admin/revoke-token` — pushes the token hash onto the
  daemon's persistent revocation list (DB + in-process cache + ConfigMap
  fallback) so even a daemon restart that re-seeded the old mapping
  would still reject the token. **Use this for incident response
  (suspected leak).**

### Routine revocation (e.g. completing a rotation)

```sh
curl -sS -X DELETE "$BEADS/v1/fleet-tokens/$BRIDGE" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -w 'HTTP %{http_code}\n'
# Expect HTTP 204 (or HTTP 404 if already revoked).
```

After this call, `GET /v1/fleet-tokens` no longer lists `$BRIDGE`. Any
request authenticated with the old token gets `401 invalid token` on
the next call (no positive cache to flush — the resolver reads from
`FleetConfig.tokenFleets` directly).

### Leaked-token incident response

The bridge token has full project-scoped write access to the daemon.
Treat any suspected exposure (paste in Slack, accidental log, git
commit, screen share) as compromise. Time matters more than tidiness:

1. **Revoke first, ask questions later.** Belt-and-braces both
   endpoints so the token is unusable regardless of which fallback the
   daemon hits:

   ```sh
   # Belt: drop the fleet mapping.
   curl -sS -X DELETE "$BEADS/v1/fleet-tokens/$BRIDGE" \
     -H "Authorization: Bearer $ADMIN_TOKEN"

   # Braces: persist the revocation by hash (DB + cache + ConfigMap).
   LEAKED_HASH=$(printf '%s' "$LEAKED_TOKEN" | sha256sum | cut -d' ' -f1)
   curl -sS -X POST "$BEADS/v1/admin/revoke-token" \
     -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
     -d "{\"token_hash\":\"$LEAKED_HASH\",\"reason\":\"leaked $(date -u +%FT%TZ)\"}"
   ```

   If you do not have the leaked plaintext (e.g. someone reported a
   screenshot but did not share it), read the current value out of the
   secret first:

   ```sh
   LEAKED_TOKEN=$(kubectl -n "$NS" get secret "$BRIDGE_SECRET" \
     -o jsonpath="{.data.$BRIDGE}" | base64 -d)
   ```

2. **Issue + roll in a fresh token** following §2 + §3 steps 1–4. The
   bridge will be down between rotating the secret and the rollout
   landing — usually < 60s.

3. **Audit the blast radius.** Pull every bead mutated by the leaked
   subject in the exposure window (see §5).

4. **File a security bead** with the timeline, leaked-hash prefix
   (`sha256(leaked_token)[:12]`), kubectl output of the new mapping,
   and a link back to this runbook. Use `kd create … --type bug` and
   block the rotation epic on it.

### Why not just delete the secret?

Deleting `$BRIDGE_SECRET` would crash all three bridges (env-var
`secretKeyRef` lookup fails → pod startup fails). The
`DELETE /v1/fleet-tokens/{fleet}` + `revoke-token` combo invalidates the
bearer without disrupting the deploy.

### Direct DB fallback (daemon HTTP unavailable)

If the daemon HTTP API is down and you absolutely must revoke a token,
delete the persisted row and bounce the daemon so it doesn't re-seed
the mapping at startup:

```sh
# Delete both the new and the legacy config-row key (dual-write window).
kubectl -n "$NS" exec -it deploy/gardenio-postgres -- psql -U postgres -d beads \
  -c "DELETE FROM config WHERE key IN ('bridge_token:$BRIDGE', 'fleet_token:$BRIDGE');"
kubectl -n "$NS" rollout restart statefulset/gardenio-beads
```

This is intentionally awkward — `DELETE /v1/bridge-tokens/{bridge}`
(alias `/v1/fleet-tokens/{fleet}`) is the supported path and handles
cache invalidation atomically.

## 5. Audit: who wrote this bead?

Bridge tokens stamp `operated_by = "<subject>"` on every write
(`bridge:slack`, `bridge:jira`, `bridge:gitlab`) and resolve
`operated_by_id` against the operator alias table when one exists. The
append-only mutation history is the canonical record.

### CLI walkthrough

```sh
kd audit bead ka-0000001713 --json | jq '.[] | {created_at, actor, mutation_type, field}'
```

`actor` is the resolved operator string for the mutation. For a write
driven by `bridge:slack` you should see `actor: "bridge:slack"` (or
`actor: "slack-bridge"` if the token was registered without a `subject`
— pre-cutover back-compat).

Without `--json` the same data renders one line per mutation, prefixed
with timestamp and actor:

```
[2026-05-24 17:14:02] bridge:slack created subject: "" -> "Bug from #ops"
[2026-05-24 17:14:02] bridge:slack created status:  "" -> "open"
[2026-05-24 17:18:50] slack:U0OPERATOR1 updated status: "open" -> "in_progress"
```

The first two rows are the bridge's own writes; the third is a human
operator (`slack:U…`) acting through the bridge. The bridge sends
`X-Operated-By: slack:U…` for human-driven mutations so the audit row
carries the human identity even though the bearer token is the
bridge's.

### SQL walkthrough (when CLI is unavailable)

```sql
-- All mutations for a bead, newest first.
SELECT created_at, actor, mutation_type, field, old_value, new_value
  FROM mutation_history
 WHERE bead_id = 'ka-0000001713'
 ORDER BY created_at DESC;

-- All writes by a bridge subject in the last hour.
SELECT bead_id, mutation_type, field, created_at
  FROM mutation_history
 WHERE actor = 'bridge:slack'
   AND created_at > now() - interval '1 hour'
 ORDER BY created_at DESC;

-- Live-state attribution on the beads table itself.
SELECT id, operated_by, operated_by_id, updated_at
  FROM beads
 WHERE id = 'ka-0000001713';
```

`operated_by` is the canonical subject string; `operated_by_id` is the
FK into the `operators` alias table (populated by the resolver chain
documented in `kbeads/internal/server/audit.go`). A NULL `operated_by_id`
on a recent row is a smell — see the §6 entry for
`operator could not be resolved`.

### Reading the subject claim from the token

The subject is what the bridge token carries; it's not stored on the
token row in a queryable shape. Inspect the daemon's in-memory mapping
indirectly:

```sh
# Lists bridges (never exposes token values). Emits {"bridges":[...]} and,
# during the compat window, the mirrored {"fleets":[...]}.
curl -sS "$BEADS/v1/bridge-tokens" -H "Authorization: Bearer $ADMIN_TOKEN"
```

The subject is whatever you set with `--subject` / `"subject"` at
registration time. The persisted config row carries the same value (new
writes use the `bridge_token:` prefix; legacy `fleet_token:` rows are still
read):

```sql
SELECT key, value FROM config WHERE key LIKE 'bridge_token:%' OR key LIKE 'fleet_token:%';
-- key = 'bridge_token:slack-bridge'
-- value = '{"token":"<plaintext>","subject":"bridge:slack","expires_at":"..."}'
```

`value` includes the plaintext token — treat psql output as sensitive.

## 6. Common errors

### `400 operator could not be resolved`

The daemon couldn't resolve the operator for a write. The error body
carries a structured `resolver_source` discriminator
(`header_missing`, `header_non_human`, `fallback_agent_only`,
`chain_root_emailless`) that tells you which fallback failed.

| `resolver_source` | What it means | Fix |
|---|---|---|
| `header_missing` | The caller did not send `X-Operated-By`, and the bearer's Subject (= fleet/agent identity) could not be resolved into a human operator. | Bridges should send `X-Operated-By: slack:U…` (or `jira:…`, `gitlab:…`) for every human-driven write. Inspect the offending request in bridge logs and confirm the header is set. |
| `header_non_human` | The header was present but had a non-human shape (`gitlab:`, `jira:`, `system:*`). | Pre-cutover legacy path; should not occur post bridge token rollout. Filter the header at the bridge before sending. |
| `fallback_agent_only` | The chain only had an agent identity, no human. | Bridge should propagate the originating Slack / Jira / GitLab user as `X-Operated-By`. |
| `chain_root_emailless` | Operator chain root has no email alias. | Add the missing alias via `kd admin operator-aliases link`. |

This error is **structured**: clients see HTTP 400 with body
`{"code":"operator_refusal","resolver_source":"…"}`. The `kd` CLI exits
64 (`EX_USAGE`) on this code. Source: `kbeads/internal/server/server.go`
(`operatorRefusalMessage`).

### `401 invalid token`

The bearer token is not in `FleetConfig.tokenFleets` AND is not the
primary admin token. Three causes:

- **TTL expired** — `FleetConfig.IsExpired(token)` returns true. Re-issue
  with a fresh TTL (§2) and roll the bridge (§3 step 2 onward).
- **Token was revoked** — someone hit `DELETE /v1/bridge-tokens/…`
  (alias `/v1/fleet-tokens/…`) or `POST /v1/admin/revoke-token` for this
  token's hash. Confirm with `GET /v1/bridge-tokens` (bridge missing from
  the list) or by searching audit logs for `revoke_token` actions.
- **Refresh failed** — bridges currently do not auto-refresh; if a
  refresh-rotation path is added and fails, the access token stops
  working at `expires_at`. Re-issue manually.

Source: `kbeads/internal/server/interceptors.go:766` (HTTP) /
`:371` (gRPC).

### `403 fleet token management requires admin token`

(Daemon-emitted error string — unchanged through the compat window.) You
called `/v1/bridge-tokens` (`/v1/fleet-tokens`) or `/v1/admin/*` with a
bridge token instead of the primary admin token. Bridge tokens are
intentionally not admin. Re-source `$ADMIN_TOKEN` from
`.Values.beads.tokenSecret`.

### `no project matched channel`

Daemon-side scoping bug introduced in MR !2902, fixed in MR !2989. If
you see it post-2989 on a redeployed cluster, the deploy hasn't picked
up the fix — check the running `kbeads` image tag and roll forward.

### `503 refresh-token manager not configured`

`with_refresh: true` was requested but the daemon was started without a
`RefreshTokenManager`. Either drop `--with-refresh` or fix the daemon
wiring (`SetRefreshTokenManager`).

## 7. Where the source lives

| Concern | File |
|---|---|
| In-memory token→bridge map, resolver, expiry | `kbeads/internal/server/fleet.go` (`FleetConfig`) |
| `POST/GET/DELETE /v1/bridge-tokens` (+ `/v1/fleet-tokens` alias) handlers | `kbeads/internal/server/http_fleet_tokens.go` |
| Route registration | `kbeads/internal/server/http.go` (`/v1/bridge-tokens` + `/v1/fleet-tokens` lines) |
| Token-hash revocation (DB + cache) | `kbeads/internal/server/http_revoke_token.go` |
| Operator-resolution audit pipeline | `kbeads/internal/server/audit.go`, `server.go` (`operatorRefusalMessage`) |
| Controller-side HTTP client | `gardenio/controller/internal/beadsapi/client.go` (bridge-token register/revoke methods, renamed in ka-0000001857) |
| `kd bridge token register` CLI (+ deprecated `kd fleet token` alias) | `kbeads/cmd/kd/bridge_token.go`, `kbeads/cmd/kd/fleet_token.go` |
| Bridge-token secret-name helper (value-alias fallback) | `gardenio/helm/gardenio/templates/_helpers.tpl` (`gardenio.beads.bridgeTokenSecretName`) |
| Per-bridge secret wiring | `gardenio/helm/gardenio/templates/{slack,jira,gitlab}-bridge/deployment.yaml` |
| Helm values (`bridgeTokenSecret`, legacy `bridgeFleetTokenSecret`) | `gardenio/helm/gardenio/values.yaml` |
| Primary token rotation (weekly cron, not bridge-aware) | `gardenio/helm/gardenio/templates/token-rotator/cronjob.yaml` |
| Bridge-token admission policy (VAP) | `gardenio/helm/gardenio/templates/controller/bridge-token-admission-policy.yaml` |

## 8. Defense-in-depth: bridge-token admission policy

A `ValidatingAdmissionPolicy` (`{release}-agents-bridge-token-guard`) blocks
any `CREATE`/`UPDATE` of the `{release}-{gitlab,jira,slack}-bridge` deployments
that would set `BEADS_DAEMON_TOKEN.valueFrom.secretKeyRef.name` to the legacy
`kd-beads-token` secret. Background:
[ka-0000007278](https://beads.gardenio.e2e.example.com/beads/ka-0000007278)
— on 2026-05-25T17:46:58Z an unidentified `kubectl patch`
(manager=`kubectl-patch`) reverted the env on all three bridges from the
bridge secret back to the legacy shared secret, breaking operator resolution
until manual remediation.

Off by default. Enable in prod values once
`beads.bridgeTokenSecret` (or the legacy `beads.bridgeFleetTokenSecret`
alias) is set:

```yaml
agents:
  bridgeTokenAdmissionPolicy:
    enabled: true
```

Compatible with `helm upgrade` — the chart renders the bridge deployments
referencing the bridge secret when a bridge token secret is set, so
admission passes. Only out-of-band patches that would put the legacy
reference BACK into the live spec are blocked. Requires Kubernetes 1.30+
(VAP GA).

## See also

- [`per-agent-tokens-enablement-plan.md`](per-agent-tokens-enablement-plan.md)
  — the per-agent equivalent (`agents.perAgentTokens=true`), currently
  deferred.
- [`edge-auth-operator-runbook.md`](edge-auth-operator-runbook.md) — the
  CLI / GitLab-PAT edge in front of the daemon.
- [`secret-rotation.md`](secret-rotation.md) — primary admin-token
  rotation, ConfigMap+DB revocation HA model.
- [ka-0000003846](https://beads.gardenio.e2e.example.com/beads/ka-0000003846)
  — parent epic for bridge tokens.
- [ka-0000001054](https://beads.gardenio.e2e.example.com/beads/ka-0000001054)
  — decision: use bridge tokens for bridge attribution.
- [ka-0000000937](https://beads.gardenio.e2e.example.com/beads/ka-0000000937)
  — original `kd fleet token register` CLI subcommand (renamed to
  `kd bridge token register` in ka-0000005788).
- [ka-0000000201](https://beads.gardenio.e2e.example.com/beads/ka-0000000201)
  — decision: fleet→bridge token rename, dual-accept compat window.
