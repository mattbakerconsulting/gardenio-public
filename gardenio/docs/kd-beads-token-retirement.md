# Retiring the shared beads token (`kd-beads-token`) from every holder but kbeads

Task gd-5QYwXtV7IA, parent phase gd-VkxF0GYXkq. #351 (gd-kLRIbndGNp) took the token out of agent pods
and split the coopmux agent token. This page tracks every other holder. Names only: no token value
belongs in this file, a bead, a log or a commit.

## Holder inventory (k3s overlay render, `helm template -f deploy-values/gardenio-k3s.yaml`)

Re-run the scan after every roll; the pass condition is the last row.

| Holder | Env var | State after this change |
|---|---|---|
| gardenio-trigger-engine | `BEADS_DAEMON_TOKEN` | **Gone** when `triggerEngine.machinePrincipal.enabled` (it is, on k3s). Every call and the SSE stream use `machine:gardenio-trigger-engine`. |
| gardenio-trigger-engine | `NATS_TOKEN` (was `kd-beads-token`) | **Gone.** Own secret via `nats.auth.clientTokenSecret`, else none. |
| gardenio-grafana-bridge | `BEADS_DAEMON_TOKEN` | **Gone** when `grafanaBridge.machinePrincipal.enabled` (it is). SSE uses the machine token. |
| gardenio-github-bridge | `BEADS_DAEMON_TOKEN` | **Gone** when `githubBridge.machinePrincipal.enabled` (it is, on k3s; gd-tCMXlS9fXW, decision gd-aZrncmZbkD "tight"). Every call and the SSE stream use `machine:gardenio-github-bridge`: `gardenio-machine-release-recorder` for release writes, `gardenio-machine-github-bridge` for the rest (ownership of `merge-request` beads, and only its PR bookkeeping fields on task/bug/feature/chore/epic). This row was missing when the daemon stopped accepting the token (2026.306.1), so the bridge 401'd on every call until the roll. Its `githubBridge.beads.tokenSecret` is the rollback only. |
| gardenio-coopmux | `COOP_MUX_NATS_TOKEN` (+relay) | **Gone.** Own secret via `nats.auth.clientTokenSecret`, else none. It is no longer `coopmux-agent-token`. |
| gardenio-release-deployer | `KD_TOKEN` | **Staged.** `releaseDeployer.machinePrincipal` + client `gardenio-release-deployer` ship; flip after the steps below. |
| gardenio-beads-viewer | `COOP_MUX_AUTH_TOKEN` | Already the agent-facing `coopmux-agent-token` (not `kd-beads-token`). Its daemon traffic (SSE included) is its machine principal; the `bridge:beads-viewer` token is retired (gd-glMVEH8phE). |
| viewer-authproxy | `COOPMUX_TOKEN` | Already `coopmux-agent-token` (manifest `helm/gardenio-ziti/viewer-authproxy.yaml`). Unchanged. |
| gardenio-slack-bridge | `BEADS_DAEMON_TOKEN` | Already `slack-bridge-agent-token`. Unchanged. |
| gardenio-agents (controller) | `BEADS_DAEMON_TOKEN`, `BEADS_TOKEN_SECRET` | **Staged (stage 3a, gd-BDirD4F7Iq).** `BEADS_DAEMON_TOKEN` is **gone** when `agents.machinePrincipal.enabled` (it is, on k3s). `BEADS_TOKEN_SECRET` stays: it is a Secret *name* injected into agent pods, not a value the controller reads. See below. |
| gardenio-coopmux | `BEADS_TOKEN` | **Gone** when `coopmux.machinePrincipal.enabled` (it is, on k3s): every coopmux daemon call is `machine:gardenio-coopmux` (gd-in7EZvvd3T); the chart dropped the env in PR #651 (ea7fe8f7, live since 2026.294.7). See below. |
| gardenio-beads | `BEADS_AUTH_TOKEN` | **Gone** in the stage 4 change below (gd-ke6PC7bRto): the daemon no longer reads it. |

## Code changes that make the chart changes safe

- `bridge.SSEStreamConfig.TokenSource` (internal/bridge/sse.go): the SSE stream fetches its bearer per
  connect from the component's machine token source and never falls back to the static token. trigger-engine
  and grafana-bridge pass it, so they no longer need the shared token for their event stream.
- `release-deploy.sh`: with `KD_OIDC_TOKEN_URL`, `KD_OIDC_CLIENT_ID`, `KD_OIDC_CLIENT_SECRET_FILE` set it reports as a
  client-credentials principal and sends neither `KD_TOKEN` nor `X-Operated-By` (the `static_header` source). The
  secret goes to curl by file reference, never argv.

## Stage 2 operator steps (release-deployer), in order

1. Create Secret `gardenio-machine-gardenio-release-deployer` (key `clientSecret`) in the release namespace.
2. Roll a chart containing the new `gardenio-release-deployer` KeycloakClient and `beads.machineClients` entry
   (this change); confirm the KeycloakClient reports OK.
3. Cut a release tag that contains the new `release-deploy.sh` (the deployer runs the TAGGED script).
4. Set `releaseDeployer.machinePrincipal.enabled: true` in `deploy-values/gardenio-k3s.yaml` and roll.

Rollback: set it false and roll; the legacy `KD_TOKEN` path returns.

NATS: nothing to do on k3s while `nats.auth.enabled=false`. If it is ever enabled without subject isolation,
create a Secret (key `token`) holding the legacy token and set `nats.auth.clientTokenSecret`.

## Stage 3: what is still blocked, and why

**Controller default client (stage 3a, gd-BDirD4F7Iq).** With `agents.machinePrincipal.enabled` the controller
authenticates *every* daemon call and its SSE event stream as `machine:gardenio-controller`, and the chart no
longer injects `BEADS_DAEMON_TOKEN` into the pod. The client carries four roles:

| Role | Grants | Replaces |
|---|---|---|
| `gardenio-machine-accounts` | `update/account` | (existing) |
| `gardenio-machine-token-mint` | `/v1/fleet-tokens` | (existing) |
| `gardenio-machine-controller-ownership` | `close/advice`; ownership of foreign `task`, `advice`, `session`, `trigger` beads; update-only (never close) on `bug`, `feature`, `chore`, `epic` while the assignee is a closed agent (stale-reaper claim release, gd-RZJk5gspFv) | the static-token ownership bypass (`write_authz.go`, `id.Type == "static"`) for the ~10 sites in inventory section 0 item 4 / App. A (O) |
| `gardenio-machine-controller-config-seed` | create/update of exactly the 47 titles `configs()` defines | `machineAllowedWrites` `create/config` + `update/config` for `EnsureConfigs` |

Pinned by `TestMachineControllerConfigSeedTitles_MatchSeed` (controller) and the golden in
`kbeads/internal/server/testdata/machine_grants.golden.json`.

**`created_by` migration: none.** Beads the controller created before the roll carry `created_by=static` (config-kind
`agent`/`account` beads never reach the ownership gate; escalations and messages it created are not mutated again by
it). The only foreign-owned non-config beads it mutates are the four typed ones above, and the ownership role covers
those regardless of `created_by`, the same way `gardenio-machine-trigger-ops` covers run beads created under
`static`. No bead is rewritten. New controller-created beads carry `created_by=machine:gardenio-controller`.

**Not granted, and no longer attempted (owner call, decision gd-7oRiFouRVY: deny):** the legacy `config:auth` bead is
credential routing and is not in the config-seed role. The controller's former failover write-back to it
(`updateAuthConfigForProject`) is removed; the controller only *reads* legacy `config:auth` beads to discover
projects and the fallback chain. Canonical failover state is the in-memory runtime and the account beads. Old beads
may still carry `primary_account`/`failover_reason`/`failed_over_at`; nothing in the controller reads or refreshes them.

**MicroVM shared-token mode is refused** when the controller holds no static token. `handleNonK8sSpawn` returns an
error ("enable agents.perAgentTokens") instead of starting a satellite that cannot authenticate. K3s runs no MicroVM
backend. Per-agent mode is unchanged and mints through the same client.

Roll order (operator): (1) roll the chart/overlay carrying the two new realm roles and the `gardenio-controller`
client's `realmRoles`; confirm the KeycloakClient and both KeycloakRealmRoles report OK. (2) Roll the controller
(`agents.machinePrincipal.enabled: true` already on k3s) — the token source re-fetches within 30s if a role lands late.
(3) Watch `gardenio_machine_token_missing_role_total` and controller logs for `auth_unavailable` / 403 on
`stale_reaper`, `advice_sweeper`, `captain_spend_sync`, `reconcile_session_spend`. Rollback: `agents.machinePrincipal.enabled: false`
and roll; the static token returns. `PER_AGENT_TOKENS` provisioning reads only the Secret *name* from
`BEADS_TOKEN_SECRET`; nothing else in the controller reads the value, so the name stays until the follow-up removes
the shared secret.

**coopmux `BEADS_TOKEN`.** Its reads (accounts, auth configs, role accounts, agent/task beads) now go through
the machine principal (`state.machine_beads_client`, via `read_client` in `coop/crates/mux/src/lib.rs`;
`gardenio-operator` covers reads), falling back to the static client only when the principal is not configured
(gd-in7EZvvd3T). The `config:auth` failover write (`update_bead_description` on the `auth` config bead)
is removed from coopmux: the owner chose "controller-owns" (decision gd-HYilfX1Bk8), so no new
`ConfigTitles: ["auth"]` grant is added for coopmux. The controller already records the failover on that bead
(`auth_failover.go` -> `updateAuthConfigForProject`); whether the controller itself may keep that write on its
machine principal is the separate decision gd-7oRiFouRVY (controller PR #512). With coopmux's code on the machine
principal there is no static call left, so the chart no longer injects `BEADS_TOKEN` when
`coopmux.machinePrincipal.enabled` (PR #651, gd-in7EZvvd3T); with it off (rollback) the env returns.
`BeadsClient::from_env` treats a missing `BEADS_TOKEN` as "no bearer", so the fallback client sends nothing and the
daemon 401s it, which is the intended fail-closed behaviour.

**Laptop-side holders** (operator, not the chart): `~/.config/gardenio/kd-beads-token`, `GARDENIO_TOKEN` in
`~/ws/shell/gardenio-env.sh`, and the "Reaching the daemon for gb" kubectl/port-forward recipe in operator
prompts/skills (bead comment 2026-10-17 03:23Z).

## Verification

1. Pod-spec scan, names only: for each workload, list env/volume/envFrom references to secret `kd-beads-token`.
   Pass = only `gardenio-beads`.
2. 24h of zero static-token requests, measured by credential type, not by resolver source:
   `increase(gardenio_beads_requests_by_auth_type_total{auth_type="static"}[24h]) == 0`, with
   `method_class` splitting reads from writes. **Do not gate on `operated_by_source=static_header`.** That label means
   "the operator came from an `X-Operated-By` header" and is emitted for both `id.Type=static` and `id.Type=fleet`
   calls; per-agent tokens are fleet tokens and agents send the header, so normal agent writes count as
   `static_header` and the gate could never be met (gd-M4pFzv6bUq). `auth_type="static"` is the primary shared
   bearer token only; a fleet token carrying `X-Operated-By` is `auth_type="fleet"`. The counter is incremented once
   per authenticated request on HTTP and gRPC; it has no source label (cardinality), so attribute a remainder from
   the `auth_subject` / `remote_ip` fields of the kbeads access log (`auth_type=static`, debug level; raise the log
   level for the window) and migrate or exclude laptop holders first. In-cluster holders must read 0.
   Separately, count `header_missing` refusals (expect 0): a static-token write with no `X-Operated-By`; a machine
   principal never produces it.

## Stage 4: kbeads stops accepting the static token (gd-ke6PC7bRto)

The final step. The code and chart change is one PR; the roll, the Secret rotation and the Secret
deletion are admin-gated and happen afterwards, in the order below.

### What the daemon does after this change

- `BEADS_AUTH_TOKEN` and `BEADS_PREVIOUS_TOKEN` are not read. If either is still set in the pod
  environment the daemon starts and logs `static bearer token env is set but IGNORED`; it does not
  fail, so a stale chart cannot crash-loop it.
- The only bearer credentials are a **fleet token** (`BEADS_FLEET_MAP`, `bridge_token:*` config rows,
  or one minted over `/v1/bridge-tokens`), an **OIDC JWT**, and a **machine principal**
  (`BEADS_MACHINE_CLIENTS`). Any other bearer, including the old shared token, gets **401** on HTTP
  (`invalid token`) and **Unauthenticated** on gRPC. A token that is JSON-wrapped
  (`{"token":"<v>"}`) still matches the registered raw fleet token.
- With no fleet token and no OIDC verifier configured the daemon still runs auth-disabled (dev and
  test only); that mode is unchanged.

### Carve-outs removed

| Was | Now |
|---|---|
| `AuthIdentity.Type == "static"` with `Subject == "static"` | Not produced. No code path builds a `static` identity. Old `created_by=static` rows are only data. |
| `X-Actor` self-attribution on the static token (`gardenio_beads_xactor_override_total`) | Removed with its metric. An agent's name comes from its per-agent fleet token. |
| `X-Fleet` override (static token only) | Ignored for every caller; the fleet comes from the credential. |
| Static header block: `X-System-Operator`, `X-No-Owner-Default`, `X-Owner-Override`, `X-Trigger-Bead`, `X-No-Auto-Parent` on the static branch | Gone. Fleet tokens keep the `X-Operated-By`, `X-User-Bead-Id`, `X-No-Owner-Default`, `X-Owner-Override` handling; per-agent tokens keep `X-Trigger-Bead` and `X-No-Auto-Parent`. |
| Ownership bypass for the static token (`write_authz.go`, `id.Type == "static"`) | Gone. Machine principals use typed grants; a privileged fleet token keeps its bypass. |
| `escalation requesting_agent` self-attribution from `X-Actor` | Gone. Only a per-agent token stamps (and verifies) it. |
| Static-token rate-limit exemption | Gone. Intra-cluster machine principals are limited like everyone else (the trigger-engine already saw 429s under load). |
| `handleRegisterBridgeToken` / list / revoke accepting the static token (`isAdminRequest`) | Only a machine principal holding `gardenio-machine-token-mint` (and an auth-disabled daemon) may mint, list or revoke. |
| `POST /v1/embeddings/*` admin check = static token | An OIDC admin only. The chart's embedding-backfill CronJob (off by default and on k3s) has no such credential, so it cannot authenticate until the route gets a machine capability. |
| `static` row in `resolveOperatedByPure` | Gone. A per-agent fleet token resolves through the agent bead. |
| Ownership grant rows `static-token` and `queue-escalation-static` (`ownership_grants.go`, `kbeads/docs/ownership-grants.md` row 3) | Removed; the golden file drops both rows. |
| `control:` mail and runner revoke admitting `Type == "static"` (`mail_control.go`, `runners.go`) | Gone. Operators, the bridge fleets in `KBEADS_CONTROL_MAIL_FLEETS` and the recipient's runner remain. |
| `whoami` reporting `static` | Not produced. |

`gardenio_beads_requests_by_auth_type_total{auth_type="static"}` stops incrementing. Keep it in the
verification below: after the roll it must stay at its last value, and any rejected-token traffic
shows as 401s in the access log rather than as a `static` series.

### PRECONDITIONS: do not roll until all hold

1. **The static-token gate is met.** `increase(gardenio_beads_requests_by_auth_type_total{auth_type="static"}[24h]) == 0`
   from in-cluster sources, measured only after #503 (`5c096f7b`) is deployed. Laptop holders
   (`~/.config/gardenio/kd-beads-token`, `GARDENIO_TOKEN`) are excluded or migrated first.
   Re-checked 2026-10-31 ~16:50Z (gd-ke6PC7bRto): `[24h]` = 0. `[7d]` = read 376, write 29, all from outside
   the cluster: about 350 on 2026-10-25 22Z to 10-26 06Z with `X-Actor: laptop-claude`
   (`gardenio_beads_xactor_override_total{caller="laptop-claude"}`), and about 55 with no `X-Actor`, the last
   on 2026-10-30 07-08Z. Those are laptop calls: they fail with 401 after the roll, so move the laptop's
   `kd`/`gb` to `kd login` (OIDC) or the `gardenio-laptop-operator` machine client, then re-run the query
   right before the roll and expect 0.
2. **Stage 3 is closed.** Both stage-3 holders are done: `gardenio-controller` (gd-BDirD4F7Iq, PR #512,
   release 2026.291.5, rolled 2026-10-18 ~21:25Z) and `gardenio-coopmux` (gd-in7EZvvd3T; the chart step that
   drops `BEADS_TOKEN` is PR #651, live since 2026.294.7). The k3s overlay rendered from this change has no
   `secretKeyRef` to `kd-beads-token` left. The controller's `BEADS_TOKEN_SECRET` env is a Secret *name*
   handed to agent pods only when `agents.perAgentTokens` is off (`podspec.go`); it is not a mount and
   not a value the controller reads. Re-run the pod-spec scan after the roll; the pass condition is that no
   workload references the Secret.
3. The release containing this change is the one being rolled (one calver tag deploys every image).

### Roll, rotate, delete (admin-gated)

All three are escalations; none of them prints the token.

1. **Roll** the chart (`helm upgrade` with `--set global.imageTag=<tag>`). The beads Deployment loses
   `BEADS_AUTH_TOKEN` and `BEADS_PREVIOUS_TOKEN`.
2. **Verify** (below). Soak for at least one full controller sweep and one release-deployer tick.
3. **Rotate**, then **delete**, `kd-beads-token`: delete the in-cluster Secret
   (`kubectl -n gardenio delete secret kd-beads-token`) and the Secrets Manager copy
   (`gardenio/kd-beads-token` in us-east-1, with a recovery window rather than force delete). Rotate first
   so a copy that leaked into a laptop file, a shell history or a CI log is worthless even if it
   was never revoked: `openssl rand -hex 24`, written to the Secret, rolled once, then removed.
   Delete only after the soak: once the Secret is gone a rollback to the previous chart cannot
   re-create the old behaviour.
4. Remove the laptop copies and the `kubectl get secret kd-beads-token` recipe from operator
   prompts and skills.

**Done 2026-11-05** (gd-gdf3L8kjT5, run from the operator laptop): the old token got 401 on `/v1/beads`
just before rotation; it was rotated, the in-cluster Secret deleted, the Secrets Manager copy scheduled
for deletion on 2026-12-05 (30-day recovery window), and the laptop copy removed. With
`agents.perAgentTokens` on, the chart no longer passes `BEADS_TOKEN_SECRET` to the controller
(25657c86d), so a bead-less spawn does not wait on the missing Secret.

Rollback before step 3 is the previous chart revision: it sets `BEADS_AUTH_TOKEN` again from the
still-present Secret. After step 3 there is no rollback to the shared token by design.

### Verification

- **Old token is refused.** From a shell that holds the old value without echoing it, hit one
  authenticated route over HTTP and one over gRPC and expect 401 / Unauthenticated. Do not
  paste the token anywhere; read it from the Secret into a variable and unset it.
- **The edge probe stays green.** The public edge authenticates with its own credential and must
  not have depended on the shared token.
- **No workload references the Secret:** the pod-spec scan in Verification above, plus
  `kubectl get cronjob,deployment,statefulset -A -o json | grep -c kd-beads-token` returning 0.
- **No 401 storm from a holder we missed:** the kbeads access log's 401 count by `remote_ip`
  for the first hour after the roll; every in-cluster source should be zero.
