# Lands threat model and security review (steps 10.7, 10.14 and 10.10)

> **Step 10.7** gd-MiaGhgmZ4L, Phase 10 Fences gd-Gx6arHBvzJ, plan gd-X07JyIHEzl.
> **Spec:** [`superpowers/specs/2026-10-19-lands-track-b-design.md`](../superpowers/specs/2026-10-19-lands-track-b-design.md) §5 and §9.
> **Shape:** follows [`gardenio-in-a-box-threat-model-signoff.md`](../gardenio-in-a-box-threat-model-signoff.md).
> **Scope (owner decision gd-3Vx9UFk7Ad):** the private-beta surface, which is steps 10.2 to 10.6 and the land door, with `lands.tenantSpawns` off.
> The 10.1 `lands-tenant` tier (`kbeads/internal/server/restricted_ops.go`) and the gatehouse steward's intake are reviewed in [10.14](#1014-the-lands-tenant-tier-and-the-gatehouse-intake) (gd-Ahv1F4wPUO), before public registration.
> **Reviewed at:** `main` 9d7ba9173, 2026-11-02.
> **Public notes:** what tenants should know is published by gd-mu3cEUKoAV, not here.

## Purpose

Lands puts strangers on the operator's fleet:

- their people in the Commons;
- their repo in the Lands App;
- later, their code in pods.

This document names what they could attack, and the control that stops each attack, as built on `main`. It lists what the review found and what the owner is asked to accept. A control counts only when it is code, chart or CI on `main`. A paragraph in the spec does not count.

## Verdict

**Signed off on conditions** by the owner, 2026-11-02 18:41Z (decision gd-fBm2JuIYOq, option `sign_off_conditional`). The conditions and the accepted risks are in [Sign-off](#sign-off).

The fences built in 10.2 to 10.6 mostly hold, and the matrix proves it. The cross-land read path, the door's routing, the kill-switch fields and the spend wall are sound. The tenant pod spec is sound once the gd-ApgYe740ps credentials work lands.

The review found:

- one live P0 that is a fleet bug as much as a Lands bug: raw bearer tokens on a viewer-rank config read (gd-zdo4DIeGWR);
- three P1s that are reachable in the private beta with tenant spawns off;
- one P1 that goes live as soon as a tenant pod holds a coopmux bearer.

The review recommends:

- fixing the P0 now, before any tenant is admitted, and then rotating the tokens it exposed;
- making every P0 and P1 below block 11.4 Open day (gd-BPwXK6CkZv);
- making every finding block the 10.10 check-in (gd-B7SoiKwC40), whose definition is "review findings closed or accepted by the owner".

## Trust boundaries

```
STRANGER (browser, repo, laptop agent)
  → EDGE (Traefik, /land/<name>/ door, forwardauth)
  → LANDGATE (route allowlist, audience, headers)
  → KBEADS / VIEWER / BRIDGE (scope, tier, freeze)
  → TENANT POD in land-<name> (NetworkPolicy, allowlisted spec)
  → NODE and FLEET (other pods, coopmux, NATS, Slack, GitHub, operator)
```

## Assets

| Asset | Where it lives | Who must never get it |
|---|---|---|
| Fleet bearer tokens (agent, bridge, laptop) | kbeads `bridge_token:*` config rows, pod env | every tenant; every other agent |
| Fleet Claude credentials and the owner's OAuth token | coopmux credential broker, `lands-template` account Secret | tenant pods |
| The fleet's beads: plans, escalations, advice, config | kbeads | land members, guests, tenant agents |
| Another land's beads, purse, members, repo | kbeads, Keycloak `/project-<land>`, Lands App installation | every other land |
| The operator's Slack workspace and decision cards | slack-bridge | tenant writes of any kind (B-D11) |
| The fleet GitHub App and operator orgs | `githubapp`, `land_operator_org.go` | tenant agents (Lands App only, one repo) |
| Spend: LiteLLM keys and the purse | `lands-<land>` account bead, LiteLLM | anyone but the land's own pods; never readable by members |
| The cluster: apiserver, IMDS, kubelet, other namespaces | k3s nodes | tenant pods |
| The operator's attention | Slack, `#support`, decisions | floods, phishing, look-alike approvals |

## Attackers

### A1. A tenant against another tenant

Goal: read or change land B's beads, purse, members or repo, or learn that something of B's exists.

| Control | Verified in | Verdict |
|---|---|---|
| Scoped reads and writes: project label, scope from token groups, empty scope denies | `scoped_principal.go:69-95, 206-320`, `read_authz_scope.go` | holds |
| Door audience: one `land-<door>` audience, global roles stripped | `land_audience.go:125-209` | holds |
| landgate refuses admins, non-members and multi-land tokens | `cmd/landgate/gate.go:273-292` | holds |
| Path tricks refused at the edge and landgate; three byte-identical allowlists | `public-edge.yaml:190`, `gate.go:97`, `land_door_routes.txt`, TestEdgeParity | holds |
| `TestCrossLandMatrix`: every route as A's member, owner, guest, principal and agent against B | `kbeads/internal/server/cross_land_matrix_test.go` | green on `main` (ci.yml run 41392494967, dd2b3a23) |
| 39 routes answer 403 where they should answer 404 (existence oracles) | `crossLandExistenceOracles` | open, ratchet only shrinks: gd-7APTAIhdC5, viewer gd-ZJOUPZWuvs |
| Account bead (raw LiteLLM key, USD) hidden from tenants | `read_authz_tenant.go:22-67` (gd-q8jegjvKI3, fixed) | holds |
| Stop gates reach only the agent's own | `gateTargetAllowed`, `http_gates.go:86` | holds |
| **Edges into another land's beads** | `explicitParentDep`, `scopedDependencyTargetDenial`, `maybeAddTriggeredByOnClaim` | **fails: gd-fgMx8iN5Uh (P1)** |
| **Tenant agent token replays another land's bearer** | `GET /v1/configs?namespace=bridge_token` | **fails: gd-zdo4DIeGWR (P0)** |

### A2. A tenant against the fleet

Goal: fleet credentials, fleet beads, fleet compute, or a foothold in a fleet agent.

| Control | Verified in | Verdict |
|---|---|---|
| Tenant pod spec stripped to the §5.3 list | `podspec_land_allowlist.go`, `TestTenantPodAllowlist` | holds for names |
| Placement from the land name only, `land-agent` SA, no API token, no hostPath | `podspec.go:830-852`, three VAPs `failurePolicy: Fail` | holds |
| `land-fence` NetworkPolicy: named peers; internet minus RFC 1918, 100.64/10 and 169.254/16; IPv4-only cluster | `landns.go:121, 246-293`; peers pair namespace and pod selectors | holds |
| Fail-closed admission: `land_unfenced`, `land_gone`, `lands_paused`, unknown purse | `spawn_wall.go:39-137`, `wall.go:130-197` | holds |
| PSS `restricted`, drop ALL, RuntimeDefault seccomp | `landns.go:231`, `manager.go:1790-1800` | holds |
| Tenant agents write only an allow-list of types (any other type, a new one included, is refused) | `landsTenantWrites`, `TestLandsTenantWrites_EveryType`, `TestCrossLandMatrix_TenantAgentWritesOnlyAllowedTypes` | holds since gd-bRLiEzvMsy (was a deny-list) |
| **coopmux bearer is fleet-wide; register trusts the caller's project** | `transport/auth.rs:117-121`, `http.rs:174-224` | **fails once copied: gd-ApgYe740ps (P1), gd-zVTBvaSIdG (P1)** |
| **Shared NATS agent user subscribes every session's input** | `templates/nats/statefulset.yaml:46-92` | **fails once copied: gd-ApgYe740ps** |
| **Tenant agent token on classAdminOnly routes; gRPC has no route-class gate** | `scoped_route_classes.go:301-315`, `interceptors.go:600-625` | **fails: gd-c1DOWNYy7W (P2)** |
| Kernel shared on runc: tenant pods required off fleet-agent, platform and control-plane nodes; lowest PriorityClass; counted in `CoopMaxPods` | `applyLandFence`, `podmanager.LandTenantAffinity`, `TestLandFence_PlacementWhateverTheRole` | accepted runtime, placement fixed by gd-NDJIOutLsv; lands still share nodes with each other |
| Defence in depth: VAP on CREATE only, RoleBinding subjects, quota counts, no reconcile, bridge mux on 8090 | `lands-namespace-rbac.yaml`, `landns.go` | gd-pXik57wZA9 (P3) |

### A3. A tenant against the operator's Slack and GitHub

Goal: post as the bot in the operator's workspace, phish an approval, or push to a repo that is not theirs.

| Control | Verified in | Verdict |
|---|---|---|
| `gb say` reaches only the land's own channels and thread | `api_conversation_say.go:561`, `api_conversation_caller.go:284` | holds |
| Commons reply path checks the Slack root is the land's | `inbound_reply.go:175, 323` | holds |
| Lands App token: one repo; contents, PRs and issues; no `workflows`; widened tokens revoked; operator orgs refused at mint | `github_app_credentials.go:285-387`, `identity.go:47`, `githubapp.go:360` | holds |
| Repo, owner and installation come from an HMAC-signed cookie, never the form | `lands_github.go:180-212`, `setup.go:78-100` | holds |
| Mail: CR/LF refused, address parsed, sent through the SES API | `mailer.go:78-94` | holds |
| Commons HTML escaped; only `https?://` linked | `commons/render.go:747-782` | holds |
| **Closed `thread-say` message posts into any Slack thread** | `thread_message_forward.go:142-220`, `router.go:133-150` | **fails: gd-KYeWPNJmvZ (P1)** |
| **Applicant purpose rendered as live mrkdwn on the decision card** | `lands_apply.go:587-617`, `bot_decisions.go:132-138` | **fails: gd-TyXgeI2OGT (P2)** |

### A4. Stranger repos and text as attackers

Repo content and tenant messages are untrusted input to the land's own agents. Through edges and the fleet's subtree views, they are also untrusted input to fleet gardeners.

- **Prompt injection.** The untrusted-input taint is on every bead of a tenant land, with effect `restrict` (gd-hGfgjuviQy, 10.1). An agent carrying it has approved escalations refused at replay, and a fleet agent carrying it also spawns, approves and writes fleet-configuring beads or config no more (gd-gaGdksntAG). It does not reach a merge made with the agent's own repo token. What a hijacked tenant agent can still do is whatever its token can do. That is why the token findings above (gd-zdo4DIeGWR, gd-c1DOWNYy7W, gd-fgMx8iN5Uh) are the blast radius of an injection, not separate risks.
- **Text reaching the fleet.** Tenant text can reach fleet agents through cross-project edges (gd-fgMx8iN5Uh) and through the label-less config and advice a tenant agent reads (gd-c1DOWNYy7W). Both are fixed by keeping tenant writes and reads inside the land.
- **Supply chain (`npm install`, build scripts).** These run as the tenant agent in its pod. The pod fence (A2) is the control. The runc kernel is the accepted gap (gd-iyZom5u11A), and gd-NDJIOutLsv keeps it away from fleet pods.
- **Repo size.** Checked at setup and again at every Lands token mint and refresh (gd-i74esUHCTg, fixed). The pod's ephemeral-storage quota bounds the damage.

### A5. A land member against their own land's controls

Goal: dodge a freeze, overspend, or exceed the abuse caps.

| Control | Verified in | Verdict |
|---|---|---|
| Kill-switch fields, project status and title, and the `lands-pause` gate are admin-only on HTTP, gRPC and replay | `land_switch_authz.go:207-420`, `write_authz.go:503` | holds |
| Freeze/Thaw POST: admin plus a same-origin check | `commons/freeze.go:160-226` | holds |
| Scoped principals write nothing in a frozen land (except a person's post in `#support`) | `land_frozen.go:36-80` | holds for people |
| LiteLLM key born at `max_budget` 0, then set from the ledger | `provision.go:715-730` | holds |
| Spawn rate keyed by the land, shared by both doors | `spawnguard/keyed.go:65` | holds (in memory; a restart resets it) |
| **A frozen land's agents keep writing and can stop the watcher parking them** | `land_frozen.go:36`, `watcher.go:639-669` | **fails: gd-qpFpWcWucD (P1)** |
| **Per-member Commons spawn cap skipped for a member-made wish head** | `inbound_compose.go:628-641` | **fails: gd-PTOUU1ufzq (P2)** |
| `lands-pause` found by exact title; 20/min message cap at the land door too | `pausegate.go` Find, `land_door_message_rate.go` (gd-i74esUHCTg, fixed) | holds |

### A6. Identity: Keycloak and the door

- A tenant holds `gardenio-project-member` through `/project-<land>` and nothing else (B-D2). Provisioner groups carry a marker attribute and only that role (`keycloakadmin.go:314-344`).
- **Default-roles drift (gd-llFKMS8Pkc, 10-25).** A fresh user who holds `gardenio-viewer` and no project-member role is an unrestricted reader in both kbeads and the viewer. The live matrix (K1, K2 in `lands-fence-matrix.sh`) checks for this at check-in; the hourly `keycloak-runtime-role-drift` CronJob checks K1 continuously (read-only, view-realm client) and `KeycloakRoleDriftProbeFailing` alerts on it: gd-L2MyfEe9lL (P2). A grant through a realm default group is still seen only by K2 at check-in (operator decision gd-TgsaQWJPQ3).
- **The door.**
  - Clients cannot set `X-Land-Door`. Cookies are blanked, so the door is bearer-only and has no CSRF surface. Upstreams are fixed, so there is no SSRF.
  - The door still forwards the agent-only header family, admits a project-bound agent token for any project, and exempts `/v1/me` from the rate limit: gd-Utbyy4xJRX (P2).

### A7. The operator as a single point of failure

One person is the approver, the support desk, the incident responder and the only holder of admin rights (spec §9; `gardenio/docs/lands/incident-process.md`).

**Risks:**

- **Losing the operator's credentials loses the fleet.** `bridge_token:local-matthew-laptop` is a fleet-wide token, and until gd-zdo4DIeGWR ships any fleet agent can read it.
- **The operator's attention is attack surface.** Applicant text on decision cards (gd-TyXgeI2OGT) and tenant posts into operator threads (gd-KYeWPNJmvZ) both reach the one person who approves.
- **Impersonation ("Become", `gardenio/docs/lands/admin-impersonation.md`) turns the admin's whole browser into the tenant's session.** An XSS in tenant-rendered content during Become would act with the tenant's rights, not the admin's. That containment holds because Become refuses admins as targets.
- **When the operator is unavailable, a freeze is the only brake.** The freeze needs an OIDC admin: there is no break-glass path for a second person.

**Mitigations on `main`:**

- manual approval of every land (Q3);
- `gb lands freeze` and `gb lands pause` in one command, with the pause failing closed;
- `tenantSpawns` off until 10.10;
- the status page outside the cluster (10.8);
- no SLA promised for Open day (gd-yjw85aEUao).

**Accepted:** one operator for the private beta. The owner decides on a second admin, or a break-glass freeze token, before public registration (10.14).

## Findings

All are children of Phase 10, gd-Gx6arHBvzJ, and are labelled `security`.

| Bead | P | Live in the private beta? | Summary |
|---|---|---|---|
| gd-zdo4DIeGWR | 0 | **yes, fleet-wide today** | `GET /v1/configs?namespace=bridge_token` returns every persisted bearer, raw, to any per-agent token. Verified live: 12 agent tokens and the unbound laptop token. |
| gd-KYeWPNJmvZ | 1 | yes | A closed `thread-say` message bead posts into any Slack thread as the bot. |
| gd-fgMx8iN5Uh | 1 | yes | `parent_bead_id`, dependency targets and `X-Trigger-Bead` link to other lands' and the fleet's beads; an existence oracle as well. |
| gd-qpFpWcWucD | 1 | yes (laptop agents) | Per-agent tokens skip the freeze; an agent can rewrite its own park fields. |
| gd-zVTBvaSIdG | 1 | once the coopmux Secret is copied | coopmux registration trusts the caller's project, URL and account. |
| gd-ApgYe740ps | 1 | once the Secrets are copied (existing) | Fleet coopmux token, owner OAuth token and the shared NATS user in tenant pods. |
| gd-c1DOWNYy7W | 2 | tenant pods only | classAdminOnly routes, label-less config and advice, and no route-class gate on gRPC for tenant agent tokens. |
| gd-Utbyy4xJRX | 2 | yes | Door forwards agent headers, admits non-tenant agent tokens, and exempts `/v1/me` from the rate limit. |
| gd-PTOUU1ufzq | 2 | tenant spawns only | Commons per-member spawn cap bypass. |
| gd-TyXgeI2OGT | 2 | yes | Applicant purpose rendered as live mrkdwn on the operator's decision card. |
| gd-L2MyfEe9lL | 2 | yes | No continuous check on the default-roles drift. |
| gd-NDJIOutLsv | 2 | tenant spawns only | runc tenant pods share nodes with fleet pods and get the interactive PriorityClass. |
| gd-pXik57wZA9 | 3 | no | Pod fence defence in depth. |
| gd-i74esUHCTg | 3 | partly | Smaller fail-open paths: pause lookup, Commons cap, purge by name, repo size, land names, org URL forms. |

Already tracked elsewhere, so not re-filed: the existence oracles (gd-7APTAIhdC5, gd-ZJOUPZWuvs) and the abuse alerts that cannot fire on k3s (gd-Yz7JmMW7OL).

## How the review ran

- Five read-only reviewers, one per boundary: the door, the pod fence, abuse and kill switches, the cross-land data plane and Keycloak, and stranger repos and provisioning. Each reviewer traced every claim to code and listed the controls that held.
- Every finding above was then re-read on `main` by the 10.7 crew before it was filed. gd-zdo4DIeGWR was also probed live, with the crew's own per-agent token, printing only counts and key names.
- Matrix results:
  - `TestCrossLandMatrix` is green on `main` (ci.yml `build + test (kbeads)`, run 41392494967).
  - `lands-fence-matrix.sh` and its self-test run in `checks.yml` `script-selftests`. The live run belongs to the 10.10 check-in.
  - Neither matrix covers config namespaces, body-field targets or gRPC. gd-zdo4DIeGWR, gd-fgMx8iN5Uh and gd-c1DOWNYy7W each add those cases.

## Preconditions for Open day (11.4)

1. gd-zdo4DIeGWR merged and deployed. Then the operator rotates `local-matthew-laptop` and any non-expiring bridge token.
2. gd-KYeWPNJmvZ, gd-fgMx8iN5Uh and gd-qpFpWcWucD merged: they are reachable with tenant spawns off.
3. gd-TyXgeI2OGT and gd-Utbyy4xJRX merged, or accepted by the owner.
4. gd-L2MyfEe9lL in place, or the live K1/K2 check run on the day.

## Preconditions for tenant spawns (10.10 and later)

1. gd-ApgYe740ps and gd-zVTBvaSIdG: per-land coopmux and NATS credentials, sessions bound server-side, and no owner OAuth token in a tenant pod.
2. gd-c1DOWNYy7W and gd-NDJIOutLsv merged, or the owner reopens OQ4 for placement.
3. The live `lands-fence-matrix.sh` passes against two test lands, extended with the config-namespace and gRPC cases.

## Accepted residual risk

The owner accepted this list with the sign-off (gd-fBm2JuIYOq).

- **runc, not gVisor, for tenant pods.** The owner chose this (gd-iyZom5u11A). It is acceptable only with gd-NDJIOutLsv's placement.
- **The spawn-rate limiter is in memory.** A controller restart resets it. `max_live_agents`, the purse and LiteLLM's `max_budget` still bound spend.
- **DNS from tenant pods.** kube-dns service enumeration and DNS tunnelling to public resolvers stay possible. The internet egress rule is by design.
- **One operator** (A7) for the private beta.
- **The P3 findings** (gd-pXik57wZA9, gd-i74esUHCTg) do not block Open day. They are scheduled before public registration.

## Sign-off

**Signed off on conditions.** The owner chose `sign_off_conditional` on decision gd-fBm2JuIYOq at 2026-11-02 18:41Z. The other options were to hold the sign-off until the P0 and every P1 merged, or to let only the P0 block Open day.

The conditions, held as `blocks` edges on the plan's beads:

- **Open day, 11.4 (gd-BPwXK6CkZv)** waits on the three P1s that are live in the private beta: gd-KYeWPNJmvZ, gd-fgMx8iN5Uh and gd-qpFpWcWucD. The P0, gd-zdo4DIeGWR, merged as 57f9e1bc (PR #1947) before the sign-off. Its deploy and the token rotation (gd-5JAznVRbyR) are still precondition 1 above.
- **The 10.10 check-in (gd-B7SoiKwC40)** waits on every open finding in the table above.
- **The P3s** are scheduled before public registration: gd-i74esUHCTg blocks 11.7 (gd-RCemZ1XjE8). gd-pXik57wZA9 merged before the sign-off.

**Accepted residual risk**, as listed above:

- runc, not gVisor, for tenant pods (gd-iyZom5u11A), only together with gd-NDJIOutLsv's placement fix;
- the in-memory spawn-rate limiter;
- DNS from tenant pods;
- one operator for the private beta.

The sign-off covers the private-beta surface named in Scope, with `lands.tenantSpawns` off. The 10.1 tier is reviewed in 10.14 (gd-Ahv1F4wPUO), before public registration.

## 10.14: the lands-tenant tier and the gatehouse intake

> **Step 10.14** gd-Ahv1F4wPUO (owner decision gd-3Vx9UFk7Ad; scope widened on 10-31 to the gatehouse tools, gd-K62coPtKkX).
> **Scope:** the 10.1 `lands-tenant` tier (PR #1328): tier data and checks, the untrusted-input taint, tenant escalations refused to `#support`; the tier with `lands.tenantSpawns` on; and the gatehouse steward's apply and demo tools (PR #1851), which are off behind `land_gatehouse_tools`.
> **Reviewed at:** `main` 6bda2b786, 2026-11-02.
> **Gates:** public registration, 11.7 (gd-RCemZ1XjE8), and turning `land_gatehouse_tools` on.

### Verdict

**Signed off on conditions** by the owner, 2026-11-02 23:55Z (decision gd-VQCGKL4ztd, option `sign_off_conditional`). `lands.tenantSpawns` and `land_gatehouse_tools` stay off until every P0, P1 and P2 below has merged. The conditions are in [Sign-off (10.14)](#sign-off-1014).

The tier's own rules hold. A tenant agent cannot rewrite its role or project, spawns only `lands-gardener` as the steward, files only a merge escalation about its own land, and its denials are structured and never escalatable. The gatehouse intake holds too: no client can forge who wrote a message, so a pass or a draft always goes to a person the daemon verified.

The tier's weak point is its shape. Scoped people are held to an allow-list (`actionTable`), but tenant agents are held to a deny-list (`landsTenantFleetTypes`). Every bead type nobody thought of is open to them, and two of those types are acted on by the fleet without a project check. The second weak point is that the tier is keyed on the agent's role, not on its land, so an agent in a tenant land whose role holds no tier is not a tenant agent at all.

The review found:

- one P0, reachable in the private beta through any tenant steward: a tenant agent can make slack-bridge spawn a fleet agent with its prompt in `gardenio` (gd-bRLiEzvMsy);
- four P1s: two more tier escapes live in the private beta (gd-kwJ0jPMdNY, gd-a2s0GZExxr), one that goes live with `tenantSpawns` (gd-PtbbUxKWds), and a taint that spreads on one read path only (gd-WdnPW8TLPq);
- three P2s and five P3s, including the gatehouse switch being writable by fleet tokens (gd-Ymdaxee0sX).

### A8. A tenant agent against the fleet, through the tier

Goal: act outside its land through the daemon, using only its own per-agent token. With `tenantSpawns` off, the attacker is a tenant land's steward led astray by text a stranger wrote. With it on, it is any agent a land member starts.

| Control | Verified in | Verdict |
|---|---|---|
| Role, project and type of the caller's own agent bead are fixed for it; field endpoints refuse `role`/`project` | `spawn_authz.go:346-401, 519`, `beads.go:416` | holds |
| Steward spawns only `lands-gardener` in its own project; the target role holds exactly the tier | `lands_tenant_authz.go:234-268` | holds |
| Home-garden steward exemption needs `land_kind` unset or `operator`; the provisioner stamps `land_kind=tenant` in the create call, and only an admin or the provisioner writes it | `provision.go:652`, `land_switch_authz.go:41, 449-466` | holds |
| Escalations: only a merge about its own land; denial not escalatable; X-Escalate refused before anything is filed; spec frozen after approval | `restricted_ops.go:176-203`, `write_authz.go:807-815`, `escalation_approval_authz.go:612-660` | holds |
| Merge escalations run as the requester, in the tenant pod, with the Lands App token for the one repo; the merge desk handles only `gardenio` | `escalation_policy.go:99`, `github_app_credentials.go:252-310` | holds (nothing binds `mr_url` to the land's repo, but no fleet identity executes it) |
| gRPC `CreateBead` shares `createBead`; config and delete RPCs are admin-only | `beads_create_steps.go:575`, `interceptors.go:583-585` | holds |
| **Fleet types are a deny-list: `reaction-rule`, `widget-type`, `dashboard` and other un-policied types are open** | `restricted_ops.go:125-140`, `config.go:1094, 1696` | **fails: gd-bRLiEzvMsy (P0), gd-a2s0GZExxr (P1)** |
| **`fields.project` is checked on update only for scoped people, runners and agent beads** | `beads_update_steps.go:252`, `beads_create_steps.go:622` | **fails: part of gd-bRLiEzvMsy** |
| **A create may carry a second `project:` label; the per-agent check passes if any label matches** | `fleet.go:658`, `beads_create_steps.go:650` | **fails: gd-kwJ0jPMdNY (P1)** |
| The tier was keyed on the role, so a Commons `thread` agent in a tenant land was not a tenant agent | `lands_tenant_authz.go` (`tenantLandOfAgent`), `bridge/thread_anchor.go` (`commonsThreadRole`) | holds: an agent bound to a tenant land by its token or bead is a tenant whatever its role, and a Commons spawn there runs `lands-gardener` (gd-PtbbUxKWds) |
| Removing a bead's `project:` label; `LiveCap` matching `crew,steward` | `scoped_principal.go` (`tenantAgentProjectLabelDenial`), `wall/live.go` (`StewardSpawn`) | holds: gd-U9AV7jG8hF (P3) fixed |

The three escapes are verified with a unit probe against the cross-land fixture, using a land steward's per-agent token. It was not committed. Creating a `reaction-rule` and then re-pointing its `fields.project` at `gardenio` answered 201 then 200, a task labelled `project:gardenio` answered 201, and a `widget-type` answered 201. The bridge and viewer halves were traced in code, not run.

### A9. Stranger text, through the taint

The untrusted-input taint (`restrict`) is registered: owner team gd-s3GHigQOuk, since 10-31. No tenant land exists yet, so there are no untainted beads to backfill. A tenant agent cannot shed it: taints live in `bead_taints`, not in fields, a child agent inherits them, and lifting one is a four-eyes OIDC scrub (`http.go:249`).

A4 above calls the taint the prompt-injection control. That overstates it:

- **It spread on one read path** (gd-WdnPW8TLPq, P1, fixed). Only `GET /v1/beads/{id}` tainted the reader. Lists, search, gRPC, comments, events, the SSE stream and the other bead-returning reads now do too (taints ADR §15.7 names each). Text that never passes through a bead, such as a tenant repo's PR bodies and diffs, still taints nobody, and neither do the roster and the JetStream bus stream.
- **It refused only server replay** of create, update and close: gd-gaGdksntAG (P2). Fixed: a tainted fleet agent is now refused spawn, escalation approval and fleet-type and config writes too. A merge with the agent's own repo token is no daemon op, so the taint does not reach it.
- **Grants.** Approving a `grant` escalation skips the owner-team check, and the filer names the principal: gd-URd8GBIXvL (P2). The taint fails open with no alert, and replay resolves grants from the approver's spawn chain: gd-Ms3cmrdW49 (P3).

For tenant-tier agents the tier, not the taint, is the control that matters, which is why A8's deny-list is the P0. For fleet agents that read tenant text, the taint is today close to a label. The rule that holds is the one in A4: keep tenant text and tenant writes inside the land.

### A10. The gatehouse intake (`land_gatehouse_tools`)

The gatehouse steward is the first agent that reads free text from people who are tenants of no land. This section checks the claims in [gatehouse.md, "For the 10.14 review"](../lands/gatehouse.md#for-the-1014-review-the-free-text-and-taint-path).

| Control | Verified in | Verdict |
|---|---|---|
| Every message create is stamped with the verified GitHub account; clients may not set or change the fields on any create, update, close or field route, in any letter case | `gatehouse_request.go:258-331`, `beads_create_steps.go:792`, `beads_update_steps.go:312`, `http_bead_fields.go:137` | holds |
| The stamp's source: only OIDC tokens; `github_id` is admin-only in Keycloak and forced from GitHub at sign-in | `land_principal.go:110`, `keycloak/realm.yaml:193-202` | holds |
| Carve-out: only the gatehouse's steward, in its own land, create only, while both fields are on; one project label; born pending; no outcome fields; land-name pattern and reserved list; fixed fields; forward-only state | `gatehouse_request.go:130-246`, `restricted_ops.go` `opGatehouseRequest` | holds |
| A guest cannot file a request: the type is admin-only and no tier row grants it | `config.go:1428`, `action_table.go:134-138` | holds |
| Reactor: a message in the gatehouse, under a day old, stamped, terms accepted; the draft is bound to the `github_id` the reactor wrote | `lands_gatehouse_reactor.go:249-276, 352-377` | holds |
| Purpose text: `html/template` in the prefill, fenced on the operator's card; cross-site POSTs refused | `templates/lands.html:334-340`, `lands_apply.go:602-616, 881`, `lands_demo.go:284` | holds |
| Taint does not block filing a request | `gatehouse_request.go:220` | accepted: the steward is always tainted, so a taint gate would turn the tools off; binding to the stamped person is the control |
| **The switch is admin-only** | `land_switch_authz.go:40-46` lists only `land_kind` and `land_frozen*` | **fails: any unbound static or fleet agent token can flip it, gd-Ymdaxee0sX (P2)** |
| Requests are not rate-limited or de-duplicated; a pass nobody asked for joins, mails and counts against the cap | `gatehouse_request.go:130-183`, `provision/invites.go:195-234` | gd-EaTO1lFPAG (P3) |
| The cap and one-pass-per-account rely on an in-process lock | `lands_demo.go:166` | gd-GPvddfQY6y (P3, one replica today) |
| Every guest reads every request; every message in every land shows its writer's GitHub id | `read_authz_tenant.go:23`, `gatehouse_request.go:274-294` | gd-R7meWqNxqr (P3) |

The doc's worst case holds: a steward led astray can name another person's message from the last day. It understates the side effects of that (gd-EaTO1lFPAG).

### A7, continued: one operator

The 10.7 sign-off accepted one operator for the private beta, and left the choice of a second admin or a break-glass freeze token to this review, before public registration. Nothing on `main` has changed since: a freeze still needs the one OIDC admin. The 10.14 sign-off keeps one operator accepted until the 11.7 go/no-go, and adds no break-glass step.

### Findings (10.14)

All are children of Phase 10, gd-Gx6arHBvzJ, labelled `security` and `lands`.

| Bead | P | Live now? | Summary |
|---|---|---|---|
| gd-bRLiEzvMsy | 0 | yes, through any tenant steward | Tenant fleet types are a deny-list. A tenant agent writes a `reaction-rule`, re-points it at `gardenio`, and slack-bridge spawns a fleet agent with its prompt. `dashboard` reaches Grafana the same way. |
| gd-kwJ0jPMdNY | 1 | yes | A tenant agent creates a bead that also carries `project:gardenio`. |
| gd-a2s0GZExxr | 1 | yes | A tenant `widget-type` is rendered as trusted HTML in the operator's viewer; no CSP. |
| gd-PtbbUxKWds | 1 | fixed | Commons spawns in a land ran as `thread`, which holds no tier. The tier now also follows the land. |
| gd-WdnPW8TLPq | 1 | fixed | The taint spread only on `GET /v1/beads/{id}`; it now spreads on every bead-returning read (ADR §15.7). Repo text outside beads is still untainted. |
| gd-Ymdaxee0sX | 2 | yes | `land_gatehouse_tools` and `land_gatehouse` can be written by fleet tokens. |
| gd-gaGdksntAG | 2 | yes | Restrict refuses only server replay. |
| gd-URd8GBIXvL | 2 | yes | Grant escalations skip the owner-team check; the filer names the principal. |
| gd-EaTO1lFPAG | 3 | once the tools are on | No rate limit or dedup on gatehouse requests; unsolicited passes. |
| gd-GPvddfQY6y | 3 | with more than one viewer | Demo cap race across viewer replicas. |
| gd-R7meWqNxqr | 3 | partly | Gatehouse read exposure; GitHub id on every message. |
| gd-Ms3cmrdW49 | 3 | latent | Taint fails open silently; grant lookup walks the approver's chain. |
| gd-U9AV7jG8hF | 3 | fixed | A tenant agent may not drop a `project:` label (PATCH or remove); `LiveCap` exempts only the exact `steward` role, and a `crew,steward` agent holds a slot. |

### How the review ran

- Three read-only reviewers, one per surface: the tier's rules and their call sites; the taint and `tenantSpawns`; the gatehouse intake. Each traced its claims to code and listed what held.
- The 10.14 crew re-read every finding on `main` before filing it, and ran one throwaway unit probe for the three tier escapes (above). Nothing was probed live.
- Findings already filed by 10.7 were not re-filed: gd-c1DOWNYy7W, gd-fgMx8iN5Uh, gd-qpFpWcWucD, gd-zdo4DIeGWR.

### Preconditions

**Now, private beta (recommended to block 11.4 Open day, gd-BPwXK6CkZv):** gd-bRLiEzvMsy, gd-kwJ0jPMdNY and gd-a2s0GZExxr. Each is reachable through a tenant steward today.

**Before `land_gatehouse_tools` is turned on:** the above, plus gd-Ymdaxee0sX and gd-EaTO1lFPAG.

**Before `lands.tenantSpawns` is on and public registration (11.7, gd-RCemZ1XjE8):**

1. every P0, P1 and P2 above merged, or accepted by the owner;
2. the tier keyed on the land as well as the role (gd-PtbbUxKWds), with the 11.7 check that a tenant-spawned agent runs with tier rights;
3. a cross-land matrix case that a tenant agent creates and updates only the types on its allow-list;
4. the owner's choice on a second admin or a break-glass freeze (A7).

### Sign-off (10.14)

**Signed off on conditions.** The owner chose `sign_off_conditional` on decision gd-VQCGKL4ztd at 2026-11-02 23:55Z. The other options were the same conditions plus a break-glass freeze token or second admin blocking 11.7, holding the sign-off until the P0 and every P1 merged, or letting only the P0 block Open day.

The conditions, held as `blocks` edges on the plan's beads:

- **Open day, 11.4 (gd-BPwXK6CkZv)** waits on the three findings live through a tenant steward today: gd-bRLiEzvMsy (P0), gd-kwJ0jPMdNY and gd-a2s0GZExxr.
- **Public registration, 11.7 (gd-RCemZ1XjE8)** waits on every P0, P1 and P2: the three above, plus gd-PtbbUxKWds, gd-WdnPW8TLPq, gd-Ymdaxee0sX, gd-gaGdksntAG and gd-URd8GBIXvL.
- **`land_gatehouse_tools` and `lands.tenantSpawns`** stay off until the same eight have merged. No bead turns either on yet; whoever writes that step makes it blocked by these eight.
- **The P3s** (gd-EaTO1lFPAG, gd-GPvddfQY6y, gd-R7meWqNxqr, gd-Ms3cmrdW49, gd-U9AV7jG8hF) are done before 11.7 but do not block it.
- Items 2 and 3 of the 11.7 preconditions above (the tier keyed on the land; a cross-land matrix case for the allow-list) are done with gd-PtbbUxKWds and gd-bRLiEzvMsy.

**Accepted residual risk:** one operator (A7), until the 11.7 go/no-go, which decides it again.

The sign-off covers the 10.1 tier, the tier with `lands.tenantSpawns` on, and the gatehouse intake, as reviewed at `main` 6bda2b786.

## 10.10: the Fences check-in

> **Step 10.10** gd-B7SoiKwC40, Phase 10 Fences gd-Gx6arHBvzJ. Re-gated by owner decision gd-3Vx9UFk7Ad: `lands.tenantSpawns` stays off here, and turning it on moves to the public-registration gate 11.7 (gd-RCemZ1XjE8).
> **Checked at:** `main` 7dcc868ce (release 2026.307.2, cut 13:29Z); live release 2026.307.1, 2026-11-03 ~13:40Z.

### Findings

Every finding in the 10.7 table is closed: gd-zdo4DIeGWR, gd-KYeWPNJmvZ, gd-fgMx8iN5Uh, gd-qpFpWcWucD, gd-zVTBvaSIdG, gd-ApgYe740ps, gd-c1DOWNYy7W, gd-Utbyy4xJRX, gd-PTOUU1ufzq, gd-TyXgeI2OGT, gd-L2MyfEe9lL, gd-NDJIOutLsv, gd-pXik57wZA9 and gd-i74esUHCTg. So are the existence oracles, gd-7APTAIhdC5 and gd-ZJOUPZWuvs. All are live on 2026.307.1 except gd-ApgYe740ps (#1917, per-land coopmux token and credential), which ships in 2026.307.2. It matters only in a tenant pod, and no tenant pod exists yet (below).

Still open, and gating something other than 10.10:

- gd-5JAznVRbyR, rotating the bearers that gd-zdo4DIeGWR exposed: Open day precondition 1.
- gd-Yz7JmMW7OL, the abuse alerts that cannot fire on k3s: tracked outside the review.
- From 10.14: gd-PtbbUxKWds (P1, live only once `tenantSpawns` is on), gd-Ms3cmrdW49 (P3) and gd-EaTO1lFPAG (P3, merged in 2026.307.2). By the 10.14 sign-off they gate 11.7, not 10.10.

### Matrix

- **CI**, green on the live tag 2026.307.1 (`ci` run 41575049790, `checks` run 41575049805):
  - `TestCrossLandMatrix`, every daemon route;
  - `TestCrossLandMatrix_GRPC`, every RPC;
  - the viewer half (gd-cIux5tj6WI);
  - `lands-fence-matrix_test.sh`.
- **Live script.** `lands-fence-matrix.sh` gains D3, the gRPC case from precondition 3 for tenant spawns. Land pods may reach the daemon on 9090 (`landns.DefaultPeers`). From A's pod, on A's token, kd over gRPC must:
  - list A's own land, which is the positive control;
  - be refused `ListConfigs` and `GetConfig`;
  - list nothing of B's.

  The config-namespace case was already D1.
- **Live run: not yet possible.** The cluster has no `land-*` namespace, so no tenant land and no tenant pod exist. This was a read-only check by infra agent gardenio-t78q, 2026-11-03 ~13:40Z, commented on gd-B7SoiKwC40. The script needs two lands that each have a running pod. With `tenantSpawns` off, that pod is each land's steward.

  A test land comes from a plot application, which needs:
  - a GitHub account that is not the operator's (`LANDS_OPERATOR_GITHUB_ORGS`);
  - a repo with the Lands App installed;
  - the operator's approval;
  - a credit rate in force.

  That is operator work. It is the same work as the 11.2 dress rehearsal on a fresh test GitHub account.

### Go/no-go

The owner's go/no-go is decision gd-6YKmORLsj6, with three options:

- make two test lands now, and go on a live PASS;
- go now, with the live run moved after the 11.2 dress rehearsal and blocking 11.4 Open day;
- no-go.
