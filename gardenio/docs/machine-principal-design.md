# Machine principals: how platform services authenticate to the beads daemon

Status: **approved** by the operator on 2026-10-12 (decision gd-4I7rIPVCLC, "approve as
written"; this accepts the recommendations in [§9](#9-decisions-for-the-operator)). Task
gd-RjE3kvuGzo, epic gd-UdM4LnnaGE, thread gd-OvrPW5UlHG. This PR is docs only; the build is
tracked in the epic's child tasks. Runtime-auth and deploy-values PRs from that build are opened
as DRAFT with label `merge-desk:hold` until the operator's 2026-10-14 watched roll.

Sibling of gd-00NASU92b2 (one identity across browser, CLI and Slack). That epic is the
**human door**; this is the **machine door**.

## 1. Problem

`/costs` froze because kbeads refuses the controller's `spend_sync` writes to `type:account`
beads. Evidence from the epic: `gardenio_authz_denials_total{action=update,bead_type=account}`
climbing; account bead `total_cost_usd` 1012.28 against a LiteLLM key at 1178+. The rate-limit
probe writes the same beads and hits the same gate.

Why it is denied, in `kbeads/internal/server/write_authz.go`:

- `authorizeWrite` (`:250`) only evaluates roles for OIDC callers. Every other caller is a
  "non-OIDC caller" and carries **no roles** (`RolesFromContext` is OIDC-only).
- `type:account` is `min_role: gardenio-admin`, so a roleless caller is denied unless the exact
  `(action, type)` pair is in `machineAllowedWrites` (`:194`, checked at `:293`).
- `account/create` is on that list; `account/update` is not. The controller authenticates with the
  shared primary static token (`BEADS_DAEMON_TOKEN`, `cmd/controller/main.go:223`), so
  `spend_sync` (`cmd/controller/spend_sync.go:224`) and `ratelimit_probe.go:699,767,835` are refused.

The operator ruled (epic description, PR #274 closed): **no new static-token exception.** Give
each machine caller its own named identity and role, and make kbeads authorize by role.

The structural weakness behind the bug is that `machineAllowedWrites` is a *global* list of
`(action, type)` pairs, granted to *whoever holds a non-OIDC token*. Six deployments
(controller, beads-viewer, trigger-engine, grafana-bridge, alertmanager-bridge, roles-server and,
unless `PER_AGENT_TOKENS` is on, every agent pod) hold the **same** primary token
(`helm/gardenio/templates/*/deployment.yaml`, `BEADS_DAEMON_TOKEN` from `gardenio.beads.tokenSecretName`).
Only slack-, gitlab- and jira-bridge have their own bridge token. Adding `account/update` to the
global list would let the viewer, a bridge or any agent pod rewrite account spend. That is exactly
what the operator declined.

## 2. What exists today

| Mechanism | Where | What it gives a machine |
|---|---|---|
| Primary static token | `kd-beads-token`, `AuthMiddleware` `interceptors.go:801` | `authMethodStatic`, subject `static`, no roles; bypasses `requireRole` below admin |
| Bridge ("fleet") token | `POST /v1/bridge-tokens` (`http_fleet_tokens.go`), rotated by `internal/bridgetokenrotator` | Named subject (`bridge:<name>`), optional project/type/op **restrictions**, TTL + refresh, `Privileged` ownership bypass. No roles. |
| Per-agent token | `cmd/controller/agenttoken.go`, PR #273 (open) | A bridge token per agent pod, subject = agent name, revoked on exit |
| OIDC (Keycloak) | `oidc.go`, `AuthInterceptor` | `authMethodOIDC`, email subject, `realm_roles`/`realm_access.roles`, `azp` via `ExtractClientID` |
| Edge-auth v2 | `edge_auth.go` | HMAC-signed operator + roles, treated as OIDC-equivalent |
| Keycloak client_credentials precedent | `gardenio-info-density-audit`, `docs/agent-pod-keycloak-auth.md` | A confidential client whose service account has `gardenio-viewer`; token minted from a K8s Secret |
| Ziti | `podmanager/ziti_access.go`, `kbeads/internal/client/ziti.go`, research doc gd-HNzv6TjeSf | Overlay reachability for pods and clients; **no server-side listener in kbeads** |

Two properties of this code drive the design:

1. **Min-role is linear** (`roleRank`: viewer < operator < admin, `interceptors.go:30`). A machine
   that needs "write accounts and nothing else admin-gated" cannot be expressed by handing it a
   role on that ladder. Handing the controller `gardenio-admin` makes a controller compromise a
   full admin compromise, which is worse than today's allow-list.
2. **"OIDC" currently means "a human".** Many gates key on `authMethodOIDC` or `AuthIdentity.Type
   == "oidc"` as a proxy for a person with an email: `read_authz.go:24`, `spawn_authz.go:68`,
   `http_hooks.go:306,319`, `http_taints.go:270`, `http_taint_grants.go:245`, `http_undo.go:62`,
   `http_scrub_requests.go:839`, `http_escalations.go:155`. Escalation approval is a fail-closed
   client allowlist (`escalation_approval_authz.go:72`). Service-account JWTs would be verified by
   the same verifier and, if simply stamped `authMethodOIDC`, would **inherit every one of those
   human-only code paths**: they have no `email` claim (empty `Subject`), would fail the
   `allowedDomains` check if one is configured (`oidc.go:109`), and could be read as an approver
   or scrub requester. So a machine must **not** be an OIDC human. It needs its own auth method.

## 3. Options considered

| | A. Keycloak service-account clients | B. kbeads-minted tokens + roles | C. Ziti identities |
|---|---|---|---|
| Credential | JWT from `client_credentials`, minutes-lived | Opaque bearer, 720h + refresh | Enrolled x509 identity |
| Who registers a principal | Chart (`keycloak.clients`, GitOps, in the realm) | `POST /v1/bridge-tokens`, **admin-gated on the primary static token** (`isAdminRequest`, `http_fleet_tokens.go:332`) | Ziti controller admin API |
| Revocation | Disable client; live tokens age out in minutes | `DELETE /v1/bridge-tokens/{name}` | Delete identity |
| New runtime dependency | Keycloak (token mint only; verify is offline against cached JWKS) | none | Ziti controller + routers, new kbeads listener |
| Carries roles | Yes (`realm_roles`) | Not today; needs a new field | No. Attribution only. |
| Fits "same authz code as humans" | Yes: same verifier, roles, `azp` | No: separate credential store | No |
| Retires the shared secret as the root of trust | **Yes** | **No**: minting any principal needs the primary static token | n/a |

**Reasons for A for long-lived platform services**

- B cannot meet the goal "retire the static-token authority" (gd-whES7ZdkAy). Every B principal is
  minted by the primary token, so that token stays the root of trust and still has to live
  somewhere. A moves the root of trust to the realm, which the human door (gd-00NASU92b2) already
  hardens: declared in config, survives a Keycloak restore, MFA'd admin accounts.
- A's tokens are short-lived. A leaked bridge token is valid for up to 30 days; a leaked service
  token for the access-token lifespan.
- Roles, `azp` and audit attribution already flow through the OIDC verifier. We reuse parsing and
  verification and add only a new *classification* of the result (§4).

**Reasons not to pick A everywhere**

- Per-agent identities (PR #273) are high-churn, minted and revoked per pod by the controller. A
  Keycloak client per agent pod would put the realm admin API on the spawn path. That stays on
  kbeads-minted tokens and is out of scope here (it is P7, gd-ik2wf4ncGp).
- A makes Keycloak a dependency of *getting a token*. Mitigation in §6; the failure mode is
  fail-closed, not a fall back to the static token.

**C (Ziti) is rejected for this problem, kept for others.** gd-HNzv6TjeSf already established the
relevant facts: kbeads has no ziti listener (`grep` for `edge.Conn|GetDialerIdentityName` matches
nothing), so it never learns the dialer; the in-cluster Ziti controller is single-replica and its
cutover is still in flight (`cac016d5`, gd-UIX5ufQHnt); and a Ziti identity carries attribution
and reachability, **not roles**. Platform services already reach kbeads over the cluster network
under NetworkPolicy. Ziti would add a hard dependency to solve a problem it does not address. What
this design **does** take from the Ziti findings:

- *Authorize on a server-verified identity, never a client-declared header.* The Ziti doc's
  `SourceIdentifier()` vs `GetDialerIdentityName()` distinction is the same lesson as
  `X-Operated-By` today (`write_authz.go:43-46`). Machine principals are authorized from the
  verified token only; `X-Operated-By`, `X-System-Operator` and `X-Owner-Override` are ignored for
  them (§4).
- *Mint / inject / revoke / TTL-backstop lifecycle* from `ziti_revoke.go` and `agenttoken.go`, for
  the agent follow-up.
- *Fail closed when the issuer is down.* Never fall back to presenting the static token.
- The `ziti_*` bead config fields and `authMethodZiti` idea remain the right shape if agents later
  dial kbeads over ziti. That composes with this design: a ziti-attested agent would be another
  principal *kind* mapped to grants through the same table in §4.

## 4. Recommended model

Two layers that can change independently.

### 4.1 Authorization: principal → grants (transport-agnostic)

New in kbeads: an `authMethodMachine` and `AuthIdentity.Type = "machine"`.

- **Classification.** A verified JWT is a machine principal iff its `azp` is a registered machine
  client **and** it is a service-account token (`preferred_username` begins `service-account-`, or
  `sub` is a service account; confirm the exact claim against a real token, see §8). Registered
  clients come from config the daemon reads (`BEADS_MACHINE_CLIENTS`, or `machine_client:<id>`
  config rows later). An unregistered client's service-account token is **rejected 401**, not
  treated as a human. The verifier and its JWKS retry holder (`oidc_verifier_holder.go`) are reused
  unchanged.
- **Identity.** `Subject = "machine:<clientId>"` (never empty, never an email), `Type = "machine"`.
  Attribution stamps this. `operated_by` for these writes is the principal, not a human.
- **Headers ignored.** For `machine`, the `X-Operated-By`, `X-System-Operator`, `X-Owner-Override`,
  `X-User-Bead-Id` trust branches are **not** honored. Today those branches are scoped to
  `id.Type == "static"` or `"fleet"` (`interceptors.go` fleet branch around `:283`), so a new type
  gets none of them by default. That is the secure default, and the viewer's on-behalf-of-user
  writes continue to use the *user's* token (§5).
- **Default behavior everywhere else.** Treat `machine` exactly like `static`/`fleet` in every
  existing gate: the `authMethod != authMethodOIDC` sites keep their current outcome, and every
  `Type == "oidc"` human-only site keeps excluding it. This means **no existing gate changes
  behavior** except the two below. A table-driven test enumerates every `authMethodOIDC` /
  `Type ==` site (the list in §2) and asserts the machine outcome, so a new gate added later has to
  state its machine behavior (this is what `TestRPCMinRole_CoversEveryDescriptorMethod` does for
  RPCs).
- **The two changed gates.**
  1. `authorizeWrite`: for `machine`, after `checkTokenScopes`, look up the token's machine roles
     in a **grants table** (below). If `(action, type)` is granted by any of its roles, allow.
     Otherwise fall through to the existing `min_role` check with the machine's baseline role,
     which denies admin-gated types. `machineAllowedWrites` is *not* consulted for `machine`.
  2. `requireRole` / `RBACInterceptor` at admin: a machine principal is denied admin-only routes
     unless the route is in its grants (`token:mint`, see §7).
- **Grants table.** Realm roles named `gardenio-machine-<capability>` map to `(action, type)`
  sets, defined in Go next to today's allow-list (same review bar: every entry cites a
  `file:line` call site). Roles are *capabilities*, not the linear ladder, so they compose
  without implying admin. Every machine client also gets `gardenio-operator`, so routine
  create/update/comment/dependency traffic, which static callers get today by bypass, keeps
  working through `requireRole` unchanged. Assignment lives in the chart
  (`keycloak.clients[].serviceAccount.realmRoles`); meaning lives in code; a chart/Go cross-check
  test fails the build if a role is assigned that the table does not define, or defined and never
  assigned.

Why Go-defined rather than a config bead: a config bead is itself an `admin`-gated write, so
"which machine may write what" would be editable by the thing we are trying to constrain, and
would need a bootstrap. Code review is the control today and stays the control. Revisit if the
table grows past what a PR reviewer can read.

### 4.2 Credential transport: Keycloak service-account JWTs

Per component, one confidential KeycloakClient with `serviceAccountsEnabled`, `standardFlow` and
`directAccess` off, in the existing `keycloak.clients` list (`templates/keycloak/clients.yaml`):

- audience mapper to the kbeads audience (the `beads-viewer-audience` mapper pattern in
  `agent-pod-keycloak-auth.md`), so the single-audience verifier (`BEADS_OIDC_AUDIENCE`) accepts it
- `realm-roles` mapper into the flat `realm_roles` claim (already read by `ExtractRoles`)
- `access.token.lifespan` raised to ~30 min for these clients only (§6)
- client secret generated by the EDP operator into a Secret in the release namespace, mounted into
  that component only

Client side: `beadsapi.Config.TokenSource` already exists (`internal/beadsapi/client.go:99,125`) and
takes precedence over the static `Token`. The new piece is a `clientcredentials` TokenSource
(`golang.org/x/oauth2/clientcredentials`, which caches and refreshes), constructed from
`BEADS_OIDC_TOKEN_URL`, `BEADS_OIDC_CLIENT_ID`, `BEADS_OIDC_CLIENT_SECRET_FILE`. When unset the
component behaves exactly as today. That makes cutover per-component and reversible by unsetting
env.

In-cluster TLS (gd-lYPZlslOM7): with `keycloak.runtime.tls.enabled` (and `inClusterClients`, on
by default) the chart renders `BEADS_OIDC_TOKEN_URL` as the keycloak-runtime https port
(`https://<svc>.<ns>.svc:8443/...`) and adds `BEADS_OIDC_CA_FILE` (release-deployer:
`KD_OIDC_CA_FILE`), the chart's in-cluster CA (`ca.crt` of `<fullname>-tls`), which the Go and
Rust sources and `release-deploy.sh` trust beside the system roots. The file is re-read on every
fetch, so a pod that starts before cert-manager has issued fails closed and then recovers.

## 5. Role per component (preliminary)

Superseded by machine-principal-inventory.md

The authoritative inventory is gd-gUOMZdeUHg, which must run before gd-Hc34EENbO9 and gd-whES7ZdkAy
merge. This table is derived from the `machineAllowedWrites` doc comment (`write_authz.go:131-190`)
and the call sites grepped above; every row needs a `file:line` before it enters the grants table.

| Principal (client id) | Needs beyond `gardenio-operator` | Evidence / note |
|---|---|---|
| `gardenio-controller` | `update/account` (spend_sync, ratelimit probe, auth-failover, account exhaustion, key ACL widen), `close/advice` (advice sweeper), `token:mint` (bridge + per-agent tokens) | `spend_sync.go:224`, `ratelimit_probe.go:699,767,835`. This is the one that fixes `/costs`. |
| `gardenio-slack-bridge` | `update/project`, `comment/project` (/schedule, /trigger), `create/config`, `update/config` (EnsureConfigs) | `bot_schedule_cmd.go`, `bot_trigger_cmd.go`, `bridge/init_ensure.go` |
| `gardenio-gitlab-bridge` | `create/config`, `update/config`; **privileged ownership bypass** | today's `Privileged` fleet flag (ka-0000001946) becomes a grant, `ownership:system-graph` |
| `gardenio-jira-bridge` | `create/config`, `update/config`, `create/jira-poller` | `bridge/jira_config.go` |
| `gardenio-grafana-bridge`, `gardenio-alertmanager-bridge` | `create/config`, `update/config` | EnsureConfigs only; they are on the primary token today |
| `gardenio-beads-viewer` | `update/user` (own-row self-service), `create/advice` | `user_status_message.go`, `settings.go`, `user_inbox.go`, `create_advice.go`. Human-initiated writes with a signed-in user forward **that user's** token (gd-00NASU92b2), so this grant covers only the viewer's own background and self-service writes. Whether `update/user` can instead be done on-behalf-of is a question for gd-Hc34EENbO9. |
| `gardenio-trigger-engine` | none identified: `AddComment` on non-admin types | `cmd/trigger-engine/main.go:1227`; confirm in the inventory |
| `gardenio-roles-server` | `create/advice`, `create/config`, `update/config` | `web_advice.go`, `web.go`, `web_instructions.go` |
| agent pods (`gb`, `kd`) | **out of scope**: per-agent identity, P7 (gd-ik2wf4ncGp, PR #273) | The `report/*`, `advice/*`, `role/create`, `account/create` entries stay in `machineAllowedWrites` until that lands |

Consequence for the allow-list: `machineAllowedWrites` shrinks only by the entries with *no agent
caller*: `project/*`, `user/*`, `config/*`, `jira-poller/create`. The agent-facing entries stay
until P7. `account/update` is never added to it.

## 6. Failure modes and what they cost

- **Keycloak down.** Verification is offline against cached JWKS, so a running kbeads keeps
  accepting valid tokens. A component keeps its cached token until expiry. With a 30-minute
  lifespan and refresh at ~50%, a Keycloak outage under ~15 minutes is invisible; beyond the
  token's remaining life, writes needing that component's roles 403/401 (**fail closed**). No
  fallback to the static token. This is a deliberate narrowing: today a Keycloak outage costs
  nothing for machine traffic.
- **Cold start with Keycloak down, or before the client exists.** kbeads' OIDC verifier is built
  through a retry holder (`serve.go:283-289`), so the daemon starts. A machine component with no
  token yet retries with backoff and surfaces `auth_unavailable`; it must not crash-loop the pod.
  The enabling roll is the sharp case (gd-pj5hT3V1la, 2026.287.9): the component starts in the
  same `helm upgrade` that creates its `KeycloakClient`, the Keycloak operator reconciles that
  asynchronously, and the first `client_credentials` fetch answers `invalid_client`. A chart
  ordering hook cannot close that gap. The token source therefore has a startup primer,
  `beadsapi.WarmMachineToken` (`machine_token.go`, `Warm`): it retries the first fetch with
  backoff (1s doubling to 30s, each attempt logged at Warn without the secret) until a token is
  held or the component's lifetime context ends, and never gives up on its own. A component
  whose seed writes run only once calls it before them. grafana-bridge is the reference: its
  health server listens first (so liveness passes while it waits), `/readyz` answers 503
  `starting` until the whole startup sequence is through, then it warms the token, seeds its
  config beads and runs the dashboard backfill, each retried on `auth_unavailable`
  (`cmd/grafana-bridge/main.go`, `seedStartup` and `retryAuthUnavailable`). While it waits the
  pod shows Running but not Ready, which is the operator-visible signal. A failure that is not
  the credential (a daemon 403) is not retried: it is a real answer.
- **kbeads OIDC misconfigured.** This has already happened once (epic gd-00NASU92b2: OIDC "looks for
  Keycloak on the wrong port", disabled from 06:32Z on 2026-10-11). With OIDC off, a machine JWT is
  rejected. During migration each component keeps its static token as the *only* fallback, until
  its cutover is verified. After gd-whES7ZdkAy the static token cannot authorize those writes, so
  the verification step must be exercised before that retire task, not after.
- **Compromised component.** Blast radius becomes that component's grants instead of "everything
  the primary token can do". A different principal making the same write gets a structured 403
  (`caller_subject: machine:<other>`), which is the epic's visible-403 acceptance test.
- **Secret exposure.** The client secret is still a static secret in a K8s Secret. Net improvement
  is scope and lifetime, not the elimination of a secret. Per-component Secrets, mounted as files
  (not env), as the Ziti doc recommends for agent credentials.

## 7. The primary static token after this

It does not disappear in this epic, and the doc should not pretend it does:

- `isAdminRequest` (`http_fleet_tokens.go:332`, 9 call sites) compares only against the primary
  token, so **minting bridge/agent tokens still needs it**. The controller does this for the
  per-agent and rotator paths. Two ways out; recommend the first:
  1. Add a `token:mint` grant and let `isAdminRequest` accept a machine principal holding it (and
     an OIDC admin). The controller then needs no static token at all.
  2. Leave the controller holding the primary token for `/v1/*-tokens` only. Smaller than today
     (one pod instead of seven plus agents) but it is still an admin-everything credential.
- Break-glass for the operator and bootstrap before Keycloak exists (chicken-and-egg) keep it.
- `gd-ZjiSd9Kzio` step 2 and the PR #34 taint gate on the primary token remain the controls for
  whoever still holds it (agent pods until P7).

## 8. Open verifications (not answerable from the repo)

I had no cluster access. These must be confirmed before gd-gQ4iLIpWCm is merged:

1. Whether kbeads OIDC is **currently on** in the k3s deployment and its audience value. The epic
   comments say it was verified live on 2026-10-11, but task gd-MpRIXUoUly (turn OIDC on) is still
   parked on escalation gd-LMHmuLp48L.
2. The exact claims in a real service-account token from this realm: `preferred_username`
   (`service-account-<clientId>`), `azp`, `sub`, whether `email` is absent, and whether
   `realm_roles` is populated from `serviceAccount.realmRoles` via the existing mapper.
3. `BEADS_OIDC_ALLOWED_EMAIL_DOMAINS`: if set, `oidcVerifier.Verify` rejects an email-less token
   (`oidc.go:109-123`). Either the machine classifier runs before that check, or the domain check
   is skipped for registered machine clients. Needs a decision during implementation, with a test.
4. Whether `NetworkPolicy` lets controller, viewer, bridges and trigger-engine reach Keycloak's
   token endpoint (the chart notes a restrictive `keycloak-runtime-ingress`,
   `values.yaml` ~15701).
5. Keycloak's per-client `access.token.lifespan` is honored by the EDP `KeycloakClient` CRD
   (`attributes:` passthrough exists in `clients.yaml`; confirm the attribute key reconciles).

## 9. Decisions for the operator

**D1. Credential transport for platform services.** Recommend **Keycloak service-account clients**
(§3, §4.2). Alternative: extend bridge tokens with roles (cheaper, no Keycloak dependency, but the
primary token stays the root of trust and tokens live 30 days). Ziti is not offered as an option
for this problem.

**D2. Role model.** Recommend **a new `machine` auth method with capability roles
(`gardenio-machine-*`) mapped to `(action, type)` grants in code**, plus `gardenio-operator` as the
baseline. Alternative: give each machine `gardenio-admin` via the OIDC path. Rejected: admin is
all-or-nothing and the OIDC path carries human-only gates (§2).

**D3. Minting.** Recommend adding a **`token:mint` grant** so the controller needs no static token
(§7 option 1). Alternative: leave the controller holding it for `/v1/*-tokens` only.

**D4. Order of cutover.** Recommend **controller first** (it is the `/costs` fix and the smallest
grant set), then each bridge, then viewer and trigger-engine, one PR per component, each with a
live check that the wrong principal gets a 403 on the same write. Static token removal
(gd-whES7ZdkAy) only after every component is verified.

## 10. Build sequence

These map onto the existing children of gd-UdM4LnnaGE; none are created or changed by this doc.

1. **gd-gUOMZdeUHg** inventory (blocked on this doc only for the role vocabulary).
2. **gd-gQ4iLIpWCm** kbeads: `authMethodMachine`, classifier, grants table, `machine` outcome for
   every gate in §2 with the table-driven test, denied-for-wrong-principal test, cross-check test.
   Behavior with no machine clients configured is identical to today.
3. **Chart**: `keycloak.clients` entries + per-component Secrets (new child; belongs with 4 and 5).
4. **gd-s7aPIf0Bgp** controller: `clientcredentials` TokenSource; verify `spend_sync` and the probe
   write accounts with no allow-list entry and `/costs` refreshes within 5 minutes.
5. **gd-Hc34EENbO9** bridges, viewer, trigger-engine, roles-server, one task each.
6. **gd-whES7ZdkAy** retire the exceptions, update `TestAuthorizeWrite_StaticToken_*`.
7. **gd-eiCV6zrLUS** verify on k3s.

## 11. Non-goals

- Per-agent identity (P7, gd-ik2wf4ncGp, PR #273) and ziti-native agent egress (gd-HNzv6TjeSf).
- The human door and the viewer's on-behalf-of-user writes (gd-00NASU92b2).
- Eliminating the primary static token (§7).
- Keycloak HA.

## 12. As built

Written for gd-whES7ZdkAy (plan gd-UdM4LnnaGE, Task 6), checked against `main` at `899839b4`. Where
this section and the sections above disagree, this one is what shipped. The outcome that matters
most: **the build retired no static-token exception.** The grants came out narrower than §5
assumed, so no allow-list entry and no static-token env var has lost its last non-machine caller.
§12.4 and §12.5 say what blocks each one.

### 12.1 Principals live on k3s

Each is a confidential Keycloak client with a service account (`deploy-values/gardenio-k3s.yaml`
`keycloak.clients`), listed in `beads.machineClients`. The chart test
`gardenio/helm/gardenio/tests/machine_principal_test.go` (`machineClientRows`) pins this table:

| Client id | Component | `gardenio-machine-*` roles (besides `gardenio-operator`) | Use |
|---|---|---|---|
| `gardenio-controller` | controller | `accounts`, `token-mint` | seven account writes (`spend_sync`, `ratelimit_probe`, `auth_failover`, `key_rotation`); fleet-token mint, dormant on k3s |
| `gardenio-coopmux` | coopmux | `accounts` | the `status=exhausted` account write in `failover.rs`, through `machine_token.rs` |
| `gardenio-beads-viewer` | beads-viewer | `plan-reactor` | the plan-step reactor's `last_nudge_at` stamp and the plan-label reactor's trigger create |
| `gardenio-grafana-bridge` | grafana-bridge | `config-seed`, `dashboard-sync` | its `type:dashboard` config seed and dashboard sync writes |
| `gardenio-trigger-engine` | trigger-engine | `trigger-ops` | trigger, task and doc writes, with trigger-lineage attribution |
| `gardenio-slack-bridge` | slack-bridge | `slack-config-seed`, `slack-dm-delivery` | its eight-title config seed and DM delivery writes |
| `gardenio-github-bridge` | github-bridge | `release-recorder` | its release writes only (create, and the field fill on an existing release); every other bridge write stays on the static token |
| `gardenio-authz-canary` | none (Job only) | none | proves a different principal is refused (§12.7) |

Every client also holds `gardenio-operator`. The principal is `machine:<clientId>`
(`machine_identity.go`) and is also its `operated_by`; no header is honoured for it. The
enable switch is `<component>.machinePrincipal.enabled`; with it off the component behaves as before.

### 12.2 Grants

`kbeads/internal/server/machine_grants.go` is the source of truth;
`testdata/machine_grants.golden.json` mirrors it and the chart test cross-checks every assigned role
against the golden file. A grant is a set of `(action, type)` writes, a typed ownership list (the
bead types the principal may update without owning them), exact config titles, or a capability flag.

| Role | Grants |
|---|---|
| `accounts` | `update/account` |
| `token-mint` | `POST/GET/DELETE /v1/fleet-tokens` and `/v1/bridge-tokens` (replaces the literal-token `isAdminRequest` check for this principal only) |
| `plan-reactor` | ownership of `epic` |
| `config-seed` | config write for title `type:dashboard` |
| `dashboard-sync` | ownership of `dashboard`; `update` only of `doc` beads labelled `grafana:dashboard-model` |
| `trigger-ops` | ownership of `trigger`, `task`, `bug`, `feature`, `chore`; `update` only of `doc` beads labelled `uncovered-open-lane-rollup`; trigger lineage |
| `slack-config-seed` | config write for the eight exact titles `type:{decision,dm,escalation,mail,message}` and `view:{decisions:pending,escalations:pending,mail:inbox}` |
| `slack-dm-delivery` | ownership of `dm` |
| `release-recorder` | `create/release`, `update/release`; ownership of `release` |

This is the "tight" outcome of decision gd-ZsfxvXRiyv, not the broader `project-ops`, `user-self`,
`advice-*`, `system-graph` or `operated-by-relay` roles the inventory (§8) sketched. Those do not
exist. A machine principal never consults `machineAllowedWrites`
(`TestAuthorizeWrite_Machine_GlobalAllowListNotConsulted`), and holding `gardenio-admin` grants it
nothing (`TestAuthorizeWrite_Machine_AdminRoleGrantsNothing`).

### 12.3 What is not a machine write

- **Human-driven writes.** beads-viewer and slack-bridge write on behalf of a person: the viewer
  forwards the person's own bearer where write-bearer enforcement is on, and Slack writes ride the
  linked person's own token (gd-OZJ6H2S3xx). Neither is a machine grant. Where enforcement is off
  (the viewer on k3s, `devAuthBypass: true`) they still go out on the static daemon token.
- **Streams and reads.** The SSE streams of trigger-engine and grafana-bridge, and the reads of
  every component, use the static token (or the viewer's read token), not the machine token.

### 12.4 `machineAllowedWrites`: removed and kept

**Removed: none.** The deletion rule is that every remaining caller of an entry holds a machine
grant for the pair. Re-checked per entry; the three the inventory (§6.1) called candidates:

| Entry | Why it stays |
|---|---|
| `update/config` | controller `EnsureConfigs` and `auth_config_legacy.go:227` (controller has no config grant); coopmux `failover.rs:710`; alertmanager-, gitlab-, jira- and wl-bridge and roles-server (no principal). `config-seed` covers only `type:dashboard` and the slack titles. |
| `comment/project` | slack-bridge `/schedule` and `/trigger` and beads-viewer `project_config.go:320`: human-driven, no machine role covers them. |
| `create/jira-poller` | jira-bridge has no machine client. |

The other twelve (`report/{create,update,close}`, `advice/{create,close}`, `role/create`,
`config/create`, `account/create`, `project/{create,update}`, `user/{create,update}`) stay for `gb`
and `kd` in agent pods, which use the shared static token until P7 per-agent tokens. `account/update`
is never added; the static token cannot write accounts. `TestMachineAllowedWrites_ExactSet` pins
the 15 entries, so any later change is a visible diff in `write_authz_test.go`.

The `machineAllowedWrites` and `requireRoleMachineAllowed` doc comments carry the same status.
`requireRoleMachineAllowed` behaves as before for static callers; only
the fleet-token and bridge-token routes have a machine-principal caller (the controller, dormant on k3s).

### 12.5 Who still holds the primary static token, and why

| Holder | Why |
|---|---|
| controller | general client and SSE watcher; non-config writes need the static ownership bypass; break-glass (§7); `isAdminRequest` on routes outside `token-mint` |
| trigger-engine | SSE stream (`cmd/trigger-engine/main.go` `buildSSEStreamConfig`) |
| grafana-bridge | SSE stream (`cmd/grafana-bridge/main.go`); the machine principal is attribution here, not containment. Follow-up gd-VmD4w6lIzU |
| beads-viewer | human-driven writes and reads; SSE streams, system reads and proxies as its machine principal (no static token while enabled, gd-glMVEH8phE) |
| coopmux | machine principal for reads and the one account write; static `BEADS_TOKEN` only as the fallback when the principal is unconfigured. No `config:auth` write (the controller owns it, gd-HYilfX1Bk8) |
| slack-bridge | holds its fleet token, not the primary one, for everything but the two machine write classes |
| `gb`, `kd` in agent pods | the shared token until P7 |
| alertmanager-, wl-, push-bridge, roles-server, inviteservice | chart-only, no principal |

So the chart still mounts `gardenio.beads.tokenSecretName` for every component above, and
`machineClientRows.StaticTokenEnv` in the chart test asserts the machine-principal consumers keep it.
No component qualified for removing it under `<component>.machinePrincipal.enabled`.

### 12.6 gRPC refusal

Machine principals are HTTP-only. `AuthInterceptor` (`interceptors.go`) classifies a bearer that is a
registered machine client's service-account token and returns `PermissionDenied: machine principals
are HTTP-only` instead of letting it fall into the OIDC human path. An unregistered client's
service-account token is `Unauthenticated`.

### 12.7 The canary

`gardenio-authz-canary` is a machine principal holding only `gardenio-operator`.
`gardenio/scripts/machine-principal-canary.sh`, run by the raw manifest
`gardenio/helm/manifests/machine-principal-canary-job.yaml`, makes it write an idempotent value
(`CANARY_FIELD`, same value) to `TARGET` and expects HTTP 403 naming
`caller_subject: machine:gardenio-authz-canary`. The operator applies and deletes the Job by hand;
procedure and the account-bead results are in
[`machine-principal-verification.md`](machine-principal-verification.md). Pointed at an `account`
bead it proves the `accounts` grant is not shared; pointed at a trigger bead and a dm bead on
2026-10-15 it returned 403 (ownership denials, `required_role` empty), proving the typed ownership
grants of trigger-engine and slack-bridge do not leak to another client. Those two runs are recorded
on gd-Hc34EENbO9 and escalation gd-FnUZfuv4j8. The script's PASS line still says `update/account`
whatever the target; that text is hard-coded.

### 12.8 Adding a principal

1. **Client (code only).** Add a `keycloak.clients` entry with `serviceAccount.enabled`, the
   audience and `realm-roles` mappers and the `access.token.lifespan` of the existing machine
   clients, and its `serviceAccount.realmRoles` (including `gardenio-operator`). Add it to
   `beads.machineClients`; list it there no later than the moment it gets the audience mapper, or its
   tokens take the human OIDC path.
2. **Secret (operator).** The operator creates `gardenio-machine-<clientId>` with key `clientSecret`.
   Crews never create it. The Secret is optional in the pod: a missing one fails the machine writes
   closed (`auth_unavailable`), never back to the static token.
3. **Grant, with evidence.** Add the role to `keycloak.roles` and to `machineCapabilityGrants` in
   `machine_grants.go`, typed as narrowly as the call sites allow, each entry citing `file:line`.
   Update `testdata/machine_grants.golden.json`. Never add `admin`-equivalent writes, and never add
   to `machineAllowedWrites` for a machine.
4. **Wire the component.** `beadsapi.MachineTokenSourceFromEnv` with `WithExpectedRoles(...)`, the
   `<component>.machinePrincipal` values block, and the three `BEADS_OIDC_*` env vars under that switch.
5. **Cross-check.** Add a `machineClientRows` row in
   `gardenio/helm/gardenio/tests/machine_principal_test.go`; it fails if a role is assigned that kbeads
   grants nothing for, or granted and never assigned. Add a denied-for-wrong-principal test beside
   the grant and run the canary.
