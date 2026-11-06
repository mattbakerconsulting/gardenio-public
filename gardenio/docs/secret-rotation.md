# Secret Rotation

Source of truth for what the chart rotates automatically, what it does NOT rotate,
and where rotation must be performed for every secret consumed by gardenio.

Context: audit ka-0000007475 (closed 2026-04-21) found that only the beads daemon
token is rotated by the chart's in-cluster rotator. This document records the
rotation owner for every other secret the chart consumes and the follow-up beads
that track closing the gaps.

## In-cluster rotator (gardenio-token-rotator CronJob)

Scoped to the beads daemon token only. Template:
`helm/gardenio/templates/token-rotator/{cronjob,rbac}.yaml`.

| Secret | Consumers | Schedule | Rolling restart |
|---|---|---|---|
| `kd-beads-token` (`.Values.beads.tokenSecret`) **RETIRED (gd-ke6PC7bRto): the daemon no longer accepts it; rotate once then delete, see `kd-beads-token-retirement.md` Stage 4. Do not enable `tokenRotation`.** | controller and coopmux only, until stage 3 | `.Values.tokenRotation.schedule` (default weekly Sun 03:00 UTC) | deployments in `.Values.tokenRotation.restartDeployments` (default: `-beads,-controller,-coopmux,-slack-bridge,-jira-bridge,-gitlab-bridge`) |

RBAC is scoped via `resourceNames` — the rotator's ServiceAccount can only
annotate the deployments/statefulsets it is configured to restart, and can only
read/update the specific token Secret + revoked-tokens ConfigMap it owns. It
cannot touch any other workload in the namespace.

**When the chart runs against ExternalSecrets** (production, gardenio-rwx, e2e):
disable this CronJob (`tokenRotation.enabled: false`) and rotate in the external
store — see below.

## Token rotation overlap & revocation HA

Each rotation produces three live token states: **current**, **previous**, and
**revoked**. The daemon (`kbeads`) accepts current+previous as bearer auth and
rejects revoked. Rotation is rolling (`tokenRotation.restartDeployments`), so
pods running off the old token coexist with pods running off the new token for
the duration of the rollout.

### Per-rotation flow

Order of operations in `helm/gardenio/templates/token-rotator/cronjob.yaml`:

1. Generate `NEW_TOKEN` (32-byte random, base64).
2. Read `CURRENT_TOKEN` and `PREVIOUS_TOKEN` (the latter becomes the **evicted**
   token for this rotation) from `.Values.beads.tokenSecret`.
3. **Persist** the evicted token's SHA-256 to two independent sinks:
   - HTTP `POST /v1/admin/revoke-token` on the beads daemon — writes to the
     `revoked_tokens` table (durable, survives pod restart and ConfigMap loss).
   - `revoked-tokens` ConfigMap — append-only, last-50 hashes, mounted into the
     beads deployment as `BEADS_REVOKED_TOKENS` (env-seed bootstrap).
4. Update the token Secret: `token=NEW_TOKEN`, `previous-token=CURRENT_TOKEN`.
5. Annotate consumer deployments to trigger rolling restart.

### How a revoked hash reaches a pod (defence in depth)

At pod startup the daemon hydrates the in-memory revocation cache from three
sources, in this order — any one of them suffices:

| Source | Path | Survives |
|---|---|---|
| **DB** | `revoked_tokens` table, loaded via `RevocationStore.Load(ctx)` at startup | pod restart, ConfigMap deletion, env unset |
| **env seed** | `BEADS_REVOKED_TOKENS` ConfigMap → env var → `Seed(hashes)` | DB outage at startup |
| **runtime** | `POST /v1/admin/revoke-token` after startup | n/a (additive) |

Runtime revocations through the admin endpoint always write the DB row first,
so the in-process cache never asserts "revoked" without DB durability.

### What happens if the revocation-list ConfigMap is briefly unavailable

The rotator's read-modify-write of the ConfigMap is the most fragile step (a
transient apiserver error during `kubectl get` used to silently truncate the
list to just the newest hash). Current behaviour:

- **`NotFound`** (first rotation, or operator deleted it): fresh ConfigMap
  created with the new hash. Expected.
- **Transient apiserver error** (timeout, 5xx, throttling): rotator aborts the
  ConfigMap write with a loud error. The evicted hash has *already* been
  persisted to the beads DB in step 3, so revocation enforcement is unaffected.
  Operator reruns the CronJob (or waits for next schedule) to refresh the
  ConfigMap audit log.
- **Daemon HTTP unreachable** at step 3 (DB push fails, ConfigMap write
  succeeds): rotator logs `WARN` and continues. The ConfigMap env-seed will
  carry the revocation to the next pod restart; DB is repopulated by the env
  seed on first daemon boot.

A simultaneous DB + ConfigMap outage at pod startup (both sources empty) means
the revocation list is briefly empty for newly-started pods. Already-running
pods keep the in-memory cache. **The defence here is that the rotated-out token
is also missing from `BEADS_AUTH_TOKEN`/`BEADS_PREVIOUS_TOKEN` after rotation,
so it fails primary auth anyway** — the revocation list is defence-in-depth
against rotation overlap leaks, not the only barrier.

### Operator runbook entries

- **Verify a hash made it to the DB**: `SELECT token_hash, revoked_by, reason
  FROM revoked_tokens ORDER BY created_at DESC LIMIT 5;` against the beads
  postgres.
- **Inspect ConfigMap state**: `kubectl get cm <release>-revoked-tokens -o
  jsonpath='{.data}'`.
- **Force-revoke a leaked token outside the rotation cadence**: call
  `POST /v1/admin/revoke-token` with `Authorization: Bearer <admin-token>` and
  body `{"token_hash":"<sha256-hex>","reason":"..."}` — persists to DB
  immediately, no rotation required.

## Secrets NOT rotated by the chart

Every secret in this table MUST be rotated externally. The chart does not cycle
them. If the external rotation is missing or silently breaks, the credential is
effectively static and the audit finding is unresolved.

| Secret | Source | Rotation owner | Follow-up bead |
|---|---|---|---|
| Slack bot token / signing secret / app token | `.Values.slackBridge.secrets.*` → ExternalSecret or raw values | Slack app owner + AWS Secrets Manager rotation | ka-0000003046 |
| JIRA PAT + webhook secret | `.Values.jiraBridge.secrets.*` → ExternalSecret | Atlassian admin + AWS Secrets Manager rotation | ka-0000007052 |
| GitLab PAT + webhook secret | `.Values.gitlabBridge.secrets.*` → ExternalSecret | GitLab service account owner + AWS SM rotation | ka-0000002121 |
| Postgres password | `{release}-postgres-password` ExternalSecret | AWS Secrets Manager rotation Lambda (RDS) or manual | ka-0000004147 |
| Keycloak admin password | `tenant-system/keycloak-secrets` | Infra team (tenant-system chart) | out of scope (separate chart) |
| Coopmux bearer token | `.Values.coopmux.bearerTokenSecret` → ExternalSecret | AWS Secrets Manager rotation | ka-0000002717 |
| LiteLLM master key | `litellm-master-key` ExternalSecret | LiteLLM admin + AWS SM | ka-0000002717 |
| Forwardauth client/cookie secrets | ExternalSecret (oauth2-proxy) | Infra team | out of scope |
| GitLab IdP credentials (Keycloak) | `gardenio-gitlab-idp-credentials` ExternalSecret | Infra team | out of scope |

## Expected rotation cadence

These are the targets the security audit set — encode them in the AWS Secrets
Manager rotation schedule (or equivalent) for each secret that is in scope:

| Class | Target cadence | Notes |
|---|---|---|
| Signing secrets (Slack signing, webhook HMAC) | 90 days | Rotating invalidates in-flight webhook retries; schedule during low-traffic window. |
| OAuth bot tokens (Slack bot/app, GitLab PAT, JIRA PAT) | 180 days | Requires updating the external provider before the new secret lands in-cluster. |
| Database passwords (Postgres) | 30 days | RDS rotation Lambda; controller + beads restart on rollout. |
| Service-to-service bearer tokens (coopmux, LiteLLM master key) | 30 days | Restart every client after the secret rotates. |
| In-cluster daemon token (beads) | 7 days | Handled by the in-cluster rotator above. |

## Adding rotation for a new provider

Two supported paths:

1. **External rotation (preferred).** Configure AWS Secrets Manager rotation for
   the backing secret, then rely on ExternalSecrets `refreshInterval` to pull the
   new value into the cluster. Add an `annotations` hash on the consuming
   Deployment that derives from the Secret's `resourceVersion`, so a rolling
   restart fires when the Secret changes. Track the rotation schedule in the
   table above.

2. **In-cluster rotation (like beads token).** Add a sibling CronJob under
   `helm/gardenio/templates/token-rotator/`. Reuse the RBAC pattern in
   `rbac.yaml`: grant `create` namespace-wide and `get/update/patch` scoped to
   the specific secret names. Grant `patch` on `deployments`/`statefulsets` only
   for the named consumers. Do NOT reuse the beads-token ServiceAccount — a new
   rotator gets its own SA + Role, so the blast radius of each rotator stays
   minimal.

Whichever path you take, update this document in the same MR so the rotation
owner for every secret the chart consumes stays easy to enumerate.
