# Pre-publish security close-out: findings and design

Status: measured 2026-10-11 against `origin/main` at 04bd8ad3 (GitHub
`mattbakerconsulting/gardenio`), plus read-only checks on the live `gardenio-k3s`
cluster. Author: laptop-claude (fork of session fimba-cf) for the owner. Plan
epic: gd-5f5bJPKLhC (P7). Companion plan:
`docs/superpowers/plans/2026-10-11-pre-publish-security.md`. Findings bundle:
gd-I9dFR2el5V (doc gd-1lIzTlLZmr is the escalation field audit, "doc 11" below).

This document is the evidence and the decisions. The plan is the work. A crew
executing a plan task reads the task, then the sections of this document the
task cites. **§0 is binding.** Items marked `ASSUMPTION (owner question Qn)`
are the author's recommended default for a design question the owner has not
answered; §7 lists them. A crew implements the default unless the owner has
answered otherwise on the plan epic.

House wording for the public surface: escalations are **designed, exercised,
enforcement in progress**. Nothing in this plan, its PRs, or SECURITY.md may
call escalations "enforced" until Phase 4 has merged and been deployed, and even
then SECURITY.md lists what remains open.

## 0. Decisions (binding)

| # | Decision | Why |
|---|---|---|
| D1 | **Agents stop holding the primary static token.** Per-agent fleet tokens (`agents.perAgentTokens`) become the only beads credential in an agent pod; their subject is the agent's own name (the value agents send today as `X-Actor`), so every ownership gate keeps working. The coopmux agent token is split from `kd-beads-token`. Token provisioning failure refuses the spawn instead of falling back to the shared token. | Every crew pod mounts `kd-beads-token` three times (live check §1.1). The primary token can mint fleet tokens with any name and `privileged:true` (`kbeads/internal/server/http_fleet_tokens.go:332-342` (`isAdminRequest`)), so no allowlist of "trusted callers" means anything while agents hold it. Every other fix in this plan rests on this one. |
| D2 | **`X-Operated-By` is attribution, never authority.** The relay keeps feeding `operated_by` for audit. It stops satisfying any access-control check (team membership, grant resolution, taint management, decision resolver identity) unless the caller is a fleet token whose subject is on the bridge list (`BEADS_BRIDGE_SUBJECTS`, default `slack-bridge,gitlab-bridge,jira-bridge,github-bridge`). | This is the fix gd-opMHKHiy9I asked for. The escalation approve verb already refuses the relay (`kbeads/internal/server/escalation_approval_authz.go:70-76` (`escalationApprover`)); team, grant, taint, and decision paths still honour it (§1.3). |
| D3 | **An approver or human resolver is an OIDC identity from an approval-capable client, or a verified v2 edge identity.** Verified v1 edge envelopes (shared-HMAC, no roles) stop counting. ASSUMPTION (owner question Q4). | gd-opMHKHiy9I asked that the `edge` decision be made deliberately. v1 carries no roles and its key is shared by every signer; v2 carries Keycloak roles (PR #250). The `azp` allowlist for OIDC comes from PR #249 and is reused, not re-implemented. Today a verified v1 envelope IS accepted by `escalationApprover` (it accepts identity type `edge` whatever the auth method), and the beads viewer signs only v1 (gd-0ORn0Mhr4I), so whoever holds the shared HMAC key can approve. |
| D4 | **The Slack bridge web API fails closed.** With no `OIDC_ISSUER_URL`, `/api/*` routes other than the agent-facing ones return 403 unless `SLACK_BRIDGE_INSECURE_OPEN_API=1` (dev only). The bridge web routes that approve or reject an escalation by PATCHing `approved_by` are removed (410). ASSUMPTION (owner question Q7). | Live, the bridge has no `OIDC_ISSUER_URL` (§1.2), so `POST /api/escalations/{id}/approve` is open to every pod in the cluster and trusts a caller-supplied `X-Auth-Request-Email`. Slack buttons and the beads viewer remain the approval surfaces. |
| D5 | **IMDS is denied by NetworkPolicy, not hop limit.** One chart template renders, per listed namespace, an egress policy that allows everything except `169.254.169.254/32` and `fd00:ec2::254/128`. Pods labelled `gardenio.io/imds: allow` are exempt only in namespaces on an explicit list, and agent pods are never exempt. Hop limit 1 stays out of scope (breaks the autoscaler, memory `gardenio-imds-exposure`). | Every pod on the pod network can reach IMDS and assume the node role, which reads the admin kubeconfig from SSM. No NetworkPolicy exists in any workload namespace today except `claimlayer-demo` (§1.4). |
| D6 | **The IMDS block is proven continuously from CI.** A job on the `gardenio-k3s-light` runner (a pod in `arc-runners` on the same AWS nodes) asserts the IMDS token PUT fails and, as its positive control, that `https://api.github.com` answers. | A policy that exists is not a policy that works; k3s enforces NetworkPolicy through kube-router and the probe is the proof. CI runners are the population gd-9ip3Fsen5c is about. |
| D7 | **Escalation verbs get caller classes, not role ranks.** `requireRole` keeps its rank logic. A new class (`operator`, `bridge`, `agent`, `service`, `runner`, `none`) is computed once per request. `approve` and `reject` admit only `operator`. `result` admits `operator`, `bridge`, and an `agent` that is the server-recorded executor or, for verdict records, any agent that is not the requester (D9). `service` (the primary static token after D1: controller, viewer, coopmux, cronjobs, laptops without `kd login`) is refused on all three. | `requireRole(RoleOperator)` admits every non-OIDC caller to non-admin routes (`kbeads/internal/server/interceptors.go:143-147` (`roleRank[role] < roleRank[RoleAdmin]`)), so `/result` and `/reject` are open to every static-token pod (doc 11 defect 1). Agents need `RoleOperator` for ordinary bead CRUD, so the rank cannot simply be raised. |
| D8 | **The daemon owns every escalation decision field.** `approved_by`, `approved_by_email`, `approved_at`, `approval_method`, `policy_rule`, `executor`, `execution_dispatch`, `replay_in_progress`, `result`, `result_status`, `rejected_by` are written only by the daemon's own verbs. `operation_type`, `operation_spec`, `target_bead_id`, `execution_target`, `target_runner`, `spawn_role`, `execution_script`, `requires_four_eyes`, `approvers`, `sensitivity` are frozen once the escalation exists. Both rules apply on create, PATCH, and the field-edit endpoints. | Doc 11 defects 1, 3, 11. The `requesting_agent` immutability (`kbeads/internal/server/escalation_requesting_agent.go:59-77` (`checkEscalationRequestingAgentImmutable`)) is the pattern. The field-edit endpoints skip the escalation guards today (§1.5). |
| D9 | **Verdict records are a distinct, never-executed class.** `github_pr_merge`, `gitlab_mr_merge`, `github_pr_review`, `doc_review` are never replayed, never dispatched, never cascade, and do not require `operation_spec`. Their result may come from an operator, a bridge, or an agent that is not the requester and not spawned by it. ASSUMPTION (owner question Q1). | 147 of 200 live escalations are these types; they are the merge desk's verdict record, resolved by a reviewer posting a result. Treating them as executable operations is what makes "required `operation_spec`" and "executor" meaningless today (doc 11 §live data). The agent-review plan's gd-S12o3mOtxg later narrows "any non-requester agent" to "the assigned reviewer". |
| D10 | **One executor, one claim, chosen by the daemon inside the approve handlers.** The daemon computes `execution_dispatch` (`server`, `local`, `service_account`, `none`) and claims it atomically before anything executes. The bridge's "non-empty `approved_by` means execute" trigger is deleted. `server` stays the replay path. `local` is executed only by the local agent named in `target_runner`. `service_account` is executed by an agent the daemon itself creates with the privileged role named in `spawn_role`, through the approved-spawn replay path; the bridge executes nothing. No fallback from local to spawning a crew. ASSUMPTION (owner questions Q2, Q3). | Doc 11 defects 3 to 7. `gd-mQrOElVxhd` ran twice (server replay and bridge spawn). |
| D11 | **Four-eyes on every verb, set by policy.** `requires_four_eyes` is set to true by the daemon at create when `sensitivity` is `sensitive` or `secret`, and is checked on `reject` and `result` as on `approve`. Replay-unfit operation types (decision gd-UQTnDZATTU) may not name any executor: they are filed only as manual requests (no `execution_target`), which the daemon never dispatches. ASSUMPTION (owner questions Q6, Q10). | Doc 11 defects 7 and 8; spec §8.1 of the local-agents design. |
| D12 | **Approve, result, and reject write mutation history; provenance is derived.** Each merged field gets a `mutation_history` row. `approval_method` is derived from the authenticated identity (`web_ui` for OIDC, `edge` for a verified v2 envelope, `policy` for the spawn policy); the body value is logged and ignored. `operation_spec` must be a non-empty object for every escalation the daemon would dispatch (a create/update/close/grant replay, or one naming `execution_target`). Verdict records and manual requests may keep `{}`. | Doc 11 defects 9, 10, 11. Doc 11 marked the `operation_spec` cause PLAUSIBLE as "validateFields checks Required only when the fields blob is empty"; that is **refuted**: `kbeads/internal/model/validate_fields.go:130-140` checks `Required` on every non-empty blob too. The real cause is the client: `gb escalation create` writes `{}` when `--operation-spec` is omitted (`gardenio/controller/cmd/gb/escalation_create.go:63-70`), and `{}` is present and non-nil. |
| D14 | **No Secret keeps a cleartext copy in `kubectl.kubernetes.io/last-applied-configuration`.** A one-off OPERATOR sweep strips the annotation from every Secret (names and counts only, never values). A names-only check script with a planted positive control, and a CI lint that fails any script or runbook code block running `kubectl apply` on a Secret (including the `create --dry-run -o yaml \| kubectl apply -f -` pipe), keep it from coming back. Runbooks use `kubectl create` for new Secrets and `kubectl replace` for rotation. | Operator add-on 2026-10-11 (§1.6). The annotation holds the whole Secret, lands in `-o yaml` dumps, backups, and support bundles, and a later `apply` can restore an old value from it (a PAT was resurrected this way before). |
| D13 | **SECURITY.md is the last task and states what is open.** It describes the threat model, what P7 closed (with the test that proves each), what stays open and why, and how to report. It uses the house wording for escalations. | Gates P1's publish step. A reviewer reads this file first. |

## 1. What a technical reviewer finds today (measured)

### 1.1 The shared token is in every agent pod (live, read-only)

`kubectl -n gardenio get pod crew-gardenio-crew-regression-guard-tests-ekc -o json`,
secretKeyRef names only:

| Env var | Secret |
|---|---|
| `BEADS_DAEMON_TOKEN`, `BEADS_AUTH_TOKEN`, `BEADS_REFRESH_TOKEN` | `kd-beads-token` |
| `COOP_MUX_TOKEN`, `COOP_BROKER_TOKEN` | `kd-beads-token` |
| `SLACK_BRIDGE_TOKEN` | `slack-bridge-agent-token` |
| `GIT_TOKEN`, `GIT_USERNAME` | `gardenio-git-credentials` |
| `CLAUDE_CODE_OAUTH_TOKEN` | `claude-oauth` |

`agents.perAgentTokens` defaults to false (`gardenio/helm/gardenio/values.yaml:4070-4072`) and the
k3s overlay does not set it. When it is on, a provisioning failure falls back
to the shared token (`gardenio/controller/cmd/controller/main.go:2622-2631`,
`gardenio/controller/cmd/controller/main.go:2990-2999`). The coopmux auth token
defaults to the beads token (`gardenio/helm/gardenio/templates/_helpers.tpl:418-424`
(`gardenio.coopmux.authTokenSecretName`)), and coopmux uses the same secret as its own
daemon bearer (`gardenio/helm/gardenio/templates/coopmux/deployment.yaml:194-198` (`BEADS_TOKEN`)),
so splitting the agent-facing token needs that line changed too.

Per-agent tokens today use subject `agent:<beadID>`
(`gardenio/controller/cmd/controller/agenttoken.go:54-60` (`agentTokenSubject`)), which no
kbeads code resolves back to an agent name, and the fleet branch of
`AuthMiddleware` drops the agent-pod headers `X-Trigger-Bead` and
`X-No-Auto-Parent` that the static branch honours
(`kbeads/internal/server/interceptors.go:866-870`, `kbeads/internal/server/interceptors.go:907-922`).
Turning the flag on as-is would break ownership checks and trigger attribution.
There is no e2e coverage of `perAgentTokens=true`.

### 1.2 The Slack bridge web API is open in-cluster (live, read-only)

The live `gardenio-slack-bridge` Deployment env names include
`SLACK_BRIDGE_AGENT_TOKEN` and no `OIDC_ISSUER_URL`; the chart template never
sets one. `bridgeRBAC` (`gardenio/controller/cmd/slack-bridge/main.go:1363-1433`
(`func bridgeRBAC`)) therefore runs regular `/api/*` routes through its "insecure dev
fallback" (`:1412-1417`). `POST /api/escalations/{id}/approve`
(`gardenio/controller/internal/bridge/api_escalations.go:226-312` (`handleApprove`)) reads the
caller's email from `X-Auth-Request-Email` (`gardenio/controller/internal/oidcauth/oidcauth.go:100-105`
(`EmailFromRequest`)), looks up that user's `approve:escalation` capability, and
PATCHes `approved_by` on the escalation, which the bridge's own `handleUpdated`
then executes. The Service `gardenio-slack-bridge:8090` is ClusterIP and no
NetworkPolicy exists in `gardenio`. **CONFIRMED by code and live env; not
exercised** (exercising it would approve a real escalation). The agent-facing
routes are gated by the shared agent token, which every crew holds by design;
that is a documented residual (§6), not this plan's fix.

The e2e suite encodes the forged-header approval as a passing scenario
(`gardenio/tests/e2e/scripts/test-escalation-execution.sh:332-333`,
`gardenio/tests/e2e/scripts/test-escalation-approval.sh:19-26`).

### 1.3 The relay still grants authority outside the approve verb

`X-Operated-By` from a static or fleet token is accepted on shape alone
(`kbeads/internal/server/audit.go:631-648` (`sanitizeOperatedByHeader`)). It feeds:

| Consumer | Effect today |
|---|---|
| `kbeads/internal/server/team_membership.go:123-143` (`teamsOf`) via `TeamsOf` | team membership for taint management and grant resolution |
| `kbeads/internal/server/taint_read_gate.go:83-93` (`grantsOfOperator`) | which tainted beads the caller may read |
| `kbeads/internal/server/http_taint_grants.go:197-206` (`granterOperatorID`) | who recorded as granting a taint |
| `kbeads/internal/server/http_decisions.go:330-348` (`decisionAttribution`) | `responded_by` / `closed_by` on a decision |

The escalation approve verb and team-approver check already refuse it
(`TeamsOfDirect`, `kbeads/internal/server/team_membership.go:119-121`), and scrub-request
approval refuses it (`scrubApprover`).

### 1.4 IMDS (live, read-only)

Namespaces with pod-network pods: `arc-runners`, `arc-systems`, `beads`,
`cert-manager`, `claimlayer-demo`, `gardenio`, `headlamp`, `kube-system`,
`operators`, `public-edge`, `ziti`, plus `gc-dogfood` and `default` (empty now).
NetworkPolicies exist only in `claimlayer-demo`. Pods that legitimately use the
node role over IMDS from the pod network:

| Workload | Namespace | Pod labels today | Source |
|---|---|---|---|
| `ecr-token-refresh` CronJob | gardenio | **none** (pod template has no metadata) | `gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml:151-157` (`jobTemplate:`) |
| `gardenio-postgres-s3-backup` CronJob | gardenio | `app.kubernetes.io/component: postgres-s3-backup` | `gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml:39-43` |
| `dolt-backup` CronJob | beads | `app: dolt-backup` | `gardenio/helm/beads/dolt-backup-cronjob.yaml:23-26` |
| `ziti-controller` (`s3-backup` sidecar) | ziti | `app: ziti-controller` | `gardenio/helm/ziti/controller.yaml:168-171` |
| cluster-autoscaler, aws-cloud-controller-manager | kube-system | | not covered: kube-system gets no policy |

`aws-node-termination-handler` and the ziti routers run `hostNetwork` and are
not subject to NetworkPolicy. A stray `ziti/node-debugger-ip-10-90-5-163-*` pod
(a leftover `kubectl debug node`, `hostNetwork`) is running; it is reported, not
touched. No workflow in `.github/workflows/` references `aws`, `AWS_`, or
`169.254` (grep on 04bd8ad3), so denying IMDS to runner pods breaks no CI job
that is visible in the repo.

The chart already ships a full NetworkPolicy suite behind
`networkPolicies.enabled` (`gardenio/helm/gardenio/values.yaml:15535-15550`), off in the
k3s overlay. It is not used here: its default-deny egress would need a soak.
The IMDS policy is a separate, allow-all-but-one template safe to enable alone.

### 1.5 Escalation lifecycle (doc 11, re-verified on 04bd8ad3)

Doc 11's defects reproduce in code with these current anchors (several doc 11
line numbers have drifted):

| Defect | Current anchor |
|---|---|
| `requireRole` admits non-OIDC to operator routes | `kbeads/internal/server/interceptors.go:143-147` |
| result merges arbitrary `fields` first, trusts `executor`, `execution_target` | `kbeads/internal/server/http_escalations.go:727-749` (`for k, v := range req.Fields`) |
| reject trusts `rejected_by` as audit actor and `closed_by` | `kbeads/internal/server/http_escalations.go:643-670` (`closedBy := req.RejectedBy`) |
| PATCH carve-out for escalations and decisions | `kbeads/internal/server/write_authz.go:482-487` (`bead.Type == model.TypeEscalation`) |
| only `requesting_agent` immutable | `kbeads/internal/server/escalation_requesting_agent.go:59-77` |
| field-edit endpoints skip the escalation and scrub-request update guards (new finding) | `kbeads/internal/server/http_beads.go:1152-1193` (`handleEditBeadSubField`); append `:1006`, reorder `:1080` |
| bridge executes on `approved_by != ""` | `gardenio/controller/internal/bridge/escalations.go:175-221` (`approvedBy := bead.Fields["approved_by"]`) |
| empty target defaults to local; local falls back to spawning | `gardenio/controller/internal/bridge/escalations.go:380-393` (`triggerExecution`), `:494-569` (`triggerLocal`) |
| `service_account` spawns a plain `crew` | `gardenio/controller/internal/bridge/escalations.go:409-489` (`triggerServiceAccount`) |
| local watcher takes any approved local escalation in its project | `gardenio/controller/cmd/gb/agent_start_escalation.go:250-263` |
| merges skip mutation history | `kbeads/internal/store/postgres/queries.go:1551-1570` (`queryMergeBeadFields`) |
| `approval_method` from client body | `kbeads/internal/server/http_escalations.go:126-135` |
| replay claim lives in a client-writable field | `kbeads/internal/store/postgres/escalation_replay_claim.go:25-43` (`replay_in_progress`) |

The field-edit endpoints (`POST /v1/beads/{id}/fields/{field}/append|reorder|edit`
and `/increment`) share one gate, `tokenAuthorizedForBead`
(`kbeads/internal/server/http_beads.go:905-935`), which checks project scope and
the type:user identity fields, then writes straight to the store. They skip the
PATCH path's per-type guards: the escalation `requesting_agent` guard and the
scrub-request update guard. P7 closes them for escalations in that one gate
(Task 11); the scrub-request gap is reported for its owner. Confirmed by code
read; not exercised.

### 1.6 Secret values in `last-applied-configuration` (live, read-only, names only)

`kubectl get secrets -A -o json`, filtered with jq to names whose metadata carries
`kubectl.kubernetes.io/last-applied-configuration`: 3 of 129 Secrets, namely
`gardenio/aws-cost-auditor`, `gardenio/gardenio-forwardauth-secrets`, and
`ziti/router-aws-1-jwt`. fimba-c6 stripped `gardenio-grafana-admin` and
`gardenio-owner-initial-password` the same day. Exposure is limited to callers
who can already read Secrets in that namespace. In tracked scripts and runbook
code blocks, one line pipes a generated Secret into `kubectl apply`
(`gardenio/docs/runbooks/websocket-auth-revocation.md:132`); no other match.

## 2. Overlap with the one-identity lane (fimba-c6, fimba-e6)

| Identity item | What it already does | P7's stance |
|---|---|---|
| PR #249 / gd-bFFquoScHy (`kd login` device grant) | `escalationApprover` accepts OIDC only from an `azp` allowlist; device-grant tokens cannot approve; `AuthIdentity.ClientID` | **Depend.** P7 reuses `escalationApprover` for reject and result instead of writing a second resolver. Tasks 3 and 9 are blocked by it because they edit or lean on the same function. Humans resolve escalations from a laptop only after `kd login`. |
| PR #250 / gd-7CDM9gdog0 (Slack acts as the mapped person) | Slack interactions sign a v2 edge envelope with the person's Keycloak roles | **No dependency; compatible.** Slack has no escalation approve or reject buttons ("Approval happens in the web UI", `gardenio/controller/internal/bridge/bot_escalations.go:767`). The v2 envelope #250 signs is exactly the edge form D3 keeps. |
| PR #251 / gd-pjKksLDLWM (`viewerCanAdmin` fails closed) | viewer UI no longer shows admin mode to roleless sessions | **Verify only.** SECURITY.md cites it. No P7 code touches the viewer. |
| gd-0ORn0Mhr4I (viewer writes as the operator) | viewer's write path carries the user's own bearer | **Depend.** Task 3 removes the static token's relay authority and Task 9 refuses the static token on reject; the viewer uses both today (`gardenio/controller/cmd/beads-viewer/escalation_actions.go:244-252`). Both are blocked by gd-0ORn0Mhr4I so viewer actions keep a real identity. P7 does not give the viewer its own credential. |
| gd-l1Jh9aFEXY (kbeads OIDC verifier disabled since boot) | nothing yet | **Depend.** Until the daemon verifies OIDC, no human can approve, reject, or resolve over OIDC. Task 9 refuses static callers on reject and result, so it is blocked by this. |
| gd-MBsUUXpJZB (owner provisioned as gardenio-admin) | nothing yet | **Depend.** Same reason as gd-l1Jh9aFEXY; blocks Task 9. |
| gd-0DMfxQdIow (alias self-link), PR #246 / gd-uAvleAZUvV | closed and merged: aliases admin-gated | **Verify only.** The relay maps through `operator_aliases`; SECURITY.md cites both. |
| P4 Task 8 (`RoleRunner`, gd-9toFMX9JjW) | not started | **P7 first.** Task 9 here adds the `runner` caller class as a reserved value; P4 Task 8 fills in its route allowlist. |
| P4 Task 16 (gd-UAUvpQPOcj) and 17 (gd-yNRhwrTKEN) | not started; plan already names P7 Phase 4 as precondition | **P7 first.** Task 13 declares `target_runner` and the create-time refusal; P4 Task 16 widens `target_runner` from "a local agent bead" to "a runner bead" and must not re-declare the field. Task 12 deletes `triggerLocal`/`spawnLocalAgent`; P4 Task 17 will find them gone. |
| P4 Task 4 (gd-FNpCYoB8zo, `control:` mail rule) | not started | **Shared list.** Both use the bridge fleet list; P7 names the env `BEADS_BRIDGE_SUBJECTS`, P4 Task 4 may read it instead of its own `KBEADS_CONTROL_MAIL_FLEETS`. |
| agent-review gd-S12o3mOtxg (reviewer authz) | not started | **P7 first, then narrows.** D9 admits any non-requester agent for verdict records; gd-S12o3mOtxg later restricts that to the assigned reviewer. |

## 3. Phase 1: token path

**Agent identity (Task 1).** A fleet token whose fleet name starts with
`agent-` (`agentTokenBridgeName`, `gardenio/controller/cmd/controller/agenttoken.go:46-48`)
is an agent identity. Its subject is the agent name. On that branch the daemon
honours `X-Trigger-Bead` and `X-No-Auto-Parent` (as for the static branch) and
`X-Operated-By` (attribution, already honoured for fleet), and ignores
`X-Actor`, `X-Fleet`, `X-System-Operator`, and `X-Spawn-Privileged-Override`.
`applyEscalationRequestingAgentGuard` stamps `requesting_agent` from it.
Provisioning failure refuses the spawn.

**Rollout (Task 2).** `agents.perAgentTokens: true` and
`coopmux.coopmuxTokenSecret: coopmux-agent-token` in the k3s overlay; coopmux's
own daemon bearer stays the beads token. The operator creates the
`coopmux-agent-token` Secret and rolls out through a release. Verified on a fresh
crew pod: no secretKeyRef names `kd-beads-token`.

**Relay (Task 3).** `relayTrusted(ctx)`: identity type `fleet` and subject on
`BEADS_BRIDGE_SUBJECTS`. `teamsOf`, `grantsOfOperator`, `granterOperatorID`,
and `decisionAttribution` consult the relay only when `relayTrusted`. The static
token's relay still sets `operated_by` for audit. `escalationApprover` refuses
verified-v1 edge (D3).

**Bridge web API (Task 4).** D4. The e2e scenarios that forge
`X-Auth-Request-Email` become negative controls.

**Secrets (Task 5).** D14. A names-only check
(`scripts/security/check-secret-last-applied.sh`, with `--strip` for the
operator) and a repo lint (`scripts/security/lint-kubectl-apply-secret.py`),
both with planted positive controls, wired into `script-selftests`. A new
runbook, `gardenio/docs/runbooks/secrets-create-not-apply.md`, states the
create/replace rule. The live sweep is an OPERATOR step.

## 4. Phase 2: IMDS

Template `gardenio/helm/gardenio/templates/networkpolicies/imds-deny.yaml`, gated
by `imdsDeny.enabled` (independent of `networkPolicies.enabled`). For each
namespace in `imdsDeny.namespaces` it renders:

- `<ns>-imds-deny`: selects pods **without** `gardenio.io/imds: allow` (or all
  pods, when the namespace is not in `imdsDeny.exemptLabelNamespaces`); egress
  allowed to `namespaceSelector: {}` (all in-cluster pods) and to
  `0.0.0.0/0 except 169.254.169.254/32` and `::/0 except fd00:ec2::254/128`.
- `<ns>-imds-deny-agents` (only in exempt-label namespaces): the same rule
  selecting `gardenio.io/agent` Exists, so an agent pod stays denied even if it
  carried the exemption label. Agent pod labels are a fixed set
  (`gardenio/controller/internal/podmanager/manager.go:884-899` (`func (s *AgentPodSpec) Labels`)).

Defaults: namespaces `gardenio, arc-runners, arc-systems, beads, gc-dogfood,
operators, default, headlamp, public-edge, cert-manager, ziti`;
exempt-label namespaces `gardenio, beads, ziti`. The four exempt workloads in
§1.4 gain the label in their pod templates (ecr-token-refresh gains a metadata
block first, per the gd-fTCYDqMYeH ruling comment). Raw manifests in
`gardenio/helm/gardenio-ziti/`, `gardenio/helm/beads/`, and `gardenio/helm/ziti/`
are applied by the operator. Order is fixed: labels first, confirm on a live
pod, then the policy.

## 5. Phase 4: escalation authorization

### 5.1 Caller classes (Task 9)

`callerClassFromContext(ctx)`:

| Class | Identity |
|---|---|
| `operator` | `authMethodOIDC` with `RoleOperator` or above and identity type `oidc` or `edge` (edge is OIDC-method only when verified v2) |
| `bridge` | identity type `fleet`, subject on `BEADS_BRIDGE_SUBJECTS` |
| `agent` | identity type `fleet`, fleet name prefix `agent-` |
| `runner` | reserved for P4 Task 8 (`gr_` tokens) |
| `service` | identity type `static` (primary token, including the `X-Actor` override) |
| `other` | authenticated with no escalation standing: OIDC below operator, verified v1 edge, any other fleet token |
| `none` | `authMethodNone` (auth disabled; dev and unit tests) |

`requireEscalationResolver(verb, handler)` replaces `requireRole(RoleOperator, …)`
on `/reject` and `/result`. `none` passes (matches `requireRole`'s auth-disabled
bypass). `/approve` keeps `escalationApprover` inside its handler.

### 5.2 Result and reject (Task 10)

- Reject: `operator` only; `rejected_by`, `closed_by`, audit actor =
  `escalationApprover` identity. Requester-as-resolver refused. Four-eyes per
  D11 (Task 14).
- Result: `closed_by` = the caller's identity (operator subject, agent name,
  bridge subject); `executor` = the dispatched executor when one was recorded,
  otherwise the caller; `execution_target` from the bead, never the body;
  `result_status` from the body (it is the executor's report) but only from an
  admitted resolver; `fields` limited to the reaper's render and witness keys
  (`reaped`, `reaped_*`, `mr_*`, `approvals_*`) and only from `bridge`; any other
  key is a 400. The reaper stops sending `approved_by`, `executor`, and
  `result_status` in `fields` (its approvals witness key becomes
  `mr_approved_by`, its satisfier `reaped_satisfier`); its card already renders
  "Complete" for `result_status=success`
  (`gardenio/controller/internal/bridge/escalations.go:257-262`).
- Agent result admission: the caller is the bead's server-recorded `executor`,
  or the escalation is a verdict record (D9) and the caller is neither the
  `requesting_agent` nor an agent whose bead's `spawned_by` is the requesting
  agent. ASSUMPTION (owner question Q8): "an agent spawned from this escalation"
  in local-agents spec §8.1 is read as "spawned by the requester"; the executor
  the daemon spawns for a `service_account` escalation is the one agent that
  must be able to post its result.

### 5.3 Immutability (Task 11), dispatch (Task 12), targets (Task 13)

D8, D10. `execution_dispatch` is the claim. The bridge reacts to nothing on
escalation update except pingback, queue alert, cross-operator notices, and
Slack card updates. Local: `target_runner` required when
`execution_target=local`; until P4's runner beads exist it names the executing
local agent's bead (an open `type=agent` with `execution_target=local`).
ASSUMPTION (owner question Q2). Dispatch values: `server` (a create/update/
close/grant replay with a spec), `local`, `service_account`, and `none` (verdict
records and escalations that name no executor; a human or reviewer posts the
result). A repeated `server` dispatch proceeds so a failed replay can be
retried; any other repeat is a 409. The watcher acts only when
`execution_dispatch=local` and `target_runner` is its own agent bead id, and no
longer writes `executor`. Service account: `spawn_role` required and must name
a role with `privileged=true`, and a reviewed `execution_script` is required (as
the bridge required before); the daemon creates agent bead
`escalation-sa-<id>` with that role through `withApprovedSpawnReplay`, stamps
`executor` server-side, and the bridge's `triggerServiceAccount` is deleted.
ASSUMPTION (owner question Q3). Until gd-KRHGbBJqRN lands, the privileged role
mounts what it mounts today.

### 5.4 Four-eyes, replay-unfit, history, provenance (Tasks 14, 15)

Replay-unfit set: operation types with prefix `terraform_` or `aws_`, and
`coopmux_account_sync`, `git_push_public`. ASSUMPTION (owner question Q6):
`k8s_*` types stay allowed because their artifact (a manifest or chart values)
can be pinned in `operation_spec`; the decision named "terraform apply / infra
changes", live account re-sync, and unpinned public push.

## 6. What stays open after P7 (goes into SECURITY.md)

- The shared Slack agent token (`SLACK_BRIDGE_TOKEN`) is held by every crew; any
  crew can post, upload, and delete bot messages through `/api/slack/*`.
- ~~The primary static token is still held by the controller, beads-viewer,
  coopmux, the release deployer, backup jobs, and the owner's laptop; whoever
  holds it can mint fleet tokens.~~ **Superseded (gd-ke6PC7bRto):** kbeads no
  longer accepts a shared static token at all, so there is nothing to hold and
  nothing that mints with it; fleet tokens are minted only by a machine
  principal with the token-mint capability. This closes only when the roll and
  the Secret deletion in `gardenio/docs/kd-beads-token-retirement.md` (Stage 4)
  have happened; until then the controller's default client and coopmux still
  hold it (gd-BDirD4F7Iq, gd-in7EZvvd3T). SECURITY.md (gd-0aRM4lYkfY) should
  state the post-roll state, not this bullet.
- Per-agent tokens are project-scoped; cross-project agents need a different
  credential (not built).
- The node IAM role is still broad (SSM admin kubeconfig, Route53, S3 backups);
  IMDS denial removes pod access, not the role.
- Field-edit endpoints still skip the scrub-request update guard (reported,
  owned elsewhere).
- Local execution results: the local watcher can post a result only with a
  per-agent token naming the stamped executor or with an operator's `kd login`
  credential; a laptop agent on the primary token is refused.
- `gb` and the CLIs accept the relay header on the static token for attribution;
  audit attribution from the static token is a claim, not proof.
- Escalations: designed, exercised, enforcement in progress until Phase 4 is
  deployed and `kd login` / Slack act-as-person are live for humans.

## 7. Owner questions (each implemented with the stated default)

| Q | Question | Recommended default | Why |
|---|---|---|---|
| Q1 | Treat `github_pr_merge`, `gitlab_mr_merge`, `github_pr_review`, `doc_review` as never-executed verdict records? | Yes | 147 of 200 live escalations; keeps the merge desk working under D7 without inventing an executor for a verdict. |
| Q2 | Until runner beads exist, may `target_runner` name a local agent bead? | Yes | Otherwise every `execution_target=local` escalation is refused until P4 M1; naming the agent bead is still explicit, which is the whole fix. |
| Q3 | `service_account` = daemon creates an agent with the privileged role in `spawn_role` via approved-spawn replay; refuse without one? | Yes | Reuses the spawn-escalation machinery and provenance stamps; removes the bridge as an executor. |
| Q4 | Stop accepting verified v1 edge envelopes as an approver or resolver? | Yes | v1 carries no roles and a shared HMAC; v2 (PR #250) replaces it for Slack. |
| Q5 | Accept that the viewer's static token loses relay authority (taint reads, decision attribution) and rely on gd-0ORn0Mhr4I for viewer identity? | Yes, Task 3 waits for gd-0ORn0Mhr4I | Avoids a P7 change in the identity lane's files; the viewer's per-user bearer is the right identity. |
| Q6 | Replay-unfit set = `terraform_*`, `aws_*`, `coopmux_account_sync`, `git_push_public`; `k8s_*` allowed? | Yes | Matches the decision's named capabilities; `k8s_*` artifacts can be pinned. |
| Q7 | Remove the bridge's web approve/reject (410) and fail closed without OIDC? | Yes | No ingress exposes it; it is reachable only from pods, i.e. only by agents. |
| Q8 | Read "agent spawned from this escalation" as "spawned by the requester"? | Yes | The daemon-spawned executor must be able to post its own result. |
| Q9 | SECURITY.md reporting channel = GitHub private vulnerability reporting, no email? | Yes | No personal address in the public repo; GitHub routes it to the owner. |
| Q10 | Four-eyes by policy only for `sensitivity` `sensitive` and `secret`? | Yes | Local-agents spec §8.1 says exactly this; widening it needs a second human who does not exist yet. |

## 8. Could not verify in this session

- Whether kube-router on this k3s build enforces `ipBlock.except` for
  link-local destinations. The CI job (D6) and the operator probe are the check.
- Whether any agent role reads beads outside its own project (a per-agent token
  is project-scoped). Task 2's e2e module and the first canary crew are the check.
- Whether the bridge `/ui/` SPA has users. No ingress serves it; Task 4 removes
  only the approve and reject actions.
- The live state of the e2e escalation scenarios: `test-escalation-crud.sh`
  Scenario 4 still documents static-token approval, which
  `escalationApprover` has refused since gd-ZjiSd9Kzio round 3.

- Whether `gb` (used by the local escalation watcher) presents a `kd login`
  credential. If not, local execution results need a per-agent token on the
  laptop agent; Task 13's help text says so and P4 runner tokens replace it.
- Whether any live escalation PATCHes a field D8 guards from a client not found
  by the grep in Task 11 step 4 (for example a script outside the repo).

## 9. Non-goals

- IMDS hop limit, node role narrowing, IRSA (terraform lane).
- Viewer credentials, Keycloak, `kd login`, Slack act-as-person (identity lane).
- `RoleRunner` routes, mail delivery, runner beads (P4).
- The reviewer-assignment policy (agent-review plan).
- Any live-cluster change by a crew. Operator steps are marked.
