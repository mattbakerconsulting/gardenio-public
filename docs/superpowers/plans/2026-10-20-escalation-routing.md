# Escalation Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every escalation says who may approve it, who executes it, and who it is waiting on right now. The owner sees only what truly needs them. Coordinators (the laptop operator, the merge desk, a land's steward) approve and execute what policy lets them, in both the beads viewer and Gardenio: Lands.

**Owner direction (2026-10-20, laptop chat):**
- "one thing im seeing in my UX is a lot of escalations that are for 'me' but really they are for you, laptop agent. hwo should we resolve this in code and ux long term?"
- "we want laptops/agents to be able to approve most MRs, btw; and you'll not that's what we've been doing for weeks. and coordinators like yourself or the currently-being-built coordinator role shoudl be able to approve MRs if allowed"
- "make sure we address gardenio lands and beads viewer both"
- "yeah make it a plan … once you understand the full picutre and can create tasks"

**Builds on PR #756 (merged 2026-10-21, `399f1ecfe`, bead gd-bmOiteQi36).** #756 shipped the display side: a server-stamped `audience` field (`owner` | `operator` | `desk`), a forced-owner rule, the owner-only lists with an "In hand" fold, owner-only DMs, and `POST /v1/escalations/{id}/audience` to re-route. It deliberately left approval authz alone. This plan adds the approval and execution side on top of it. **`audience` stays the one coarse field every surface reads; `held_by` refines it** (D11).

**Architecture:**
1. **Routing model (kbeads).** Three new server-stamped facts on every escalation:
   - `approvers`: who MAY approve. The field already exists; today only spawn escalations get it stamped.
   - `execute_by`: who DOES the work.
   - `held_by`: which lane it is WAITING ON now. Its lane decides `audience` (D11), so #756's surfaces follow it with no change of their own.

   All three are lane refs in one grammar. A new `escalation_policy` config bead (first-match rules, the same shape as `spawn_policy`) stamps them at create. The operator-only floor is #756's forced-owner predicate, extended (D3): there is one floor, in code, and no policy can configure it away.
2. **Coordinator authority (kbeads).** The approve endpoint accepts a coordinator principal: a machine client, an agent with a named role, or a land's steward. It does so only when the escalation's stamped `approvers` name that coordinator's lane and the lane currently holds it, never on an operator-only escalation, and never for the coordinator's own request. `/result` is gated on approval plus execute_by membership. That finishes what PR #162 started.
3. **Surfaces.** #756 already made the viewer, the /garden Mailbox and map, the Slack DMs and the Commons read `audience`. This plan adds what `audience` cannot say: who exactly holds it (`held_by`, in the card and the In-hand rows), which land owner it waits on, and whether the viewer may approve it (`can-approve`), plus execution by `execute_by` and hand-up DMs.
4. **Lands.** A land owner may delegate approval of chosen operation types to the land's steward (`land_steward_approves` on the land's project bead). Tenant-land merges route to the steward or the land owner and never reach the operator.

**Tech stack:** Go 1.26 (go.work: `./kbeads` module `gardenio/kbeads`, `./gardenio/controller` module `gardenio/controller`). Daemon HTTP handlers in `kbeads/internal/server`. Viewer is server-rendered `html/template` + htmx in `gardenio/controller/cmd/beads-viewer`. Gardenhub and Commons are in `gardenio/controller/internal/{gardenhub,commons}`. Slack bridge is in `gardenio/controller/internal/bridge`. Keycloak service-account clients are used for machine principals.

**Spec:** this document's "Design decisions" and "Interface contract" sections are the spec. Lands context: `gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md` (§4.2 steward, §5.1 tiers) and `gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md` (§2.4 DMs, §2.5 card placement, §6 action table). Both are amended by Task 10. #756's own contract is `kbeads/internal/server/escalation_audience_authz.go` and `gardenio/controller/internal/beadsapi/escalation_audience.go`.

**Plan bead:** minted with labels `plan`, `plan:escalation-routing` and `project:gardenio`, and `plan_state=ready`. The owner clicks Approve & build. Phase and step bead ids are filled into the task map when minted.

**Anchors:** verified against `origin/main` at `399f1ecfe` (2026-10-21, the #756 merge; #762 and #734 are also in). Line numbers drift: every anchor is quoted text. Where a step says "re-locate by `<snippet>`", the snippet occurs exactly once in that file on `399f1ecfe` (checked by script against every such step). The tasks give no line numbers; a helper cited by name and file is found with `grep -n 'func <name>('`. The appendix notes keep their historical line numbers from `85dd8344`.

## Changes since #726

PR #726 (merged 2026-10-20 as `d1953d95d`) was written before #756. This revision rebuilds it on #756 instead of beside it. Nothing here has been minted.

- **One coarse field.** `audience` (#756) stays what every surface reads. `held_by` refines it, and the server keeps the two in step through one mapping, `laneAudience` (D11): every write that sets `held_by` sets `audience` in the same store write. No surface computes "for me" from `held_by` alone any more, so old Task 9 (Mailbox and map by `held_by`) is **dropped**: #756's In-hand fold does it once `audience` follows `held_by`.
- **One floor.** The plan's separate `operatorOnlyFloor` is gone. #756's `escalationAudienceForced` becomes `escalationForcedReason`, extended with D3's operation types; `operator_only` is stamped from it (D3). The read-side mirror (`beadsapi.EscalationAudienceForced`) gets the same list.
- **A #756 bug the plan would trigger, fixed first (Task 1).** #756's `approversNamePerson` counts every non-`team:` entry as a person, so stamping lane refs into `approvers` would force every escalation to the owner. Both copies now count only legacy person entries (`oidc:`, `slack:`, bare email).
- **Routing fields are frozen like `audience`.** `held_by`, `held_at`, `execute_by` and `operator_only` join #756's `escalationFrozenFields`, so even an admin PATCH is refused; repair goes through the route endpoint. #726's `TestEscalationRouteFields_AdminRepairAllowed` is replaced by `TestEscalationRouteFields_AdminPatchRefusedRouteRepairs`.
- **Filer `--audience` and `gb escalation route` are one rule (D11),** enforced in the server: a filer may only raise to `owner`; the route endpoint moves `held_by` to a lane of the target audience.
- **Policy rules gain `hold_by`** (D7). Rules 4 and 5 start held by `coordinator:laptop`, which keeps #756's default ("everything that isn't a merge is the operators'") without letting the laptop approve them.
- **Coordinators need the hold (D4).** A coordinator may approve only while its lane holds the escalation. Otherwise a filer's raise to `owner` would be display-only.
- **Unstamped escalations (Global Constraint 7)** read #756's audience, not "operator"; no coordinator may act on them until the sweep stamps them, and the sweep keeps the audience #756 gave them.
- **Stale anchors re-verified.** 22 cited files changed between `85dd8344` and `399f1ecfe`. Notable: `handleApproveEscalation` now calls `authorizeEscalationVerb` and `replayKillSwitchDenial` (#762, the land kill switch), which Tasks 3 and 4 add to their gate lists; the Commons escalation card's Approve and Reject are live (#729, `commonsEscalations` in `cmd/beads-viewer/commons_wiring.go`), so Task 12 is rewritten for them; `TestEscalationApprove_DuplicateUserBeadsUnion` is now `TestEscalationApprove_DuplicateUserBeadsFailClosed`.
- **Task changes.** Tasks 1–6 keep their shape with the edits above (Task 5 also extends #756's route endpoint). Old Task 7 shrinks to the lane helpers, `held_by` labels in the In-hand fold, land-owner filtering and `/up`. Old Task 8 shrinks to the routing panel beside #756's chip and the `can-approve` buttons. Old Task 9 is dropped. Old Task 10 (now 9) drops the cross-operator change (#756 covers it). Old Task 13 (now 12) is rewritten for the live Commons buttons. 15 tasks become **14**, in the same **6** phases; tasks 10 onwards are renumbered down by one.
- **Behaviour change: the pingback ladder warms only owner-audience escalations** (Task 9 uses `beadsapi.EscalationForOwner`; #726's `LaneIsHuman` is gone). A desk- or operator-audience escalation gets no automatic pingbacks; the idle hand-up (D5) is its timeout.
- **New owner choices** are listed in the PR and marked **(owner choice)** below: D4's hold rule, D7's `hold_by`, D11's route fallback and land-owner mapping, D3's `credential_requirements`, and the `manual_owner_action` default.

## The problem, measured (2026-10-20, 13 open/in_progress escalations)

| Really needs | Count | Examples |
|---|---|---|
| The owner's hands or judgement | 4 | passkey enrolment gd-0lWOG2ZT09; device soak gd-bH4HHHzucm; Commons live walk-through gd-C9kdXMyDWl; granting a person access gd-1nDlKw5PWA |
| A coordinator's approval (merge desk / laptop) | 5 | Merge PR #718, #716, #669, #715 (`gitlab_mr_merge`); `helm_deploy_values` gd-ClHbsJtneO |
| Already approved, waiting on an executor | 4 | Keycloak client gd-YjL6XkDlqU; merge 663 + redeploy gd-5mFCwUapcL; agent-create denial gd-3aYqRdf1SO; Slack e2e provisioning gd-fWeCNSRdCd |

On 2026-10-20 every surface showed all 13 to the owner, because each decided "for me" by `operated_by`. **#756 fixed the display half:** `audience` now folds the desk's and the operators' escalations into one "In hand" line on the viewer, the Mailbox, the map and the Commons, and only owner-audience escalations DM the owner. What #756 did not change, and this plan does:
- `audience` is a label, not authority. `kbeads/internal/server/escalation_approval_authz.go` `escalationApprover` still accepts only a human OIDC or edge caller, so no agent can approve. The merge desk still works round this by closing with a result and never approving, and the laptop still approves with the owner's own `kd login`.
- `handleCaptureEscalationResult` (`kbeads/internal/server/http_escalations.go`) still closes an escalation without checking that it was approved. PR #162, open since 2026-09-24 (four rounds sent back), tried to fix this. Task 6 supersedes it.
- `audience=operator` says "the operators have it", not which one, and nothing ever brings an unattended one back to the owner.

## Design decisions (settled here; reopen only via `gb decision create`)

**D1. Three questions, three fields.** `approvers` (may approve), `execute_by` (does the work) and `held_by` (waiting on now) are separate. The server stamps all three; no client may write them (400 on create or PATCH, like the `requesting_agent` guard). `held_by` decides `audience` (D11); surfaces read `audience` for "is this mine to see", and `held_by` for "who exactly".

**D2. One lane-ref grammar** for `approvers` entries, `execute_by` and `held_by`:

| Ref | Who is a member |
|---|---|
| `operator` | A human (OIDC/edge on an allowlisted browser client) holding `gardenio-admin`: the owner. (Not #756's `operator` audience, which means "the operators", such as the laptop.) |
| `coordinator:<name>` | A principal listed under `coordinators.<name>` in the active `escalation_policy`: `machine:<keycloak-client-id>` (machine principal) or `role:<role-name>` (an agent on its own per-agent token whose agent bead has that role) |
| `steward:<land>` | An agent on its own per-agent token, role `steward`, project `<land>` |
| `land-owner:<land>` | An OIDC human in Keycloak group `/project-<land>/owners` (existing land roles) |
| `requester` | The escalation's server-stamped `requesting_agent` (on its per-agent token) |
| `server` | The daemon's own replay (bead create/update/close/grant) |
| `oidc:<email>`, `team:<id>`, `slack:<id>`, bare email | Unchanged legacy `approvers` entries; valid in `approvers` only |

The shared static token and X-Operated-By relays are members of NO lane (per gd-ZjiSd9Kzio round 3).

**D3. One floor: #756's forced-owner rule, extended (code, not config).** `escalationForcedReason(ctx, bead)` (kbeads, Task 2) returns why an escalation must go to the owner, or `""`. It is #756's `escalationAudienceForced` with D3's operation types added; `escalationAudienceForced` stays as a wrapper so #756's callers are unchanged. Reasons, in order:
- `sensitivity_secret`: `sensitivity` is `secret` (#756);
- `credential_requirements`: `credential_requirements` asks for anything (#756). **(owner choice: #726's floor did not include this; #756 forces it to the owner, and a coordinator approving what the owner's surfaces show as theirs would be confusing.)**
- the operation type itself: `credential_rotation`, `secret_write`, `gardenio_user_capability`, `escalation_policy_write`, `spawn_policy_write`, `role_write`, `keycloak_group_membership`, or any type starting `credential_` or `secret_` (new);
- `approvers_name_person`: the server-stamped `approvers` name a legacy person entry (#756, with Task 1's fix);
- `spawn_unresolvable`, `spawn_never_auto_approve`, `spawn_privileged`, `spawn_rule_names_person`: #756's spawn checks.

The first three are **floor reasons**: the escalation is stamped `operator_only=true`, `approvers=["operator"]`, `policy_rule=floor:<reason>`. The last two kinds keep the server-stamped `approvers` (a spawn rule's list) and are not `operator_only`, because a named person who is not `gardenio-admin` must still be able to approve; coordinators are refused there anyway (no coordinator lane in the list, and Task 4's privileged-spawn gate). Every forced reason puts the escalation in an owner-audience lane (D11).

This encodes the owner's standing rule that only two things are operator-only: secrets/credentials, and changes to who may approve. **Amended 11-04 (P10.F): the floor is now the owner's five categories; see [D3/D7 amendment: the owner floor](#d3d7-amendment-the-owner-floor-p10f-2026-11-04).** The read-side mirror, `beadsapi.EscalationAudienceForced`, gets the same operation-type list and also reads `operator_only=true`, so a surface never shows a floor escalation as in hand.

**D4. Coordinator approval** is a new `approval_method=coordinator`. The approve endpoint accepts a non-human caller only if all of these hold:
- the escalation is not `operator_only`;
- `held_by` is a lane the caller is a member of, and that lane is in `approvers` **(owner choice: #726 accepted any listed lane. Requiring the hold means a filer's raise to `owner`, a hand-up or an operator's re-route really takes the escalation out of the coordinators' hands. To let the laptop approve a gardenio merge the desk holds, route it to `operator`.)**;
- the caller's principal is not the requester, and for `role:`/`steward:` lanes its agent bead is not the requesting agent bead;
- for `coordinator:` lanes on machine principals, the client id is in `BEADS_ESCALATION_COORDINATOR_CLIENTS` (defence in depth; empty means no machine coordinators).

The server stamps `approved_by` (the principal subject) and `approved_as` (the lane ref). The human path is unchanged: the operator lane may approve anything it may approve today, whatever holds it.

**D5. `held_by` transitions** (server-only). Each one also stamps `audience = laneAudience(held_by)` and `held_at` in the same write (D11):
- create → see D11's create rule. By default, the rule's `hold_by`, else the first `approvers` lane whose membership is currently non-empty (for example, `steward:<land>` is skipped when the land does not delegate to its steward), else `operator`.
- approve → `execute_by`.
- `claim` → unchanged, with `claimed_by`/`claimed_at` stamped. Only a member of `held_by` may claim.
- `hand-up` → `operator` (or, for tenant lands, `land-owner:<land>`), with `handed_up_by`/`handed_up_at`/`hand_up_reason` and a pingback stamped. Only a member of `held_by` may hand up. A hand-up skips the other coordinators: it is "this needs the owner".
- idle lane → if a `coordinator:*` or `steward:*` lane holds an unclaimed escalation for longer than `BEADS_ESCALATION_LANE_IDLE_HANDUP` (default `6h`; `0` disables), the route sweep hands it up exactly like a manual hand-up, with `handed_up_by=system:escalation-route`. An offline coordinator can never hide an escalation. **Amended 11-04 (P10.F2, gd-fj06w7XsPb):** an escalation off the floor (`escalationForcedReason` empty, not `operator_only`), not in a tenant land and not yet approved goes to the next staffed `coordinator:*` or `steward:*` lane listed after the idled lane in its `approvers` (desk → laptop), with `hand_up_reason` ending `; to <lane>`; only when none is left does it go to `operator`. The chain only moves forward, so it ends. A lane missing from `approvers` (a `hold_by`-only holder), a floor item, a land's item and an approved one (P7.1 moves its execution) go up as before. The manual `hand-up` is unchanged.
- route (`POST /v1/escalations/{id}/audience`, #756's endpoint, extended by Task 5) → a lane of the target audience (D11).
- reject / result → closed.

`held_at` (RFC3339 UTC) is stamped whenever `held_by` changes. `claimed_at` and `handed_up_at` are RFC3339 UTC too. `claim` never changes `status`.

**D6. `/result` gate** (behind `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL`, default off until Task 14 flips it). A result is accepted only when one of these holds:
- (a) the escalation carries a server-stamped approval and the caller is a member of `execute_by`;
- (b) `execute_by=server` and the caller is the daemon itself;
- (c) the caller is the requester and `result_status=failure` (withdrawal);
- (d) the caller is a machine principal in `BEADS_ESCALATION_RESULT_MACHINES`. This branch is kept for a future reaper. The only EscalationReaper today lives in the disabled `cmd/gitlab-bridge`, so the env var ships empty.

**D7. Default policy (seeded by Task 13; first match wins):**

| # | When | approvers (in order) | execute_by | hold_by | audience at create |
|---|---|---|---|---|---|
| 0 | floor (D3) | `operator` | `coordinator:laptop` | — | owner |
| 1 | `operation_types: [github_pr_merge, gitlab_mr_merge]`, `projects: [gardenio]` | `coordinator:merge-desk`, `coordinator:laptop`, `operator` | `coordinator:merge-desk` | — | desk |
| 2 | `operation_types: [github_pr_merge, gitlab_mr_merge]`, `lands: ["*"]` | `steward:{land}`, `land-owner:{land}` | `requester` | — | desk (steward) or owner (land owner) |
| 3 | `operation_types: [manual_owner_action, soak_verification]` | `operator` | `operator` | — | owner **(owner choice: #756's default puts these under operator)** |
| 4 | `operation_types: [create, update, close, grant]` | `operator` | `server` | `coordinator:laptop` | operator |
| 5 | (no `when`: everything else) | `operator` | `coordinator:laptop` | `coordinator:laptop` | operator |

`coordinators: {"merge-desk": {"principals": ["role:reviewer"], "audience": "desk"}, "laptop": {"principals": ["machine:gardenio-laptop-operator"], "audience": "operator"}}`.

`hold_by` is new **(owner choice)**: an optional lane that holds the escalation at create without being an approver. On rules 4 and 5 the laptop holds it, prepares it, and hands it up when it needs the owner's approval, so the owner's surfaces stay as #756 left them (non-merge work is the operators'). The idle hand-up (D5) bounds how long that can last. Without `hold_by`, rules 4 and 5 would be held by `operator` and every non-merge escalation would move back onto the owner's list.

`{land}` is templated from the escalation's project when that project bead has `land_kind == "tenant"` (`landKindTenant`, `kbeads/internal/server/lands_ledger.go`). Operator projects (gardenio, claimlayer) are not tenant lands, even when they carry `land_default_channel`. Rule 2 needs tenant agents to file merge escalations about their own land; today Track B §5.1 and step 10.1 (gd-g77WcV22SK) refuse all tenant escalations. Task 10 amends the spec for that carve-out and Task 11 implements it.

**D8. Lands delegation.** The project bead field `land_steward_approves` (json list of operation types, default `[]`) is the land owner's grant to its steward. `steward:<land>` is a member-bearing lane for an escalation only when its `operation_type` is in that list. Writing the field is a new land action, `steward_delegate`, allowed for the land owner only (both action tables: `kbeads/internal/server/action_table.go`, `gardenio/controller/internal/conversation/landrole.go`). The steward still never approves anything `operator_only`, and nothing it approves can reach the operator lane.

**D9. Surfaces read `audience`; `held_by` refines it; buttons ask the daemon.**
- Whether an escalation is the owner's to see is #756's `beadsapi.EscalationForOwner` (owner audience), unchanged. Everything else is In hand.
- Within the owner audience, an escalation held by `land-owner:<land>` is "needs you" only for a viewer who owns that land (`viewerLaneRefs`); for anyone else it folds into In hand as "land owners". `operator`-held ones are the gardenio owner's.
- In-hand rows and the escalation card name the holding lane (`LaneLabel(held_by)`, e.g. "coordinator laptop").
- Approve and Reject buttons render only if `POST /v1/escalations/{id}/can-approve` (dry-run, same checks as D4 plus the human path) says yes, on the bead page and on the Commons card. The /garden Mailbox keeps #756's capability gate for drawing its buttons; its POST goes through the bead page's handler, which asks `can-approve`.

**D10. Non-goals.**
- No PR-path patterns in the policy. Crews already self-merge non-human paths; merge escalations exist only for human-path PRs, and the merge desk owns those.
- No daily approval caps.
- No Slack approve buttons. Slack stays notify-only.
- No change to decisions (a separate type with its own `owner_seat`).
- No new `audience` values. #756's three stay.

**D11. `audience` ↔ `held_by`: one mapping, one stamping rule, enforced by the server.**

The mapping lives in one place, `laneAudience(ref, policy)` in `kbeads/internal/server/escalation_policy.go` (Task 2). The read side never recomputes it; it reads the stamped `audience`.

| `held_by` lane | `audience` |
|---|---|
| `operator` | `owner` |
| `land-owner:<land>` | `owner` (that land's owner; #756's per-land Commons and DMs work unchanged) **(owner choice: the alternative is a fourth audience value, which D10 rules out)** |
| `coordinator:<name>` | `coordinators.<name>.audience` in the policy: `desk` or `operator` (default `operator`; `owner` is refused by validation) |
| `steward:<land>` | `desk` |
| `requester`, `server` | `operator` |

The stamping rule, the same on every path:
1. **Every server write that sets `held_by` sets `audience = laneAudience(held_by)` and `held_at` in the same store write.** That covers create, the sweep, approve, claim (which leaves both alone), hand-up, idle hand-up and route. These writes go through `mergeBeadFields` or `updateBead` under `escalationRouteKey`, both of which #756's frozen-field guard already lets through; no client PATCH can write either field.
2. **Create.** #756's `stampEscalationAudience` still runs first and still refuses an invalid `--audience` (400). After the project is stamped, `stampEscalationRoute` picks `held_by`:
   - forced (D3), or the filer asked for `owner` → the first owner-audience lane in `approvers` (`operator` or `land-owner:<land>`), else `operator`;
   - otherwise the rule's `hold_by`, staffed or not, else `firstStaffedLane(approvers)`. An unstaffed holder (the laptop before Task 13 allowlists its client) keeps #756's `operator` audience; the operator lane can still approve it, and the idle hand-up moves it to the owner after `BEADS_ESCALATION_LANE_IDLE_HANDUP`;

   then it overwrites `audience` with `laneAudience(held_by)`. A filer's `desk` or `operator` is ignored, as in #756. If the policy is unreadable, the bead keeps #756's audience and no `held_by`, and the sweep retries.
3. **Route** (`POST /v1/escalations/{id}/audience`, #756; `gb escalation route`, the viewer's re-route chips). The target audience `A` picks the lane:
   - `A = owner` → the first owner-audience lane in `approvers`, else `operator`;
   - otherwise the first staffed lane in `approvers` whose audience is `A`; if there is none, the first policy coordinator (by name) whose audience is `A`, staffed or not as for `hold_by`, which then holds the escalation without being able to approve it **(owner choice: the alternative is 409, which would stop the operators routing a non-merge escalation to the laptop, the main thing #756's route is used for)**; if there is none, 409 `no <A> lane can hold this escalation`.
   - Forced and `A ≠ owner` → 409, as #756 does. Already approved → only `A = owner` is accepted (409 otherwise): an approved escalation is held by its executor.
   - `held_by`, `held_at` and `audience` are written together (and any claim is cleared), and #756's audit comment gains the lane when `held_by` moved: `Audience re-routed: desk → operator (coordinator:laptop) by <who>`.
   - An unstamped escalation (no `held_by`) is routed as #756 does, `audience` only; the sweep then picks a lane of that audience (rule 4).
4. **Sweep.** An open escalation with no `held_by` is stamped by rule 2, except that an `audience` already stamped by #756 (or re-routed since) is honoured: the sweep applies rule 3's lane choice for that audience and keeps the policy's lane only when rule 3 finds none (logged).

### D3/D7 amendment: the owner floor (P10.F, 2026-11-04)

Owner ruling on decision gd-CJUlVSMzEe (14:52Z 11-04): shrink what needs the owner to a
written floor, and have policy or agent four-eyes approve the rest, act-and-tell with an
audit row and a revert path. Standing owner rule 11-04: "minimum human intervention in this
system unless absolutely necessary". Step gd-JJpjOjyvIL, phase P10 gd-9aoE8JuSyu.

**The floor (D3, code).** Only these reach the owner. `escalationFloorOperationTypes`
(kbeads `escalation_audience_authz.go`) and its read-side copy
(`beadsapi/escalation_audience.go`) hold the list; kbeads'
`TestEscalationFloorMatchesReadSide` parses the beadsapi source, so the two cannot drift.
An alias of a floor type is on the floor (kbeads resolves it, beadsapi spells it out).
`sensitivity=secret`, `credential_requirements`, a named person in `approvers` and #756's
spawn checks still force the owner as before.

| Category | Operation types (registered; `credential_*`, `secret_*`, `billing_*` by prefix) |
|---|---|
| Secrets and credentials | the whole `credential` class: `credential_rotation`, `credential_provisioning`, `secret_write`, `k8s_secret_seed`, `litellm_key_rotation`, `litellm_virtual_key_mount`, `vault_provisioning`, `project_secrets_and_credentials`, `helm_values_secret_change`, `aws_ses_smtp_k8s_secret_deploy_values` |
| Global roles and identity | `grant` (a taint grant), `kbeads_project_access`, `kbeads_role_grant`, `capability_grant`, `kbeads_fleet_token_admin`, `agent_token_scope`, `role_write`, `kbeads_role_update`, `create_role`, `role_scaffold`, `k8s_rbac`, `k8s_rbac_grant`, `aws_iam`, `keycloak_client`, `keycloak_idp_rollout`, `kcadm_write`, `keycloak_group_membership`, `gardenio_user_capability`, `user_identity_pin`, `ziti_identity_management`, `github_token_scope`, `github_app_permissions`, `slack_app_scope` |
| gardenio's own policy | `escalation_policy_write`, `spawn_policy_write`, `kbeads_authz_change`, `github_repo_settings`, `github_ruleset_bypass` (new), `trigger_reseal`, `trigger_prompt_install` (a sealed trigger prompt is what a privileged agent is told to do) |
| Money and billing | new class `billing`: `billing_change`, `spend_limit_change` (any cap that spends money: LiteLLM budgets, `ci_max_nodes`, node pools) |
| Irreversible data deletion | new class `data_deletion`: `data_deletion`, `terraform_destroy`, `db_data_delete`, `storage_delete`; and `git_push_public` (publishing outside the farm, P10.F4) |

Judgement calls a reader may want to check: `kcadm_read_only`, `lands_tenant_test_identity`,
`github_repo_create`, `agent_despawn`, `k8s_pod_delete` (a pod comes back) and
`kbeads_trigger_update` (an unsealed field) are off the floor. `keycloak_group_membership`
is on it except for a tenant land's own groups (P10.C, below). A filer
who deletes data or spends money under another type (a `terraform_apply` that destroys)
must file it under the floor type or `--audience owner`; the type is what the daemon reads.

**The class-to-lane table (D7, the `escalation_policy` bead).** Every class off the floor is
held and approved first by a coordinator lane; `operator` stays last in `approvers` only so
the owner can still approve what a hand-up brings them (`humanSatisfiesApprovers` accepts
the owner only through an `operator` entry).

| # | When | approvers | execute_by | hold_by | Act-and-tell: audit and revert |
|---|---|---|---|---|---|
| 0 | the floor (code) | `operator` | `coordinator:laptop` | — | owner |
| 1 | merges, project `gardenio` | `coordinator:merge-desk`, `coordinator:laptop`, `operator` | `coordinator:merge-desk` | — | desk verdict on the PR; revert with `git revert` of the merge |
| 2 | merges, tenant lands | `steward:{land}`, `land-owner:{land}` | `requester` | — | unchanged (a land owner is the tenant, not the gardenio owner) |
| 3 | class `bead_replay` (`create`, `update`, `close`; `grant` is on the floor) | `coordinator:cluster`, `coordinator:laptop`, `operator` | `server` | `coordinator:cluster` | the replay's mutation_history row; revert with `kd undo` / the inverse write |
| 4 | classes `bead_admin`, `bead_close`, `advice` | `coordinator:cluster`, `coordinator:laptop`, `operator` | `coordinator:cluster` | `coordinator:cluster` | `approved_targets` bounds the writes; the result lists them; revert by reopening or the inverse write |
| 5 | everything else: `release_roll` (helm_deploy), `deploy_values`, `chart_edit`, `k8s_apply`, `edge`, `terraform`, `cloud_infra`, `database`, `prod_read`, `github_actions`, `review`, `lands`, `agent_lifecycle`, `owner_action`, the rest of `trigger`, `app_access`, `identity`, unregistered types | `coordinator:laptop`, `operator` | `coordinator:laptop` | `coordinator:laptop` | the laptop's result names what it ran; revert: `helm rollback`, `git revert` of a values or chart change, `kubectl` re-apply of the previous manifest, cancel the Actions run |

The approval is the audit row (`approved_by`, `approved_as`, `approval_method=coordinator`,
D4), the result is the tell. A coordinator never approves what it requested (D4).

The policy bead content (replaces gd-r8WKkZb7H1's rules; a policy write is a floor
escalation, so the owner approves this one):

```json
{
 "rules": [
  {
   "when": {
    "operation_types": [
     "github_pr_merge",
     "gitlab_mr_merge"
    ],
    "projects": [
     "gardenio"
    ]
   },
   "approvers": [
    "coordinator:merge-desk",
    "coordinator:laptop",
    "operator"
   ],
   "execute_by": "coordinator:merge-desk"
  },
  {
   "when": {
    "operation_types": [
     "github_pr_merge",
     "gitlab_mr_merge"
    ],
    "lands": [
     "*"
    ]
   },
   "approvers": [
    "steward:{land}",
    "land-owner:{land}"
   ],
   "execute_by": "requester"
  },
  {
   "when": {
    "operation_classes": [
     "bead_replay"
    ]
   },
   "approvers": [
    "coordinator:cluster",
    "coordinator:laptop",
    "operator"
   ],
   "execute_by": "server",
   "hold_by": "coordinator:cluster"
  },
  {
   "when": {
    "operation_classes": [
     "bead_admin",
     "bead_close",
     "advice"
    ]
   },
   "approvers": [
    "coordinator:cluster",
    "coordinator:laptop",
    "operator"
   ],
   "execute_by": "coordinator:cluster",
   "hold_by": "coordinator:cluster"
  },
  {
   "when": {},
   "approvers": [
    "coordinator:laptop",
    "operator"
   ],
   "execute_by": "coordinator:laptop",
   "hold_by": "coordinator:laptop"
  }
 ],
 "coordinators": {
  "laptop": {
   "audience": "operator",
   "principals": [
    "machine:gardenio-laptop-operator"
   ]
  },
  "cluster": {
   "audience": "operator",
   "principals": [
    "machine:gardenio-cluster-coordinator"
   ]
  },
  "merge-desk": {
   "audience": "desk",
   "principals": [
    "role:reviewer"
   ]
  }
 }
}
```

**What the policy alone does not do.** In the baseline week (10-28..11-04), 228 of the 428
owner approvals reached the owner by the idle hand-up, not by a rule: 75 desk-held merges
and about 150 laptop-held items sat for `BEADS_ESCALATION_LANE_IDLE_HANDUP` (6h) and
`handUpTarget` moved them to `operator`. Another 79 were merges the owner approved while the
desk held them. And `coordinator:cluster`'s prompt says it never approves. So the
acceptance measurement (only floor classes reach the owner over 24h) also needs:
- the idle hand-up of an escalation off the floor to move to the next staffed coordinator
  in its `approvers` before `operator` (P10.F2, gd-fj06w7XsPb; D5's idle-lane rule);
- the cluster coordinator to approve the bead-only work it holds, after its checks, then
  execute it and say so (prompt v2 plus a reseal, P10.F3, gd-mrf4AxPebA).

The measurement is one count, taken once both follow-ups and this policy are live: owner
`web_ui` approvals over the next 24h of traffic, by `policy_rule`. Every one that is not
`floor:*` is a bug on P10.F.

**P10.F4 fail-fast fixes (gd-QiDj0JvkFr, 2026-11-05).** The measurement's fail-fast found
two classes the owner owns by rule that still reached them under a non-floor rule:
- **Operator-class merges** (docs/merge-desk.md, "Sensitive paths", classes (1)-(3)):
  rule 1 left them with the desk, so the owner approved them with "Approve anyway"
  (gd-Yp6EEmbCeW, #2381). Once the desk names the class, it hands the merge up with
  `operator_class` (`kd|gb escalation hand-up <id> --operator-class N --reason ...`).
  The hand-up writes `approvers=["operator"]`, `operator_only=true`,
  `policy_rule=floor:operator_class_merge` and the server-only, frozen `operator_class`, and
  `held_by=operator`. `execute_by` stays `coordinator:merge-desk`, so the desk merges once
  the owner approves. `escalationFloorReason` reads a PR merge with `operator_class` as
  the floor reason `operator_class_merge`, so a re-route keeps it there, and
  `beadsapi.EscalationAudienceForced` reads it too. Only the merge's holder may name the
  class, only before approval (409 after), and only on a PR merge (400 otherwise). It
  narrows who may approve; it widens nothing.
- **Publishing outside the farm** (`git_push_public`, gd-MAq8HE3t0d): on the floor by type,
  beside irreversible deletion. A public push cannot be taken back and faces outward, so
  no coordinator lane approves it.

### Presence amendment: a person approves on the page (P10.B, 2026-11-04)

Owner ruling on decision gd-CJUlVSMzEe: "page" presence, no passkey step-up for now (it can
be added to the page later without changing any door). Design note gd-B2kAow1r1O and
zi56's review on it. Step gd-4mEAhIE01U.

**Presence is the page session.** A person's approve is accepted only from the session that
renders the escalation's page in the browser or on a phone: an OIDC token whose `azp` is the
edge forward-auth client (`SetForwardAuthClients`, the OIDC audience,
`gardenio-traefik-forwardauth` on k3s), or a verified v2 edge envelope with
`gardenio-operator`. `escalationApprover` is that check. The `kd login` / `gl` client
(`gardenio-cli`) never approves, so an agent on a person's CLI login cannot approve.
Authority is unchanged: lanes, `gardenio-admin` for `operator`, the owners group for
`land-owner:<land>`. A result needs authority only (step A, `callerHasLaneAuthority`).

**Floor items, both verbs.** An escalation on the owner floor (`escalationOperatorOnly`:
a floor reason or `operator_only`) is approved *and rejected* only by the `operator` lane on
the page session. Approve already required it. Reject now does too: before P10.B any
`RoleOperator` caller, a CLI login included, could reject a floor item. A reject off the
floor keeps its `RoleOperator` rule. The refusal text ends "approve or reject it on its page
in the browser or on a phone (kd and gl print the link)".

**Every door hands the same page.**

| Door | What it does |
|---|---|
| Browser / phone (bead page, Commons card) | approves and rejects; buttons follow `can-approve` |
| `kd escalation approve` / `reject` | sends the request; on a 403 prints `$BEADS_VIEWER_URL/<id>` (else `https://<daemon host>/<id>`; `approve_url` under `--json`) and fails having done nothing. A coordinator lane's own principal still approves through kd. |
| `gb escalation approve` / `reject` | the same as kd: a 403 names `$BEADS_VIEWER_URL/<id>` |
| `gl escalation approve` / `reject` | reads the escalation and prints `<web>/garden?bead=<id>` (`--open` opens it); sends no write |
| Slack | notify-only (D10); its cards already link `$BEADS_WEB_URL/<id>` (`bridge.EscalationReviewURL`), which is the same page |

`/<id>` on the viewer retires into `/garden?bead=<id>` (garden_retire.go), so the kd/Slack
shape and the gl shape open one page.

**`BEADS_ESCALATION_APPROVER_CLIENTS` is gone from the daemon.** The step said to replace it
once no caller depended on it as a client allowlist. That held at P10.B: P10.S
(gd-RMmWllb8PS, PR #2279) dropped `gardenio-slack-link`, leaving
`gardenio-traefik-forwardauth`, which is the OIDC audience and so already the forward-auth
client. The daemon no longer reads the variable and logs a warning when it is set. The chart
knob `beads.escalationApproverClients` and its deploy-values line now do nothing; removing
them is a values change for the operator (gd-WgHArryHIF).

**Tests.** `escalation_page_presence_test.go`: a `gardenio-cli` admin's approve and reject of
a floor item are 403 and leave it untouched; the browser client's approve and reject pass; a
CLI reject off the floor still passes. Non-floor approval by its lane without the owner is
`TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge`. kd:
`TestEscalationApproveReject_403HandsThePageLink`; gb:
`TestEscalationApproveReject_403NamesThePage`; gl:
`TestEscalationApproveReject_HandThePageLink`.

### Lands amendment: a land owner runs the land's people (P10.C, 2026-11-04)

Step gd-wi44L8Qy4I, owner ruling gd-CJUlVSMzEe ("keep the owner floor to the minimum"). The
floor's `keycloak_group_membership` entry meant "global roles and gardenio's own groups" but
also caught a tenant adding a person to their own land.

- **The floor narrows by one case.** `escalationForcedReason` lets a
  `keycloak_group_membership` escalation off the floor when `escalationLandPeople` holds:
  exactly one `project:<land>` label, `land_kind=tenant`, no other `fields.project`, an
  `operation_spec` (object or JSON string) naming at least one group, every group
  `/project-<land>` or `/project-<land>/<subgroup>`, and no keys besides `group`, `groups`,
  `user`, `users`, `action` (`add`/`remove`) and `reason`. Anything else is the floor as before:
  a global group, another land's group, a role key, `sensitivity=secret`,
  `credential_requirements`. The type stays in `escalationFloorOperationTypes` and its read-side
  copy. The read side cannot see a land's kind, so it still shows such an escalation as
  owner-audience, which `land-owner:<land>` is anyway (D11).
- **Routed in code beside the floor**, not by a policy rule, so no `escalation_policy_write`
  (itself a floor item) is needed: `approvers=["land-owner:<land>"]`, `held_by` that lane,
  `execute_by=coordinator:laptop` (else `operator`), `operator_only=false`,
  `policy_rule=land:people` (`escalationRouteFieldsFor`).
- **Approval.** A scoped owner of the land may resolve it (`landOwnerPeopleApproval`, the twin
  of `landOwnerMergeApproval`, both on `landOwnerLaneApproval`), on the page session only
  (P10.B). The steward's delegation is unchanged: `land_steward_approves` still refuses the type.
  A lands-tenant agent still files only merges, so the owner files the ask from `gl`/`kd`.
- **Specs amended:** Commons §2.2 and §6.4, Track B §4.2.
- **Tests** (`land_people_test.go`): the floor table; the tenant owner token shape
  (`gardenio-project-member`, `/project-claimlayer/owners`) files from its CLI login, is refused
  on that login with the page link, a member is refused, and the page approves; a global group
  stays on the floor against the same owner; the steward cannot be delegated the type.

## Global Constraints

1. **Lanes.**
   - CREW tasks are code by PR.
   - OPERATOR tasks are the laptop operator session's config writes and rollouts.
   - OWNER tasks need the owner in person (Keycloak client secret, deploy-values secrets, decision answers).
   - Each task states its lane under **Who does what**.
2. **Operator-merge PRs.** Every PR that changes who may approve escalations (Tasks 2, 4, 6 and 11, the Task 13 chart/values PR "13a" and the Task 14 gate-flip roll "14a") is operator-only to merge, per the 2026-09-23 merge rule. Open it as a draft, label its merge escalation `merge-desk:hold`, and file a `type=decision` bead to the operator with the diff summary. The merge desk reviews but does not merge it.
3. **Every other PR** follows the crew SELF-MERGE rules (advice gd-3wArIbnnWn): green on the full head SHA, not behind main, no blocking review, no human-only paths. kbeads authz files (`*authz*`, `interceptors.go`, `write_authz.go`) are human paths, so the merge desk takes those.
4. **Fail closed.**
   - An unknown lane ref, a malformed policy bead, a missing `held_by`, or an identity resolution error means: deny coordinator approval, route to `operator`, and log at WARN with the escalation id.
   - Never fall back to `operated_by`.
5. **Server-only fields.** Clients can never write `held_by`, `held_at`, `execute_by`, `approvers`, `operator_only`, `approved_as`, `claimed_by`, `claimed_at`, `handed_up_by`, `handed_up_at`, `hand_up_reason` or `policy_rule`. Create or PATCH with any of them returns 400 `inputError`. Mirror the existing `requesting_agent` guard in `escalation_approval_authz.go`. `held_by`, `held_at`, `execute_by` and `operator_only` are also #756 frozen fields, so not even an admin PATCH writes them; `audience` stays frozen as #756 made it.
6. **Positive controls.** Every refusal test has a sibling acceptance test on the same fixture, so a test that passes because nothing works fails.
7. **Compatibility.** An escalation with no `held_by` (filed before Task 2 shipped, or while the policy was unreadable) shows wherever #756's `audience` puts it, never vanishing. Until the Task 2 sweep stamps it, no coordinator may approve, claim or hand it up (it reads as held by `operator`); the human approve path and #756's route endpoint behave as they do today. With `execute_by` empty, the bridge's legacy `execution_target` behaviour runs unchanged.
8. **Branches** are `agent/escalation-routing/<task-bead-id>`; there is one PR per task.
9. **Go hygiene.**
   - `gofumpt -l` is empty.
   - `go vet` and `staticcheck` are clean on touched packages.
   - kbeads PRs also run `cd kbeads && golangci-lint run ./internal/server/...`; CI's kbeads lint job is blocking and authoritative.
   - Tests: `cd kbeads && go test ./internal/server -run '<names>' -count=1` and `cd gardenio/controller && go test ./<pkg> -run '<names>' -count=1`. Run them in CI or a crew pod, never on the laptop (WSL dies under local Go test load).
10. **No secrets in beads, PR bodies or logs.** The laptop operator's client secret lives only in AWS Secrets Manager `gardenio/laptop-operator-client` (profile gardenio, us-east-1) and the laptop's 0600 file.
11. **Naming is exact.** Field names, lane refs, env vars, endpoint paths and Go identifiers are those in the Interface contract below. A task that needs a new shared name adds it to the contract in its own PR.

## Interface contract

Every task uses these exact names. Later tasks rely on earlier tasks' signatures.

**Escalation fields** (kbeads `config.go` `"type:escalation"`, mirrored in controller `internal/bridge/init.go`):

| Field | Type | Writer | Task |
|---|---|---|---|
| `audience` | enum `owner`/`operator`/`desk` (#756, frozen) | server; now `laneAudience(held_by)` | 2, 4, 5 |
| `held_by` | string (lane ref), frozen | server | 1, 2, 4, 5 |
| `execute_by` | string (lane ref), frozen | server | 1, 2 |
| `approvers` | json list (existing) | server; now stamped for every escalation | 2 |
| `operator_only` | bool, frozen | server | 1, 2 |
| `policy_rule` | string (existing): `escalation#<i>` or `floor:<reason>` | server | 2 |
| `approval_method` | enum: existing values + `coordinator` | server | 1, 4 |
| `approved_as` | string (lane ref) | server | 1, 4 |
| `held_at` | RFC3339 UTC string, frozen | server | 1, 2, 4, 5 |
| `claimed_by`, `claimed_at` | string, RFC3339 UTC | server | 1, 5 |
| `handed_up_by`, `handed_up_at`, `hand_up_reason` | string, timestamp, string | server | 1, 5 |
| `approved_targets` | json list of bead ids, frozen | server, at approve | 15 |

**Project (land) field:** `land_steward_approves`, json list of operation types, written by the land owner via the action `steward_delegate` (Task 11), only through `POST /v1/lands/{land}/steward-delegation`; a create, PATCH, field endpoint or close body that sets it is a 400 for every caller. Delegable: `github_pr_merge`, `gitlab_mr_merge`, and the `true` entries of `scopedApprovableOperations` (`create`, `update`, `close`); never a D3 floor type.

**Config bead type:** `escalation_policy`, a singleton. The active one is the newest open bead of that type; zero beads means the built-in default D7. Fields: `rules` (json list) and `coordinators` (json object).

**kbeads Go** (package `server`, unless noted):
```go
// escalation_lane.go (Task 1)
type laneKind string // "operator","coordinator","steward","land-owner","requester","server","legacy"
type laneRef struct{ Kind laneKind; Name string; Raw string }
func parseLaneRef(s string) (laneRef, error)
func (r laneRef) String() string

// escalation_audience_authz.go (#756; Task 1 fixes approversNamePerson, Task 2 adds the rest)
func approversNamePerson(v any) bool // legacy person entries only (oidc:, slack:, bare email)
var escalationFloorOperationTypes map[string]bool
func escalationFloorOperation(opType string) string // "" or the op name: the D3 operation-type floor
func escalationFloorReason(f map[string]any) string // sensitivity_secret | credential_requirements | <op> | ""
func isEscalationFloorReason(reason string) bool
func (s *BeadsServer) escalationForcedReason(ctx context.Context, bead *model.Bead) string // D3, "" = not forced
func (s *BeadsServer) escalationAudienceForced(ctx context.Context, bead *model.Bead) bool // #756, now a wrapper

// escalation_policy.go (Task 2)
type escalationPolicyWhen struct {
    OperationTypes []string `json:"operation_types,omitempty"`
    Projects       []string `json:"projects,omitempty"`
    Lands          []string `json:"lands,omitempty"` // "*" = any tenant land
    Sensitivity    []string `json:"sensitivity,omitempty"`
}
type escalationPolicyRule struct {
    When      escalationPolicyWhen `json:"when"`
    Approvers []string             `json:"approvers"`         // lane refs; may contain "{land}"
    ExecuteBy string               `json:"execute_by"`        // lane ref; may contain "{land}"
    HoldBy    string               `json:"hold_by,omitempty"` // lane ref that holds at create; not an approver
}
type escalationCoordinator struct {
    Principals []string `json:"principals"`         // "machine:<id>" | "role:<name>"
    Audience   string   `json:"audience,omitempty"` // "desk" | "operator" (default)
}
type escalationPolicy struct {
    Rules        []escalationPolicyRule           `json:"rules"`
    Coordinators map[string]escalationCoordinator `json:"coordinators"`
}
type escalationFacts struct{ OperationType, Project, Land, Sensitivity string }
type escalationRoute struct {
    Rule      string // "escalation#<i>" | "floor:no_matching_rule"
    Approvers []string
    ExecuteBy string
    HoldBy    string
}
func defaultEscalationPolicy() escalationPolicy
func evaluateEscalationPolicy(p escalationPolicy, f escalationFacts) escalationRoute // rules only; the floor is escalationForcedReason
func laneAudience(ref string, p escalationPolicy) string // D11, the one mapping
func (s *BeadsServer) activeEscalationPolicy(ctx context.Context) (escalationPolicy, error)
func (s *BeadsServer) escalationFactsFor(ctx context.Context, b *model.Bead) escalationFacts
func (s *BeadsServer) stampEscalationRoute(ctx context.Context, b *model.Bead) error // create path + sweep
func (s *BeadsServer) StartEscalationRouteSweep(ctx context.Context)
func (s *BeadsServer) firstStaffedLane(ctx context.Context, esc *model.Bead, approvers []string, p escalationPolicy) string
func (s *BeadsServer) laneForAudience(ctx context.Context, esc *model.Bead, audience string, p escalationPolicy) (string, error) // D11 rule 3
func (s *BeadsServer) landDelegatesToSteward(ctx context.Context, land, opType string) (bool, error) // reused by Tasks 3 and 11

// action_table.go, land_steward_delegation.go (Task 11)
const actionStewardDelegate = "steward_delegate" // ownerActions row: model.TypeProject
const landStewardApprovesField = "land_steward_approves"
func validateLandStewardApproves(ops []string) error
func (s *BeadsServer) handleSetStewardDelegation(w http.ResponseWriter, r *http.Request)

// escalation_lane_members.go (Task 3)
func (s *BeadsServer) callerInLane(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error)
func (s *BeadsServer) escalationOperatorOnly(ctx context.Context, esc *model.Bead) bool
type approveCheck struct{ Allowed bool; Method string; As string; Reason string }
func (s *BeadsServer) checkMayApprove(ctx context.Context, esc *model.Bead) approveCheck
```

**HTTP** (kbeads, `http.go` route table):
- `POST /v1/escalations/{id}/can-approve` → `200 {"allowed":bool,"method":"web_ui|coordinator|...","as":"<lane ref>","reason":"..."}` (RoleViewer; dry-run; Task 3).
- `POST /v1/escalations/{id}/approve`: extended for coordinators (Task 4).
- `POST /v1/escalations/{id}/claim` → `200` bead (Task 5).
- `POST /v1/escalations/{id}/hand-up` with body `{"reason":"..."}` → `200` bead (Task 5).
- `POST /v1/escalations/{id}/audience` (#756) with body `{"audience","reason"}`: now also moves `held_by` per D11 rule 3 (Task 5).
- `GET /v1/escalations?held_by=<ref>[,<ref>|@me]`: server-side filter; `operator` also matches an empty `held_by`, and `@me` expands to the caller's lanes (Task 5).
- `POST /v1/escalations/{id}/result`: gated per D6 (Task 6).
- `POST /v1/lands/{land}/steward-delegation` with body `{"operation_types":[...]}` → `200 {"land","operation_types"}` (RoleViewer; owner of that land or gardenio-admin; tenant lands only; Task 11).

**Env (kbeads daemon, deploy-values):**
- `BEADS_ESCALATION_COORDINATOR_CLIENTS`: comma-separated Keycloak client ids (Task 2 wires it, Task 4 uses it; set in Task 13).
- `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL`: `true|false`, default `false` (Task 6; flipped in Task 14).
- `BEADS_ESCALATION_RESULT_MACHINES`: comma-separated client ids allowed by D6(d) (Task 6; ships empty).
- `BEADS_ESCALATION_LANE_IDLE_HANDUP`: Go duration, default `6h`, `0` disables (Task 5).

**Controller Go:**
```go
// internal/beadsapi, escalation_audience.go (#756; Task 1 fixes approversNamePerson, Task 2 extends EscalationAudienceForced)
func EscalationAudienceForced(fields map[string]string) bool // + D3 op types, + operator_only=="true"

// internal/beadsapi (ListBeadsQuery.HeldBy + ListEscalationsHeldBy: Task 5; the rest: Task 7)
func (c *Client) ListEscalationsHeldBy(ctx context.Context, heldBy []string, statuses []string, limit int) ([]*BeadDetail, error)
func (c *Client) CanApproveEscalation(ctx context.Context, id string) (CanApproveResult, error)
type CanApproveResult struct{ Allowed bool; Method, As, Reason string }
func EscalationHeldBy(b *BeadDetail) string // "" when unstamped (Global Constraint 7: read audience instead)

// internal/beadsapi helpers (Task 7): HeldByFromFields(map[string]string) string, EscalationHeldBy, EscalationExecuteBy, LandOwnerLane, LaneLabel, Lane* constants; InHand.Lands + InHand.AddLand
// internal/beadsapi (Task 5): ClaimEscalation, HandUpEscalation
// internal/beadsapi (Task 11)
func (c *Client) SetStewardDelegation(ctx context.Context, land string, ops []string) error
// internal/conversation (Task 11)
const ActionStewardDelegate Action = "steward_delegate" // LandRoleOwner
// internal/conversation (Task 9; Task 12 reuses)
type EscalationAddressee struct{ Lane, Land string }
func EscalationAddressees(heldBy, land string) []EscalationAddressee

// cmd/beads-viewer (Task 7)
func (s *Server) viewerLaneRefs(ctx context.Context, email string) []string
// cmd/beads-viewer (Task 8)
func (s *Server) escalationApproveGate(ctx context.Context, id, email string) (bool, string)

// cmd/gb (Task 5)
// gb escalation claim <id>; gb escalation hand-up <id> --reason <text>;
// gb escalation list --held-by <ref>[,<ref>] | --needs-me
```

## Review Focus

1. **Forged routing:** an agent creates or PATCHes an escalation with `held_by=coordinator:laptop` or `approvers=["role:crew"]` and expects 400, with the bead unchanged; an admin PATCH of `held_by` is refused too, and the route endpoint repairs it. Pinned: Task 1 `TestEscalationRouteFieldsServerOnly_CreateAndPatch` and `TestEscalationRouteFields_AdminPatchRefusedRouteRepairs`.
2. **Self-approval:** the merge desk reviewer approves an escalation that a reviewer agent itself requested, or the laptop approves its own `requesting_agent`, and expects 403. Pinned: Task 4 `TestCoordinatorApprove_RefusesOwnRequest`.
3. **Floor bypass:** a policy bead whose rule lists `coordinator:laptop` for `credential_rotation` must still produce `operator_only=true`, `audience=owner`, and refuse the coordinator. Pinned: Task 2 `TestEscalationCreate_FloorBeatsConfig` and Task 4 `TestCoordinatorApprove_RefusesOperatorOnly`.
4. **`audience` and `held_by` never disagree:** after create, sweep, approve, hand-up, idle hand-up and route, `audience == laneAudience(held_by)`; a filer's raise to `owner` holds the escalation in `operator`, and a coordinator's approve is then 403. Pinned: Task 2 `TestEscalationCreate_AudienceFollowsHeldBy`, Task 3 `TestCoordinatorApprove_NeedsTheHold`, Task 4 `TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge`, Task 5 `TestRouteEscalation_MovesHeldBy` and `TestHandUpEscalation`.
5. **Legacy escalation with no `held_by`:** it shows where #756's audience puts it (never vanishes), no coordinator can act on it until the sweep stamps it, and a floor escalation missing its `operator_only` stamp is still operator-only. Pinned: Task 3 `TestCheckMayApprove_UnstampedOperatorOnly` and Task 7 `TestHandleEscalations_UnstampedFollowsAudience`.
6. **Tenant land without delegation:** a lands-gardener's merge escalation in land `acme` routes to `land-owner:acme` (owner audience, but not the gardenio owner's "needs you"), not the steward and not the operator, and the steward's approve is 403 until `land_steward_approves` includes `github_pr_merge`. Pinned: Task 11 `TestStewardApprove_RequiresLandDelegation`, Task 7 `TestHandleEscalations_LandOwnerFoldsForOthers`.

## Task to step map and dependencies

| Task | Title | Phase | Lane | Merge | Was (#726) |
|---|---|---|---|---|---|
| 1 | Routing fields, lane-ref grammar, server-only guards, `approversNamePerson` fix | 1 Routing model | CREW | desk | 1 |
| 2 | `escalation_policy` evaluator, one floor, create-time stamping, audience, sweep | 1 Routing model | CREW | operator | 2 |
| 3 | Lane membership + `can-approve` dry-run | 2 Coordinator authority | CREW | desk | 3 |
| 4 | Coordinator approve path | 2 Coordinator authority | CREW | operator | 4 |
| 5 | `claim` / `hand-up` / route / `held_by` filter + gb/kd verbs | 2 Coordinator authority | CREW | desk | 5 |
| 6 | `/result` gate (supersedes PR #162) | 2 Coordinator authority | CREW | operator | 6 |
| 7 | Viewer: lane helpers, who holds it, land-owner fold, `/up` | 3 Beads viewer | CREW | desk | 7 (shrunk) |
| 8 | Viewer: routing panel + daemon-checked buttons | 3 Beads viewer | CREW | desk | 8 (shrunk) |
| — | /garden Mailbox + map scoped by `held_by` | — | — | — | 9 (**dropped**: #756) |
| 9 | Bridge: DMs, hand-up, pingbacks and executors follow `held_by`/`execute_by` | 4 Notifications | CREW | desk | 10 (shrunk) |
| 10 | Lands spec amendment: steward and land-owner approvals | 5 Lands | CREW (docs) | desk | 11 |
| 11 | Land delegation: `land_steward_approves`, `steward_delegate`, land lanes | 5 Lands | CREW | operator | 12 |
| 12 | Commons escalation cards: who holds it, buttons by `can-approve` | 5 Lands | CREW | desk | 13 (rewritten) |
| 13 | Identities, env and policy seed | 6 Rollout | OWNER + OPERATOR (CREW writes chart PR 13a) | operator (13a) | 14 |
| 14 | Laptop switch-over, gate flip, supersede old beads, acceptance | 6 Rollout | OPERATOR | operator (14a) | 15 |
| 15 | In-cluster coordinator for bead-only escalations (decision gd-B71zgAalvu=a; steps gd-sZNUPjzygT, gd-EwfINsGbxD, gd-Khn7cXyokc, gd-yT5xAPB0uJ, gd-8gfzBvgFrO) | 6 Rollout | CREW (1–3) + OPERATOR (4–5) | operator (1, 2, 4) | — |

**Blocked-by edges:**
- 2←1
- 3←1, 3←2
- 4←3
- 5←2, 5←3
- 6←4
- 7←5, 7←3
- 8←3, 8←7
- 9←5, 9←7
- 10 has no blockers
- 11←10, 11←4
- 12←8, 12←9 (reuses `EscalationAddressees`)
- 13←4, 13←6
- 14←7, 14←8, 14←9, 14←13
- 15 has no blockers in Tasks 1–14; inside it, Step 4←Steps 1, 2 and Step 5←Steps 3, 4

---

## Phase 1: Routing model (kbeads)

### Task 1: Routing fields, lane-ref grammar and server-only guards

**Step bead:** (minted later). Phase 1, Routing model. Blocked by: none. Blocks: Tasks 2 and 3.

**Who does what:** CREW writes the code and opens the PR. The merge desk reviews and merges it: `*authz*` is a human path (Global Constraint 3).

**Files:**
- Create `server/escalation_lane.go`, which holds `laneKind`, `laneRef`, `parseLaneRef`, `(laneRef).String`, `escalationRouteFields` and `escalationHasServerApproval`.
- Create `server/escalation_route_fields_test.go`.
- Modify `server/escalation_approval_authz.go`: re-locate by `var escalationStampedFields = []string{"spawn_role", "approvers", "requires_four_eyes", escalationRequestingAgentVerifiedField}` and the line below it, `var escalationImmutableFields = …`; and `func escalationFieldsExempt(ctx context.Context) bool {`.
- Modify `server/escalation_audience_authz.go` (#756): re-locate by `var escalationFrozenFields = []string{escalationAudienceField, "sensitivity", "credential_requirements"}`, `const escalationFrozenDenied = ` and `func approversNamePerson(v any) bool {`.
- Modify `gardenio/controller/internal/beadsapi/escalation_audience.go` (#756): re-locate by `func approversNamePerson(raw string) bool {`.
- Modify `server/escalation_audience_authz_test.go`, `TestEscalationAudienceForcedByApprovers` (re-locate by ``{"team-only approvers", `{"approvers":["team:ops"]}`, false},``), and `gardenio/controller/internal/beadsapi/escalation_audience_test.go`, `TestEscalationAudience` (re-locate by `{"team-only approvers are not forced", map[string]string{`).
- Modify `server/escalation_approval_authz.go`, `func checkEscalationApprovalFieldsImmutable`, which returns `inputError("spawn_role, approvers, requires_four_eyes, requesting_agent, operation_type, operation_spec and target_bead_id are immutable on an escalation after create")`. Keep the substring `immutable on an escalation`, which `scoped_residuals_test.go` asserts.
- Modify `server/http_escalations.go`, `handleCaptureEscalationResult`, at the loop `for k := range req.Fields {` / `if err := checkEscalationFieldEndpointWrite(r.Context(), resultTarget, k); err != nil {`.
- Modify `server/http_escalations.go`, `extractEscalationFields`, at `stringKeys := []string{` (`"approved_by", "approved_at", "approval_method", "policy_rule",`), at `jsonKeys := []string{` and at the boolean block `if raw, ok := fields["requires_four_eyes"]; ok {`.
- Modify `server/config.go`, `"type:escalation"`, at the lines `{"name": "approval_method",          "type": "enum", "values": ["web_ui", "api_token", "cli", "policy"]},` and `{"name": "policy_rule",              "type": "string"},`.
- Modify `gardenio/controller/internal/bridge/init.go`, re-locate by `{Name: "approval_method", Type: "enum", Values: []string{"web_ui", "api_token", "cli"}},`. This mirrors the kbeads field list.

**Interfaces:**
- Consumes: `escalationFieldValues(fields json.RawMessage, names []string) map[string]any` and `escalationFieldsExempt(ctx) bool` (both in `escalation_approval_authz.go`); #756's `escalationFrozenFields`, `escalationRouteKey{}`, `checkEscalationFrozenFields` and `approversNamePerson` (`escalation_audience_authz.go`).
- Produces:
```go
type laneKind string
const (
	laneOperator    laneKind = "operator"
	laneCoordinator laneKind = "coordinator"
	laneSteward     laneKind = "steward"
	laneLandOwner   laneKind = "land-owner"
	laneRequester   laneKind = "requester"
	laneServer      laneKind = "server"
	laneLegacy      laneKind = "legacy"
)
type laneRef struct{ Kind laneKind; Name string; Raw string }
func parseLaneRef(s string) (laneRef, error)
func (r laneRef) String() string
func validLaneName(n string) bool
var escalationRouteFields []string
func escalationHasServerApproval(b *model.Bead) bool
func isLegacyPersonApprover(entry string) bool // oidc:, slack:, bare email; never a lane ref or team:
```
  Changed (#756): `approversNamePerson` in both copies counts only `isLegacyPersonApprover` entries. Without this, Task 2's stamped `approvers` (all lane refs) would force every escalation to the owner.

**Steps:**

- [ ] **1. Write the failing tests** in `server/escalation_route_fields_test.go`. They use these helpers:
  - `newMockStore` and `decodeJSON` (`http_test.go`);
  - `newTestServer` (`http_test.go`, default caller = OIDC admin `test@example.com`);
  - the static-token pattern of `TestEscalationCreate_RequestingAgentServerStamped` (`escalation_requesting_agent_test.go`: `s.NewHTTPHandler("primary-token", nil)` + `X-Actor`).

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestParseLaneRef(t *testing.T) {
	good := map[string]laneRef{
		"operator":               {Kind: laneOperator, Raw: "operator"},
		" requester ":            {Kind: laneRequester, Raw: "requester"},
		"server":                 {Kind: laneServer, Raw: "server"},
		"coordinator:merge-desk": {Kind: laneCoordinator, Name: "merge-desk", Raw: "coordinator:merge-desk"},
		"steward:acme":           {Kind: laneSteward, Name: "acme", Raw: "steward:acme"},
		"land-owner:acme":        {Kind: laneLandOwner, Name: "acme", Raw: "land-owner:acme"},
		"oidc:a@example.com":     {Kind: laneLegacy, Raw: "oidc:a@example.com"},
		"team:gd-team1":          {Kind: laneLegacy, Raw: "team:gd-team1"},
		"slack:U0ABC":            {Kind: laneLegacy, Raw: "slack:U0ABC"},
		"a@example.com":          {Kind: laneLegacy, Raw: "a@example.com"},
	}
	for in, want := range good {
		got, err := parseLaneRef(in)
		if err != nil || got != want {
			t.Errorf("parseLaneRef(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != strings.TrimSpace(in) {
			t.Errorf("parseLaneRef(%q).String() = %q, want the trimmed input", in, got.String())
		}
	}
	for _, in := range []string{"", "  ", "coordinator:", "steward:", "land-owner:a b", "coordinator:{land}", "operators", "role:reviewer", "machine:x", "oidc:"} {
		if got, err := parseLaneRef(in); err == nil {
			t.Errorf("parseLaneRef(%q) = %+v, want an error", in, got)
		}
	}
}

// doStaticEscalationReq sends a request on the shared static token with an
// X-Actor claim, the way agent pods used to authenticate. The static token is
// a member of no lane and is not an admin, so the route-field guards apply.
func doStaticEscalationReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "gd-crew-agent")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// forgedRouteValues has one forged value per server-only field (Global
// Constraint 5 and Review Focus 1).
var forgedRouteValues = map[string]any{
	"held_by":           "coordinator:laptop",
	"held_at":           "2026-10-20T00:00:00Z",
	"execute_by":        "requester",
	"approvers":         []string{"role:crew"},
	"operator_only":     false,
	"policy_rule":       "escalation#1",
	"approved_by":       "machine:gardenio-laptop-operator",
	"approved_by_email": "x@example.com",
	"approved_at":       "2026-10-20T00:00:00Z",
	"approval_method":   "coordinator",
	"approved_as":       "coordinator:laptop",
	"claimed_by":        "gd-crew-agent",
	"claimed_at":        "2026-10-20T00:00:00Z",
	"handed_up_by":      "gd-crew-agent",
	"handed_up_at":      "2026-10-20T00:00:00Z",
	"hand_up_reason":    "forged",
}

func TestEscalationRouteFieldsServerOnly_CreateAndPatch(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)

	baseFields := func() map[string]any {
		return map[string]any{"operation_type": "github_pr_merge", "operation_spec": map[string]any{"pr": 1}}
	}

	// Positive control: the same create without route fields succeeds.
	rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "merge PR 1", "type": "escalation", "kind": "data", "fields": baseFields(),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("control create: got %d, want 201: %s", rec.Code, rec.Body.String())
	}

	for field, forged := range forgedRouteValues {
		t.Run("create/"+field, func(t *testing.T) {
			f := baseFields()
			f[field] = forged
			rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
				"title": "forged " + field, "type": "escalation", "kind": "data", "fields": f,
			})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), field) {
				t.Fatalf("create with %s: got %d %s, want 400 naming the field", field, rec.Code, rec.Body.String())
			}
		})
	}

	seed := func(id string) {
		fields, _ := json.Marshal(map[string]any{
			"operation_type": "github_pr_merge", "operation_spec": map[string]any{"pr": 2},
			"held_by": "operator", "execute_by": "coordinator:merge-desk",
			"approvers": []string{"coordinator:merge-desk", "operator"},
			"operator_only": false, "policy_rule": "escalation#1",
		})
		ms.beads[id] = &model.Bead{
			ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: "merge PR 2",
			Status: model.StatusOpen, Fields: fields, CreatedBy: "gd-crew-agent",
		}
	}

	// Positive control: PATCHing an unguarded field on the same fixture works.
	seed("gd-esc-patch-ok")
	rec = doStaticEscalationReq(t, h, http.MethodPatch, "/v1/beads/gd-esc-patch-ok", map[string]any{
		"fields": map[string]any{"context": "more detail"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("control patch: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	for field, forged := range forgedRouteValues {
		t.Run("patch/"+field, func(t *testing.T) {
			id := "gd-esc-patch-" + strings.ReplaceAll(field, "_", "-")
			seed(id)
			before := string(ms.beads[id].Fields)
			rec := doStaticEscalationReq(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{
				"fields": map[string]any{field: forged},
			})
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "immutable on an escalation") {
				t.Fatalf("patch %s: got %d %s, want 400", field, rec.Code, rec.Body.String())
			}
			if after := string(ms.beads[id].Fields); after != before {
				t.Fatalf("patch %s: bead changed despite the 400:\nbefore %s\nafter  %s", field, before, after)
			}
		})
	}
}

// TestEscalationRouteFields_AdminPatchRefusedRouteRepairs replaces #726's
// AdminRepairAllowed (CONTRACT ISSUE 9, revised): held_by, held_at, execute_by
// and operator_only are #756 frozen fields, so even a gardenio-admin's PATCH
// is refused; otherwise held_by and audience could disagree (D11). Repair is
// the route endpoint, whose escalationRouteKey context passes both guards.
func TestEscalationRouteFields_AdminPatchRefusedRouteRepairs(t *testing.T) {
	s, ms, h := newTestServer() // OIDC gardenio-admin
	fields, _ := json.Marshal(map[string]any{"operation_type": "github_pr_merge", "held_by": "coordinator:laptop", "audience": "operator"})
	ms.beads["gd-esc-repair"] = &model.Bead{
		ID: "gd-esc-repair", Kind: model.KindData, Type: model.TypeEscalation, Title: "t",
		Status: model.StatusOpen, Fields: fields,
	}
	for _, field := range []string{"held_by", "held_at", "execute_by", "operator_only"} {
		before := string(ms.beads["gd-esc-repair"].Fields)
		rec := doJSON(t, h, http.MethodPatch, "/v1/beads/gd-esc-repair", map[string]any{
			"fields": map[string]any{field: forgedRouteValues[field]},
		})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "fixed when an escalation is filed") {
			t.Fatalf("admin patch %s: got %d %s, want 400 (frozen)", field, rec.Code, rec.Body.String())
		}
		if string(ms.beads["gd-esc-repair"].Fields) != before {
			t.Fatalf("admin patch %s changed the bead despite the 400", field)
		}
	}
	// Positive control: an unfrozen field on the same bead, same admin.
	rec := doJSON(t, h, http.MethodPatch, "/v1/beads/gd-esc-repair", map[string]any{
		"fields": map[string]any{"context": "repair note"},
	})
	requireStatus(t, rec, http.StatusOK)
	// The route context writes held_by (Task 5 builds the endpoint on it).
	ctx := context.WithValue(context.Background(), escalationRouteKey{}, true)
	patch, _ := json.Marshal(map[string]any{"held_by": "operator", "audience": "owner"})
	if _, err := s.updateBead(ctx, "gd-esc-repair", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("route-context write refused: %v", err)
	}
}

// TestApproversNamePerson_LaneRefsAreNotPeople: #756's forced-owner rule must
// not read Task 2's stamped lane refs as people. Legacy person entries still
// force (the #756 rows in TestEscalationAudienceForcedByApprovers stay).
func TestApproversNamePerson_LaneRefsAreNotPeople(t *testing.T) {
	for _, list := range [][]string{
		{"operator"}, {"coordinator:merge-desk", "coordinator:laptop", "operator"},
		{"steward:acme", "land-owner:acme"}, {"requester"}, {"server"}, {"team:ops"},
	} {
		if approversNamePerson(list) || approversNamePerson(anySlice(list)) {
			t.Errorf("approversNamePerson(%v) = true, want false (lane refs)", list)
		}
	}
	for _, list := range [][]string{{"oidc:owner@example.com"}, {"operator", "owner@example.com"}, {"slack:U0ABC"}} {
		if !approversNamePerson(anySlice(list)) {
			t.Errorf("approversNamePerson(%v) = false, want true (positive control)", list)
		}
	}
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func TestCaptureEscalationResult_ApprovedByOnlyWhenUnapproved(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)
	seed := func(id string, extra map[string]any) {
		f := map[string]any{"operation_type": "gitlab_mr_merge"}
		for k, v := range extra {
			f[k] = v
		}
		raw, _ := json.Marshal(f)
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: "t", Status: model.StatusOpen, Fields: raw}
	}

	// Positive control: an unapproved escalation takes the display stamp
	// (EscalationReaper's merged_by → approved_by).
	seed("gd-esc-r1", nil)
	rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-esc-r1/result", map[string]any{
		"result_status": "success", "fields": map[string]string{"approved_by": "alice"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("unapproved display stamp: got %d %s, want 200", rec.Code, rec.Body.String())
	}

	// A real approval is never overwritten through /result.
	seed("gd-esc-r2", map[string]any{"approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z"})
	rec = doStaticEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-esc-r2/result", map[string]any{
		"result_status": "success", "fields": map[string]string{"approved_by": "mallory"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overwrite of a real approval: got %d %s, want 400", rec.Code, rec.Body.String())
	}

	// Every other route or provenance field is refused on /result.
	for _, field := range []string{"held_by", "approved_at", "approval_method", "approved_as", "execute_by"} {
		seed("gd-esc-r3-"+field, nil)
		rec = doStaticEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-esc-r3-"+field+"/result", map[string]any{
			"result_status": "success", "fields": map[string]string{field: "x"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("/result fields.%s: got %d %s, want 400", field, rec.Code, rec.Body.String())
		}
	}
}

func TestBuiltinEscalationRouteFieldsDeclared(t *testing.T) {
	var tc model.TypeConfig
	if err := json.Unmarshal(builtinConfigs["type:escalation"].Value, &tc); err != nil {
		t.Fatal(err)
	}
	have := map[string]model.FieldDef{}
	for _, f := range tc.Fields {
		have[f.Name] = f
	}
	for _, name := range append([]string{"approvers"}, escalationRouteFields...) {
		if _, ok := have[name]; !ok {
			t.Errorf("type:escalation does not declare %q", name)
		}
	}
	if !strings.Contains(strings.Join(have["approval_method"].Values, ","), "coordinator") {
		t.Errorf("approval_method enum lacks coordinator: %v", have["approval_method"].Values)
	}
}
```

  (`model.FieldDef` is `kbeads/internal/model/type_config.go`, with `Values []string`.)

- [ ] **2. Run the tests and watch them fail.**
  Run: `cd kbeads && go test ./internal/server -run 'TestParseLaneRef|TestEscalationRouteFieldsServerOnly_CreateAndPatch|TestEscalationRouteFields_AdminPatchRefusedRouteRepairs|TestApproversNamePerson_LaneRefsAreNotPeople|TestCaptureEscalationResult_ApprovedByOnlyWhenUnapproved|TestBuiltinEscalationRouteFieldsDeclared' -count=1`
  Expected: FAIL to build with `undefined: laneRef` / `undefined: parseLaneRef` / `undefined: escalationRouteFields`. Once `escalation_lane.go` exists, the run still fails: `create/held_by` returns 201 instead of 400, and `fields.approved_at` on `/result` returns 200.

- [ ] **3. Implement.** Create `server/escalation_lane.go`:

```go
package server

import (
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
)

// laneKind is the kind of a lane ref: the one grammar for an escalation's
// approvers entries, execute_by and held_by (escalation-routing plan D2).
type laneKind string

const (
	laneOperator    laneKind = "operator"
	laneCoordinator laneKind = "coordinator"
	laneSteward     laneKind = "steward"
	laneLandOwner   laneKind = "land-owner"
	laneRequester   laneKind = "requester"
	laneServer      laneKind = "server"
	// laneLegacy is a pre-routing approvers entry (oidc:, team:, slack:, bare
	// email). It is valid in approvers only and is matched by
	// escalationApproverListed, never by lane membership.
	laneLegacy laneKind = "legacy"
)

// laneRef is one parsed lane ref. Name is the coordinator name or the land for
// the named kinds; Raw is the trimmed input.
type laneRef struct {
	Kind laneKind
	Name string
	Raw  string
}

var legacyApproverPrefixes = []string{"oidc:", "team:", "slack:"}

// parseLaneRef parses one lane ref. An unknown or malformed ref is an error:
// callers fail closed (route to operator, deny coordinators, WARN).
func parseLaneRef(s string) (laneRef, error) {
	raw := strings.TrimSpace(s)
	switch raw {
	case "":
		return laneRef{}, fmt.Errorf("empty lane ref")
	case string(laneOperator), string(laneRequester), string(laneServer):
		return laneRef{Kind: laneKind(raw), Raw: raw}, nil
	}
	for _, k := range []laneKind{laneCoordinator, laneSteward, laneLandOwner} {
		if name, ok := strings.CutPrefix(raw, string(k)+":"); ok {
			if !validLaneName(name) {
				return laneRef{}, fmt.Errorf("lane ref %q: invalid name %q", raw, name)
			}
			return laneRef{Kind: k, Name: name, Raw: raw}, nil
		}
	}
	for _, p := range legacyApproverPrefixes {
		if rest, ok := strings.CutPrefix(raw, p); ok && strings.TrimSpace(rest) != "" {
			return laneRef{Kind: laneLegacy, Raw: raw}, nil
		}
	}
	if strings.Contains(raw, "@") && !strings.ContainsAny(raw, " :") {
		return laneRef{Kind: laneLegacy, Raw: raw}, nil
	}
	return laneRef{}, fmt.Errorf("unknown lane ref %q", raw)
}

// validLaneName reports whether n can name a coordinator or a land.
func validLaneName(n string) bool {
	if n == "" || len(n) > 128 {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// String renders the canonical form: what the server stamps and what a
// held_by filter compares.
func (r laneRef) String() string {
	switch r.Kind {
	case laneCoordinator, laneSteward, laneLandOwner:
		return string(r.Kind) + ":" + r.Name
	case laneOperator, laneRequester, laneServer:
		return string(r.Kind)
	}
	return r.Raw
}

// escalationRouteFields are the routing and approval-provenance fields only the
// server writes (plan Global Constraint 5). held_at (RFC3339 UTC) is restamped
// whenever held_by changes and drives the idle hand-up. Writers: the create-time route stamp
// (escalation_policy.go), the approve paths, and claim/hand-up. approvers is in
// the older stamped lists already.
var escalationRouteFields = []string{
	"held_by", "held_at", "execute_by", "operator_only", "policy_rule",
	"approved_by", "approved_by_email", "approved_at", "approval_method", "approved_as",
	"claimed_by", "claimed_at", "handed_up_by", "handed_up_at", "hand_up_reason",
}

// escalationHasServerApproval reports whether b carries an approval the server
// stamped. approved_at is the witness: since escalationRouteFields went
// server-only, no client can write it on create, PATCH or /result.
// approved_by alone proves nothing, because /result may carry it as a display
// stamp (handleCaptureEscalationResult).
func escalationHasServerApproval(b *model.Bead) bool {
	if b == nil {
		return false
	}
	for _, v := range escalationFieldValues(b.Fields, []string{"approved_at"}) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}
```

  In `server/escalation_approval_authz.go`, the lists change. Before:
```go
var escalationStampedFields = []string{"spawn_role", "approvers", "requires_four_eyes", escalationRequestingAgentVerifiedField}
var escalationImmutableFields = []string{"spawn_role", "approvers", "requires_four_eyes", "requesting_agent", escalationRequestingAgentVerifiedField, "operation_type", "operation_spec", "target_bead_id"}
```
  After:
```go
var escalationStampedFields = append([]string{"spawn_role", "approvers", "requires_four_eyes", escalationRequestingAgentVerifiedField}, escalationRouteFields...)
var escalationImmutableFields = append([]string{"spawn_role", "approvers", "requires_four_eyes", "requesting_agent", escalationRequestingAgentVerifiedField, "operation_type", "operation_spec", "target_bead_id"}, escalationRouteFields...)
```
  Extend the comment above the lists with one sentence: "escalationRouteFields (escalation_lane.go) are server-only too: routing (held_by, execute_by, operator_only, policy_rule), approval provenance (approved_*, approval_method) and claim/hand-up stamps."

  The message in `checkEscalationApprovalFieldsImmutable` changes. Before:
```go
		return inputError("spawn_role, approvers, requires_four_eyes, requesting_agent, operation_type, operation_spec and target_bead_id are immutable on an escalation after create")
```
  After:
```go
		return inputError("approval and routing fields (spawn_role, approvers, requires_four_eyes, requesting_agent, operation_type, operation_spec, target_bead_id, held_by, held_at, execute_by, operator_only, policy_rule, approved_*, approval_method, claimed_*, handed_up_*, hand_up_reason) are immutable on an escalation after create")
```

  In `escalationFieldsExempt` (`escalation_approval_authz.go`), add the route context, so #756's route endpoint (extended in Task 5) can write `held_by` beside `audience`. It writes only fields it computes itself. Add as the first check in the function body:
```go
	if v, _ := ctx.Value(escalationRouteKey{}).(bool); v {
		return true // the route endpoint (escalation_audience_authz.go) writes only server-computed fields
	}
```

  In `server/escalation_audience_authz.go` (#756), before:
```go
var escalationFrozenFields = []string{escalationAudienceField, "sensitivity", "credential_requirements"}
```
  After:
```go
// held_by, held_at, execute_by and operator_only are frozen too
// (escalation-routing D11): audience is derived from held_by, so only the
// daemon and the route endpoint may move either. Not even an admin PATCH.
var escalationFrozenFields = []string{escalationAudienceField, "sensitivity", "credential_requirements", "held_by", "held_at", "execute_by", "operator_only"}
```
  and `escalationFrozenDenied` becomes:
```go
const escalationFrozenDenied = "audience, held_by, execute_by, operator_only, sensitivity and credential_requirements are fixed when an escalation is filed; re-route with POST /v1/escalations/{id}/audience (gb escalation route), which needs gardenio-operator or gardenio-admin on your own sign-in"
```
  Keep the substring `fixed when an escalation is filed`; the new test asserts it.

  Still in `escalation_audience_authz.go`, `approversNamePerson` changes. Before:
```go
	for _, e := range list {
		if a, _ := e.(string); strings.TrimSpace(a) != "" && !strings.HasPrefix(strings.TrimSpace(a), "team:") {
			return true
		}
	}
	return false
```
  After:
```go
	for _, e := range list {
		if a, _ := e.(string); isLegacyPersonApprover(a) {
			return true
		}
	}
	return false
```
  and in `escalation_lane.go`, beside `legacyApproverPrefixes`:
```go
// isLegacyPersonApprover reports whether an approvers entry names one person:
// oidc:<email>, slack:<id> or a bare email. Lane refs (escalation-routing D2)
// and team: entries do not, so #756's forced-owner rule ignores them.
func isLegacyPersonApprover(entry string) bool {
	lane, err := parseLaneRef(entry)
	return err == nil && lane.Kind == laneLegacy && !strings.HasPrefix(lane.Raw, "team:")
}
```
  `approversNamePerson` also accepts a `[]string` once Task 2 stamps one; add `case []string:` beside `case []any:` and convert. An unparseable entry is no longer a person (it was under #756); Task 2 validates every stamped entry, so only a hand-written legacy value can be unparseable, and its approval path is `escalationApproverListed`, unchanged.

  In `gardenio/controller/internal/beadsapi/escalation_audience.go` (#756), the mirror changes the same way. Before:
```go
	for _, a := range list {
		if a = strings.TrimSpace(a); a != "" && !strings.HasPrefix(a, "team:") {
			return true
		}
	}
```
  After:
```go
	for _, a := range list {
		if isLegacyPersonApprover(a) {
			return true
		}
	}
```
  with, in the same file:
```go
// isLegacyPersonApprover mirrors kbeads (escalation_lane.go): oidc:, slack: or
// a bare email is a person; operator, coordinator:, steward:, land-owner:,
// requester, server and team: are not.
func isLegacyPersonApprover(entry string) bool {
	e := strings.TrimSpace(entry)
	switch {
	case strings.HasPrefix(e, "oidc:") && len(e) > len("oidc:"), strings.HasPrefix(e, "slack:") && len(e) > len("slack:"):
		return true
	case strings.Contains(e, "@") && !strings.ContainsAny(e, " :"):
		return true
	}
	return false
}
```
  Add rows to `TestEscalationAudience` (beadsapi): `"lane-ref approvers are not people"` with ``"approvers": `["coordinator:merge-desk","operator"]` ``, `"operation_type": "gitlab_mr_merge"` → `AudienceDesk, false`; and `"slack approver is a person"` → `AudienceOwner, true`. Add the same two rows to `TestEscalationAudienceForcedByApprovers` (kbeads).

  In `handleCaptureEscalationResult` (`server/http_escalations.go`), the field loop changes. Before:
```go
	for k := range req.Fields {
		if err := checkEscalationFieldEndpointWrite(r.Context(), resultTarget, k); err != nil {
```
  After:
```go
	for k := range req.Fields {
		// approved_by is the one exception: the merge reaper stamps the GitHub
		// or GitLab merger there for display. It may not overwrite a real
		// approval, and D6 trusts approved_at, never approved_by.
		if strings.EqualFold(k, "approved_by") && !escalationHasServerApproval(resultTarget) {
			continue
		}
		if err := checkEscalationFieldEndpointWrite(r.Context(), resultTarget, k); err != nil {
```

  In `extractEscalationFields`, the string keys change. Before:
```go
		"approved_by", "approved_at", "approval_method", "policy_rule",
```
  After:
```go
		"approved_by", "approved_at", "approval_method", "policy_rule",
		"approved_as", "held_by", "held_at", "execute_by",
		"claimed_by", "claimed_at", "handed_up_by", "handed_up_at", "hand_up_reason",
```
  Then add `"approvers",` to `jsonKeys`, and after the `requires_four_eyes` boolean block add:
```go
	if raw, ok := fields["operator_only"]; ok {
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			esc["operator_only"] = b
		}
	}
```

  In `server/config.go` `"type:escalation"`, these lines change. Before:
```
			{"name": "approval_method",          "type": "enum", "values": ["web_ui", "api_token", "cli", "policy"]},
			{"name": "policy_rule",              "type": "string"},
```
  After:
```
			{"name": "approval_method",          "type": "enum", "values": ["web_ui", "api_token", "cli", "policy", "coordinator", "commons"]},
			{"name": "policy_rule",              "type": "string"},
			{"name": "approved_as",              "type": "string"},
			{"name": "held_by",                  "type": "string"},
			{"name": "held_at",                  "type": "string"},
			{"name": "execute_by",               "type": "string"},
			{"name": "operator_only",            "type": "boolean"},
			{"name": "claimed_by",               "type": "string"},
			{"name": "claimed_at",               "type": "string"},
			{"name": "handed_up_by",             "type": "string"},
			{"name": "handed_up_at",             "type": "string"},
			{"name": "hand_up_reason",           "type": "string"},
```

  In `gardenio/controller/internal/bridge/init.go` the mirror changes. Before:
```go
				{Name: "approval_method", Type: "enum", Values: []string{"web_ui", "api_token", "cli"}},
```
  After:
```go
				{Name: "approval_method", Type: "enum", Values: []string{"web_ui", "api_token", "cli", "policy", "coordinator", "commons"}},
				// Escalation routing (kbeads escalation_lane.go). All server-stamped lane refs.
				{Name: "approved_as", Type: "string"},
				{Name: "held_by", Type: "string"},
				{Name: "held_at", Type: "string"},
				{Name: "execute_by", Type: "string"},
				{Name: "operator_only", Type: "boolean"},
				{Name: "claimed_by", Type: "string"},
				{Name: "claimed_at", Type: "string"},
				{Name: "handed_up_by", Type: "string"},
				{Name: "handed_up_at", Type: "string"},
				{Name: "hand_up_reason", Type: "string"},
```

- [ ] **4. Run the tests and watch them pass.**
  Run: `cd kbeads && go test ./internal/server -run 'TestParseLaneRef|TestEscalationRouteFieldsServerOnly_CreateAndPatch|TestEscalationRouteFields_AdminPatchRefusedRouteRepairs|TestApproversNamePerson_LaneRefsAreNotPeople|TestCaptureEscalationResult_ApprovedByOnlyWhenUnapproved|TestBuiltinEscalationRouteFieldsDeclared|TestEscalation|TestCaptureEscalation|TestApproveEscalation|TestScoped' -count=1`
  Expected: PASS. The existing `TestApproveEscalation*` and `TestCaptureEscalationResult` still pass, because the approve and result paths write through `mergeBeadFields`, which bypasses the PATCH guard.
  Then run: `cd gardenio/controller && go test ./internal/bridge -run 'TestInit|TestEscalation' -count=1`
  Expected: PASS.

- [ ] **5. Lint and vet.**
  Run: `gofumpt -l kbeads/internal/server gardenio/controller/internal/bridge` (expect empty), then `cd kbeads && go vet ./internal/server && staticcheck ./internal/server && golangci-lint run ./internal/server/...`, then `cd gardenio/controller && go vet ./internal/bridge`.

- [ ] **6. Commit and PR.**
  - Branch: `agent/escalation-routing/<task-1-bead-id>`.
  - Commit message: `kbeads: escalation lane-ref grammar; routing + approval provenance fields server-only (escalation-routing T1)`.
  - Merge lane: desk. `escalation_approval_authz.go` is a human path, so open the PR ready (not draft) and let the merge desk review and merge. Self-merge doesn't apply.
  - The PR body names CONTRACT ISSUES 8 and 9 (revised: admin PATCH of a routing field is refused, R1), the `approversNamePerson` fix to #756 (R2), and the `commons` approval method #729 already writes.

### Task 2: `escalation_policy` evaluator, one floor, create-time stamping, audience and sweep

**Step bead:** (minted later). Phase 1, Routing model. Blocked by: Task 1. Blocks: Tasks 3 and 5.

**Who does what:** CREW writes the code and opens the PR as a draft. Only the OPERATOR merges it, because it changes who may approve (Global Constraint 2). The merge desk reviews it but does not merge.

**Files:**
- Create `server/escalation_policy.go`. It holds:
  - the contract types: `escalationPolicyWhen`, `escalationPolicyRule`, `escalationCoordinator`, `escalationPolicy`, `escalationFacts` and `escalationRoute`;
  - `defaultEscalationPolicy`, `evaluateEscalationPolicy` (rules only) and `laneAudience` (D11, the one mapping);
  - `parseEscalationPolicy` and `validateEscalationPolicy`;
  - `activeEscalationPolicy`, `escalationFactsFor`, `escalationRouteFieldsFor`, `laneForAudience` and `stampEscalationRoute`;
  - the sweep.
- Modify `server/escalation_audience_authz.go` (#756), the one floor (D3): re-locate by `func (s *BeadsServer) escalationAudienceForced(ctx context.Context, bead *model.Bead) bool {`. It becomes a wrapper over the new `escalationForcedReason`; add `escalationFloorOperationTypes`, `escalationFloorOperation`, `escalationFloorReason` and `isEscalationFloorReason` above it.
- Modify `gardenio/controller/internal/beadsapi/escalation_audience.go` (#756): re-locate by `func EscalationAudienceForced(fields map[string]string) bool {`, the read-side mirror.
- Create `server/escalation_lane_members.go`. It holds `SetEscalationCoordinatorClients`, `coordinatorClientAllowed`, `tenantLandBead`, `isTenantLand`, `landDelegatesToSteward`, `laneStaffed` and `firstStaffedLane` (CONTRACT ISSUES 10, 11 and 14).
- Create `server/escalation_policy_test.go`.
- Modify `server/config.go`: re-locate by `"type:spawn_policy": {Key: "type:spawn_policy", Value: json.RawMessage(` and add a sibling `"type:escalation_policy"` entry right after the spawn_policy entry closes.
- Modify `server/beads.go`, `createBead`: re-locate by `	case "spawn_policy":` and add `case escalationPolicyType:`.
- Modify `server/beads.go`, `createBead`: after the block `if denial := spawnLineageSealPatchDenial("create", beadType, bead.Fields, nil); denial != nil {`, stamp the route. That is after #756's `s.stampEscalationAudience(ctx, bead)`, which this task leaves in place: it still validates the filer's `--audience`.
- Modify `server/beads.go`, the update path: re-locate by `		if bead.Type == "spawn_policy" {` and add the matching `escalation_policy` validation.
- Modify `server/server.go`: re-locate by `approverClients map[string]struct{}` and add `coordinatorClients map[string]struct{}` beside it.
- Modify `kbeads/internal/config/config.go`: re-locate by `EscalationApproverClients []string` and `EscalationApproverClients: parseCommaSeparated(os.Getenv("BEADS_ESCALATION_APPROVER_CLIENTS")),`; add `EscalationCoordinatorClients`.
- Modify `kbeads/cmd/kd/serve.go`: re-locate by `beadsServer.SetEscalationApproverClients(approverClients)` and `beadsServer.StartSpawnPolicySweep(auditRetentionCtx)`; wire the setter and the sweep.

**Interfaces:**
- Consumes:
  - from Task 1: `parseLaneRef`, `laneRef`, `laneKind` constants and `validLaneName`;
  - `parseSpawnEscalationFields(b) spawnEscalationFields` (`spawn_policy.go`);
  - `normalizeOperationType` (`escalation_scope.go`);
  - `escalationApprovers(b) ([]string, error)`;
  - `projectLabelValues(labels) []string` (`channel_validation.go`), `projectFromFields(fields) string` (`write_authz.go`) and `exactFieldString(fields, key) (string, bool, error)` (`role_list.go`);
  - `setFieldValue(fields, key, value any) json.RawMessage` (`scrub_request.go`);
  - `s.mergeBeadFields(ctx, id, extra map[string]any) error` (`http.go`, re-locate by `func (s *BeadsServer) mergeBeadFields(`); it writes straight to the store, so #756's frozen-field guard does not see it;
  - from #756 (`escalation_audience_authz.go`): `escalationAudienceField`, `audienceOwner`, `audienceOperator`, `audienceDesk`, `validEscalationAudience`, `credentialRequirementsSet`, `approversNamePerson`, `escalationSpawnTarget`, `isNeverAutoApproveRole`, `roleIsPrivileged` and `matchingHumanSpawnRule`;
  - `s.store.FindProjectByName(ctx, name) (*model.Bead, error)` and `s.store.TryAdvisoryLock(ctx, key, fn) (bool, error)`;
  - `landKindTenant` (`lands_ledger.go`).
- Produces: the contract signatures, plus:
```go
const escalationPolicyType = "escalation_policy"
func parseEscalationPolicy(fields json.RawMessage) (escalationPolicy, error)
func validateEscalationPolicy(fields json.RawMessage) string
func (s *BeadsServer) escalationRouteFieldsFor(ctx context.Context, b *model.Bead) (map[string]any, error)
func (s *BeadsServer) SetEscalationCoordinatorClients(clients []string)
func (s *BeadsServer) coordinatorClientAllowed(client string) bool
func (s *BeadsServer) tenantLandBead(ctx context.Context, land string) (*model.Bead, error)
func (s *BeadsServer) isTenantLand(ctx context.Context, land string) (bool, error)
func (s *BeadsServer) landDelegatesToSteward(ctx context.Context, land, opType string) (bool, error)
func (s *BeadsServer) laneStaffed(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error)
func (s *BeadsServer) firstOwnerLane(esc *model.Bead, approvers []string) string // D11 rule 2/3: first operator|land-owner lane, else operator
```
  and the #756-side floor from the Interface contract: `escalationFloorOperationTypes`, `escalationFloorOperation`, `escalationFloorReason`, `isEscalationFloorReason` and `(s) escalationForcedReason`.

**Steps:**

- [ ] **1. Write the failing tests** in `server/escalation_policy_test.go`. They use these helpers:
  - `newMockStore`;
  - `doStaticEscalationReq` (Task 1, `escalation_route_fields_test.go`);
  - `newTestServer`, `doJSON`, `requireStatus` and `decodeJSON` (`http_test.go`).

  The mock store's `FindProjectByName` (`http_test.go`) matches `projectNameFromTitle(b.Title)`, so the seeded project's title is the bare land name.

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestDefaultEscalationPolicy_IsD7(t *testing.T) {
	merges := []string{"github_pr_merge", "gitlab_mr_merge"}
	want := escalationPolicy{
		Rules: []escalationPolicyRule{
			{When: escalationPolicyWhen{OperationTypes: merges, Projects: []string{"gardenio"}}, Approvers: []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, ExecuteBy: "coordinator:merge-desk"},
			{When: escalationPolicyWhen{OperationTypes: merges, Lands: []string{"*"}}, Approvers: []string{"steward:{land}", "land-owner:{land}"}, ExecuteBy: "requester"},
			{When: escalationPolicyWhen{OperationTypes: []string{"manual_owner_action", "soak_verification"}}, Approvers: []string{"operator"}, ExecuteBy: "operator"},
			{When: escalationPolicyWhen{OperationTypes: []string{"create", "update", "close", "grant"}}, Approvers: []string{"operator"}, ExecuteBy: "server", HoldBy: "coordinator:laptop"},
			{Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"},
		},
		Coordinators: map[string]escalationCoordinator{
			"merge-desk": {Principals: []string{"role:reviewer"}, Audience: "desk"},
			"laptop":     {Principals: []string{"machine:gardenio-laptop-operator"}, Audience: "operator"},
		},
	}
	if got := defaultEscalationPolicy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultEscalationPolicy() =\n%+v\nwant D7\n%+v", got, want)
	}
	raw, _ := json.Marshal(want)
	if reason := validateEscalationPolicy(raw); reason != "" {
		t.Fatalf("D7 must validate, got %q", reason)
	}
}

// TestEscalationForcedReason: the one floor (D3) is #756's forced-owner rule
// plus D3's operation types, with a reason for each.
func TestEscalationForcedReason(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	floor := map[string]string{
		`{"operation_type":"github_pr_merge","sensitivity":"secret"}`:             "sensitivity_secret",
		`{"operation_type":"github_pr_merge","credential_requirements":["gh"]}`:   "credential_requirements",
		`{"operation_type":"credential_rotation"}`:                                 "credential_rotation",
		`{"operation_type":"Secret_Write"}`:                                        "secret_write",
		`{"operation_type":"gardenio_user_capability"}`:                            "gardenio_user_capability",
		`{"operation_type":"escalation_policy_write"}`:                             "escalation_policy_write",
		`{"operation_type":"spawn_policy_write"}`:                                  "spawn_policy_write",
		`{"operation_type":"role_write"}`:                                          "role_write",
		`{"operation_type":"keycloak_group_membership"}`:                           "keycloak_group_membership",
		`{"operation_type":"credential_provisioning"}`:                             "credential_provisioning",
		`{"operation_type":"secret_rotate_db"}`:                                    "secret_rotate_db",
		`{"operation_type":"create","approvers":["oidc:owner@example.com"]}`:       "approvers_name_person",
	}
	for raw, want := range floor {
		b := &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(raw)}
		if got := s.escalationForcedReason(context.Background(), b); got != want {
			t.Errorf("escalationForcedReason(%s) = %q, want %q", raw, got, want)
		}
		if !s.escalationAudienceForced(context.Background(), b) {
			t.Errorf("escalationAudienceForced(%s) = false; #756's wrapper must agree", raw)
		}
		if isEscalationFloorReason(want) != (want != "approvers_name_person") {
			t.Errorf("isEscalationFloorReason(%q) wrong", want)
		}
	}
	// Positive control: ordinary operations, and Task 2's own lane-ref
	// approvers, are not forced.
	for _, raw := range []string{
		`{"operation_type":"github_pr_merge","sensitivity":"internal"}`,
		`{"operation_type":"helm_deploy"}`,
		`{"operation_type":"create"}`,
		`{"operation_type":"aws_iam","sensitivity":"sensitive"}`,
		`{"operation_type":"github_pr_merge","approvers":["coordinator:merge-desk","coordinator:laptop","operator"]}`,
	} {
		b := &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(raw)}
		if got := s.escalationForcedReason(context.Background(), b); got != "" {
			t.Errorf("escalationForcedReason(%s) = %q, want not forced", raw, got)
		}
	}
}

// TestLaneAudience pins D11's mapping, the only place it lives.
func TestLaneAudience(t *testing.T) {
	p := defaultEscalationPolicy()
	for ref, want := range map[string]string{
		"operator": "owner", "land-owner:acme": "owner",
		"coordinator:merge-desk": "desk", "coordinator:laptop": "operator",
		"steward:acme": "desk", "requester": "operator", "server": "operator",
		// Unknown coordinators and refs fail toward the owner (Global Constraint 4).
		"coordinator:ghost": "owner", "bogus": "owner", "": "owner",
	} {
		if got := laneAudience(ref, p); got != want {
			t.Errorf("laneAudience(%q) = %q, want %q", ref, got, want)
		}
	}
	// A coordinator with no audience is operator (the default).
	p.Coordinators["x"] = escalationCoordinator{Principals: []string{"role:x"}}
	if got := laneAudience("coordinator:x", p); got != "operator" {
		t.Errorf("default coordinator audience = %q, want operator", got)
	}
}

func TestEvaluateEscalationPolicy_D7Routes(t *testing.T) {
	p := defaultEscalationPolicy()
	cases := []struct {
		f    escalationFacts
		want escalationRoute
	}{
		{escalationFacts{OperationType: "github_pr_merge", Project: "gardenio"},
			escalationRoute{Rule: "escalation#1", Approvers: []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, ExecuteBy: "coordinator:merge-desk"}},
		{escalationFacts{OperationType: "gitlab_mr_merge", Project: "acme", Land: "acme"},
			escalationRoute{Rule: "escalation#2", Approvers: []string{"steward:acme", "land-owner:acme"}, ExecuteBy: "requester"}},
		// claimlayer is an operator project (no tenant land_kind): a merge there is not rule 2.
		{escalationFacts{OperationType: "github_pr_merge", Project: "claimlayer"},
			escalationRoute{Rule: "escalation#5", Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"}},
		{escalationFacts{OperationType: "soak_verification", Project: "gardenio"},
			escalationRoute{Rule: "escalation#3", Approvers: []string{"operator"}, ExecuteBy: "operator"}},
		{escalationFacts{OperationType: "close"},
			escalationRoute{Rule: "escalation#4", Approvers: []string{"operator"}, ExecuteBy: "server", HoldBy: "coordinator:laptop"}},
		{escalationFacts{OperationType: "helm_deploy", Project: "gardenio"},
			escalationRoute{Rule: "escalation#5", Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"}},
		// The evaluator no longer floors (escalationForcedReason does, at stamp
		// time): a credential_rotation falls to the catch-all here.
		{escalationFacts{OperationType: "credential_rotation", Project: "gardenio"},
			escalationRoute{Rule: "escalation#5", Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"}},
	}
	for _, c := range cases {
		if got := evaluateEscalationPolicy(p, c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("evaluate(%+v) = %+v, want %+v", c.f, got, c.want)
		}
	}
	// Fail closed when nothing matches (CONTRACT ISSUE 15).
	got := evaluateEscalationPolicy(escalationPolicy{Rules: []escalationPolicyRule{{When: escalationPolicyWhen{OperationTypes: []string{"x"}}, Approvers: []string{"operator"}, ExecuteBy: "operator"}}}, escalationFacts{OperationType: "y"})
	if !got.OperatorOnly || got.Rule != "floor:no_matching_rule" || !reflect.DeepEqual(got.Approvers, []string{"operator"}) {
		t.Fatalf("no match: got %+v, want floor:no_matching_rule operator-only", got)
	}
}

func TestValidateEscalationPolicy(t *testing.T) {
	bad := map[string]string{
		"not json":            `{"rules":`,
		"no rules":            `{"rules":[],"coordinators":{}}`,
		"unknown rule key":    `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","mode":"auto"}],"coordinators":{}}`,
		"unknown when key":    `{"rules":[{"when":{"project":["gardenio"]},"approvers":["operator"],"execute_by":"operator"},{"when":{},"approvers":["operator"],"execute_by":"operator"}],"coordinators":{}}`,
		"no approvers":        `{"rules":[{"when":{},"approvers":[],"execute_by":"operator"}],"coordinators":{}}`,
		"bad lane":            `{"rules":[{"when":{},"approvers":["role:crew"],"execute_by":"operator"}],"coordinators":{}}`,
		"undefined coord":     `{"rules":[{"when":{},"approvers":["coordinator:ghost"],"execute_by":"operator"}],"coordinators":{}}`,
		"land without lands":  `{"rules":[{"when":{},"approvers":["steward:{land}"],"execute_by":"operator"}],"coordinators":{}}`,
		"requester approves":  `{"rules":[{"when":{},"approvers":["requester"],"execute_by":"operator"}],"coordinators":{}}`,
		"legacy execute_by":   `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"oidc:a@example.com"}],"coordinators":{}}`,
		"empty execute_by":    `{"rules":[{"when":{},"approvers":["operator"],"execute_by":""}],"coordinators":{}}`,
		"no catch-all":        `{"rules":[{"when":{"operation_types":["x"]},"approvers":["operator"],"execute_by":"operator"}],"coordinators":{}}`,
		"empty principals":    `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":[]}}}`,
		"bad principal":       `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":["user:x"]}}}`,
		"missing coordinators": `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator"}]}`,
		"owner coordinator":    `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":["role:x"],"audience":"owner"}}}`,
		"bad coordinator aud":  `{"rules":[{"when":{},"approvers":["coordinator:a"],"execute_by":"operator"}],"coordinators":{"a":{"principals":["role:x"],"audience":"everyone"}}}`,
		"requester holds":      `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","hold_by":"requester"}],"coordinators":{}}`,
		"legacy holds":         `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","hold_by":"oidc:a@example.com"}],"coordinators":{}}`,
		"undefined holder":     `{"rules":[{"when":{},"approvers":["operator"],"execute_by":"operator","hold_by":"coordinator:ghost"}],"coordinators":{}}`,
	}
	for name, raw := range bad {
		if reason := validateEscalationPolicy(json.RawMessage(raw)); reason == "" {
			t.Errorf("%s: validateEscalationPolicy accepted %s", name, raw)
		}
	}
	// Positive control: a minimal valid policy with legacy approvers and a land rule.
	good := `{"rules":[
		{"when":{"lands":["*"]},"approvers":["steward:{land}","oidc:a@example.com"],"execute_by":"requester"},
		{"when":{},"approvers":["operator"],"execute_by":"coordinator:a","hold_by":"coordinator:a"}],
		"coordinators":{"a":{"principals":["role:reviewer","machine:gardenio-laptop-operator"],"audience":"desk"}}}`
	if reason := validateEscalationPolicy(json.RawMessage(good)); reason != "" {
		t.Fatalf("valid policy refused: %s", reason)
	}
}

func TestEscalationPolicyWrite_MalformedIs400(t *testing.T) {
	_, ms, h := newTestServer() // OIDC gardenio-admin: the type's write_policy min_role
	valid := map[string]any{
		"rules":        []any{map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "operator"}},
		"coordinators": map[string]any{},
	}
	rec := doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{"title": "escalation policy", "type": "escalation_policy", "kind": "config", "fields": valid})
	requireStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	id, _ := created["id"].(string)

	bad := map[string]any{"rules": []any{map[string]any{"when": map[string]any{}, "approvers": []string{"coordinator:ghost"}, "execute_by": "operator"}}, "coordinators": map[string]any{}}
	rec = doJSON(t, h, http.MethodPost, "/v1/beads", map[string]any{"title": "bad policy", "type": "escalation_policy", "kind": "config", "fields": bad})
	requireStatus(t, rec, http.StatusBadRequest)

	before := string(ms.beads[id].Fields)
	rec = doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": bad})
	requireStatus(t, rec, http.StatusBadRequest)
	if string(ms.beads[id].Fields) != before {
		t.Fatal("a refused policy update changed the bead")
	}
}

func seedTenantLand(ms *mockStore, land string, stewardApproves []string) {
	f := map[string]any{"land_kind": "tenant"}
	if stewardApproves != nil {
		f["land_steward_approves"] = stewardApproves
	}
	raw, _ := json.Marshal(f)
	ms.beads["gd-proj-"+land] = &model.Bead{ID: "gd-proj-" + land, Kind: model.KindConfig, Type: model.BeadType("project"), Title: land, Status: model.StatusOpen, Fields: raw}
}

func createEscalationStatic(t *testing.T, h http.Handler, labels []string, fields map[string]any) map[string]any {
	t.Helper()
	rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "routed", "type": "escalation", "kind": "data", "labels": labels, "fields": fields,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, rec, &resp)
	out, _ := resp["fields"].(map[string]any)
	return out
}

func assertRoute(t *testing.T, got map[string]any, heldBy, executeBy, rule string, operatorOnly bool, approvers ...string) {
	t.Helper()
	var gotApprovers []string
	for _, a := range got["approvers"].([]any) {
		gotApprovers = append(gotApprovers, a.(string))
	}
	if at, _ := got["held_at"].(string); at == "" {
		t.Fatalf("held_at not stamped with held_by: %v", got)
	}
	// D11: audience always follows held_by (laneAudience on the D7 coordinators;
	// a coordinator a test defines without an audience is operator).
	wantAudience := "operator"
	switch {
	case heldBy == "operator", strings.HasPrefix(heldBy, "land-owner:"):
		wantAudience = "owner"
	case heldBy == "coordinator:merge-desk", strings.HasPrefix(heldBy, "steward:"):
		wantAudience = "desk"
	}
	if got["audience"] != wantAudience {
		t.Fatalf("audience = %v with held_by=%s, want %s (D11)", got["audience"], heldBy, wantAudience)
	}
	if got["held_by"] != heldBy || got["execute_by"] != executeBy || got["policy_rule"] != rule || got["operator_only"] != operatorOnly || !reflect.DeepEqual(gotApprovers, approvers) {
		t.Fatalf("route = held_by=%v execute_by=%v policy_rule=%v operator_only=%v approvers=%v; want %s %s %s %v %v",
			got["held_by"], got["execute_by"], got["policy_rule"], got["operator_only"], gotApprovers, heldBy, executeBy, rule, operatorOnly, approvers)
	}
}

func TestEscalationCreate_StampsRoute(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)
	seedTenantLand(ms, "acme", nil)
	seedTenantLand(ms, "delegated", []string{"github_pr_merge"})

	// gardenio merge: the merge desk (role:reviewer) is always staffed; the laptop
	// is not (BEADS_ESCALATION_COORDINATOR_CLIENTS unset), and it sits second anyway.
	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:merge-desk", "coordinator:merge-desk", "escalation#1", false, "coordinator:merge-desk", "coordinator:laptop", "operator")

	// Floor.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "credential_rotation"})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:credential_rotation", true, "operator")

	// Tenant land without delegation: the steward is skipped (Review Focus 5).
	got = createEscalationStatic(t, h, []string{"project:acme"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "land-owner:acme", "requester", "escalation#2", false, "steward:acme", "land-owner:acme")

	// Positive control: the same merge in a land that delegates merges to its steward.
	got = createEscalationStatic(t, h, []string{"project:delegated"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "steward:delegated", "requester", "escalation#2", false, "steward:delegated", "land-owner:delegated")

	// Everything else: held by the laptop (hold_by), which is not an approver.
	// It need not be staffed to hold (D11 rule 2): before Task 13 allowlists its
	// client the escalation still reads as #756's operator audience.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "helm_deploy"})
	assertRoute(t, got, "coordinator:laptop", "coordinator:laptop", "escalation#5", false, "operator")

	// The owner's own kind of work is held by the operator lane.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "manual_owner_action"})
	assertRoute(t, got, "operator", "operator", "escalation#3", false, "operator")
}

// TestEscalationCreate_AudienceFollowsHeldBy pins D11 rule 2 (Review Focus 4):
// a filer may raise an escalation to the owner, which holds it in an
// owner-audience lane; a filer's desk or operator is ignored (#756); an
// invalid value is still #756's 400.
func TestEscalationCreate_AudienceFollowsHeldBy(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)

	// Raised to the owner: same approvers, but held by the operator lane.
	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge", "audience": "owner"})
	assertRoute(t, got, "operator", "coordinator:merge-desk", "escalation#1", false, "coordinator:merge-desk", "coordinator:laptop", "operator")

	// Positive control: the same merge without the raise goes to the desk.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:merge-desk", "coordinator:merge-desk", "escalation#1", false, "coordinator:merge-desk", "coordinator:laptop", "operator")

	// A filer cannot lower: desk on a helm_deploy is ignored.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "helm_deploy", "audience": "desk"})
	assertRoute(t, got, "coordinator:laptop", "coordinator:laptop", "escalation#5", false, "operator")

	// #756's 400 on an invalid value is unchanged.
	rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/beads", map[string]any{
		"title": "bad", "type": "escalation", "kind": "data", "fields": map[string]any{"operation_type": "helm_deploy", "audience": "everyone"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid audience: got %d, want 400", rec.Code)
	}

	// credential_requirements is a floor reason now (D3).
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge", "credential_requirements": []string{"gh-token"}})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:credential_requirements", true, "operator")
}

// Review Focus 3: a policy bead that hands credential_rotation to the laptop
// is overridden by the code floor.
func TestEscalationCreate_FloorBeatsConfig(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)
	pol, _ := json.Marshal(map[string]any{
		"rules": []any{
			map[string]any{"when": map[string]any{"operation_types": []string{"credential_rotation", "github_pr_merge"}}, "approvers": []string{"coordinator:laptop"}, "execute_by": "coordinator:laptop"},
			map[string]any{"when": map[string]any{}, "approvers": []string{"operator"}, "execute_by": "operator"},
		},
		"coordinators": map[string]any{"laptop": map[string]any{"principals": []string{"machine:gardenio-laptop-operator"}, "audience": "operator"}},
	})
	ms.beads["gd-pol-floor"] = &model.Bead{ID: "gd-pol-floor", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})

	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "credential_rotation"})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:credential_rotation", true, "operator")
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge", "sensitivity": "secret"})
	assertRoute(t, got, "operator", "coordinator:laptop", "floor:sensitivity_secret", true, "operator")

	// Positive control: the same rule routes a non-floor operation as configured.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:laptop", "coordinator:laptop", "escalation#1", false, "coordinator:laptop")
}

func TestEscalationCreate_KeepsServerStampedSpawnApprovers(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	ctx := withServerStampedEscalation(context.Background())
	fields, _ := json.Marshal(map[string]any{"operation_type": "create", "approvers": []string{"oidc:owner@example.com"}})
	b := &model.Bead{Type: model.TypeEscalation, Fields: fields}
	if err := s.stampEscalationRoute(ctx, b); err != nil {
		t.Fatal(err)
	}
	approvers, _ := escalationApprovers(b)
	if !reflect.DeepEqual(approvers, []string{"oidc:owner@example.com"}) {
		t.Fatalf("spawn-rule approvers overwritten: %v", approvers)
	}
	if v, _, _ := exactFieldString(b.Fields, "held_by"); v != "operator" {
		t.Fatalf("held_by = %q, want operator (a legacy entry is not a lane)", v)
	}
	if v, _, _ := exactFieldString(b.Fields, "execute_by"); v != "server" {
		t.Fatalf("execute_by = %q, want server (rule 4)", v)
	}
	// approvers_name_person forces the owner without the floor: the named
	// person must still be able to approve, so it is not operator_only, and
	// rule 4's hold_by is overridden.
	if v, _, _ := exactFieldString(b.Fields, "audience"); v != "owner" {
		t.Fatalf("audience = %q, want owner (forced)", v)
	}
	if v := escalationFieldValues(b.Fields, []string{"operator_only"})["operator_only"]; v != false {
		t.Fatalf("operator_only = %v, want false for approvers_name_person", v)
	}
}

func TestEscalationCreate_ActivePolicyBeadWins(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)
	pol, _ := json.Marshal(map[string]any{
		"rules":        []any{map[string]any{"when": map[string]any{}, "approvers": []string{"coordinator:desk", "operator"}, "execute_by": "operator"}},
		"coordinators": map[string]any{"desk": map[string]any{"principals": []string{"role:reviewer"}}},
	})
	ms.beads["gd-pol1"] = &model.Bead{ID: "gd-pol1", Kind: model.KindConfig, Type: model.BeadType(escalationPolicyType), Title: "escalation policy", Status: model.StatusOpen, Fields: pol}

	got := createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "github_pr_merge"})
	assertRoute(t, got, "coordinator:desk", "operator", "escalation#1", false, "coordinator:desk", "operator")

	// Positive control: the floor still wins over the configured catch-all.
	got = createEscalationStatic(t, h, []string{"project:gardenio"}, map[string]any{"operation_type": "secret_write"})
	assertRoute(t, got, "operator", "operator", "floor:secret_write", true, "operator")
}

func TestFirstStaffedLane(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTenantLand(ms, "acme", nil)
	p := defaultEscalationPolicy()
	esc := &model.Bead{ID: "gd-e", Type: model.TypeEscalation, Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
	ctx := context.Background()

	if got := s.firstStaffedLane(ctx, esc, []string{"coordinator:laptop", "operator"}, p); got != "operator" {
		t.Fatalf("laptop with no allowlisted client: got %q, want operator", got)
	}
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})
	if got := s.firstStaffedLane(ctx, esc, []string{"coordinator:laptop", "operator"}, p); got != "coordinator:laptop" {
		t.Fatalf("laptop allowlisted: got %q, want coordinator:laptop", got)
	}
	if got := s.firstStaffedLane(ctx, esc, []string{"bogus:x", "requester", "steward:acme", "land-owner:acme"}, p); got != "land-owner:acme" {
		t.Fatalf("got %q, want land-owner:acme (bogus, unverified requester and undelegated steward skipped)", got)
	}
	if got := s.firstStaffedLane(ctx, esc, []string{"land-owner:notaland"}, p); got != "operator" {
		t.Fatalf("land-owner of a non-tenant project: got %q, want operator", got)
	}
}

func TestEscalationRouteSweep_StampsOnlyUnstamped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seed := func(id string, status model.Status, f map[string]any) {
		raw, _ := json.Marshal(f)
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: id, Status: status, Fields: raw, Labels: []string{"project:gardenio"}}
	}
	seed("gd-legacy", model.StatusOpen, map[string]any{"operation_type": "manual_owner_action"})
	seed("gd-legacy-ip", model.StatusInProgress, map[string]any{"operation_type": "github_pr_merge"})
	seed("gd-stamped", model.StatusOpen, map[string]any{"operation_type": "github_pr_merge", "held_by": "coordinator:laptop"})
	seed("gd-closed", model.StatusClosed, map[string]any{"operation_type": "github_pr_merge"})
	// #756 already stamped (or an operator re-routed) these: the sweep keeps
	// the audience and picks a lane of it (D11 rule 4).
	seed("gd-routed-desk", model.StatusOpen, map[string]any{"operation_type": "helm_deploy", "audience": "desk"})
	seed("gd-routed-owner", model.StatusOpen, map[string]any{"operation_type": "github_pr_merge", "audience": "owner"})

	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy"].Fields, "held_by"); v != "operator" {
		t.Errorf("gd-legacy held_by = %q, want operator (rule 3)", v)
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy"].Fields, "held_at"); v == "" {
		t.Error("gd-legacy held_at not stamped by the sweep")
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy-ip"].Fields, "held_by"); v != "coordinator:merge-desk" {
		t.Errorf("gd-legacy-ip held_by = %q, want coordinator:merge-desk", v)
	}
	if v, _, _ := exactFieldString(ms.beads["gd-legacy-ip"].Fields, "audience"); v != "desk" {
		t.Errorf("gd-legacy-ip audience = %q, want desk (laneAudience)", v)
	}
	for id, want := range map[string][2]string{
		"gd-routed-desk":  {"coordinator:merge-desk", "desk"}, // no desk approver: the desk coordinator holds it
		"gd-routed-owner": {"operator", "owner"},
	} {
		held, _, _ := exactFieldString(ms.beads[id].Fields, "held_by")
		aud, _, _ := exactFieldString(ms.beads[id].Fields, "audience")
		if held != want[0] || aud != want[1] {
			t.Errorf("%s: held_by=%q audience=%q, want %q %q", id, held, aud, want[0], want[1])
		}
	}
	if v, _, _ := exactFieldString(ms.beads["gd-stamped"].Fields, "held_by"); v != "coordinator:laptop" {
		t.Errorf("an already-routed escalation was re-stamped: held_by = %q", v)
	}
	if _, present, _ := exactFieldString(ms.beads["gd-closed"].Fields, "held_by"); present {
		t.Error("a closed escalation was stamped")
	}
}
```

- [ ] **2. Run the tests and watch them fail.**
  Run: `cd kbeads && go test ./internal/server -run 'TestDefaultEscalationPolicy_IsD7|TestEscalationForcedReason|TestLaneAudience|TestEvaluateEscalationPolicy_D7Routes|TestValidateEscalationPolicy|TestEscalationPolicyWrite_MalformedIs400|TestEscalationCreate_StampsRoute|TestEscalationCreate_AudienceFollowsHeldBy|TestEscalationCreate_FloorBeatsConfig|TestEscalationCreate_KeepsServerStampedSpawnApprovers|TestEscalationCreate_ActivePolicyBeadWins|TestFirstStaffedLane|TestEscalationRouteSweep_StampsOnlyUnstamped' -count=1`
  Expected: FAIL to build with `undefined: escalationPolicy`, `undefined: defaultEscalationPolicy`, `undefined: (*BeadsServer).escalationForcedReason` and `undefined: (*BeadsServer).sweepUnroutedEscalationsLocked`.

- [ ] **3. Implement.** Create `server/escalation_policy.go`:

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// escalationPolicyType is the singleton config bead that routes escalations
// (escalation-routing plan D7). The newest open one is active; none means
// defaultEscalationPolicy.
const escalationPolicyType = "escalation_policy"

type escalationPolicyWhen struct {
	OperationTypes []string `json:"operation_types,omitempty"`
	Projects       []string `json:"projects,omitempty"`
	Lands          []string `json:"lands,omitempty"` // "*" = any tenant land
	Sensitivity    []string `json:"sensitivity,omitempty"`
}

type escalationPolicyRule struct {
	When      escalationPolicyWhen `json:"when"`
	Approvers []string             `json:"approvers"`  // lane refs; may contain "{land}"
	ExecuteBy string               `json:"execute_by"` // lane ref; may contain "{land}"
	// HoldBy holds the escalation at create without being an approver (D7):
	// it may claim and hand up, never approve. Empty: the first staffed approver.
	HoldBy string `json:"hold_by,omitempty"`
}

type escalationCoordinator struct {
	Principals []string `json:"principals"` // "machine:<id>" | "role:<name>"
	// Audience is #756's audience for an escalation this coordinator holds
	// (D11): "desk" or "operator" (the default). Never "owner".
	Audience string `json:"audience,omitempty"`
}

type escalationPolicy struct {
	Rules        []escalationPolicyRule           `json:"rules"`
	Coordinators map[string]escalationCoordinator `json:"coordinators"`
}

// escalationFacts are what a rule matches on. Land is set only when Project is
// a tenant land (land_kind == "tenant").
type escalationFacts struct{ OperationType, Project, Land, Sensitivity string }

// escalationRoute is what the policy's rules say. The D3 floor is not here:
// it is escalationForcedReason (escalation_audience_authz.go), applied by
// escalationRouteFieldsFor, so there is one floor for routing and audience.
type escalationRoute struct {
	Rule         string // "escalation#<i>" (1-based) | "floor:no_matching_rule"
	Approvers    []string
	ExecuteBy    string
	HoldBy       string
	OperatorOnly bool // only floor:no_matching_rule
}

// laneAudience is D11's mapping, the only one: the #756 audience an escalation
// held by ref has. The read side never recomputes it; it reads the stamp. An
// unknown ref or coordinator fails toward the owner (Global Constraint 4).
func laneAudience(ref string, p escalationPolicy) string {
	lane, err := parseLaneRef(ref)
	if err != nil {
		return audienceOwner
	}
	switch lane.Kind {
	case laneOperator, laneLandOwner:
		return audienceOwner
	case laneSteward:
		return audienceDesk
	case laneRequester, laneServer:
		return audienceOperator
	case laneCoordinator:
		c, ok := p.Coordinators[lane.Name]
		if !ok {
			return audienceOwner
		}
		if c.Audience == audienceDesk {
			return audienceDesk
		}
		return audienceOperator
	}
	return audienceOwner
}

// defaultEscalationPolicy is D7, used while no escalation_policy bead exists.
func defaultEscalationPolicy() escalationPolicy {
	merges := []string{"github_pr_merge", "gitlab_mr_merge"}
	return escalationPolicy{
		Rules: []escalationPolicyRule{
			{When: escalationPolicyWhen{OperationTypes: merges, Projects: []string{"gardenio"}}, Approvers: []string{"coordinator:merge-desk", "coordinator:laptop", "operator"}, ExecuteBy: "coordinator:merge-desk"},
			{When: escalationPolicyWhen{OperationTypes: merges, Lands: []string{"*"}}, Approvers: []string{"steward:{land}", "land-owner:{land}"}, ExecuteBy: "requester"},
			{When: escalationPolicyWhen{OperationTypes: []string{"manual_owner_action", "soak_verification"}}, Approvers: []string{"operator"}, ExecuteBy: "operator"},
			{When: escalationPolicyWhen{OperationTypes: []string{"create", "update", "close", "grant"}}, Approvers: []string{"operator"}, ExecuteBy: "server", HoldBy: "coordinator:laptop"},
			{Approvers: []string{"operator"}, ExecuteBy: "coordinator:laptop", HoldBy: "coordinator:laptop"},
		},
		Coordinators: map[string]escalationCoordinator{
			"merge-desk": {Principals: []string{"role:reviewer"}, Audience: audienceDesk},
			"laptop":     {Principals: []string{"machine:gardenio-laptop-operator"}, Audience: audienceOperator},
		},
	}
}

func policyListHas(list []string, v string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return strings.EqualFold(strings.TrimSpace(e), v) })
}

func (w escalationPolicyWhen) empty() bool {
	return len(w.OperationTypes) == 0 && len(w.Projects) == 0 && len(w.Lands) == 0 && len(w.Sensitivity) == 0
}

func (w escalationPolicyWhen) matches(f escalationFacts) bool {
	if len(w.OperationTypes) > 0 && !policyListHas(w.OperationTypes, normalizeOperationType(f.OperationType)) {
		return false
	}
	if len(w.Projects) > 0 && (f.Project == "" || !slices.Contains(w.Projects, f.Project)) {
		return false
	}
	if len(w.Lands) > 0 {
		if f.Land == "" || (!slices.Contains(w.Lands, "*") && !slices.Contains(w.Lands, f.Land)) {
			return false
		}
	}
	if len(w.Sensitivity) > 0 && !policyListHas(w.Sensitivity, strings.ToLower(strings.TrimSpace(f.Sensitivity))) {
		return false
	}
	return true
}

func templateLand(ref, land string) string { return strings.ReplaceAll(ref, "{land}", land) }

// evaluateEscalationPolicy routes f by the first matching rule. It does not
// apply the D3 floor; escalationRouteFieldsFor does, from
// escalationForcedReason. Nothing matching fails closed to an operator-only
// route.
func evaluateEscalationPolicy(p escalationPolicy, f escalationFacts) escalationRoute {
	for i, rule := range p.Rules {
		if !rule.When.matches(f) {
			continue
		}
		approvers := make([]string, 0, len(rule.Approvers))
		for _, a := range rule.Approvers {
			approvers = append(approvers, templateLand(a, f.Land))
		}
		return escalationRoute{Rule: fmt.Sprintf("escalation#%d", i+1), Approvers: approvers, ExecuteBy: templateLand(rule.ExecuteBy, f.Land), HoldBy: templateLand(rule.HoldBy, f.Land)}
	}
	return escalationRoute{Rule: "floor:no_matching_rule", Approvers: []string{string(laneOperator)}, ExecuteBy: string(laneOperator), OperatorOnly: true}
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// parseEscalationPolicy decodes a policy bead's fields. rules and coordinators
// are decoded strictly, so a misspelt key is an error rather than a rule that
// silently matches everything.
func parseEscalationPolicy(fields json.RawMessage) (escalationPolicy, error) {
	var top struct {
		Rules        json.RawMessage `json:"rules"`
		Coordinators json.RawMessage `json:"coordinators"`
	}
	if len(fields) == 0 {
		return escalationPolicy{}, fmt.Errorf("no fields")
	}
	if err := json.Unmarshal(fields, &top); err != nil {
		return escalationPolicy{}, err
	}
	if len(top.Rules) == 0 || len(top.Coordinators) == 0 {
		return escalationPolicy{}, fmt.Errorf("rules and coordinators are both required")
	}
	var p escalationPolicy
	if err := strictDecode(top.Rules, &p.Rules); err != nil {
		return escalationPolicy{}, fmt.Errorf("rules: %w", err)
	}
	if err := strictDecode(top.Coordinators, &p.Coordinators); err != nil {
		return escalationPolicy{}, fmt.Errorf("coordinators: %w", err)
	}
	return p, nil
}

// validateEscalationPolicy returns the 400 reason for a malformed policy, or "".
func validateEscalationPolicy(fields json.RawMessage) string {
	p, err := parseEscalationPolicy(fields)
	if err != nil {
		return "escalation_policy is malformed: " + err.Error()
	}
	if len(p.Rules) == 0 {
		return "escalation_policy needs at least one rule"
	}
	for name, c := range p.Coordinators {
		if !validLaneName(name) {
			return fmt.Sprintf("escalation_policy coordinator name %q is invalid", name)
		}
		if len(c.Principals) == 0 {
			return fmt.Sprintf("escalation_policy coordinator %q lists no principals", name)
		}
		if c.Audience != "" && c.Audience != audienceDesk && c.Audience != audienceOperator {
			return fmt.Sprintf("escalation_policy coordinator %q audience must be desk or operator", name)
		}
		for _, pr := range c.Principals {
			kind, rest, ok := strings.Cut(pr, ":")
			if !ok || (kind != "machine" && kind != "role") || strings.TrimSpace(rest) == "" {
				return fmt.Sprintf("escalation_policy coordinator %q principal %q must be machine:<client-id> or role:<role>", name, pr)
			}
		}
	}
	for i, r := range p.Rules {
		n := i + 1
		landRule := len(r.When.Lands) > 0
		if len(r.Approvers) == 0 {
			return fmt.Sprintf("escalation_policy rule %d lists no approvers", n)
		}
		for _, a := range r.Approvers {
			if reason := validatePolicyLaneRef(p, a, landRule, true); reason != "" {
				return fmt.Sprintf("escalation_policy rule %d approver %q: %s", n, a, reason)
			}
		}
		if reason := validatePolicyLaneRef(p, r.ExecuteBy, landRule, false); reason != "" {
			return fmt.Sprintf("escalation_policy rule %d execute_by %q: %s", n, r.ExecuteBy, reason)
		}
		if r.HoldBy != "" {
			if reason := validatePolicyLaneRef(p, r.HoldBy, landRule, true); reason != "" {
				return fmt.Sprintf("escalation_policy rule %d hold_by %q: %s", n, r.HoldBy, reason)
			}
			if lane, _ := parseLaneRef(templateLand(r.HoldBy, "land")); lane.Kind == laneLegacy {
				return fmt.Sprintf("escalation_policy rule %d hold_by %q: a legacy entry cannot hold an escalation", n, r.HoldBy)
			}
		}
	}
	if !p.Rules[len(p.Rules)-1].When.empty() {
		return "escalation_policy's last rule must have an empty when (the catch-all)"
	}
	return ""
}

func validatePolicyLaneRef(p escalationPolicy, ref string, landRule, approver bool) string {
	if strings.Contains(ref, "{land}") && !landRule {
		return "{land} is only valid in a rule with when.lands"
	}
	lane, err := parseLaneRef(templateLand(ref, "land"))
	if err != nil {
		return err.Error()
	}
	switch lane.Kind {
	case laneLegacy:
		if !approver {
			return "legacy approver entries are valid in approvers only"
		}
	case laneServer, laneRequester:
		if approver {
			return string(lane.Kind) + " may execute but never approve"
		}
	case laneCoordinator:
		if _, ok := p.Coordinators[lane.Name]; !ok {
			return fmt.Sprintf("no coordinator named %q", lane.Name)
		}
	}
	return ""
}

// activeEscalationPolicy returns the newest open escalation_policy bead, or the
// built-in D7 when there is none. A malformed bead is an error, never a
// fallback to D7: callers fail closed.
func (s *BeadsServer) activeEscalationPolicy(ctx context.Context) (escalationPolicy, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{escalationPolicyType},
		Kind:   []model.Kind{"config"},
		Status: []model.Status{model.StatusOpen},
		Sort:   "-created_at",
		Limit:  1,
	})
	if err != nil {
		return escalationPolicy{}, fmt.Errorf("listing escalation_policy: %w", err)
	}
	if len(beads) == 0 {
		return defaultEscalationPolicy(), nil
	}
	p, err := parseEscalationPolicy(beads[0].Fields)
	if err != nil {
		return escalationPolicy{}, fmt.Errorf("escalation_policy %s: %w", beads[0].ID, err)
	}
	return p, nil
}

// escalationFactsFor reads the routing facts from an escalation. Several
// project labels match no project rule; a land lookup failure means no land.
// Both fall to the catch-all (operator), never to a wider lane.
func (s *BeadsServer) escalationFactsFor(ctx context.Context, b *model.Bead) escalationFacts {
	sens, _, _ := exactFieldString(b.Fields, "sensitivity")
	f := escalationFacts{
		OperationType: normalizeOperationType(parseSpawnEscalationFields(b).OperationType),
		Sensitivity:   strings.ToLower(strings.TrimSpace(sens)),
	}
	switch projects := projectLabelValues(b.Labels); len(projects) {
	case 0:
		f.Project = projectFromFields(b.Fields)
	case 1:
		f.Project = projects[0]
	default:
		slog.Warn("escalation routing: several project labels; no project rule applies", "escalation", b.ID, "projects", projects)
	}
	if f.Project != "" {
		tenant, err := s.isTenantLand(ctx, f.Project)
		if err != nil {
			slog.Warn("escalation routing: land lookup failed; routing as a non-land project", "escalation", b.ID, "project", f.Project, "error", err)
		} else if tenant {
			f.Land = f.Project
		}
	}
	return f
}

// escalationRouteFieldsFor computes the stamped routing fields for b, audience
// included (D11 rule 2). A server-stamped approvers list already on b
// (handleEscalation's spawn rule) is kept unless a floor reason applies
// (CONTRACT ISSUE 7). The floor is escalationForcedReason, #756's forced-owner
// rule extended (D3), so routing and audience can never disagree about it.
func (s *BeadsServer) escalationRouteFieldsFor(ctx context.Context, b *model.Bead) (map[string]any, error) {
	f := s.escalationFactsFor(ctx, b)
	forced := s.escalationForcedReason(ctx, b)
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		if !isEscalationFloorReason(forced) {
			return nil, err
		}
		slog.Warn("escalation routing: policy unreadable; a floor escalation routes on the built-in default", "escalation", b.ID, "error", err)
		p = defaultEscalationPolicy()
	}
	route := evaluateEscalationPolicy(p, f)
	approvers, operatorOnly, rule := route.Approvers, route.OperatorOnly, route.Rule
	if isEscalationFloorReason(forced) {
		route.ExecuteBy = string(laneOperator)
		if _, ok := p.Coordinators["laptop"]; ok {
			route.ExecuteBy = "coordinator:laptop" // CONTRACT ISSUE 17
		}
		approvers, operatorOnly, rule = []string{string(laneOperator)}, true, "floor:"+forced
	} else if existing, err := escalationApprovers(b); err == nil && len(existing) > 0 {
		approvers = existing
	}
	asked, _, _ := exactFieldString(b.Fields, escalationAudienceField) // #756 stamped it already
	var held string
	switch {
	case forced != "" || asked == audienceOwner || operatorOnly:
		held = s.firstOwnerLane(b, approvers)
	case route.HoldBy != "":
		held = route.HoldBy // staffed or not (D11 rule 2); the idle hand-up bounds an unstaffed holder
	default:
		held = s.firstStaffedLane(ctx, b, approvers, p)
	}
	return map[string]any{
		"approvers":             approvers,
		"execute_by":            route.ExecuteBy,
		"held_by":               held,
		"held_at":               time.Now().UTC().Format(time.RFC3339),
		escalationAudienceField: laneAudience(held, p),
		"operator_only":         operatorOnly,
		"policy_rule":           rule,
	}, nil
}

// firstOwnerLane is the first owner-audience lane among approvers (operator or
// a land-owner), else operator: where a forced or raised escalation is held.
func (s *BeadsServer) firstOwnerLane(esc *model.Bead, approvers []string) string {
	for _, a := range approvers {
		if lane, err := parseLaneRef(a); err == nil && (lane.Kind == laneOperator || lane.Kind == laneLandOwner) {
			return lane.String()
		}
	}
	return string(laneOperator)
}

// laneForAudience is D11 rule 3: the lane an escalation moves to when it is
// routed to audience a. Owner: the first owner-audience approver, else
// operator. Desk or operator: the first staffed approver lane of that
// audience, else the first policy coordinator of that audience (it holds
// without approving), else an error (the route endpoint's 409).
func (s *BeadsServer) laneForAudience(ctx context.Context, esc *model.Bead, a string, p escalationPolicy) (string, error) {
	approvers, _ := escalationApprovers(esc)
	if a == audienceOwner {
		return s.firstOwnerLane(esc, approvers), nil
	}
	for _, ref := range approvers {
		lane, err := parseLaneRef(ref)
		if err != nil || lane.Kind == laneLegacy || laneAudience(lane.String(), p) != a {
			continue
		}
		if staffed, err := s.laneStaffed(ctx, lane, esc, p); err == nil && staffed {
			return lane.String(), nil
		}
	}
	names := make([]string, 0, len(p.Coordinators))
	for name := range p.Coordinators {
		names = append(names, name)
	}
	slices.Sort(names) // deterministic; D7 has one coordinator per audience
	for _, name := range names {
		if ref := "coordinator:" + name; laneAudience(ref, p) == a {
			return ref, nil
		}
	}
	return "", fmt.Errorf("no %s lane can hold this escalation", a)
}

// stampEscalationRoute writes the route, audience included, into b.Fields
// (the create path). On a policy error b keeps #756's audience and no
// held_by (Global Constraint 7), and the sweep retries.
func (s *BeadsServer) stampEscalationRoute(ctx context.Context, b *model.Bead) error {
	fields, err := s.escalationRouteFieldsFor(ctx, b)
	if err != nil {
		slog.Warn("escalation routing: policy unreadable; escalation left unrouted (#756 audience until the sweep)", "escalation", b.ID, "error", err)
		return err
	}
	for k, v := range fields {
		b.Fields = setFieldValue(b.Fields, k, v)
	}
	return nil
}

const (
	escalationRouteSweepCap      = 500
	escalationRouteSweepInterval = time.Minute
	// escalationRouteAdvisoryLockKey serializes the sweep across replicas.
	// It must differ from every other key (decision aging 0001, audit retention
	// 0002, slow tool calls 0003, spawn policy 0004).
	escalationRouteAdvisoryLockKey int64 = 0x6265_6164_0005
)

// StartEscalationRouteSweep stamps open escalations that have no held_by
// (filed before routing shipped, or while the policy was unreadable). It is
// always on, and it only writes escalations that are missing held_by.
func (s *BeadsServer) StartEscalationRouteSweep(ctx context.Context) {
	go s.runEscalationRouteSweep(ctx, escalationRouteSweepInterval)
}

func (s *BeadsServer) runEscalationRouteSweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.sweepUnroutedEscalations(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepUnroutedEscalations(ctx)
		}
	}
}

func (s *BeadsServer) sweepUnroutedEscalations(ctx context.Context) {
	acquired, err := s.store.TryAdvisoryLock(ctx, escalationRouteAdvisoryLockKey, s.sweepUnroutedEscalationsLocked)
	if err != nil {
		slog.Warn("escalation routing: advisory lock error", "error", err)
		return
	}
	if !acquired {
		slog.Info("escalation routing: skipping tick, another replica holds the sweep lock")
	}
}

// sweepUnroutedEscalationsLocked is the sweep body. Errors are logged and
// retried next tick, so it always returns nil.
func (s *BeadsServer) sweepUnroutedEscalationsLocked(ctx context.Context) error {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Limit:  escalationRouteSweepCap,
		Sort:   "created_at",
	})
	if err != nil {
		slog.Warn("escalation routing: failed to list open escalations", "error", err)
		return nil
	}
	if len(beads) == escalationRouteSweepCap {
		slog.Warn("escalation routing: sweep hit the working-set cap; remainder deferred", "cap", escalationRouteSweepCap)
	}
	stamped := 0
	for _, b := range beads {
		if held, _, _ := exactFieldString(b.Fields, "held_by"); strings.TrimSpace(held) != "" {
			continue
		}
		fields, err := s.escalationRouteFieldsFor(ctx, b)
		if err != nil {
			slog.Warn("escalation routing: policy unreadable; sweep stops this tick", "escalation", b.ID, "error", err)
			return nil
		}
		// D11 rule 4: an audience #756 stamped (or an operator re-routed) is
		// kept; the lane is chosen within it. Forced escalations are already
		// owner-held by escalationRouteFieldsFor.
		if had, _, _ := exactFieldString(b.Fields, escalationAudienceField); validEscalationAudience(had) && had != fields[escalationAudienceField] && s.escalationForcedReason(ctx, b) == "" {
			p, _ := s.activeEscalationPolicy(ctx) // read just now by escalationRouteFieldsFor
			if lane, err := s.laneForAudience(ctx, b, had, p); err == nil {
				fields["held_by"], fields[escalationAudienceField] = lane, had
			} else {
				slog.Warn("escalation routing: sweep cannot keep the stamped audience; using the policy's lane", "escalation", b.ID, "audience", had, "held_by", fields["held_by"], "error", err)
			}
		}
		if err := s.mergeBeadFields(ctx, b.ID, fields); err != nil {
			slog.Warn("escalation routing: failed to stamp route", "escalation", b.ID, "error", err)
			continue
		}
		stamped++
	}
	slog.Info("escalation routing: sweep complete", "considered", len(beads), "stamped", stamped)
	return nil
}
```

  Create `server/escalation_lane_members.go`:

```go
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// SetEscalationCoordinatorClients sets BEADS_ESCALATION_COORDINATOR_CLIENTS:
// the Keycloak client ids that may act in a coordinator lane through a
// machine:<id> principal (D4, defence in depth). Empty means none.
func (s *BeadsServer) SetEscalationCoordinatorClients(clients []string) {
	m := make(map[string]struct{}, len(clients))
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = struct{}{}
		}
	}
	s.coordinatorClients = m
}

func (s *BeadsServer) coordinatorClientAllowed(client string) bool {
	_, ok := s.coordinatorClients[client]
	return ok && client != ""
}

// tenantLandBead returns the project bead for land when it is a tenant land
// (land_kind == "tenant", CONTRACT ISSUE 1), else nil. The lookup reads
// through the store, never the caller's scope.
func (s *BeadsServer) tenantLandBead(ctx context.Context, land string) (*model.Bead, error) {
	if land == "" {
		return nil, nil
	}
	p, err := s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("project lookup %q: %w", land, err)
	}
	kind, _, _ := exactFieldString(p.Fields, "land_kind")
	if kind != landKindTenant {
		return nil, nil
	}
	return p, nil
}

func (s *BeadsServer) isTenantLand(ctx context.Context, land string) (bool, error) {
	p, err := s.tenantLandBead(ctx, land)
	return p != nil, err
}

// landDelegatesToSteward reports whether land's owner delegated opType to the
// land's steward (D8: project field land_steward_approves, default []).
// Task 11 declares the field and the steward_delegate action, and calls this
// helper rather than re-implementing it.
func (s *BeadsServer) landDelegatesToSteward(ctx context.Context, land, opType string) (bool, error) {
	p, err := s.tenantLandBead(ctx, land)
	if err != nil || p == nil {
		return false, err
	}
	var f struct {
		Approves []string `json:"land_steward_approves"`
	}
	if len(p.Fields) > 0 {
		if err := json.Unmarshal(p.Fields, &f); err != nil {
			return false, fmt.Errorf("land %q land_steward_approves: %w", land, err)
		}
	}
	return policyListHas(f.Approves, normalizeOperationType(opType)), nil
}

// laneStaffed reports whether a lane could act on esc right now (D5: the
// create-time held_by is the first staffed approver lane). It asks about the
// lane, not about a caller.
func (s *BeadsServer) laneStaffed(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error) {
	switch lane.Kind {
	case laneOperator:
		return true, nil
	case laneCoordinator:
		c, ok := p.Coordinators[lane.Name]
		if !ok {
			return false, nil
		}
		for _, pr := range c.Principals {
			kind, name, _ := strings.Cut(pr, ":")
			if kind == "role" || (kind == "machine" && s.coordinatorClientAllowed(name)) {
				return true, nil
			}
		}
		return false, nil
	case laneSteward:
		return s.landDelegatesToSteward(ctx, lane.Name, parseSpawnEscalationFields(esc).OperationType)
	case laneLandOwner:
		return s.isTenantLand(ctx, lane.Name)
	case laneRequester:
		v, _, _ := exactFieldString(esc.Fields, escalationRequestingAgentVerifiedField)
		return v == "true", nil
	}
	// server never approves; legacy entries are not routable lanes.
	return false, nil
}

// firstStaffedLane returns the first staffed lane among approvers, else
// "operator". Unknown refs and lookup errors are skipped with a WARN, so they
// can only push routing toward the operator.
func (s *BeadsServer) firstStaffedLane(ctx context.Context, esc *model.Bead, approvers []string, p escalationPolicy) string {
	for _, a := range approvers {
		lane, err := parseLaneRef(a)
		if err != nil {
			slog.Warn("escalation routing: unknown lane ref in approvers; skipped", "escalation", esc.ID, "ref", a, "error", err)
			continue
		}
		staffed, err := s.laneStaffed(ctx, lane, esc, p)
		if err != nil {
			slog.Warn("escalation routing: could not resolve lane staffing; skipped", "escalation", esc.ID, "lane", lane.String(), "error", err)
			continue
		}
		if staffed {
			return lane.String()
		}
	}
	return string(laneOperator)
}
```

  In `server/escalation_audience_authz.go` (#756), the one floor. Before:
```go
func (s *BeadsServer) escalationAudienceForced(ctx context.Context, bead *model.Bead) bool {
	var f map[string]any
	if bead == nil || len(bead.Fields) == 0 || json.Unmarshal(bead.Fields, &f) != nil {
		return false
	}
	if sens, _ := f["sensitivity"].(string); strings.EqualFold(strings.TrimSpace(sens), "secret") {
		return true
	}
	if credentialRequirementsSet(f["credential_requirements"]) {
		return true
	}
	if approversNamePerson(f["approvers"]) {
		return true
	}
	sf := parseSpawnEscalationFields(bead)
	role, isSpawn, err := escalationSpawnTarget(sf)
	if !isSpawn {
		return false
	}
	if err != nil || isNeverAutoApproveRole(role) {
		return true
	}
	if privileged, err := roleIsPrivileged(ctx, s.store, role); err != nil || privileged {
		return true
	}
	if rule := s.matchingHumanSpawnRule(ctx, role, sf.OperationSpec, bead.CreatedBy); rule != nil {
		for _, a := range rule.Approvers {
			if approversNamePerson([]any{a}) {
				return true
			}
		}
	}
	return false
}
```
  After:
```go
// escalationFloorOperationTypes are D3's operator-only operation types: the
// owner's standing rule that secrets and credentials, and changes to who may
// approve, are the owner's alone. Code, not config.
var escalationFloorOperationTypes = map[string]bool{
	"credential_rotation":       true,
	"secret_write":              true,
	"gardenio_user_capability":  true,
	"escalation_policy_write":   true,
	"spawn_policy_write":        true,
	"role_write":                true,
	"keycloak_group_membership": true,
}

// escalationFloorOperation returns the normalized operation type when it is a
// D3 floor type, else "". Task 11's steward delegation refuses these.
func escalationFloorOperation(opType string) string {
	op := normalizeOperationType(opType)
	if escalationFloorOperationTypes[op] || strings.HasPrefix(op, "credential_") || strings.HasPrefix(op, "secret_") {
		return op
	}
	return ""
}

// escalationFloorReason is the floor part of escalationForcedReason, on decoded
// fields: sensitivity_secret, credential_requirements, or the operation type.
func escalationFloorReason(f map[string]any) string {
	if sens, _ := f["sensitivity"].(string); strings.EqualFold(strings.TrimSpace(sens), "secret") {
		return "sensitivity_secret"
	}
	if credentialRequirementsSet(f["credential_requirements"]) {
		return "credential_requirements"
	}
	op, _ := f["operation_type"].(string)
	return escalationFloorOperation(op)
}

// isEscalationFloorReason reports whether a forced reason makes the escalation
// operator_only with approvers=["operator"] (D3), as opposed to the reasons
// that keep a server-stamped approvers list (a named person, a spawn).
func isEscalationFloorReason(reason string) bool {
	return reason != "" && reason != "approvers_name_person" && !strings.HasPrefix(reason, "spawn_")
}

// escalationForcedReason is the one floor (escalation-routing D3): why bead
// must go to the owner, or "". #756's forced-owner rule plus D3's operation
// types. escalationAudienceForced, stampEscalationAudience, the route endpoint
// and escalationRouteFieldsFor all read it.
func (s *BeadsServer) escalationForcedReason(ctx context.Context, bead *model.Bead) string {
	var f map[string]any
	if bead == nil || len(bead.Fields) == 0 || json.Unmarshal(bead.Fields, &f) != nil {
		return ""
	}
	if r := escalationFloorReason(f); r != "" {
		return r
	}
	if approversNamePerson(f["approvers"]) {
		return "approvers_name_person"
	}
	sf := parseSpawnEscalationFields(bead)
	role, isSpawn, err := escalationSpawnTarget(sf)
	switch {
	case !isSpawn:
		return ""
	case err != nil:
		return "spawn_unresolvable"
	case isNeverAutoApproveRole(role):
		return "spawn_never_auto_approve"
	}
	if privileged, err := roleIsPrivileged(ctx, s.store, role); err != nil || privileged {
		return "spawn_privileged"
	}
	if rule := s.matchingHumanSpawnRule(ctx, role, sf.OperationSpec, bead.CreatedBy); rule != nil {
		for _, a := range rule.Approvers {
			if approversNamePerson([]any{a}) {
				return "spawn_rule_names_person"
			}
		}
	}
	return ""
}

// escalationAudienceForced is #756's predicate, kept for its callers.
func (s *BeadsServer) escalationAudienceForced(ctx context.Context, bead *model.Bead) bool {
	return s.escalationForcedReason(ctx, bead) != ""
}
```
  #756's own tests (`TestEscalationAudienceForcedByApprovers`) pass unchanged: every row they pin keeps its verdict.

  In `gardenio/controller/internal/beadsapi/escalation_audience.go` (#756), the read-side mirror. Before:
```go
func EscalationAudienceForced(fields map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(fields["sensitivity"]), "secret") {
		return true
	}
	return credentialRequirementsSet(fields["credential_requirements"]) || approversNamePerson(fields["approvers"])
}
```
  After:
```go
func EscalationAudienceForced(fields map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(fields["sensitivity"]), "secret") || strings.TrimSpace(fields["operator_only"]) == "true" {
		return true
	}
	if escalationFloorOperation(fields["operation_type"]) {
		return true
	}
	return credentialRequirementsSet(fields["credential_requirements"]) || approversNamePerson(fields["approvers"])
}

// escalationFloorOperation mirrors kbeads' D3 operation-type floor
// (escalation_audience_authz.go escalationFloorOperationTypes). Keep the two
// lists equal; TestEscalationAudience pins this copy.
func escalationFloorOperation(op string) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	switch op {
	case "credential_rotation", "secret_write", "gardenio_user_capability", "escalation_policy_write",
		"spawn_policy_write", "role_write", "keycloak_group_membership":
		return true
	}
	return strings.HasPrefix(op, "credential_") || strings.HasPrefix(op, "secret_")
}
```
  Add rows to `TestEscalationAudience`: `credential_rotation` → `AudienceOwner, true`; `"operator_only": "true"` on a `helm_deploy` → `AudienceOwner, true`; `helm_deploy` alone stays `AudienceOperator, false` (the control).

  In `server/server.go`, beside `approverClients map[string]struct{}`, add:
```go
	// coordinatorClients is BEADS_ESCALATION_COORDINATOR_CLIENTS: machine
	// clients that may act in a coordinator lane (escalation_lane_members.go).
	coordinatorClients map[string]struct{}
```

  In `server/config.go`, right after the `"type:spawn_policy"` entry's closing `}`)},`, add:
```go
	// type:escalation_policy routes escalations (escalation_policy.go): who may
	// approve, who executes, who holds it now. The newest open bead is active;
	// none means the built-in D7. Validated on write (validateEscalationPolicy).
	// Changing it changes who may approve, so it is admin-only.
	"type:escalation_policy": {Key: "type:escalation_policy", Value: json.RawMessage(`{
		"kind": "config",
		"write_policy": {"min_role": "gardenio-admin"},
		"fields": [
			{"name": "rules",        "type": "json", "required": true},
			{"name": "coordinators", "type": "json", "required": true}
		]
	}`)},
```

  In `server/beads.go` `createBead`, the type switch gains a case. Before:
```go
	case "spawn_policy":
		if reason := s.validateSpawnPolicy(ctx, bead.Fields); reason != "" {
			return nil, inputError(reason)
		}
```
  After:
```go
	case "spawn_policy":
		if reason := s.validateSpawnPolicy(ctx, bead.Fields); reason != "" {
			return nil, inputError(reason)
		}
	case escalationPolicyType:
		if reason := validateEscalationPolicy(bead.Fields); reason != "" {
			return nil, inputError(reason)
		}
```

  Later in `createBead`, the route is stamped. Before:
```go
	if denial := spawnLineageSealPatchDenial("create", beadType, bead.Fields, nil); denial != nil {
		return nil, denial
	}
```
  After:
```go
	if denial := spawnLineageSealPatchDenial("create", beadType, bead.Fields, nil); denial != nil {
		return nil, denial
	}

	// Escalation routing (escalation_policy.go): stamp approvers, execute_by,
	// held_by, held_at, operator_only, policy_rule, and audience from held_by
	// (D11). This runs after the agent-token project stamp, so the project is
	// final, and after #756's stampEscalationAudience, which validated the
	// filer's audience. A policy error leaves #756's audience and no held_by
	// (the sweep retries) and never fails the create.
	if beadType == model.TypeEscalation {
		_ = s.stampEscalationRoute(ctx, bead)
	}
```

  In the update path (`server/beads.go`), before:
```go
		if bead.Type == "spawn_policy" {
			if reason := s.validateSpawnPolicy(ctx, bead.Fields); reason != "" {
				return nil, inputError(reason)
			}
		}
```
  After:
```go
		if bead.Type == "spawn_policy" {
			if reason := s.validateSpawnPolicy(ctx, bead.Fields); reason != "" {
				return nil, inputError(reason)
			}
		}
		if bead.Type == escalationPolicyType {
			if reason := validateEscalationPolicy(bead.Fields); reason != "" {
				return nil, inputError(reason)
			}
		}
```

  In `kbeads/internal/config/config.go`, after `EscalationApproverClients []string` add:
```go
	// EscalationCoordinatorClients (BEADS_ESCALATION_COORDINATOR_CLIENTS,
	// comma-separated) are the machine client ids that may act in an
	// escalation coordinator lane. Empty means none.
	EscalationCoordinatorClients []string
```
  and after `EscalationApproverClients: parseCommaSeparated(os.Getenv("BEADS_ESCALATION_APPROVER_CLIENTS")),` add:
```go
		EscalationCoordinatorClients: parseCommaSeparated(os.Getenv("BEADS_ESCALATION_COORDINATOR_CLIENTS")),
```

  In `kbeads/cmd/kd/serve.go`, after `logger.Info("escalation approval allowlist", "clients", approverClients)` add:
```go
		beadsServer.SetEscalationCoordinatorClients(cfg.EscalationCoordinatorClients)
		logger.Info("escalation coordinator clients", "clients", cfg.EscalationCoordinatorClients)
```
  and after the spawn-policy block that ends `logger.Info("spawn policy evaluator enabled")` / `}` add:
```go
		// Escalation routing sweep: stamps held_by & co. on open escalations
		// filed before routing shipped (escalation_policy.go).
		beadsServer.StartEscalationRouteSweep(auditRetentionCtx)
```

- [ ] **4. Run the tests and watch them pass.**
  Run: `cd kbeads && go test ./internal/server -run 'TestDefaultEscalationPolicy_IsD7|TestOperatorOnlyFloor|TestEvaluateEscalationPolicy_FloorBeatsConfig|TestEvaluateEscalationPolicy_D7Routes|TestValidateEscalationPolicy|TestEscalationPolicyWrite_MalformedIs400|TestEscalationCreate_StampsRoute|TestEscalationCreate_KeepsServerStampedSpawnApprovers|TestEscalationCreate_ActivePolicyBeadWins|TestFirstStaffedLane|TestEscalationRouteSweep_StampsOnlyUnstamped|TestEscalation|TestApproveEscalation|TestSpawnPolicy|TestBuiltin' -count=1`
  Expected: PASS.

  The existing `TestApproveEscalation*` must still pass. They seed beads directly into `ms.beads`, so nothing is stamped and today's unrestricted-approvers behaviour holds (Global Constraint 7). A test that creates an escalation through `POST /v1/beads` and then approves it as a non-admin human will now see `approvers=["operator"]`, and it fails until Task 3's `humanSatisfiesApprovers` lands. If CI shows one, fix it in this PR: the test seeds its bead directly. Never weaken the stamp to make it pass.

  Then run: `cd kbeads && go test ./internal/config ./cmd/kd -run 'TestLoad|TestServe' -count=1`
  Expected: PASS.

- [ ] **5. Lint and vet.**
  Run: `gofumpt -l kbeads/internal/server kbeads/internal/config kbeads/cmd/kd` (expect empty), then `cd kbeads && go vet ./internal/server ./internal/config ./cmd/kd && staticcheck ./internal/server && golangci-lint run ./internal/server/...`.

- [ ] **6. Commit and PR (operator-merge).**
  - Branch: `agent/escalation-routing/<task-2-bead-id>`.
  - Commit message: `kbeads: escalation_policy routing (D3 floor, D7 default), create-time held_by/execute_by stamp, route sweep (escalation-routing T2)`.
  - Open the PR as a draft: `gh pr create --draft --title "kbeads: escalation_policy routing + create-time stamping (escalation-routing T2)" --body-file /tmp/t2-body.md`.
  - The PR body lists CONTRACT ISSUES 1, 5, 6, 7, 10, 11, 14, 15, 16 and 17, and R3–R6 (the one floor, D11's stamping rule, `hold_by`, the sweep keeping #756's audience).
  - The merge escalation carries the hold label: `gb escalation create --title "Merge PR #<n> (escalation-routing T2)" --operation-type github_pr_merge --operation-spec '{"pr":<n>,"repo":"mattbakerconsulting/gardenio"}' --label merge-desk:hold --label project:gardenio`.
  - Then file the operator decision:
```
gb decision create --prompt "Merge escalation-routing T2 (escalation_policy + routing stamp)?" \
  --context "PR #<n>. Adds type:escalation_policy (admin-only, validated) and the D7 default; extends #756's forced-owner rule into the one D3 floor (adds the operator-only operation types); stamps approvers/execute_by/held_by/operator_only/policy_rule at create and sets #756's audience from held_by (D11). Sweep stamps legacy open escalations, keeping their #756 audience. Narrows human approval of routed escalations to gardenio-admin (CONTRACT ISSUE 6). No approval path accepts coordinators yet (Task 4)." \
  --option "merge:Merge it:report" --option "send-back:Send back with notes:report" --no-wait
```


---

## Phase 2: Coordinator authority (kbeads, gb, kd)

### Task 3: Lane membership and the `can-approve` dry run

**Step bead:** (minted later). Phase 2, Coordinator authority. Blocked by: Tasks 1 and 2. Blocks: Tasks 4, 5, 7 and 8.

**Who does what:** CREW writes the code and opens the PR. The merge desk reviews and merges it: it touches `escalation_approval_authz.go` and the approve handler, which are human paths. No approval path changes behaviour for non-humans in this task. The handler only swaps its approvers matcher for one that understands lane refs (CONTRACT ISSUE 5).

**Files:**
- Modify `server/escalation_lane_members.go` (from Task 2). Add `callerInLane`, `callerIsPrincipal`, `callerAgentBead`, `agentRole`, `callerIsRequester`, `escalationOperatorOnly`, `approveCheck`, `checkMayApprove`, `checkHumanMayApprove`, `checkCoordinatorMayApprove` and `humanSatisfiesApprovers`.
- Modify `server/http_escalations.go`, `handleApproveEscalation`, at the block `ok, err := s.escalationApproverListed(r.Context(), approvedBy, approvers)`. It becomes `humanSatisfiesApprovers`.
- Modify `server/http_escalations.go`: add `handleCanApproveEscalation` after `handleApproveEscalation`.
- Modify `server/http.go`: re-locate by `mux.HandleFunc("POST /v1/escalations/{id}/approve", requireRole(RoleOperator, s.handleApproveEscalation))` and add the `can-approve` route above it.
- Create `server/escalation_lane_members_test.go`.

**Interfaces:**
- Consumes:
  - from Tasks 1 and 2: `parseLaneRef`, `activeEscalationPolicy`, `escalationForcedReason`, `isEscalationFloorReason`, `landDelegatesToSteward`, `isTenantLand` and `coordinatorClientAllowed`;
  - `s.escalationApprover(ctx) (string, bool)`;
  - `callerMayApproveAsAdmin(ctx) bool`;
  - `s.escalationApproverListed(ctx, approvedBy, approvers) (bool, error)`;
  - `escalationRequiresFourEyes`, `s.escalationFourEyesViolation`, `s.spawnApprovalDenial`, `s.replayWritePolicyDenial`, `s.replayKillSwitchDenial` (#762, `land_switch_authz.go`) and `s.scopedReplayDenial`;
  - `s.scopedApproverAllowed(ctx, esc) (bool, string)`;
  - `s.authorizeEscalationVerb(w, r, id, op, verb) (*model.Bead, bool)` (#762, `read_authz_scope.go`): `fetchResolutionTarget` plus the scoped-approver 403;
  - `agentBeadIDFromContext` (`agent_identity.go`), `agentRoleField` (`spawn_authz.go`) and `parseRoleList` (`role_list.go`);
  - `groupsToLandRoles(GroupsFromContext(ctx))` and `landRoleOwner` (`land_role.go`);
  - `authMethodFromContext(ctx) == authMethodMachine` (`machine_identity.go`) and `isMachineWrite` (`write_authz.go`).
- Produces:
```go
func (s *BeadsServer) callerInLane(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error)
type approveCheck struct{ Allowed bool; Method string; As string; Reason string }
func (s *BeadsServer) checkMayApprove(ctx context.Context, esc *model.Bead) approveCheck
func (s *BeadsServer) humanSatisfiesApprovers(ctx context.Context, esc *model.Bead, approvedBy string, approvers []string) (as string, ok bool, err error)
func (s *BeadsServer) callerIsRequester(ctx context.Context, esc *model.Bead) bool
func (s *BeadsServer) escalationOperatorOnly(ctx context.Context, esc *model.Bead) bool
const approvalMethodCoordinator = "coordinator"
// POST /v1/escalations/{id}/can-approve -> 200 {"allowed","method","as","reason"}
```

**Steps:**

- [ ] **1. Write the failing tests** in `server/escalation_lane_members_test.go`. They use these helpers:
  - `agentFleetCtx(beadID, name)` (`agent_identity_equivalence_test.go`);
  - `machineCtx(client, roles...)` (`machine_grants_test.go`);
  - `testBrowserClient` (`escalation_device_grant_test.go`);
  - `seedTenantLand` (Task 2);
  - `withAuthMethod`, `WithRoles` and `WithGroups` (`interceptors.go`).

  Handlers are called directly with `r.SetPathValue`, as `charts_render_image_test.go` does. That puts the principal on the context without a verifier, and it skips the `requireRole` wrapper, which non-OIDC callers pass anyway.

```go
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func operatorCtx(email string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: email, ClientID: testBrowserClient})
	return WithRoles(withAuthMethod(ctx, authMethodOIDC), []string{RoleAdmin})
}

func humanCtx(email string, roles []string, groups ...string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: email, ClientID: testBrowserClient})
	return WithGroups(WithRoles(withAuthMethod(ctx, authMethodOIDC), roles), groups)
}

// agentCtx is an agent on its own per-agent token, project-bound like the real ones.
func agentCtx(beadID, name, project string) context.Context {
	return WithTokenProject(agentFleetCtx(beadID, name), project)
}

func seedAgent(ms *mockStore, id, role, project string) {
	f, _ := json.Marshal(map[string]any{"role": role, "project": project})
	ms.beads[id] = &model.Bead{ID: id, Kind: model.KindData, Type: model.BeadType("agent"), Title: id, Status: model.StatusOpen, Fields: f, Labels: []string{"project:" + project}}
}

// seedRoutedEscalation seeds an escalation as Task 2 stamps it.
func seedRoutedEscalation(ms *mockStore, id, project string, f map[string]any) *model.Bead {
	if _, ok := f["operation_spec"]; !ok {
		f["operation_spec"] = map[string]any{}
	}
	raw, _ := json.Marshal(f)
	b := &model.Bead{ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: id, Status: model.StatusOpen, Fields: raw, Labels: []string{"project:" + project}, CreatedBy: "crew-a"}
	ms.beads[id] = b
	return b
}

func mergeDeskEscalation(requestingAgent string) map[string]any {
	return map[string]any{
		"operation_type": "github_pr_merge",
		"approvers":      []string{"coordinator:merge-desk", "coordinator:laptop", "operator"},
		"execute_by":     "coordinator:merge-desk", "held_by": "coordinator:merge-desk",
		"operator_only": false, "policy_rule": "escalation#1",
		"requesting_agent": requestingAgent, "requesting_agent_verified": "true",
	}
}

func newLaneTestServer() (*BeadsServer, *mockStore) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	s.SetEscalationApproverClients([]string{testBrowserClient})
	s.SetEscalationCoordinatorClients([]string{"gardenio-laptop-operator"})
	seedAgent(ms, "gd-rev1", "reviewer", "gardenio")
	seedAgent(ms, "gd-crew1", "crew", "gardenio")
	seedAgent(ms, "gd-stew1", "steward", "acme")
	return s, ms
}

// callEscalationVerb calls an escalation handler directly with ctx as the
// authenticated principal.
func callEscalationVerb(t *testing.T, ctx context.Context, h http.HandlerFunc, id, verb string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/escalations/"+id+"/"+verb, bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func TestCallerInLane(t *testing.T) {
	s, ms := newLaneTestServer()
	seedTenantLand(ms, "acme", nil)
	seedTenantLand(ms, "delegated", []string{"github_pr_merge"})
	seedAgent(ms, "gd-stew2", "steward", "delegated")
	p := defaultEscalationPolicy()
	esc := seedRoutedEscalation(ms, "gd-e1", "gardenio", mergeDeskEscalation("gd-crew1"))
	static := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "gd-rev1"})

	cases := []struct {
		name string
		lane string
		ctx  context.Context
		want bool
	}{
		{"operator: admin on browser client", "operator", operatorCtx("owner@example.com"), true},
		{"operator: gardenio-operator is not admin", "operator", humanCtx("op@example.com", []string{RoleOperator}), false},
		{"operator: kd login device client", "operator", WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "owner@example.com", ClientID: DeviceGrantClientID}), authMethodOIDC), []string{RoleAdmin}), false},
		{"operator: static token", "operator", static, false},
		{"coordinator:merge-desk: reviewer agent", "coordinator:merge-desk", agentCtx("gd-rev1", "reviewer-a", "gardenio"), true},
		{"coordinator:merge-desk: crew agent", "coordinator:merge-desk", agentCtx("gd-crew1", "crew-a", "gardenio"), false},
		{"coordinator:merge-desk: static X-Actor naming the reviewer", "coordinator:merge-desk", static, false},
		{"coordinator:laptop: allowlisted machine", "coordinator:laptop", machineCtx("gardenio-laptop-operator"), true},
		{"coordinator:laptop: other machine", "coordinator:laptop", machineCtx("gardenio-controller"), false},
		{"coordinator:ghost: undefined", "coordinator:ghost", machineCtx("gardenio-laptop-operator"), false},
		{"steward: undelegated land", "steward:acme", agentCtx("gd-stew1", "steward-acme", "acme"), false},
		{"steward: delegated land", "steward:delegated", agentCtx("gd-stew2", "steward-d", "delegated"), true},
		{"steward: steward of another land", "steward:delegated", agentCtx("gd-stew1", "steward-acme", "acme"), false},
		{"land-owner: owner group", "land-owner:acme", humanCtx("o@acme.test", []string{RoleProjectMember}, "/project-acme/owners"), true},
		{"land-owner: member group", "land-owner:acme", humanCtx("m@acme.test", []string{RoleProjectMember}, "/project-acme"), false},
		{"land-owner: not a tenant land", "land-owner:gardenio", humanCtx("o@x.test", []string{RoleProjectMember}, "/project-gardenio/owners"), false},
		{"requester: the verified requesting agent", "requester", agentCtx("gd-crew1", "crew-a", "gardenio"), true},
		{"requester: another agent", "requester", agentCtx("gd-rev1", "reviewer-a", "gardenio"), false},
		{"server: daemon replay", "server", withMachineWrite(context.Background()), true},
		{"server: machine principal", "server", machineCtx("gardenio-laptop-operator"), false},
	}
	for _, c := range cases {
		lane, err := parseLaneRef(c.lane)
		if err != nil {
			t.Fatal(err)
		}
		e := esc
		if lane.Kind == laneSteward || lane.Kind == laneLandOwner {
			e = seedRoutedEscalation(ms, "gd-e-"+lane.Name, lane.Name, map[string]any{"operation_type": "github_pr_merge"})
		}
		got, err := s.callerInLane(c.ctx, lane, e, p)
		if err != nil || got != c.want {
			t.Errorf("%s: callerInLane = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestCheckMayApprove_HumanLegacyApproversUnchanged(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-legacy", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"oidc:alice@example.com"}})
	if c := s.checkMayApprove(humanCtx("alice@example.com", []string{RoleOperator}), esc); !c.Allowed {
		t.Fatalf("named legacy approver refused: %+v", c)
	}
	if c := s.checkMayApprove(humanCtx("bob@example.com", []string{RoleOperator}), esc); c.Allowed {
		t.Fatalf("unnamed human allowed: %+v", c)
	}
}

func TestCheckMayApprove_OperatorLaneEntry(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-op", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "operator"})
	c := s.checkMayApprove(operatorCtx("owner@example.com"), esc)
	if !c.Allowed || c.Method != "web_ui" || c.As != "operator" {
		t.Fatalf("admin on the operator lane: %+v, want allowed web_ui as operator", c)
	}
	if c := s.checkMayApprove(humanCtx("op@example.com", []string{RoleOperator}), esc); c.Allowed {
		t.Fatalf("gardenio-operator (not admin) allowed on an operator-lane escalation: %+v", c)
	}
	// No coordinator lane is listed, so the laptop is refused.
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), esc); c.Allowed {
		t.Fatalf("laptop allowed on an operator-only lane list: %+v", c)
	}
}

func TestCheckMayApprove_CoordinatorPath(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-m1", "gardenio", mergeDeskEscalation("gd-crew1"))
	c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), esc)
	if !c.Allowed || c.Method != approvalMethodCoordinator || c.As != "coordinator:merge-desk" {
		t.Fatalf("merge desk on a crew merge: %+v", c)
	}
	if c := s.checkMayApprove(agentCtx("gd-crew1", "crew-a", "gardenio"), esc); c.Allowed {
		t.Fatalf("the requesting crew agent may not approve its own merge: %+v", c)
	}
	unstamped := seedRoutedEscalation(ms, "gd-m2", "gardenio", map[string]any{"operation_type": "github_pr_merge"})
	if c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), unstamped); c.Allowed {
		t.Fatalf("an unstamped escalation must refuse coordinators (fail closed): %+v", c)
	}
}

// TestCoordinatorApprove_NeedsTheHold pins D4 as tightened in the #756
// revision: a coordinator approves only while its lane holds the escalation
// and is listed in approvers. Otherwise the laptop could approve a merge the
// desk audience still has in hand.
func TestCoordinatorApprove_NeedsTheHold(t *testing.T) {
	s, ms := newLaneTestServer()
	esc := seedRoutedEscalation(ms, "gd-hold", "gardenio", mergeDeskEscalation("gd-crew1")) // held by the merge desk
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), esc); c.Allowed {
		t.Fatalf("laptop approved an escalation the merge desk holds: %+v", c)
	}
	// Positive control: once an operator routes it to the operators (D11 rule
	// 3 picks coordinator:laptop), the laptop holds it and may approve.
	f := mergeDeskEscalation("gd-crew1")
	f["held_by"] = "coordinator:laptop"
	handed := seedRoutedEscalation(ms, "gd-hold2", "gardenio", f)
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), handed); !c.Allowed || c.As != "coordinator:laptop" {
		t.Fatalf("laptop holding a merge: %+v, want allowed as coordinator:laptop", c)
	}
	// Holding is not enough: the hold_by lane of rule 5 is not an approver.
	g := map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "policy_rule": "escalation#5"}
	held := seedRoutedEscalation(ms, "gd-hold3", "gardenio", g)
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), held); c.Allowed {
		t.Fatalf("laptop approved a rule-5 escalation it only holds: %+v", c)
	}
	// The operator can approve whoever holds it (humans need no hold).
	if c := s.checkMayApprove(operatorCtx("owner@example.com"), held); !c.Allowed {
		t.Fatalf("operator refused on a laptop-held escalation: %+v", c)
	}
}

// TestCheckMayApprove_UnstampedOperatorOnly: escalationOperatorOnly re-derives
// the one floor (escalationForcedReason) at decision time, so a floor
// escalation with no operator_only stamp still refuses non-operators.
func TestCheckMayApprove_UnstampedOperatorOnly(t *testing.T) {
	s, ms := newLaneTestServer()
	for _, f := range []map[string]any{
		{"operation_type": "credential_rotation", "approvers": []string{"coordinator:laptop", "operator"}, "held_by": "coordinator:laptop"},
		{"operation_type": "github_pr_merge", "credential_requirements": []string{"gh"}, "approvers": []string{"coordinator:laptop", "operator"}, "held_by": "coordinator:laptop"},
	} {
		esc := seedRoutedEscalation(ms, "gd-uoo", "gardenio", f)
		if !s.escalationOperatorOnly(context.Background(), esc) {
			t.Fatalf("escalationOperatorOnly(%v) = false", f)
		}
		if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), esc); c.Allowed {
			t.Fatalf("laptop allowed on unstamped floor %v: %+v", f, c)
		}
		if c := s.checkMayApprove(operatorCtx("owner@example.com"), esc); !c.Allowed || c.As != "operator" {
			t.Fatalf("operator on unstamped floor %v: %+v", f, c)
		}
	}
	// approvers_name_person forces the owner audience but is not operator-only:
	// the named person still approves.
	esc := seedRoutedEscalation(ms, "gd-uoo2", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"oidc:alice@example.com"}})
	if s.escalationOperatorOnly(context.Background(), esc) {
		t.Fatal("a named-person escalation must not be operator-only")
	}
}

func TestCanApproveEndpoint(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-ca1", "gardenio", mergeDeskEscalation("gd-crew1"))
	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		if rec.Code != http.StatusOK {
			t.Fatalf("can-approve: got %d %s", rec.Code, rec.Body.String())
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	got := decode(callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleCanApproveEscalation, "gd-ca1", "can-approve", map[string]any{}))
	if got["allowed"] != true || got["method"] != "coordinator" || got["as"] != "coordinator:merge-desk" {
		t.Fatalf("reviewer: %v", got)
	}
	got = decode(callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleCanApproveEscalation, "gd-ca1", "can-approve", map[string]any{}))
	if got["allowed"] != false || got["reason"] == "" {
		t.Fatalf("crew requester: %v, want allowed=false with a reason", got)
	}
	// Dry run: nothing changed.
	if b := ms.beads["gd-ca1"]; b.Status != model.StatusOpen {
		t.Fatalf("can-approve mutated the escalation: status %s", b.Status)
	}
}

func TestApproveEscalation_HumanOnOperatorLaneStillWorks(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-h1", "gardenio", map[string]any{"operation_type": "manual_owner_action", "approvers": []string{"operator"}, "execute_by": "operator", "held_by": "operator"})
	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-h1", "approve", map[string]any{"approval_method": "web_ui"})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin approve of an operator-lane escalation: got %d %s", rec.Code, rec.Body.String())
	}
	// Refusal sibling: a gardenio-operator is not on the operator lane.
	seedRoutedEscalation(ms, "gd-h2", "gardenio", map[string]any{"operation_type": "manual_owner_action", "approvers": []string{"operator"}, "execute_by": "operator", "held_by": "operator"})
	rec = callEscalationVerb(t, humanCtx("op@example.com", []string{RoleOperator}), s.handleApproveEscalation, "gd-h2", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator-role approve of an operator-lane escalation: got %d, want 403", rec.Code)
	}
}
```

- [ ] **2. Run the tests and watch them fail.**
  Run: `cd kbeads && go test ./internal/server -run 'TestCallerInLane|TestCheckMayApprove_HumanLegacyApproversUnchanged|TestCheckMayApprove_OperatorLaneEntry|TestCheckMayApprove_CoordinatorPath|TestCoordinatorApprove_NeedsTheHold|TestCheckMayApprove_UnstampedOperatorOnly|TestCanApproveEndpoint|TestApproveEscalation_HumanOnOperatorLaneStillWorks' -count=1`
  Expected: FAIL to build with `undefined: (*BeadsServer).callerInLane`, `undefined: (*BeadsServer).checkMayApprove`, `undefined: approvalMethodCoordinator` and `undefined: (*BeadsServer).handleCanApproveEscalation`. With stubs, `TestApproveEscalation_HumanOnOperatorLaneStillWorks` fails 403 with "approval requires being named in…", the CONTRACT ISSUE 5 bug.

- [ ] **3. Implement.** Append to `server/escalation_lane_members.go`. The imports gain `slices`.

```go
const approvalMethodCoordinator = "coordinator"

// callerInLane reports whether the authenticated caller is a member of lane
// for esc (D2). The shared static token and X-Operated-By relays are members
// of no lane. A legacy ref is never a lane: humanSatisfiesApprovers matches it.
func (s *BeadsServer) callerInLane(ctx context.Context, lane laneRef, esc *model.Bead, p escalationPolicy) (bool, error) {
	switch lane.Kind {
	case laneOperator:
		_, human := s.escalationApprover(ctx)
		return human && callerMayApproveAsAdmin(ctx), nil
	case laneCoordinator:
		c, ok := p.Coordinators[lane.Name]
		if !ok {
			return false, nil
		}
		for _, pr := range c.Principals {
			in, err := s.callerIsPrincipal(ctx, pr)
			if err != nil || in {
				return in, err
			}
		}
		return false, nil
	case laneSteward:
		agent, err := s.callerAgentBead(ctx)
		if err != nil || agent == nil {
			return false, err
		}
		if !slices.Contains(parseRoleList(agentRole(agent)), "steward") || projectFromFields(agent.Fields) != lane.Name {
			return false, nil
		}
		return s.landDelegatesToSteward(ctx, lane.Name, parseSpawnEscalationFields(esc).OperationType)
	case laneLandOwner:
		if _, human := s.escalationApprover(ctx); !human {
			return false, nil
		}
		if groupsToLandRoles(GroupsFromContext(ctx))[lane.Name] != landRoleOwner {
			return false, nil
		}
		return s.isTenantLand(ctx, lane.Name)
	case laneRequester:
		beadID, ok := agentBeadIDFromContext(ctx)
		if !ok {
			return false, nil
		}
		req, _, _ := exactFieldString(esc.Fields, "requesting_agent")
		verified, _, _ := exactFieldString(esc.Fields, escalationRequestingAgentVerifiedField)
		return verified == "true" && req != "" && req == beadID, nil
	case laneServer:
		return isMachineWrite(ctx), nil
	}
	return false, nil
}

// callerIsPrincipal matches one coordinator principal: machine:<client> (a
// machine principal whose client is also in BEADS_ESCALATION_COORDINATOR_CLIENTS)
// or role:<role> (an agent on its own per-agent token whose open agent bead
// carries the role).
func (s *BeadsServer) callerIsPrincipal(ctx context.Context, principal string) (bool, error) {
	kind, name, _ := strings.Cut(principal, ":")
	switch kind {
	case "machine":
		if authMethodFromContext(ctx) != authMethodMachine {
			return false, nil
		}
		return AuthIdentityFromContext(ctx).ClientID == name && s.coordinatorClientAllowed(name), nil
	case "role":
		agent, err := s.callerAgentBead(ctx)
		if err != nil || agent == nil {
			return false, err
		}
		return slices.Contains(parseRoleList(agentRole(agent)), name), nil
	}
	return false, nil
}

// callerAgentBead returns the caller's own open agent bead when it is on a
// per-agent token, else nil. The static token's X-Actor is a claim and never
// reaches here (agentBeadIDFromContext).
func (s *BeadsServer) callerAgentBead(ctx context.Context) (*model.Bead, error) {
	beadID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return nil, nil
	}
	b, err := s.store.GetBead(ctx, beadID)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && b == nil):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("agent bead %s: %w", beadID, err)
	}
	if b.Type != model.BeadType("agent") || b.Status == model.StatusClosed {
		return nil, nil
	}
	return b, nil
}

func agentRole(b *model.Bead) string {
	r, _, _ := exactFieldString(b.Fields, agentRoleField)
	return r
}

// callerIsRequester is D4's self-approval check: the caller's agent bead is
// the requesting agent, or its principal subject is the creator or the
// recorded requester (a machine principal filing its own escalation).
func (s *BeadsServer) callerIsRequester(ctx context.Context, esc *model.Bead) bool {
	req, _, _ := exactFieldString(esc.Fields, "requesting_agent")
	if beadID, ok := agentBeadIDFromContext(ctx); ok && req != "" && beadID == req {
		return true
	}
	subj := AuthIdentityFromContext(ctx).Subject
	return subj != "" && (subj == esc.CreatedBy || subj == req)
}

// escalationOperatorOnly re-derives D3 at decision time as well as reading the
// stamp, so a missing or stale operator_only never opens a floor escalation.
// The floor is escalationForcedReason, the same one #756's audience and the
// create stamp use; only its floor reasons are operator-only (a named person
// or a spawn keeps its own approvers).
func (s *BeadsServer) escalationOperatorOnly(ctx context.Context, esc *model.Bead) bool {
	if isEscalationFloorReason(s.escalationForcedReason(ctx, esc)) {
		return true
	}
	for _, v := range escalationFieldValues(esc.Fields, []string{"operator_only"}) {
		if b, ok := v.(bool); ok && b {
			return true
		}
	}
	return false
}

// approveCheck is checkMayApprove's verdict. Method is what the approval would
// be stamped with ("web_ui" for a human, "coordinator"); As is the lane it acts
// in; Reason is the refusal text.
type approveCheck struct {
	Allowed bool
	Method  string
	As      string
	Reason  string
}

// checkMayApprove answers "may this caller approve esc?" with the same checks
// handleApproveEscalation runs. POST .../can-approve returns it, so the viewer
// shows buttons only when the click would succeed.
func (s *BeadsServer) checkMayApprove(ctx context.Context, esc *model.Bead) approveCheck {
	if esc == nil || esc.Type != model.TypeEscalation {
		return approveCheck{Reason: "not an escalation"}
	}
	if esc.Status != model.StatusOpen && esc.Status != model.StatusInProgress {
		return approveCheck{Reason: "escalation is already " + string(esc.Status)}
	}
	if allowed, reason := s.scopedApproverAllowed(ctx, esc); !allowed {
		return approveCheck{Reason: reason}
	}
	if approvedBy, human := s.escalationApprover(ctx); human {
		return s.checkHumanMayApprove(ctx, esc, approvedBy)
	}
	return s.checkCoordinatorMayApprove(ctx, esc)
}

// checkHumanMayApprove is the unchanged human path: four-eyes, approvers,
// operator-only, spawn policy, replay write policy, scoped replay. The
// refusal texts are the handler's 403 messages.
func (s *BeadsServer) checkHumanMayApprove(ctx context.Context, esc *model.Bead, approvedBy string) approveCheck {
	if escalationRequiresFourEyes(esc) && s.escalationFourEyesViolation(ctx, esc, approvedBy) {
		return approveCheck{Reason: "four-eyes required: approver must differ from the requester"}
	}
	approvers, err := escalationApprovers(esc)
	if err != nil {
		return approveCheck{Reason: "escalation approvers field is malformed; refusing to approve"}
	}
	as := ""
	if len(approvers) > 0 {
		matched, ok, err := s.humanSatisfiesApprovers(ctx, esc, approvedBy, approvers)
		if err != nil {
			return approveCheck{Reason: "failed to resolve approver's teams"}
		}
		if !ok {
			return approveCheck{Reason: "approval requires being named in, or a member of a team named in, this escalation's approvers"}
		}
		as = matched
	}
	if s.escalationOperatorOnly(ctx, esc) && as != string(laneOperator) {
		if in, _ := s.callerInLane(ctx, laneRef{Kind: laneOperator, Raw: string(laneOperator)}, esc, escalationPolicy{}); !in {
			return approveCheck{Reason: "this escalation is operator-only (secrets, credentials or approval rights)"}
		}
		as = string(laneOperator)
	}
	for _, gate := range []func(context.Context, *model.Bead) (string, error){
		func(ctx context.Context, b *model.Bead) (string, error) { return s.spawnApprovalDenial(ctx, b, approvedBy) },
		s.replayWritePolicyDenial,
		s.replayKillSwitchDenial, // #762
		s.scopedReplayDenial,
	} {
		denial, err := gate(ctx, esc)
		if err != nil {
			return approveCheck{Reason: "could not resolve who may approve this escalation; refusing to approve"}
		}
		if denial != "" {
			return approveCheck{Reason: denial}
		}
	}
	return approveCheck{Allowed: true, Method: "web_ui", As: as}
}

// humanSatisfiesApprovers matches a human approver against an approvers list
// that may mix lane refs and legacy entries (CONTRACT ISSUE 5). operator and
// land-owner:<land> go to callerInLane. Coordinator, steward, requester and
// server are never human lanes. Everything else, unparseable entries
// included, keeps today's escalationApproverListed matching. as is the lane
// matched ("" for a legacy entry).
func (s *BeadsServer) humanSatisfiesApprovers(ctx context.Context, esc *model.Bead, approvedBy string, approvers []string) (as string, ok bool, err error) {
	var legacy []string
	for _, a := range approvers {
		lane, perr := parseLaneRef(a)
		if perr != nil || lane.Kind == laneLegacy {
			legacy = append(legacy, a)
			continue
		}
		if lane.Kind != laneOperator && lane.Kind != laneLandOwner {
			continue
		}
		in, err := s.callerInLane(ctx, lane, esc, escalationPolicy{})
		if err != nil {
			return "", false, err
		}
		if in {
			return lane.String(), true, nil
		}
	}
	if len(legacy) == 0 {
		return "", false, nil
	}
	ok, err = s.escalationApproverListed(ctx, approvedBy, legacy)
	return "", ok, err
}

// checkCoordinatorMayApprove is D4: not operator-only, the caller in the
// coordinator or steward lane that holds the escalation, that lane listed in
// approvers, and not the requester. An unstamped escalation has no held_by,
// so it is refused (fail closed).
func (s *BeadsServer) checkCoordinatorMayApprove(ctx context.Context, esc *model.Bead) approveCheck {
	if s.escalationOperatorOnly(ctx, esc) {
		return approveCheck{Reason: "this escalation is operator-only (secrets, credentials or approval rights); a coordinator may not approve it"}
	}
	approvers, err := escalationApprovers(esc)
	if err != nil {
		return approveCheck{Reason: "escalation approvers field is malformed; refusing to approve"}
	}
	if s.callerIsRequester(ctx, esc) {
		return approveCheck{Reason: "a coordinator may not approve an escalation it requested"}
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		slog.Warn("escalation approve: policy unreadable; refusing coordinator approval", "escalation", esc.ID, "error", err)
		return approveCheck{Reason: "could not read the escalation policy; refusing coordinator approval"}
	}
	heldRef, _, _ := exactFieldString(esc.Fields, "held_by")
	held, herr := parseLaneRef(strings.TrimSpace(heldRef))
	if herr != nil || (held.Kind != laneCoordinator && held.Kind != laneSteward) {
		return approveCheck{Reason: "a coordinator approves only an escalation its lane holds; this one is held by " + strconvQuote(heldRef)}
	}
	for _, a := range approvers {
		lane, err := parseLaneRef(a)
		if err != nil {
			slog.Warn("escalation approve: unknown lane ref in approvers; skipped", "escalation", esc.ID, "ref", a, "error", err)
			continue
		}
		if lane.String() != held.String() {
			continue // D4: only the holding lane, and only if it is an approver
		}
		in, err := s.callerInLane(ctx, lane, esc, p)
		if err != nil {
			slog.Warn("escalation approve: lane membership unresolvable; refusing", "escalation", esc.ID, "lane", lane.String(), "error", err)
			return approveCheck{Reason: "could not resolve lane membership; refusing to approve"}
		}
		if in {
			return approveCheck{Allowed: true, Method: approvalMethodCoordinator, As: lane.String()}
		}
	}
	return approveCheck{Reason: "caller is not in the approver lane that holds this escalation (" + held.String() + "); ask an operator to route it (gb escalation route)"}
}
```

  In the `escalation_lane_members.go` import block, add `"slices"`. The file already imports `context`, `database/sql`, `encoding/json`, `errors`, `fmt`, `log/slog` and `strings`.

  In `handleApproveEscalation` (`server/http_escalations.go`), before:
```go
	if len(approvers) > 0 {
		ok, err := s.escalationApproverListed(r.Context(), approvedBy, approvers)
		if err != nil {
```
  After:
```go
	if len(approvers) > 0 {
		// Lane refs (operator, land-owner:<land>) match by lane membership,
		// legacy entries by escalationApproverListed (escalation_lane_members.go).
		_, ok, err := s.humanSatisfiesApprovers(r.Context(), targetBead, approvedBy, approvers)
		if err != nil {
```
  The rest of that block (the 500 on error, the 403 text) is unchanged.

  Add after `handleApproveEscalation`:
```go
// handleCanApproveEscalation handles POST /v1/escalations/{id}/can-approve: a
// dry run of handleApproveEscalation's checks for the caller (D9). It never
// writes. The scoped-approver gate is inside checkMayApprove, so a scoped
// principal gets allowed=false with the reason, not a 403.
func (s *BeadsServer) handleCanApproveEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bead, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleCanApproveEscalation")
	if !ok {
		return
	}
	c := s.checkMayApprove(r.Context(), bead)
	writeJSON(w, http.StatusOK, map[string]any{"allowed": c.Allowed, "method": c.Method, "as": c.As, "reason": c.Reason})
}
```

  In `server/http.go`, before:
```go
	mux.HandleFunc("POST /v1/escalations/{id}/approve", requireRole(RoleOperator, s.handleApproveEscalation))
```
  After:
```go
	mux.HandleFunc("POST /v1/escalations/{id}/can-approve", requireRole(RoleViewer, s.handleCanApproveEscalation))
	mux.HandleFunc("POST /v1/escalations/{id}/approve", requireRole(RoleOperator, s.handleApproveEscalation))
```

- [ ] **4. Run the tests and watch them pass.**
  Run: `cd kbeads && go test ./internal/server -run 'TestCallerInLane|TestCheckMayApprove_HumanLegacyApproversUnchanged|TestCheckMayApprove_OperatorLaneEntry|TestCheckMayApprove_CoordinatorPath|TestCoordinatorApprove_NeedsTheHold|TestCheckMayApprove_UnstampedOperatorOnly|TestCanApproveEndpoint|TestApproveEscalation_HumanOnOperatorLaneStillWorks|TestApproveEscalation|TestEscalation|TestMachineGates' -count=1`
  Expected: PASS. That includes `TestMachineGates_TableCoversEverySite`: the new code uses no `machineGatePattern` token (CONTRACT ISSUE 22).

  If `fetchResolutionTarget`'s read gate (`authorizeBeadRead`) refuses the bare `agentFleetCtx` in this mock, the `WithTokenProject` wrap in `agentCtx` plus the bead's `project:` label is what a real per-agent token carries. Keep both rather than loosening the gate.

- [ ] **5. Lint and vet.**
  Run: `gofumpt -l kbeads/internal/server` (expect empty), then `cd kbeads && go vet ./internal/server && staticcheck ./internal/server && golangci-lint run ./internal/server/...`.

- [ ] **6. Commit and PR.**
  - Branch: `agent/escalation-routing/<task-3-bead-id>`.
  - Commit message: `kbeads: escalation lane membership, checkMayApprove, POST /v1/escalations/{id}/can-approve (escalation-routing T3)`.
  - Merge lane: desk. Open the PR ready, not draft. The approve handler change is the CONTRACT ISSUE 5 fix, and the PR body says so. The body also says that `escalationOperatorOnly` reads #756's forced-owner rule (D3), and that a coordinator needs the hold (D4, owner choice O2).

### Task 4: Coordinator approve path

**Step bead:** (minted later). Phase 2, Coordinator authority. Blocked by: Task 3. Blocks: Tasks 6, 11 and 13.

**Who does what:** CREW writes the code and opens the PR as a draft. Only the OPERATOR merges it (Global Constraint 2): from this PR on, a non-human principal can approve. The merge desk reviews it but does not merge. OWNER and OPERATOR have nothing to do in this task: `BEADS_ESCALATION_COORDINATOR_CLIENTS` was wired in Task 2 and is set in Task 13.

**Files:**
- Modify `server/http_escalations.go`, `handleApproveEscalation`:
  - the non-human refusal block that starts `approvedBy, ok := s.escalationApprover(r.Context())`;
  - the tail from `extra := map[string]any{` to the end of the function, which moves into `finishEscalationApproval`.
- Modify `server/escalation_lane_members.go`, `checkCoordinatorMayApprove` (Task 3), at the line `if in {` / `return approveCheck{Allowed: true, Method: approvalMethodCoordinator, As: lane.String()}`. Add `coordinatorReplayDenial`.
- Modify `server/audit_log.go`: re-locate by `func (s *BeadsServer) auditEscalationApproval(ctx context.Context, escalationID, approvedBy string) {` and add `auditEscalationCoordinatorApproval`.
- Modify `server/machine_gates_test.go`:
  - the row `{"http_escalations.go", `if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {`, 1, gateStatic, "only shapes the refusal message; machine is never an approver"}`;
  - the machine-identity test that checks `if _, ok := srv.escalationApprover(ctx); ok {`.
- Create `server/escalation_coordinator_approve_test.go`.

**Interfaces:**
- Consumes:
  - from Task 3: `checkMayApprove`, `approveCheck`, `approvalMethodCoordinator` and the test helpers `newLaneTestServer`, `seedRoutedEscalation`, `mergeDeskEscalation`, `agentCtx`, `operatorCtx` and `callEscalationVerb`;
  - `escalationSpawnTarget(f) (string, bool, error)`, `isNeverAutoApproveRole`, `roleIsPrivileged(ctx, st, role) (bool, error)` and `strconvQuote` (`escalation_scope.go`);
  - `s.replayEscalation(ctx, bead, approvedBy, grantUntil)`.
- Produces:
```go
func (s *BeadsServer) coordinatorReplayDenial(ctx context.Context, esc *model.Bead) (string, error)
func (s *BeadsServer) finishEscalationApproval(w http.ResponseWriter, r *http.Request, esc *model.Bead, extra map[string]any, approvedBy, as string, grantUntil *time.Time)
func (s *BeadsServer) auditEscalationCoordinatorApproval(ctx context.Context, escalationID, approvedBy, as string)
var coordinatorRefusedOperations map[string]bool
```
  Stamped on a coordinator approval: `approved_by=<principal subject>`, `approved_at`, `approval_method=coordinator`, `approved_as=<lane ref>`, `held_by=<execute_by>`, `audience=laneAudience(execute_by)` (D11), `held_at` and cleared `claimed_*`. Every approval also gets `held_by=execute_by` and its audience when `execute_by` is set (D5).
  The approve path is reached through `authorizeEscalationVerb` (#762) for both humans and coordinators, so a scoped principal is refused before either path.

**Steps:**

- [ ] **1. Write the failing tests** in `server/escalation_coordinator_approve_test.go`. They use the Task 3 helpers listed above, plus `doStaticEscalationReq` (Task 1). Every refusal has an acceptance sibling on the same fixture.

```go
package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

func escField(t *testing.T, ms *mockStore, id, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ms.beads[id].Fields, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func requireUnapproved(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	if v := escField(t, ms, id, "approved_by"); v != nil {
		t.Fatalf("%s: approved_by = %v after a refused approval", id, v)
	}
	if ms.beads[id].Status != model.StatusOpen {
		t.Fatalf("%s: status %s after a refused approval", id, ms.beads[id].Status)
	}
}

// Review Focus 2.
func TestCoordinatorApprove_RefusesOwnRequest(t *testing.T) {
	s, ms := newLaneTestServer()

	// The merge-desk reviewer requested this merge itself.
	seedRoutedEscalation(ms, "gd-own-rev", "gardenio", mergeDeskEscalation("gd-rev1"))
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-own-rev", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reviewer approving its own request: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-own-rev")

	// The laptop approving an escalation it filed (created_by = its subject).
	ownF := mergeDeskEscalation("")
	ownF["held_by"] = "coordinator:laptop" // holding it, so only the self-approval rule refuses
	own := seedRoutedEscalation(ms, "gd-own-laptop", "gardenio", ownF)
	own.CreatedBy = "machine:gardenio-laptop-operator"
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-own-laptop", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("laptop approving its own escalation: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-own-laptop")
}

// Positive controls for TestCoordinatorApprove_RefusesOwnRequest: same lanes,
// a crew agent's request.
func TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge(t *testing.T) {
	s, ms := newLaneTestServer()

	seedRoutedEscalation(ms, "gd-crew-m1", "gardenio", mergeDeskEscalation("gd-crew1"))
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-crew-m1", "approve", map[string]any{"approved_by": "forged@example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge desk approve: got %d %s", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]any{
		"approved_by":     "reviewer-a",
		"approval_method": "coordinator",
		"approved_as":     "coordinator:merge-desk",
		"held_by":         "coordinator:merge-desk",
	} {
		if got := escField(t, ms, "gd-crew-m1", k); got != want {
			t.Errorf("merge desk approve: %s = %v, want %v", k, got, want)
		}
	}
	if escField(t, ms, "gd-crew-m1", "approved_at") == nil {
		t.Error("approved_at not stamped")
	}
	if ms.beads["gd-crew-m1"].Status != model.StatusInProgress {
		t.Errorf("status = %s, want in_progress", ms.beads["gd-crew-m1"].Status)
	}

	laptopHeld := mergeDeskEscalation("gd-crew1")
	laptopHeld["held_by"] = "coordinator:laptop" // routed to the operators (D4 needs the hold)
	seedRoutedEscalation(ms, "gd-crew-m2", "gardenio", laptopHeld)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-crew-m2", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("laptop approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-crew-m2", "approved_as"); got != "coordinator:laptop" {
		t.Errorf("laptop approved_as = %v", got)
	}
	if got := escField(t, ms, "gd-crew-m2", "approved_by"); got != "machine:gardenio-laptop-operator" {
		t.Errorf("laptop approved_by = %v", got)
	}
	// D11: the approval moved held_by to execute_by (merge-desk), so the
	// audience follows it.
	if got := escField(t, ms, "gd-crew-m2", "audience"); got != "desk" {
		t.Errorf("audience after approval = %v, want desk (laneAudience(execute_by))", got)
	}
}

// Review Focus 3, approve side.
func TestCoordinatorApprove_RefusesOperatorOnly(t *testing.T) {
	s, ms := newLaneTestServer()

	// A floor operation whose approvers were (somehow) widened to the laptop:
	// the floor is re-derived at approve time.
	f := mergeDeskEscalation("gd-crew1")
	f["operation_type"] = "credential_rotation"
	f["operator_only"] = false
	f["approvers"] = []string{"coordinator:laptop"}
	f["held_by"] = "coordinator:laptop"
	seedRoutedEscalation(ms, "gd-floor", "gardenio", f)
	rec := callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-floor", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on credential_rotation: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-floor")

	// A stamped operator_only=true is honoured even for a merge.
	g := mergeDeskEscalation("gd-crew1")
	g["operator_only"] = true
	g["held_by"] = "coordinator:laptop"
	seedRoutedEscalation(ms, "gd-oo", "gardenio", g)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-oo", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on operator_only=true: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-oo")

	// Positive control: the same merge fixture with operator_only=false.
	okF := mergeDeskEscalation("gd-crew1")
	okF["held_by"] = "coordinator:laptop"
	seedRoutedEscalation(ms, "gd-oo-ok", "gardenio", okF)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-oo-ok", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("coordinator on a plain merge: got %d %s, want 200", rec.Code, rec.Body.String())
	}

	// The operator can still approve the floor escalation (human path unchanged).
	rec = callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-oo", "approve", map[string]any{"approval_method": "web_ui"})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator on operator_only: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestCoordinatorApprove_RefusesGrantAndPrivilegedSpawn(t *testing.T) {
	s, ms := newLaneTestServer()
	lanes := func(op string, spec any) map[string]any {
		f := mergeDeskEscalation("gd-crew1")
		f["operation_type"] = op
		f["operation_spec"] = spec
		f["approvers"] = []string{"coordinator:laptop", "operator"}
		f["held_by"] = "coordinator:laptop"
		return f
	}
	seedRoutedEscalation(ms, "gd-grant", "gardenio", lanes("grant", map[string]any{"taint": "x"}))
	rec := callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-grant", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on grant: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	seedRoutedEscalation(ms, "gd-spawn", "gardenio", lanes("create", map[string]any{"type": "agent", "title": "a", "fields": map[string]any{"role": "infra"}}))
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-spawn", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on a privileged spawn: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	// Positive control: the same lanes on a non-replayed operation.
	seedRoutedEscalation(ms, "gd-plain", "gardenio", lanes("helm_deploy", map[string]any{}))
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-plain", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("coordinator on helm_deploy: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestCoordinatorApprove_StaticTokenAndUnlistedMachineRefused(t *testing.T) {
	s, ms := newLaneTestServer()
	h := s.NewHTTPHandler("primary-token", nil)
	seedRoutedEscalation(ms, "gd-st", "gardenio", mergeDeskEscalation("gd-crew1"))
	// The static token with X-Actor naming the reviewer is in no lane.
	rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-st/approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("static token: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-st")

	// The laptop client without BEADS_ESCALATION_COORDINATOR_CLIENTS.
	s.SetEscalationCoordinatorClients(nil)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-st", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unlisted machine: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-st")

	// Positive control: the same fixture, the reviewer on its own token.
	rec = callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-st", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reviewer on its own token: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestHumanApprove_StampsHeldByExecuteBy(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-hx", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "operator", "claimed_by": "owner@example.com"})
	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-hx", "approve", map[string]any{"approval_method": "web_ui"})
	if rec.Code != http.StatusOK {
		t.Fatalf("human approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-hx", "held_by"); got != "coordinator:laptop" {
		t.Fatalf("held_by = %v, want execute_by coordinator:laptop", got)
	}
	if got := escField(t, ms, "gd-hx", "audience"); got != "operator" {
		t.Fatalf("audience = %v, want operator (laneAudience(coordinator:laptop), D11)", got)
	}
	if at, _ := escField(t, ms, "gd-hx", "held_at").(string); at == "" {
		t.Fatal("held_at not restamped when approval moved held_by")
	}
	if got := escField(t, ms, "gd-hx", "approval_method"); got != "web_ui" {
		t.Fatalf("approval_method = %v, want web_ui (human path unchanged)", got)
	}
	if got := escField(t, ms, "gd-hx", "approved_as"); got != nil {
		t.Fatalf("approved_as = %v on a human approval, want unset", got)
	}
	if got := escField(t, ms, "gd-hx", "claimed_by"); got != "" {
		t.Fatalf("claimed_by = %v, want cleared", got)
	}
	// Legacy (no execute_by): held_by is left alone.
	seedRoutedEscalation(ms, "gd-hy", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	rec = callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-hy", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy human approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-hy", "held_by"); got != nil {
		t.Fatalf("legacy held_by = %v, want unset", got)
	}
	if got := escField(t, ms, "gd-hy", "audience"); got != nil {
		t.Fatalf("legacy audience = %v, want untouched", got)
	}
}
```

  `roleIsPrivileged` resolves `infra` as privileged through `isNeverAutoApproveRole` or the role bead. If the mock has no `infra` role bead and `isNeverAutoApproveRole("infra")` is false, seed one the way `escalation_scope_test.go` seeds a privileged role for `scopedApproverAllowed`'s spawn case, and reuse its fixture.

- [ ] **2. Run the tests and watch them fail.**
  Run: `cd kbeads && go test ./internal/server -run 'TestCoordinatorApprove_RefusesOwnRequest|TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge|TestCoordinatorApprove_RefusesOperatorOnly|TestCoordinatorApprove_RefusesGrantAndPrivilegedSpawn|TestCoordinatorApprove_StaticTokenAndUnlistedMachineRefused|TestHumanApprove_StampsHeldByExecuteBy' -count=1`
  Expected: FAIL. The positive controls get 403 `escalation approval requires an OIDC-authenticated identity`, because the handler still refuses every non-human. `TestHumanApprove_StampsHeldByExecuteBy` fails because `held_by` stays `operator`.

- [ ] **3. Implement.** In `handleApproveEscalation`, before:
```go
	approvedBy, ok := s.escalationApprover(r.Context())
	if !ok {
		if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {
			msg := "escalation approval requires a sign-in through an approval-capable client; approve from the web UI or Slack"
			if cid.ClientID == DeviceGrantClientID {
				msg = "escalation approval is not available to a `kd login` session; approve from the web UI or Slack"
			}
			slog.Warn("handleApproveEscalation: refused oidc token from non-allowlisted client",
				"bead", id, "azp", cid.ClientID)
			writeError(w, http.StatusForbidden, msg)
			return
		}
		writeError(w, http.StatusForbidden, "escalation approval requires an OIDC-authenticated identity")
		return
	}
```
  After:
```go
	approvedBy, ok := s.escalationApprover(r.Context())
	if !ok {
		if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {
			msg := "escalation approval requires a sign-in through an approval-capable client; approve from the web UI or Slack"
			if cid.ClientID == DeviceGrantClientID {
				msg = "escalation approval is not available to a `kd login` session; approve from the web UI or Slack"
			}
			slog.Warn("handleApproveEscalation: refused oidc token from non-allowlisted client",
				"bead", id, "azp", cid.ClientID)
			writeError(w, http.StatusForbidden, msg)
			return
		}
		// D4: a non-human approves only in a coordinator or steward lane listed
		// in approvers, never its own request, never operator-only, never a
		// grant or privileged spawn. The static token is in no lane.
		check := s.checkMayApprove(r.Context(), targetBead)
		if check.Allowed && check.Method == approvalMethodCoordinator {
			principal := AuthIdentityFromContext(r.Context()).Subject
			if req.ApprovedBy != "" && req.ApprovedBy != principal {
				slog.Warn("handleApproveEscalation: ignoring body approved_by, using the coordinator principal",
					"bead", id, "body_approved_by", req.ApprovedBy, "principal", principal)
			}
			s.finishEscalationApproval(w, r, targetBead, map[string]any{
				"approved_by":     principal,
				"approved_at":     time.Now().UTC().Format(time.RFC3339),
				"approval_method": approvalMethodCoordinator,
				"approved_as":     check.As,
			}, principal, check.As, req.GrantUntil)
			return
		}
		msg := "escalation approval requires an OIDC-authenticated identity"
		if _, agent := agentBeadIDFromContext(r.Context()); (agent || authMethodFromContext(r.Context()) == authMethodMachine) && check.Reason != "" {
			msg = check.Reason
		}
		writeError(w, http.StatusForbidden, msg)
		return
	}
```

  The tail of `handleApproveEscalation` changes. Before (from the `extra` block to the closing brace):
```go
	extra := map[string]any{
		"approved_by": approvedBy,
		"approved_at": time.Now().UTC().Format(time.RFC3339),
	}
	if req.ApprovalMethod != "" {
		extra["approval_method"] = req.ApprovalMethod
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
	...
	writeJSON(w, http.StatusOK, resp)
}
```
  After:
```go
	extra := map[string]any{
		"approved_by": approvedBy,
		"approved_at": time.Now().UTC().Format(time.RFC3339),
	}
	if req.ApprovalMethod != "" {
		extra["approval_method"] = req.ApprovalMethod
	}
	s.finishEscalationApproval(w, r, targetBead, extra, approvedBy, "", req.GrantUntil)
}

// finishEscalationApproval is the shared tail of a human or coordinator
// approval: stamp the approval (plus held_by=execute_by and its audience per
// D5 and D11, clearing any claim), move open → in_progress, audit, replay and
// respond. The body is the pre-routing tail of handleApproveEscalation, moved
// unchanged apart from the held_by stamp and the audit choice.
func (s *BeadsServer) finishEscalationApproval(w http.ResponseWriter, r *http.Request, esc *model.Bead, extra map[string]any, approvedBy, as string, grantUntil *time.Time) {
	id := esc.ID
	if exec, _, _ := exactFieldString(esc.Fields, "execute_by"); strings.TrimSpace(exec) != "" {
		extra["held_by"] = strings.TrimSpace(exec)
		// D11: every write that sets held_by sets audience. An unreadable
		// policy maps an unknown coordinator to owner (fail toward the owner).
		p, err := s.activeEscalationPolicy(r.Context())
		if err != nil {
			slog.Warn("escalation approve: policy unreadable; audience from the built-in default", "escalation", id, "error", err)
			p = defaultEscalationPolicy()
		}
		extra[escalationAudienceField] = laneAudience(strings.TrimSpace(exec), p)
		extra["held_at"] = time.Now().UTC().Format(time.RFC3339)
		extra["claimed_by"] = ""
		extra["claimed_at"] = ""
	}

	if err := s.mergeBeadFields(r.Context(), id, extra); err != nil {
		slog.Warn("failed to merge escalation approval fields", "bead", id, "error", err)
	}

	// Re-fetch to pick up the merged approval fields before the status
	// transition + replay (mergeBeadFields wrote a fresh fields blob).
	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || bead == nil {
		writeError(w, http.StatusNotFound, "escalation not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to get escalation")
		return
	}

	if bead.Status == model.StatusOpen {
		bead.Status = model.StatusInProgress
		if err := s.store.UpdateBead(r.Context(), bead); err != nil {
			writeServerError(r.Context(), w, "handleApproveEscalation", err, "failed to update escalation status")
			return
		}

		s.recordAndPublish(r.Context(), events.TopicBeadUpdated, bead.ID, approvedBy, events.BeadUpdated{
			Bead:    bead,
			Changes: map[string]any{"status": string(model.StatusInProgress)},
		})
	}

	if as != "" {
		s.auditEscalationCoordinatorApproval(r.Context(), id, approvedBy, as)
	} else {
		s.auditEscalationApproval(r.Context(), id, approvedBy)
	}

	// Attempt auto-replay of the denied operation.
	replayResult := s.replayEscalation(r.Context(), bead, approvedBy, grantUntil)

	// Re-fetch the bead to include any fields updated by replay.
	bead, _ = s.store.GetBead(r.Context(), id)

	resp := map[string]any{
		"escalation": extractEscalationFields(bead),
		"issue":      bead,
	}
	if replayResult != nil {
		resp["replay"] = replayResult
	}

	writeJSON(w, http.StatusOK, resp)
}
```

  In `server/escalation_lane_members.go`, `checkCoordinatorMayApprove` changes. Before:
```go
		if in {
			return approveCheck{Allowed: true, Method: approvalMethodCoordinator, As: lane.String()}
		}
```
  After:
```go
		if in {
			denial, err := s.coordinatorReplayDenial(ctx, esc)
			if err != nil {
				slog.Warn("escalation approve: coordinator replay gate unresolvable; refusing", "escalation", esc.ID, "error", err)
				return approveCheck{Reason: "could not resolve the write policy for this escalation's operation; refusing to approve"}
			}
			if denial != "" {
				return approveCheck{Reason: denial}
			}
			return approveCheck{Allowed: true, Method: approvalMethodCoordinator, As: lane.String()}
		}
```
  and append:
```go
// coordinatorRefusedOperations are never approvable by a coordinator: a taint
// grant widens access fleet-wide and its replay has no write-policy check
// (replayWritePolicyDenial's default arm).
var coordinatorRefusedOperations = map[string]bool{"grant": true}

// coordinatorReplayDenial is the coordinator's version of the human
// spawn-policy and replay gates (CONTRACT ISSUE 12). It refuses grants and
// privileged spawns outright, then checks the replayed write against the
// coordinator's own roles. A non-OIDC principal has no ladder role, so any
// min_role type is refused.
func (s *BeadsServer) coordinatorReplayDenial(ctx context.Context, esc *model.Bead) (string, error) {
	f := parseSpawnEscalationFields(esc)
	if coordinatorRefusedOperations[normalizeOperationType(f.OperationType)] {
		return "a coordinator may not approve a " + strconvQuote(f.OperationType) + " escalation", nil
	}
	role, isSpawn, rerr := escalationSpawnTarget(f)
	if isSpawn {
		if rerr != nil {
			return "a coordinator may not approve a spawn whose role cannot be resolved", nil
		}
		privileged := isNeverAutoApproveRole(role)
		if !privileged {
			var err error
			if privileged, err = roleIsPrivileged(ctx, s.store, role); err != nil {
				return "", err
			}
		}
		if privileged {
			return "a coordinator may not approve a spawn of privileged role " + strconvQuote(role), nil
		}
	}
	if denial, err := s.replayWritePolicyDenial(ctx, esc); err != nil || denial != "" {
		return denial, err
	}
	// #762: a coordinator may not throw a land kill switch by approving.
	if denial, err := s.replayKillSwitchDenial(ctx, esc); err != nil || denial != "" {
		return denial, err
	}
	return s.scopedReplayDenial(ctx, esc)
}
```

  In `server/audit_log.go`, after `auditEscalationApproval` add:
```go
// auditEscalationCoordinatorApproval records a coordinator-lane approval (D4):
// the principal and the lane it approved as.
func (s *BeadsServer) auditEscalationCoordinatorApproval(ctx context.Context, escalationID, approvedBy, as string) {
	s.recordAuditEntry(ctx, auditEntry{
		Actor:        approvedBy,
		Action:       "approve",
		Resource:     escalationID,
		ResourceType: "escalation",
		Outcome:      "allow",
		Reason:       "escalation approved by coordinator lane " + as,
	})
}
```

  In `server/machine_gates_test.go`, row 68 changes. Before:
```go
	{"http_escalations.go", `if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {`, 1, gateStatic, "only shapes the refusal message; machine is never an approver"},
```
  After:
```go
	{"http_escalations.go", `if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {`, 1, gateStatic, "shapes the refusal for a non-allowlisted OIDC token only; a machine skips it and may approve solely through a coordinator lane (checkMayApprove, D4: listed in approvers, client in BEADS_ESCALATION_COORDINATOR_CLIENTS, not its own request, not operator-only)"},
```
  In the machine-identity test, after the `if _, ok := srv.escalationApprover(ctx); ok {` check, add:
```go
		// A machine with no coordinator lane on an unstamped escalation is refused (D4).
		esc := &model.Bead{ID: "gd-gate-esc", Type: model.TypeEscalation, Status: model.StatusOpen, Fields: json.RawMessage(`{"operation_type":"github_pr_merge"}`)}
		if c := srv.checkMayApprove(ctx, esc); c.Allowed {
			t.Errorf("%s: checkMayApprove allowed a lane-less escalation: %+v", name, c)
		}
```
  Add `encoding/json` to that file's imports if it isn't already there.

- [ ] **4. Run the tests and watch them pass.**
  Run: `cd kbeads && go test ./internal/server -run 'TestCoordinatorApprove_RefusesOwnRequest|TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge|TestCoordinatorApprove_RefusesOperatorOnly|TestCoordinatorApprove_RefusesGrantAndPrivilegedSpawn|TestCoordinatorApprove_StaticTokenAndUnlistedMachineRefused|TestHumanApprove_StampsHeldByExecuteBy|TestApproveEscalation|TestEscalation|TestSpawn|TestMachineGates|TestCanApproveEndpoint' -count=1`
  Expected: PASS. All existing `TestApproveEscalation*` pass unchanged, including the static-token and X-Operated-By refusals (`TestApproveEscalation_StaticTokenWithOperatedByHuman`): the static token is in no lane.

- [ ] **5. Lint and vet.**
  Run: `gofumpt -l kbeads/internal/server` (expect empty), then `cd kbeads && go vet ./internal/server && staticcheck ./internal/server && golangci-lint run ./internal/server/...`.

- [ ] **6. Commit and PR (operator-merge).**
  - Branch: `agent/escalation-routing/<task-4-bead-id>`.
  - Commit message: `kbeads: coordinator approval (approval_method=coordinator, approved_as, held_by=execute_by) (escalation-routing T4)`.
  - Open the PR as a draft: `gh pr create --draft …`.
  - The PR body lists CONTRACT ISSUES 12 and 22 and the D4 refusal matrix: own request, operator-only, not in a listed lane, unlisted machine, static token, grant, privileged spawn and min_role replay.
  - Merge escalation: `gb escalation create --title "Merge PR #<n> (escalation-routing T4)" --operation-type github_pr_merge --operation-spec '{"pr":<n>,"repo":"mattbakerconsulting/gardenio"}' --label merge-desk:hold --label project:gardenio`.
  - Then file the operator decision:
```
gb decision create --prompt "Merge escalation-routing T4 (coordinators may approve)?" \
  --context "PR #<n>. A per-agent reviewer token (coordinator:merge-desk) or an allowlisted machine client (coordinator:laptop, BEADS_ESCALATION_COORDINATOR_CLIENTS, empty until Task 13) may approve an escalation its lane holds and that lists its lane, unless operator-only (the one floor, #756's forced-owner rule extended), its own request, a grant, a privileged spawn or a kill switch (#762). Stamps approval_method=coordinator, approved_as, held_by=execute_by and audience from it; audits. Human path unchanged except held_by=execute_by and audience." \
  --option "merge:Merge it:report" --option "send-back:Send back with notes:report" --no-wait
```

### Task 5: `claim`, `hand-up`, route by lane, the `held_by` filter, idle hand-up, and the gb/kd verbs

**Step bead:** (minted later). Phase 2, Coordinator authority. Blocked by: Tasks 2 and 3. Blocks: Tasks 7 and 9.

**Who does what:** CREW writes the code and opens the PR. The merge desk merges it: the kbeads handlers are escalation authz, which is a human path. No OWNER or OPERATOR step; `BEADS_ESCALATION_LANE_IDLE_HANDUP` defaults to `6h` and is overridden in Task 13 only if the owner wants.

**Files:**
- Create `server/http_escalation_hold.go`. It holds:
  - `escalationHeldBy`, `laneActor`, `callerHoldsEscalation`, `handUpTarget`, `errNothingAbove` and `applyHandUp`;
  - the handlers `handleClaimEscalation` and `handleHandUpEscalation`;
  - `filterEscalationsHeldBy`, `heldByMe`, `escalationLaneIdleHandUp` and `maybeIdleHandUp`.
- Create `server/http_escalation_hold_test.go`.
- Modify `server/http_escalations.go`, `handleListEscalations` (re-locate by `func (s *BeadsServer) handleListEscalations(`), at `filter.Limit = clampListBeadsLimit(filter.Limit, 50)` and `beads, _, err := s.store.ListBeads(r.Context(), filter)`.
- Modify `server/http.go`: re-locate by `mux.HandleFunc("POST /v1/escalations/{id}/result", requireRole(RoleOperator, s.handleCaptureEscalationResult))` and add the `claim` and `hand-up` routes after it.
- Modify `server/escalation_audience_authz.go` (#756), `handleRouteEscalation` (re-locate by `func (s *BeadsServer) handleRouteEscalation(`): move `held_by` with `audience` (D11 rule 3).
- `TestEscalationRouteEndpoint` (#756, `server/escalation_audience_authz_test.go`) is left unchanged: its fixture is unstamped, so its exact audit text still holds. `TestRouteEscalation_MovesHeldBy` goes in `server/http_escalation_hold_test.go` with the other lane tests.
- Modify `server/escalation_policy.go` (Task 2), `sweepUnroutedEscalationsLocked`, at the `held_by` skip.
- Modify `gardenio/controller/internal/beadsapi/client_escalations.go`. Add `ListEscalationsHeldBy`, `ClaimEscalation`, `HandUpEscalationRequest` and `HandUpEscalation` (CONTRACT ISSUES 4 and 27). The import block gains `strconv` and `strings`.
- Modify `gardenio/controller/internal/beadsapi/client_query.go`: re-locate by `type ListBeadsQuery struct {` (add `HeldBy []string`) and `func (c *Client) ListBeadsFiltered(`.
- Modify `gardenio/controller/internal/beadsapi/client_escalations_test.go`.
- Create `gardenio/controller/cmd/gb/escalation_hold.go`, holding `escalationClaimCmd` and `escalationHandUpCmd`.
- Modify `gardenio/controller/cmd/gb/escalation.go`, `init()`, at `escalationListCmd.Flags().String("lane", …)` and `escalationCmd.AddCommand(escalationPingbackCmd)`.
- Modify `gardenio/controller/cmd/gb/escalation_list.go`, `escalationListCmd.RunE`, at `scope, err := resolveOwnershipScope(cmd)`. Extract the printing tail, from `if jsonOutput {` through the truncation banner, into `printEscalationList`.
- Modify `gardenio/controller/cmd/gb/escalation_test.go`.
- Modify the kd side:
  - `kbeads/internal/client/escalations.go`, after `func (c *HTTPClient) RejectEscalation(`;
  - `kbeads/internal/client/client.go`, re-locate by `RejectEscalation(ctx context.Context, id string, req RejectEscalationRequest) error`;
  - `kbeads/internal/client/grpc.go`, re-locate by `func (c *GRPCClient) RejectEscalation(`;
  - `kbeads/internal/client/escalations_test.go`;
  - `kbeads/cmd/kd/escalation.go`, adding `escalationClaimCmd` and `escalationHandUpCmd`, plus `init()` at `escalationCmd.AddCommand(escalationResolveCmd)`;
  - `kbeads/cmd/kd/mock_client_test.go`, re-locate by `RejectEscalationCalls []struct {` and `func (m *mockBeadsClient) RejectEscalation(`;
  - `kbeads/cmd/kd/escalation_test.go`, `newEscalationCmdForTest` at `root.AddCommand(list, show, approve, reject, resolve)`.

**Interfaces:**
- Consumes:
  - from Tasks 2 and 3: `activeEscalationPolicy`, `escalationFactsFor`, `parseLaneRef`, `laneAudience`, `laneForAudience`, `callerInLane` and `sweepUnroutedEscalationsLocked`;
  - from Task 1: `escalationHasServerApproval`;
  - from #756: `handleRouteEscalation`, `escalationAudienceField`, `audienceOwner` and the `escalationRouteKey{}` write (`updateBead` under that context passes both field guards after Task 1);
  - `s.fetchResolutionTarget`, `s.mergeBeadFields` and `s.recordAndPublish(ctx, events.TopicBeadUpdated, id, actor, events.BeadUpdated{…})`;
  - `maxListBeadsLimit` and `clampListBeadsLimit` (`http_beads.go`, re-locate by `func clampListBeadsLimit(`).
- Produces:
```go
// kbeads server
const heldByMe = "@me"
const escalationRouteSweepActor = "system:escalation-route"
var errNothingAbove = errors.New("nothing above this lane")
func escalationHeldBy(b *model.Bead) string // "" → "operator"
func laneActor(ctx context.Context) string
func (s *BeadsServer) callerHoldsEscalation(ctx context.Context, esc *model.Bead) (laneRef, bool, error)
func (s *BeadsServer) handUpTarget(ctx context.Context, esc *model.Bead, from laneRef) (string, error)
func (s *BeadsServer) applyHandUp(ctx context.Context, esc *model.Bead, from laneRef, actor, reason string) (*model.Bead, error)
func (s *BeadsServer) filterEscalationsHeldBy(ctx context.Context, beads []*model.Bead, refs []string) ([]*model.Bead, error)
func escalationLaneIdleHandUp() time.Duration // BEADS_ESCALATION_LANE_IDLE_HANDUP, default 6h, 0 disables
func (s *BeadsServer) maybeIdleHandUp(ctx context.Context, b *model.Bead, idle time.Duration) bool
// POST /v1/escalations/{id}/audience {"audience","reason"} (#756) -> also moves held_by (D11 rule 3); 409 when no lane of that audience can hold it, or when approved and not owner
// POST /v1/escalations/{id}/claim             -> 200 bead   (RoleViewer route; lane-gated; status unchanged)
// POST /v1/escalations/{id}/hand-up {"reason"} -> 200 bead  (RoleViewer route; lane-gated)
// GET  /v1/escalations?held_by=<ref>[,<ref>|@me]

// controller beadsapi
func (c *Client) ListEscalationsHeldBy(ctx context.Context, heldBy []string, statuses []string, limit int) ([]*BeadDetail, error)
func (c *Client) ClaimEscalation(ctx context.Context, id string) (*BeadDetail, error)
type HandUpEscalationRequest struct{ Reason string `json:"reason"` }
func (c *Client) HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*BeadDetail, error)
// ListBeadsQuery.HeldBy []string → ListBeadsFiltered delegates to ListEscalationsHeldBy

// kbeads client (kd)
func (c *HTTPClient) ClaimEscalation(ctx context.Context, id string) (*model.Bead, error)
type HandUpEscalationRequest struct{ Reason string `json:"reason"` }
func (c *HTTPClient) HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*model.Bead, error)

// CLIs
// gb escalation claim <id>; gb escalation hand-up <id> --reason <text>
// gb escalation list --held-by <ref>[,<ref>] | --needs-me
// kd escalation claim <id>; kd escalation hand-up <id> --reason <text>
```

**Steps:**

- [ ] **1. Write the failing tests.**

  Server tests go in `server/http_escalation_hold_test.go`. They use these helpers:
  - from Task 3: `newLaneTestServer`, `seedRoutedEscalation`, `mergeDeskEscalation`, `agentCtx`, `operatorCtx`, `humanCtx` and `callEscalationVerb`;
  - from Task 2: `seedTenantLand`;
  - from Task 4: `escField`;
  - `doStaticEscalationReq` (Task 1).

```go
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestClaimEscalation_OnlyHolderAndStatusUnchanged(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-c1", "gardenio", mergeDeskEscalation("gd-crew1"))

	// Refusal: a crew agent is not in coordinator:merge-desk.
	rec := callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleClaimEscalation, "gd-c1", "claim", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-holder claim: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	if escField(t, ms, "gd-c1", "claimed_by") != nil {
		t.Fatal("refused claim stamped claimed_by")
	}

	// Positive control: the reviewer holds it.
	rec = callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleClaimEscalation, "gd-c1", "claim", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("holder claim: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-c1", "claimed_by"); got != "gd-rev1" {
		t.Fatalf("claimed_by = %v, want the reviewer's agent bead id", got)
	}
	at, _ := escField(t, ms, "gd-c1", "claimed_at").(string)
	if _, err := time.Parse(time.RFC3339, at); err != nil || at[len(at)-1] != 'Z' {
		t.Fatalf("claimed_at = %q, want RFC3339 UTC", at)
	}
	// CONTRACT ISSUE 28: gardenhub reads in_progress as approved.
	if ms.beads["gd-c1"].Status != model.StatusOpen {
		t.Fatalf("claim changed status to %s", ms.beads["gd-c1"].Status)
	}
	if got := escField(t, ms, "gd-c1", "held_by"); got != "coordinator:merge-desk" {
		t.Fatalf("claim changed held_by to %v", got)
	}

	// A second holder cannot steal the claim.
	seedAgent(ms, "gd-rev2", "reviewer", "gardenio")
	rec = callEscalationVerb(t, agentCtx("gd-rev2", "reviewer-b", "gardenio"), s.handleClaimEscalation, "gd-c1", "claim", map[string]any{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second claim: got %d, want 409", rec.Code)
	}
}

func TestClaimEscalation_UnstampedIsOperatorHeld(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-c2", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	if rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleClaimEscalation, "gd-c2", "claim", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator claim of an unstamped escalation: got %d, want 403", rec.Code)
	}
	if rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleClaimEscalation, "gd-c2", "claim", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("operator claim of an unstamped escalation: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestHandUpEscalation(t *testing.T) {
	s, ms := newLaneTestServer()
	seedTenantLand(ms, "acme", nil)
	seedRoutedEscalation(ms, "gd-h1", "gardenio", mergeDeskEscalation("gd-crew1"))

	if rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("hand-up without reason: got %d, want 400", rec.Code)
	}
	if rec := callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "gardenio"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{"reason": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-holder hand-up: got %d, want 403", rec.Code)
	}
	if got := escField(t, ms, "gd-h1", "held_by"); got != "coordinator:merge-desk" {
		t.Fatalf("refused hand-up moved held_by to %v", got)
	}

	// Positive control.
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{"reason": "touches deploy-values"})
	if rec.Code != http.StatusOK {
		t.Fatalf("holder hand-up: got %d %s", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]any{
		"held_by": "operator", "audience": "owner", "handed_up_by": "gd-rev1", "hand_up_reason": "touches deploy-values",
		"pingback_source": "hand-up", "pingback_reason": "touches deploy-values", "pingback_count": "1",
	} {
		if got := escField(t, ms, "gd-h1", k); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	for _, k := range []string{"handed_up_at", "held_at", "pingback_requested_at"} {
		v, _ := escField(t, ms, "gd-h1", k).(string)
		if ts, err := time.Parse(time.RFC3339, v); err != nil || ts.Location() != time.UTC {
			t.Errorf("%s = %q, want RFC3339 UTC", k, v)
		}
	}

	// Top of the chain: the operator has nothing above.
	if rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleHandUpEscalation, "gd-h1", "hand-up", map[string]any{"reason": "x"}); rec.Code != http.StatusConflict {
		t.Fatalf("operator hand-up: got %d, want 409", rec.Code)
	}

	// Tenant land: requester-held goes to the land owner, not the operator.
	f := map[string]any{"operation_type": "github_pr_merge", "approvers": []string{"steward:acme", "land-owner:acme"}, "execute_by": "requester", "held_by": "requester", "requesting_agent": "gd-crew1", "requesting_agent_verified": "true"}
	seedRoutedEscalation(ms, "gd-h2", "acme", f)
	rec = callEscalationVerb(t, agentCtx("gd-crew1", "crew-a", "acme"), s.handleHandUpEscalation, "gd-h2", "hand-up", map[string]any{"reason": "CI is red"})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant hand-up: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-h2", "held_by"); got != "land-owner:acme" {
		t.Fatalf("tenant hand-up held_by = %v, want land-owner:acme", got)
	}
	if got := escField(t, ms, "gd-h2", "audience"); got != "owner" {
		t.Fatalf("tenant hand-up audience = %v, want owner (D11: the land's owner)", got)
	}
}

// TestRouteEscalation_MovesHeldBy pins D11 rule 3: #756's route endpoint
// moves held_by to a lane of the target audience, so a route is never
// display-only (Review Focus 4).
func TestRouteEscalation_MovesHeldBy(t *testing.T) {
	s, ms := newLaneTestServer()
	route := func(id, to string) *httptest.ResponseRecorder {
		return callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleRouteEscalation, id, "audience", map[string]any{"audience": to, "reason": "test"})
	}
	desk := mergeDeskEscalation("gd-crew1")
	desk["audience"] = "desk"
	seedRoutedEscalation(ms, "gd-r1", "gardenio", desk)

	// The laptop cannot approve while the desk holds it.
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), ms.beads["gd-r1"]); c.Allowed {
		t.Fatalf("laptop allowed before the route: %+v", c)
	}
	// desk → operator: the first staffed operator-audience approver lane.
	if rec := route("gd-r1", "operator"); rec.Code != http.StatusOK {
		t.Fatalf("route to operator: %d %s", rec.Code, rec.Body.String())
	}
	if h, a := escField(t, ms, "gd-r1", "held_by"), escField(t, ms, "gd-r1", "audience"); h != "coordinator:laptop" || a != "operator" {
		t.Fatalf("after route to operator: held_by=%v audience=%v", h, a)
	}
	if c := ms.comments["gd-r1"]; len(c) != 1 || !strings.Contains(c[0].Text, "desk → operator (coordinator:laptop) by") {
		t.Fatalf("audit comment = %+v, want the lane named", c)
	}
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), ms.beads["gd-r1"]); !c.Allowed {
		t.Fatalf("laptop refused after the route (positive control): %+v", c)
	}
	// operator → owner: held by the operator lane; the laptop is out again.
	if rec := route("gd-r1", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("route to owner: %d %s", rec.Code, rec.Body.String())
	}
	if h, a := escField(t, ms, "gd-r1", "held_by"), escField(t, ms, "gd-r1", "audience"); h != "operator" || a != "owner" {
		t.Fatalf("after route to owner: held_by=%v audience=%v", h, a)
	}
	if c := s.checkMayApprove(machineCtx("gardenio-laptop-operator"), ms.beads["gd-r1"]); c.Allowed {
		t.Fatalf("laptop allowed on an owner-held escalation: %+v", c)
	}

	// No desk approver: the desk coordinator holds it without approving.
	seedRoutedEscalation(ms, "gd-r2", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "coordinator:laptop", "audience": "operator", "policy_rule": "escalation#5"})
	if rec := route("gd-r2", "desk"); rec.Code != http.StatusOK {
		t.Fatalf("route helm_deploy to desk: %d %s", rec.Code, rec.Body.String())
	}
	if h := escField(t, ms, "gd-r2", "held_by"); h != "coordinator:merge-desk" {
		t.Fatalf("held_by = %v, want coordinator:merge-desk (holds without approving)", h)
	}
	if c := s.checkMayApprove(agentCtx("gd-rev1", "reviewer-a", "gardenio"), ms.beads["gd-r2"]); c.Allowed {
		t.Fatalf("the desk approved an escalation it holds but is not an approver of: %+v", c)
	}

	// Approved: only owner.
	appr := mergeDeskEscalation("gd-crew1")
	appr["audience"], appr["approved_by"], appr["approved_at"], appr["approval_method"] = "desk", "owner@example.com", "2026-10-21T00:00:00Z", "web_ui"
	seedRoutedEscalation(ms, "gd-r3", "gardenio", appr)
	if rec := route("gd-r3", "operator"); rec.Code != http.StatusConflict {
		t.Fatalf("route an approved escalation to operator: got %d, want 409", rec.Code)
	}
	if rec := route("gd-r3", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("route an approved escalation to owner (positive control): %d %s", rec.Code, rec.Body.String())
	}

	// Unstamped: audience only, as #756 does; the sweep picks the lane later.
	seedRoutedEscalation(ms, "gd-r4", "gardenio", map[string]any{"operation_type": "helm_deploy", "audience": "operator"})
	if rec := route("gd-r4", "owner"); rec.Code != http.StatusOK {
		t.Fatalf("route unstamped: %d %s", rec.Code, rec.Body.String())
	}
	if h, a := escField(t, ms, "gd-r4", "held_by"), escField(t, ms, "gd-r4", "audience"); h != nil || a != "owner" {
		t.Fatalf("unstamped route: held_by=%v audience=%v, want unset/owner", h, a)
	}
}

func TestListEscalations_HeldByFilter(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-l-desk", "gardenio", mergeDeskEscalation("gd-crew1"))
	seedRoutedEscalation(ms, "gd-l-op", "gardenio", map[string]any{"operation_type": "helm_deploy", "held_by": "operator", "approvers": []string{"operator"}})
	seedRoutedEscalation(ms, "gd-l-legacy", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	list := func(ctx context.Context, q string) map[string]bool {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/escalations?"+q, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		s.handleListEscalations(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, rec.Code, rec.Body.String())
		}
		var resp struct {
			Escalations []struct {
				Issue model.Bead `json:"issue"`
			} `json:"escalations"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		ids := map[string]bool{}
		for _, e := range resp.Escalations {
			ids[e.Issue.ID] = true
		}
		return ids
	}
	op := operatorCtx("owner@example.com")
	if got := list(op, "held_by=operator"); !got["gd-l-op"] || !got["gd-l-legacy"] || got["gd-l-desk"] {
		t.Fatalf("held_by=operator = %v, want gd-l-op and the unstamped gd-l-legacy (Global Constraint 7)", got)
	}
	if got := list(op, "held_by=coordinator:merge-desk,coordinator:laptop"); len(got) != 1 || !got["gd-l-desk"] {
		t.Fatalf("held_by=coordinator:* = %v", got)
	}
	if got := list(agentCtx("gd-rev1", "reviewer-a", "gardenio"), "held_by=@me"); len(got) != 1 || !got["gd-l-desk"] {
		t.Fatalf("reviewer @me = %v, want only the merge-desk escalation", got)
	}
	if got := list(op, "held_by=@me"); !got["gd-l-op"] || !got["gd-l-legacy"] || got["gd-l-desk"] {
		t.Fatalf("operator @me = %v", got)
	}
	// Positive control: no filter lists all three.
	if got := list(op, ""); len(got) != 3 {
		t.Fatalf("unfiltered = %v, want 3", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/escalations?held_by=bogus", nil).WithContext(op)
	rec := httptest.NewRecorder()
	s.handleListEscalations(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ref: got %d, want 400", rec.Code)
	}
}

func TestEscalationRouteSweep_IdleHandUp(t *testing.T) {
	s, ms := newLaneTestServer()
	old := time.Now().UTC().Add(-7 * time.Hour).Format(time.RFC3339)
	idle := mergeDeskEscalation("gd-crew1")
	idle["held_at"] = old
	seedRoutedEscalation(ms, "gd-idle", "gardenio", idle)
	claimed := mergeDeskEscalation("gd-crew1")
	claimed["held_at"] = old
	claimed["claimed_by"] = "gd-rev1"
	seedRoutedEscalation(ms, "gd-claimed", "gardenio", claimed)
	seedRoutedEscalation(ms, "gd-op-old", "gardenio", map[string]any{"operation_type": "helm_deploy", "held_by": "operator", "held_at": old, "approvers": []string{"operator"}})
	fresh := mergeDeskEscalation("gd-crew1")
	fresh["held_at"] = time.Now().UTC().Format(time.RFC3339)
	seedRoutedEscalation(ms, "gd-fresh", "gardenio", fresh)
	noClock := mergeDeskEscalation("gd-crew1")
	seedRoutedEscalation(ms, "gd-noclock", "gardenio", noClock)

	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "6h")
	if err := s.sweepUnroutedEscalationsLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := escField(t, ms, "gd-idle", "held_by"); got != "operator" {
		t.Fatalf("idle coordinator-held: held_by = %v, want operator", got)
	}
	if got := escField(t, ms, "gd-idle", "handed_up_by"); got != "system:escalation-route" {
		t.Fatalf("handed_up_by = %v", got)
	}
	if got := escField(t, ms, "gd-idle", "hand_up_reason"); got != "no coordinator:merge-desk activity for 6h0m0s" {
		t.Fatalf("hand_up_reason = %v", got)
	}
	// Positive controls: claimed, operator-held and fresh escalations are untouched.
	for _, id := range []string{"gd-claimed", "gd-fresh"} {
		if got := escField(t, ms, id, "held_by"); got != "coordinator:merge-desk" {
			t.Errorf("%s: held_by = %v, want untouched", id, got)
		}
	}
	if got := escField(t, ms, "gd-op-old", "handed_up_by"); got != nil {
		t.Errorf("operator-held escalation was handed up: %v", got)
	}
	// No held_at: the clock starts, nothing is handed up.
	if got := escField(t, ms, "gd-noclock", "held_by"); got != "coordinator:merge-desk" {
		t.Errorf("gd-noclock handed up with no clock")
	}
	if at, _ := escField(t, ms, "gd-noclock", "held_at").(string); at == "" {
		t.Error("gd-noclock held_at not started")
	}

	// 0 disables.
	again := mergeDeskEscalation("gd-crew1")
	again["held_at"] = old
	seedRoutedEscalation(ms, "gd-off", "gardenio", again)
	t.Setenv("BEADS_ESCALATION_LANE_IDLE_HANDUP", "0")
	_ = s.sweepUnroutedEscalationsLocked(context.Background())
	if got := escField(t, ms, "gd-off", "held_by"); got != "coordinator:merge-desk" {
		t.Errorf("idle hand-up ran with BEADS_ESCALATION_LANE_IDLE_HANDUP=0")
	}
}
```

  Add to `gardenio/controller/internal/beadsapi/client_escalations_test.go`. It uses `newTestClientWithToken(t, srv, token)`, the helper the existing `TestApproveEscalation_UsesUserBearerOnWire` uses.

```go
func TestListEscalationsHeldBy_Query(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(`{"escalations":[{"escalation":{"held_by":"operator"},"issue":{"id":"gd-e1","type":"escalation","status":"open","fields":{"held_by":"operator"}}}]}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")
	got, err := c.ListEscalationsHeldBy(context.Background(), []string{"operator", "coordinator:laptop"}, []string{"open"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations" || gotQuery.Get("held_by") != "operator,coordinator:laptop" || gotQuery.Get("status") != "open" || gotQuery.Get("limit") != "20" {
		t.Fatalf("request = %s ?%s", gotPath, gotQuery.Encode())
	}
	if len(got) != 1 || got[0].ID != "gd-e1" || got[0].Fields["held_by"] != "operator" {
		t.Fatalf("decoded = %+v", got)
	}
	if _, err := c.ListEscalationsHeldBy(context.Background(), nil, nil, 0); err == nil {
		t.Fatal("empty heldBy must be an error")
	}
}

func TestListBeadsFiltered_HeldByDelegates(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"escalations":[]}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"escalation"}, HeldBy: []string{"@me"}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations" {
		t.Fatalf("HeldBy query went to %s, want /v1/escalations", gotPath)
	}
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"task"}, HeldBy: []string{"operator"}}); err == nil {
		t.Fatal("HeldBy on a non-escalation query must be an error")
	}
}

func TestClaimAndHandUpEscalation_Wire(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		_, _ = w.Write([]byte(`{"id":"gd-e1","type":"escalation","status":"open","fields":{"held_by":"operator","claimed_by":"gd-a"}}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "tok")
	b, err := c.ClaimEscalation(context.Background(), "gd-e1")
	if err != nil || gotPath != "/v1/escalations/gd-e1/claim" || b.Fields["claimed_by"] != "gd-a" {
		t.Fatalf("claim: %v %s %+v", err, gotPath, b)
	}
	if _, err := c.HandUpEscalation(context.Background(), "gd-e1", HandUpEscalationRequest{Reason: "needs owner"}); err != nil || gotPath != "/v1/escalations/gd-e1/hand-up" || !strings.Contains(gotBody, `"reason":"needs owner"`) {
		t.Fatalf("hand-up: %v %s %s", err, gotPath, gotBody)
	}
}
```
  That file's imports gain `io`, `net/url` and `strings`.

  Add to `gardenio/controller/cmd/gb/escalation_test.go`. It follows `TestEscalationCreate_AcceptsServerTarget`: an httptest daemon swapped into `daemon`.

```go
func withEscalationDaemon(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = old })
}

func TestEscalationClaim_PostsClaim(t *testing.T) {
	var gotPath string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{"id": "gd-e1", "type": "escalation", "status": "open", "fields": map[string]string{"held_by": "coordinator:laptop", "claimed_by": "machine:gardenio-laptop-operator"}})
	})
	cmd := escalationClaimCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /v1/escalations/gd-e1/claim" {
		t.Fatalf("request = %s", gotPath)
	}
}

func TestEscalationHandUp_RequiresReason(t *testing.T) {
	called := false
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	cmd := escalationHandUpCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "")
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err == nil || err.Error() != "--reason is required" {
		t.Fatalf("got %v, want --reason is required", err)
	}
	if called {
		t.Fatal("hand-up without a reason reached the daemon")
	}
}

func TestEscalationHandUp_PostsReason(t *testing.T) {
	var gotPath, gotBody string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		json.NewEncoder(w).Encode(map[string]any{"id": "gd-e1", "type": "escalation", "status": "open", "fields": map[string]string{"held_by": "operator"}})
	})
	cmd := escalationHandUpCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("reason", "touches deploy-values")
	defer cmd.Flags().Set("reason", "")
	if err := cmd.RunE(cmd, []string{"gd-e1"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations/gd-e1/hand-up" || !strings.Contains(gotBody, `"reason":"touches deploy-values"`) {
		t.Fatalf("request = %s %s", gotPath, gotBody)
	}
}

func TestEscalationList_HeldByAndNeedsMe(t *testing.T) {
	var gotPath, gotHeldBy string
	withEscalationDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHeldBy = r.URL.Path, r.URL.Query().Get("held_by")
		json.NewEncoder(w).Encode(map[string]any{"escalations": []any{}})
	})
	cmd := escalationListCmd
	cmd.SetContext(context.Background())
	defer func() {
		_ = cmd.Flags().Set("held-by", "")
		_ = cmd.Flags().Set("needs-me", "false")
		_ = cmd.Flags().Set("mine", "false")
	}()

	_ = cmd.Flags().Set("held-by", "coordinator:laptop,operator")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/escalations" || gotHeldBy != "coordinator:laptop,operator" {
		t.Fatalf("--held-by request = %s held_by=%q", gotPath, gotHeldBy)
	}

	_ = cmd.Flags().Set("held-by", "")
	_ = cmd.Flags().Set("needs-me", "true")
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if gotHeldBy != "@me" {
		t.Fatalf("--needs-me held_by=%q, want @me", gotHeldBy)
	}

	_ = cmd.Flags().Set("held-by", "operator")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("--held-by with --needs-me must be refused")
	}
	_ = cmd.Flags().Set("needs-me", "false")
	_ = cmd.Flags().Set("mine", "true")
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("--held-by with --mine must be refused")
	}
}
```
  That file's imports gain `io`.

  Add to `kbeads/internal/client/escalations_test.go`, mirroring `TestHTTPClient_RejectEscalation`:

```go
func TestHTTPClient_ClaimEscalation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{"id":"gd-egqpg0fc45","type":"escalation","status":"open","fields":{"claimed_by":"gd-a"}}`}
	c, srv := newTestClient(h)
	defer srv.Close()
	b, err := c.ClaimEscalation(context.Background(), "gd-egqpg0fc45")
	if err != nil {
		t.Fatalf("ClaimEscalation() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/escalations/gd-egqpg0fc45/claim" {
		t.Errorf("request = %s %s, want POST .../claim", h.method, h.path)
	}
	if b == nil || b.ID != "gd-egqpg0fc45" {
		t.Errorf("bead = %+v", b)
	}
}

func TestHTTPClient_HandUpEscalation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{"id":"gd-egqpg0fc45","type":"escalation","status":"open"}`}
	c, srv := newTestClient(h)
	defer srv.Close()
	if _, err := c.HandUpEscalation(context.Background(), "gd-egqpg0fc45", HandUpEscalationRequest{Reason: "needs owner"}); err != nil {
		t.Fatalf("HandUpEscalation() error = %v", err)
	}
	if h.path != "/v1/escalations/gd-egqpg0fc45/hand-up" || !strings.Contains(h.body, `"reason":"needs owner"`) {
		t.Errorf("request = %s %s", h.path, h.body)
	}
}
```

  Add to `kbeads/cmd/kd/escalation_test.go`, mirroring `TestEscalationReject_WiresRequest` (`mc.EscalationsByID`, `withMockClient`, `captureStdout`, `execEscalation`):

```go
func TestEscalationClaim_WiresRequest(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	_ = captureStdout(t, func() {
		if err := execEscalation(t, "claim", "gd-egqpg0fc45"); err != nil {
			t.Fatalf("claim errored: %v", err)
		}
	})
	if len(mc.ClaimEscalationCalls) != 1 || mc.ClaimEscalationCalls[0] != "gd-egqpg0fc45" {
		t.Fatalf("ClaimEscalation calls = %v", mc.ClaimEscalationCalls)
	}
}

func TestEscalationHandUp_RequiresReason(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	if err := execEscalation(t, "hand-up", "gd-egqpg0fc45"); err == nil {
		t.Fatal("hand-up without --reason must fail")
	}
	if len(mc.HandUpEscalationCalls) != 0 {
		t.Fatal("hand-up without --reason reached the client")
	}
}

func TestEscalationHandUp_WiresRequest(t *testing.T) {
	mc := newMockClient()
	mc.EscalationsByID = map[string]*client.EscalationDetail{
		"gd-egqpg0fc45": ptrDetail(escDetail("gd-egqpg0fc45", "t", 0, model.StatusOpen, "a", time.Now())),
	}
	withMockClient(t, mc)
	_ = captureStdout(t, func() {
		if err := execEscalation(t, "hand-up", "gd-egqpg0fc45", "--reason", "needs owner"); err != nil {
			t.Fatalf("hand-up errored: %v", err)
		}
	})
	if len(mc.HandUpEscalationCalls) != 1 || mc.HandUpEscalationCalls[0].Req.Reason != "needs owner" {
		t.Fatalf("HandUpEscalation calls = %+v", mc.HandUpEscalationCalls)
	}
}
```

- [ ] **2. Run the tests and watch them fail.**
  - `cd kbeads && go test ./internal/server -run 'TestClaimEscalation_OnlyHolderAndStatusUnchanged|TestClaimEscalation_UnstampedIsOperatorHeld|TestHandUpEscalation|TestRouteEscalation_MovesHeldBy|TestListEscalations_HeldByFilter|TestEscalationRouteSweep_IdleHandUp' -count=1`
    Expected: FAIL to build with `undefined: (*BeadsServer).handleClaimEscalation` and `undefined: (*BeadsServer).handleHandUpEscalation`. With stubs, `TestRouteEscalation_MovesHeldBy` fails at "after route to operator": #756's route writes `audience` only.
  - `cd kbeads && go test ./internal/client ./cmd/kd -run 'TestHTTPClient_ClaimEscalation|TestHTTPClient_HandUpEscalation|TestEscalationClaim_WiresRequest|TestEscalationHandUp_RequiresReason|TestEscalationHandUp_WiresRequest' -count=1`
    Expected: FAIL to build with `c.ClaimEscalation undefined` and `mc.ClaimEscalationCalls undefined`.
  - `cd gardenio/controller && go test ./internal/beadsapi ./cmd/gb -run 'TestListEscalationsHeldBy_Query|TestListBeadsFiltered_HeldByDelegates|TestClaimAndHandUpEscalation_Wire|TestEscalationClaim_PostsClaim|TestEscalationHandUp_RequiresReason|TestEscalationHandUp_PostsReason|TestEscalationList_HeldByAndNeedsMe' -count=1`
    Expected: FAIL to build with `c.ListEscalationsHeldBy undefined` and `undefined: escalationClaimCmd`.

- [ ] **3. Implement.** Create `server/http_escalation_hold.go`:

```go
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// heldByMe in a held_by filter means "every lane the caller is a member of",
// resolved per escalation by callerInLane (CONTRACT ISSUE 20).
const heldByMe = "@me"

// escalationRouteSweepActor is who an idle hand-up is recorded as.
const escalationRouteSweepActor = "system:escalation-route"

// errNothingAbove is a hand-up from the top of the chain: the operator, or a
// tenant land's owner (CONTRACT ISSUE 25).
var errNothingAbove = errors.New("nothing above this lane")

// escalationHeldBy reads held_by. An escalation filed before routing shipped
// has none and is held by the operator (Global Constraint 7).
func escalationHeldBy(b *model.Bead) string {
	v, _, _ := exactFieldString(b.Fields, "held_by")
	if v = strings.TrimSpace(v); v == "" {
		return string(laneOperator)
	}
	return v
}

// laneActor is who a claim or hand-up is recorded as: the agent bead id on a
// per-agent token (stable across renames), else the principal subject.
func laneActor(ctx context.Context) string {
	if beadID, ok := agentBeadIDFromContext(ctx); ok {
		return beadID
	}
	return AuthIdentityFromContext(ctx).Subject
}

// callerHoldsEscalation reports whether the caller is a member of esc's
// held_by lane. An unparseable held_by is read as operator (fail closed).
func (s *BeadsServer) callerHoldsEscalation(ctx context.Context, esc *model.Bead) (laneRef, bool, error) {
	lane, err := parseLaneRef(escalationHeldBy(esc))
	if err != nil {
		slog.Warn("escalation routing: unparseable held_by; treating as operator", "escalation", esc.ID, "held_by", escalationHeldBy(esc), "error", err)
		lane = laneRef{Kind: laneOperator, Raw: string(laneOperator)}
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		return lane, false, err
	}
	in, err := s.callerInLane(ctx, lane, esc, p)
	return lane, in, err
}

// handUpTarget is D5's hand-up destination: land-owner:<land> for a tenant
// land's escalation, else operator. Already at the top is errNothingAbove.
func (s *BeadsServer) handUpTarget(ctx context.Context, esc *model.Bead, from laneRef) (string, error) {
	if f := s.escalationFactsFor(ctx, esc); f.Land != "" {
		owner := string(laneLandOwner) + ":" + f.Land
		if from.String() == owner {
			return "", errNothingAbove
		}
		return owner, nil
	}
	if from.Kind == laneOperator {
		return "", errNothingAbove
	}
	return string(laneOperator), nil
}

// applyHandUp moves held_by up one level, with its audience (D11), and stamps
// the hand-up plus a pingback, so the bridge re-notifies the new holder. It clears any claim and
// publishes beads.bead.updated. The manual verb and the idle sweep share it.
func (s *BeadsServer) applyHandUp(ctx context.Context, esc *model.Bead, from laneRef, actor, reason string) (*model.Bead, error) {
	target, err := s.handUpTarget(ctx, esc, from)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	count := 0
	if v, _, _ := exactFieldString(esc.Fields, "pingback_count"); v != "" {
		count, _ = strconv.Atoi(v)
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		p = defaultEscalationPolicy() // the hand-up targets are operator or land-owner, both owner in any policy
	}
	extra := map[string]any{
		"held_by": target, "held_at": now, escalationAudienceField: laneAudience(target, p), // D11
		"handed_up_by": actor, "handed_up_at": now, "hand_up_reason": reason,
		"claimed_by": "", "claimed_at": "",
		"pingback_requested_at": now, "pingback_count": strconv.Itoa(count + 1),
		"pingback_by": actor, "pingback_source": "hand-up", "pingback_reason": reason,
	}
	if err := s.mergeBeadFields(ctx, esc.ID, extra); err != nil {
		return nil, err
	}
	bead, err := s.store.GetBead(ctx, esc.ID)
	if err != nil || bead == nil {
		return nil, fmt.Errorf("re-read escalation %s after hand-up: %w", esc.ID, err)
	}
	s.recordAndPublish(ctx, events.TopicBeadUpdated, bead.ID, actor, events.BeadUpdated{
		Bead:    bead,
		Changes: map[string]any{"held_by": target, "handed_up_by": actor},
	})
	return bead, nil
}

func openEscalation(w http.ResponseWriter, esc *model.Bead) bool {
	if esc.Status != model.StatusOpen && esc.Status != model.StatusInProgress {
		writeError(w, http.StatusConflict, "escalation is already "+string(esc.Status))
		return false
	}
	return true
}

// handleClaimEscalation handles POST /v1/escalations/{id}/claim (D5). Only a
// member of held_by may claim. Status is left alone (gardenhub reads
// in_progress as approved), and so is held_by.
func (s *BeadsServer) handleClaimEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	esc, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleClaimEscalation")
	if !ok || !openEscalation(w, esc) {
		return
	}
	lane, in, err := s.callerHoldsEscalation(r.Context(), esc)
	if err != nil {
		slog.Warn("handleClaimEscalation: lane membership unresolvable; refusing", "escalation", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve lane membership; refusing")
		return
	}
	if !in {
		writeError(w, http.StatusForbidden, "only a member of "+lane.String()+" (held_by) may claim this escalation")
		return
	}
	actor := laneActor(r.Context())
	if prev, _, _ := exactFieldString(esc.Fields, "claimed_by"); prev != "" && prev != actor {
		writeError(w, http.StatusConflict, "already claimed by "+prev)
		return
	}
	if err := s.mergeBeadFields(r.Context(), id, map[string]any{
		"claimed_by": actor,
		"claimed_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		writeServerError(r.Context(), w, "handleClaimEscalation", err, "failed to claim escalation")
		return
	}
	bead, err := s.store.GetBead(r.Context(), id)
	if err != nil || bead == nil {
		writeServerError(r.Context(), w, "handleClaimEscalation", err, "failed to re-read escalation")
		return
	}
	s.recordAndPublish(r.Context(), events.TopicBeadUpdated, id, actor, events.BeadUpdated{
		Bead: bead, Changes: map[string]any{"claimed_by": actor},
	})
	writeJSON(w, http.StatusOK, bead)
}

type handUpEscalationRequest struct {
	Reason string `json:"reason"`
}

const maxHandUpReason = 2000

// handleHandUpEscalation handles POST /v1/escalations/{id}/hand-up (D5). Only
// a member of held_by may hand up.
func (s *BeadsServer) handleHandUpEscalation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	var req handUpEscalationRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > maxHandUpReason {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("reason is required (at most %d bytes)", maxHandUpReason))
		return
	}
	esc, ok := s.fetchResolutionTarget(w, r, id, "escalation", "handleHandUpEscalation")
	if !ok || !openEscalation(w, esc) {
		return
	}
	lane, in, err := s.callerHoldsEscalation(r.Context(), esc)
	if err != nil {
		slog.Warn("handleHandUpEscalation: lane membership unresolvable; refusing", "escalation", id, "error", err)
		writeError(w, http.StatusForbidden, "could not resolve lane membership; refusing")
		return
	}
	if !in {
		writeError(w, http.StatusForbidden, "only a member of "+lane.String()+" (held_by) may hand this escalation up")
		return
	}
	bead, err := s.applyHandUp(r.Context(), esc, lane, laneActor(r.Context()), reason)
	switch {
	case errors.Is(err, errNothingAbove):
		writeError(w, http.StatusConflict, "nothing above "+lane.String())
		return
	case err != nil:
		writeServerError(r.Context(), w, "handleHandUpEscalation", err, "failed to hand up escalation")
		return
	}
	writeJSON(w, http.StatusOK, bead)
}

// filterEscalationsHeldBy keeps the escalations whose held_by (empty =
// operator) is one of refs, or, for "@me", a lane the caller is in. A bad ref
// is a 400.
func (s *BeadsServer) filterEscalationsHeldBy(ctx context.Context, beads []*model.Bead, refs []string) ([]*model.Bead, error) {
	want := map[string]bool{}
	me := false
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == heldByMe {
			me = true
			continue
		}
		lane, err := parseLaneRef(r)
		if err != nil || lane.Kind == laneLegacy {
			return nil, inputError("held_by: " + strconvQuote(r) + " is not a lane ref")
		}
		want[lane.String()] = true
	}
	var p escalationPolicy
	if me {
		var err error
		if p, err = s.activeEscalationPolicy(ctx); err != nil {
			return nil, err
		}
	}
	out := make([]*model.Bead, 0, len(beads))
	for _, b := range beads {
		lane, err := parseLaneRef(escalationHeldBy(b))
		if err != nil {
			lane = laneRef{Kind: laneOperator, Raw: string(laneOperator)}
		}
		if want[lane.String()] {
			out = append(out, b)
			continue
		}
		if !me {
			continue
		}
		in, err := s.callerInLane(ctx, lane, b, p)
		if err != nil {
			slog.Warn("held_by=@me: lane membership unresolvable; escalation omitted", "escalation", b.ID, "error", err)
			continue
		}
		if in {
			out = append(out, b)
		}
	}
	return out, nil
}

// escalationLaneIdleHandUp is BEADS_ESCALATION_LANE_IDLE_HANDUP: how long a
// coordinator or steward lane may hold an unclaimed escalation before the
// sweep hands it up. Default 6h; 0 disables; an unparseable value disables
// it, with a WARN.
func escalationLaneIdleHandUp() time.Duration {
	v := strings.TrimSpace(os.Getenv("BEADS_ESCALATION_LANE_IDLE_HANDUP"))
	if v == "" {
		return 6 * time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		slog.Warn("BEADS_ESCALATION_LANE_IDLE_HANDUP unparseable; idle hand-up disabled", "value", v, "error", err)
		return 0
	}
	return d
}

// maybeIdleHandUp hands up b when a coordinator or steward lane has held it,
// unclaimed, for longer than idle. A missing or unparseable held_at starts the
// clock instead. It reports whether b was handed up.
func (s *BeadsServer) maybeIdleHandUp(ctx context.Context, b *model.Bead, idle time.Duration) bool {
	if idle <= 0 {
		return false
	}
	lane, err := parseLaneRef(escalationHeldBy(b))
	if err != nil || (lane.Kind != laneCoordinator && lane.Kind != laneSteward) {
		return false
	}
	if c, _, _ := exactFieldString(b.Fields, "claimed_by"); strings.TrimSpace(c) != "" {
		return false
	}
	at, _, _ := exactFieldString(b.Fields, "held_at")
	heldAt, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	if err != nil {
		if err := s.mergeBeadFields(ctx, b.ID, map[string]any{"held_at": time.Now().UTC().Format(time.RFC3339)}); err != nil {
			slog.Warn("escalation routing: failed to start the held_at clock", "escalation", b.ID, "error", err)
		}
		return false
	}
	if time.Since(heldAt) < idle {
		return false
	}
	reason := fmt.Sprintf("no %s activity for %s", lane.String(), idle)
	if _, err := s.applyHandUp(ctx, b, lane, escalationRouteSweepActor, reason); err != nil {
		if !errors.Is(err, errNothingAbove) {
			slog.Warn("escalation routing: idle hand-up failed", "escalation", b.ID, "error", err)
		}
		return false
	}
	slog.Info("escalation routing: idle hand-up", "escalation", b.ID, "from", lane.String(), "idle", idle)
	return true
}
```
  `idle` formats as `6h0m0s`. That is `time.Duration.String()`, and the test pins it.

  In `server/escalation_audience_authz.go` (#756), `handleRouteEscalation` moves `held_by` with `audience` (D11 rule 3). Before:
```go
	if to == from {
		writeJSON(w, http.StatusOK, map[string]any{"escalation": extractEscalationFields(bead), "issue": bead})
		return
	}
	if from == "" {
		from = "(unset)"
	}
	fields, _ := json.Marshal(map[string]any{escalationAudienceField: to})
```
  After:
```go
	// D11 rule 3: a stamped escalation moves held_by to a lane of the target
	// audience in the same write. An unstamped one keeps #756's behaviour
	// (audience only) and the sweep picks its lane (D11 rule 4).
	held, _ := f["held_by"].(string)
	held = strings.TrimSpace(held)
	lane := held
	if held != "" {
		if escalationHasServerApproval(bead) && to != audienceOwner {
			writeError(w, http.StatusConflict, "this escalation is approved and held by its executor; it can only be routed to the owner")
			return
		}
		p, err := s.activeEscalationPolicy(ctx)
		if err != nil {
			slog.Warn("escalation route: policy unreadable; refusing", "bead", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not read the escalation policy; try again")
			return
		}
		if laneAudience(held, p) != to {
			if lane, err = s.laneForAudience(ctx, bead, to, p); err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
		}
	}
	if to == from && lane == held {
		writeJSON(w, http.StatusOK, map[string]any{"escalation": extractEscalationFields(bead), "issue": bead})
		return
	}
	if from == "" {
		from = "(unset)"
	}
	patch := map[string]any{escalationAudienceField: to}
	if lane != held {
		patch["held_by"], patch["held_at"] = lane, time.Now().UTC().Format(time.RFC3339)
		patch["claimed_by"], patch["claimed_at"] = "", ""
	}
	fields, _ := json.Marshal(patch)
```
  and the audit note, before:
```go
	note := "Audience re-routed: " + from + " → " + to + " by " + who
```
  After:
```go
	note := "Audience re-routed: " + from + " → " + to
	if lane != held {
		note += " (" + lane + ")"
	}
	note += " by " + who
```
  The `escalationRouteKey{}` context the handler already uses passes #756's frozen-field guard and, after Task 1, the immutable-field guard, so the write of `held_by` and `claimed_*` is accepted. #756's `TestEscalationRouteEndpoint` keeps passing: its fixture has no `held_by`.

  In `handleListEscalations` (`server/http_escalations.go`), before:
```go
	filter.Limit = clampListBeadsLimit(filter.Limit, 50)
```
  After:
```go
	filter.Limit = clampListBeadsLimit(filter.Limit, 50)

	// held_by is filtered in Go after the scope filter: it needs empty →
	// operator and "@me" lane resolution, which BeadFilter.Fields cannot
	// express. The open working set is small, so fetch to the cap and cut to
	// the requested limit afterwards.
	var heldBy []string
	wantLimit := filter.Limit
	if v := strings.TrimSpace(q.Get("held_by")); v != "" {
		heldBy = strings.Split(v, ",")
		filter.Limit = clampListBeadsLimit(maxListBeadsLimit, 50)
	}
```
  and before:
```go
	beads, _, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListEscalations", err, "failed to list escalations")
		return
	}
```
  After:
```go
	beads, _, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListEscalations", err, "failed to list escalations")
		return
	}
	if len(heldBy) > 0 {
		if beads, err = s.filterEscalationsHeldBy(r.Context(), beads, heldBy); err != nil {
			var ie inputError
			if errors.As(err, &ie) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeServerError(r.Context(), w, "handleListEscalations", err, "failed to resolve held_by")
			return
		}
		if len(beads) > wantLimit {
			beads = beads[:wantLimit]
		}
	}
```
  `inputError` is a type, and `inputError("…")` is a conversion; the 400 mapping mirrors `http_beads.go` (`var ie inputError` / `if errors.As(err, &ie) {`).

  In `server/http.go`, after the `/result` route add:
```go
	// Claim and hand-up are gated on held_by lane membership in the handler;
	// RoleViewer so a land-owner (project member) is not refused before it.
	mux.HandleFunc("POST /v1/escalations/{id}/claim", requireRole(RoleViewer, s.handleClaimEscalation))
	mux.HandleFunc("POST /v1/escalations/{id}/hand-up", requireRole(RoleViewer, s.handleHandUpEscalation))
```

  In `sweepUnroutedEscalationsLocked` (`server/escalation_policy.go`), the `held_by` skip changes. Before:
```go
	stamped := 0
	for _, b := range beads {
		if held, _, _ := exactFieldString(b.Fields, "held_by"); strings.TrimSpace(held) != "" {
			continue
		}
```
  After:
```go
	stamped, handedUp := 0, 0
	idle := escalationLaneIdleHandUp()
	for _, b := range beads {
		if held, _, _ := exactFieldString(b.Fields, "held_by"); strings.TrimSpace(held) != "" {
			if s.maybeIdleHandUp(ctx, b, idle) {
				handedUp++
			}
			continue
		}
```
  and the final log line becomes `slog.Info("escalation routing: sweep complete", "considered", len(beads), "stamped", stamped, "idle_handed_up", handedUp)`.

  In `gardenio/controller/internal/beadsapi/client_escalations.go`, append:
```go
// ListEscalationsHeldBy lists escalations whose held_by is one of heldBy (an
// unstamped escalation counts as "operator"; "@me" means the caller's own
// lanes, resolved by the daemon). statuses defaults to open,in_progress on
// the daemon.
func (c *Client) ListEscalationsHeldBy(ctx context.Context, heldBy []string, statuses []string, limit int) ([]*BeadDetail, error) {
	if len(heldBy) == 0 {
		return nil, fmt.Errorf("listing escalations by held_by: no lane refs given")
	}
	q := url.Values{}
	q.Set("held_by", strings.Join(heldBy, ","))
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var resp struct {
		Escalations []escalationDetailJSON `json:"escalations"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/escalations?"+q.Encode(), nil, &resp); err != nil {
		return nil, fmt.Errorf("listing escalations held by %s: %w", strings.Join(heldBy, ","), err)
	}
	out := make([]*BeadDetail, 0, len(resp.Escalations))
	for i := range resp.Escalations {
		if d := resp.Escalations[i].toDetail(); d.Escalation != nil {
			out = append(out, d.Escalation)
		}
	}
	return out, nil
}

// ClaimEscalation claims an escalation the caller's lane holds
// (POST /v1/escalations/{id}/claim). Status is unchanged.
func (c *Client) ClaimEscalation(ctx context.Context, id string) (*BeadDetail, error) {
	return c.postEscalationVerb(ctx, id, "claim", struct{}{})
}

// HandUpEscalationRequest is the body for POST /v1/escalations/{id}/hand-up.
type HandUpEscalationRequest struct {
	Reason string `json:"reason"`
}

// HandUpEscalation hands an escalation up one level (operator, or the land
// owner for a tenant land) with a reason.
func (c *Client) HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*BeadDetail, error) {
	return c.postEscalationVerb(ctx, id, "hand-up", req)
}

// postEscalationVerb forwards the per-request user bearer when present, like
// ApproveEscalation, so a human in the viewer acts as themselves.
func (c *Client) postEscalationVerb(ctx context.Context, id, verb string, body any) (*BeadDetail, error) {
	path := "/v1/escalations/" + url.PathEscape(id) + "/" + verb
	var b beadJSON
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, body, &b)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, body, &b)
	}
	if err != nil {
		return nil, fmt.Errorf("%s escalation %s: %w", verb, id, err)
	}
	return b.toDetail(), nil
}
```
  In `client_query.go`, add this inside `ListBeadsQuery` after `Assignee`:
```go
	// HeldBy filters escalations by held_by lane ref (or "@me"). It is served
	// by GET /v1/escalations, so ListBeadsFiltered delegates to
	// ListEscalationsHeldBy and Types must be exactly ["escalation"].
	HeldBy []string
```
  At the top of `ListBeadsFiltered`, add:
```go
	if len(q.HeldBy) > 0 {
		if len(q.Types) != 1 || q.Types[0] != "escalation" {
			return nil, fmt.Errorf("ListBeadsQuery.HeldBy is only valid with Types=[escalation]")
		}
		beads, err := c.ListEscalationsHeldBy(ctx, q.HeldBy, q.Statuses, q.Limit)
		if err != nil {
			return nil, err
		}
		return &ListBeadsResult{Beads: beads, Total: len(beads)}, nil
	}
```

  Create `gardenio/controller/cmd/gb/escalation_hold.go`:
```go
package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var escalationClaimCmd = &cobra.Command{
	Use:   "claim <id>",
	Short: "Claim an escalation your lane holds (held_by)",
	Long: `Claim an escalation so the other members of its held_by lane know you are on it.
Only a member of the held_by lane may claim. The status does not change.

Examples:
  gb escalation claim gd-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		b, err := daemon.ClaimEscalation(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("claiming escalation: %w", err)
		}
		fmt.Printf("Claimed: %s\n", b.ID)
		fmt.Printf("  Held by: %s\n", b.Fields["held_by"])
		fmt.Printf("  Claimed by: %s\n", b.Fields["claimed_by"])
		return nil
	},
}

var escalationHandUpCmd = &cobra.Command{
	Use:   "hand-up <id>",
	Short: "Hand an escalation up to the operator (or the land owner)",
	Long: `Hand an escalation your lane holds up one level: to the operator, or for a tenant
land to its owner. The reason is stamped on the bead and re-notifies the new holder.

Examples:
  gb escalation hand-up gd-abc123 --reason "touches deploy-values; needs the owner"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reason, _ := cmd.Flags().GetString("reason")
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		b, err := daemon.HandUpEscalation(cmd.Context(), args[0], beadsapi.HandUpEscalationRequest{Reason: reason})
		if err != nil {
			return fmt.Errorf("handing up escalation: %w", err)
		}
		fmt.Printf("Handed up: %s\n", b.ID)
		fmt.Printf("  Now held by: %s\n", b.Fields["held_by"])
		return nil
	},
}
```
  In `cmd/gb/escalation.go` `init()`, after `escalationListCmd.Flags().String("lane", …)` add:
```go
	escalationListCmd.Flags().String("held-by", "", "only escalations held by these lanes (comma-separated lane refs, e.g. operator,coordinator:laptop; an unstamped escalation counts as operator). Mutually exclusive with --mine/--lane/--needs-me")
	escalationListCmd.Flags().Bool("needs-me", false, "only escalations held by a lane YOU are a member of (held_by=@me, resolved by the daemon)")

	escalationHandUpCmd.Flags().String("reason", "", "why this needs the next level up (required)")
```
  and after `escalationCmd.AddCommand(escalationPingbackCmd)` add:
```go
	escalationCmd.AddCommand(escalationClaimCmd)
	escalationCmd.AddCommand(escalationHandUpCmd)
```
  In `cmd/gb/escalation_list.go` `RunE`, before:
```go
		scope, err := resolveOwnershipScope(cmd)
```
  After:
```go
		heldBy, _ := cmd.Flags().GetString("held-by")
		needsMe, _ := cmd.Flags().GetBool("needs-me")
		if heldBy != "" || needsMe {
			if heldBy != "" && needsMe {
				return fmt.Errorf("--held-by and --needs-me are mutually exclusive")
			}
			mine, _ := cmd.Flags().GetBool("mine")
			laneSeat, _ := cmd.Flags().GetString("lane")
			if mine || laneSeat != "" {
				return fmt.Errorf("--held-by/--needs-me cannot be combined with --mine or --lane")
			}
			refs := strings.Split(heldBy, ",")
			if needsMe {
				refs = []string{"@me"}
			}
			beads, err := daemon.ListEscalationsHeldBy(cmd.Context(), refs, statuses, limit)
			if err != nil {
				return fmt.Errorf("listing escalations: %w", err)
			}
			return printEscalationList(&beadsapi.ListBeadsResult{Beads: beads, Total: len(beads)}, limit)
		}

		scope, err := resolveOwnershipScope(cmd)
```
  Then move the body from `if jsonOutput {` through the truncation banner's closing `}` into:
```go
// printEscalationList renders an escalation listing (JSON or the table) and
// the truncation banner. Shared by the ownership and held_by paths.
func printEscalationList(result *beadsapi.ListBeadsResult, limit int) error {
	// …moved body, unchanged…
	return nil
}
```
  The original `RunE` ends with `return printEscalationList(result, limit)`.

  In `kbeads/internal/client/escalations.go`, after `RejectEscalation` add:
```go
// ClaimEscalation claims an escalation the caller's held_by lane holds
// (POST /v1/escalations/{id}/claim). Status is unchanged.
func (c *HTTPClient) ClaimEscalation(ctx context.Context, id string) (*model.Bead, error) {
	var b model.Bead
	path := "/v1/escalations/" + url.PathEscape(id) + "/claim"
	if err := c.doJSON(ctx, http.MethodPost, path, struct{}{}, &b); err != nil {
		return nil, fmt.Errorf("claiming escalation %s: %w", id, err)
	}
	return &b, nil
}

// HandUpEscalationRequest is the body for POST /v1/escalations/{id}/hand-up.
type HandUpEscalationRequest struct {
	Reason string `json:"reason"`
}

// HandUpEscalation hands an escalation up one level with a reason.
func (c *HTTPClient) HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*model.Bead, error) {
	var b model.Bead
	path := "/v1/escalations/" + url.PathEscape(id) + "/hand-up"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &b); err != nil {
		return nil, fmt.Errorf("handing up escalation %s: %w", id, err)
	}
	return &b, nil
}
```
  Add `"gardenio/kbeads/internal/model"` to that file's imports if it isn't there.

  In `kbeads/internal/client/client.go`, after `RejectEscalation(ctx context.Context, id string, req RejectEscalationRequest) error` add:
```go
	ClaimEscalation(ctx context.Context, id string) (*model.Bead, error)
	HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*model.Bead, error)
```
  In `kbeads/internal/client/grpc.go`, after `func (c *GRPCClient) RejectEscalation(` add:
```go
func (c *GRPCClient) ClaimEscalation(_ context.Context, _ string) (*model.Bead, error) {
	return nil, fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) HandUpEscalation(_ context.Context, _ string, _ HandUpEscalationRequest) (*model.Bead, error) {
	return nil, fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}
```
  In `kbeads/cmd/kd/mock_client_test.go`, after the `RejectEscalationCalls` field add:
```go
	ClaimEscalationCalls  []string
	HandUpEscalationCalls []struct {
		ID  string
		Req client.HandUpEscalationRequest
	}
```
  and after `func (m *mockBeadsClient) RejectEscalation(` add:
```go
func (m *mockBeadsClient) ClaimEscalation(_ context.Context, id string) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ClaimEscalationCalls = append(m.ClaimEscalationCalls, id)
	return &model.Bead{ID: id, Type: model.TypeEscalation, Status: model.StatusOpen}, nil
}

func (m *mockBeadsClient) HandUpEscalation(_ context.Context, id string, req client.HandUpEscalationRequest) (*model.Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.HandUpEscalationCalls = append(m.HandUpEscalationCalls, struct {
		ID  string
		Req client.HandUpEscalationRequest
	}{id, req})
	return &model.Bead{ID: id, Type: model.TypeEscalation, Status: model.StatusOpen}, nil
}
```
  In `kbeads/cmd/kd/escalation.go`, add:
```go
// ── escalation claim / hand-up ──────────────────────────────────────────

var escalationClaimCmd = &cobra.Command{
	Use:   "claim <id>",
	Short: "Claim an escalation your held_by lane holds (status unchanged)",
	Long: `Claim an escalation (POST /v1/escalations/{id}/claim). Only a member of the
escalation's held_by lane may claim; the daemon refuses anyone else with 403.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		if err := requireOpenEscalation(cmd.Context(), id); err != nil {
			return err
		}
		b, err := beadsClient.ClaimEscalation(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("claiming escalation %s: %w", id, err)
		}
		if jsonOutput {
			emitJSON(b)
		} else {
			fmt.Printf("Claimed %s\n", id)
		}
		return nil
	},
}

var escalationHandUpCmd = &cobra.Command{
	Use:   "hand-up <id>",
	Short: "Hand an escalation up to the operator (or the land owner)",
	Long: `Hand an escalation your held_by lane holds up one level
(POST /v1/escalations/{id}/hand-up): to the operator, or for a tenant land to
its owner. --reason is required and re-notifies the new holder.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		reason, _ := cmd.Flags().GetString("reason")
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		if err := requireOpenEscalation(cmd.Context(), id); err != nil {
			return err
		}
		b, err := beadsClient.HandUpEscalation(cmd.Context(), id, client.HandUpEscalationRequest{Reason: reason})
		if err != nil {
			return fmt.Errorf("handing up escalation %s: %w", id, err)
		}
		if jsonOutput {
			emitJSON(b)
		} else {
			fmt.Printf("Handed up %s: %s\n", id, reason)
		}
		return nil
	},
}
```
  In its `init()`, after `escalationCmd.AddCommand(escalationResolveCmd)` add:
```go
	escalationHandUpCmd.Flags().String("reason", "", "why this needs the next level up (required)")
	escalationCmd.AddCommand(escalationClaimCmd)
	escalationCmd.AddCommand(escalationHandUpCmd)
```
  Add `"strings"` to `escalation.go`'s imports if it isn't there. In `newEscalationCmdForTest` (`kbeads/cmd/kd/escalation_test.go`), before:
```go
	root.AddCommand(list, show, approve, reject, resolve)
```
  After:
```go
	claim := &cobra.Command{Use: "claim <id>", Args: cobra.ExactArgs(1), RunE: escalationClaimCmd.RunE, SilenceUsage: true}
	handUp := &cobra.Command{Use: "hand-up <id>", Args: cobra.ExactArgs(1), RunE: escalationHandUpCmd.RunE, SilenceUsage: true}
	handUp.Flags().String("reason", "", "")

	root.AddCommand(list, show, approve, reject, resolve, claim, handUp)
```

- [ ] **4. Run the tests and watch them pass.** Run the three commands from step 2 again, adding `|TestEscalation|TestListEscalations|TestHTTPClient_` to the kbeads `-run` patterns and `|TestEscalation|TestApproveEscalation|TestListBeadsFiltered` to the controller ones.
  Expected: PASS. `TestEscalationRouteSweep_StampsOnlyUnstamped` (Task 2) still passes: its fixtures have no `held_at` older than 6h.

- [ ] **5. Lint and vet.**
  Run: `gofumpt -l kbeads/internal/server kbeads/internal/client kbeads/cmd/kd gardenio/controller/internal/beadsapi gardenio/controller/cmd/gb` (expect empty), then `cd kbeads && go vet ./internal/server ./internal/client ./cmd/kd && staticcheck ./internal/server ./internal/client ./cmd/kd && golangci-lint run ./internal/server/...`, then `cd gardenio/controller && go vet ./internal/beadsapi ./cmd/gb && staticcheck ./internal/beadsapi ./cmd/gb`.

- [ ] **6. Commit and PR.**
  - Branch: `agent/escalation-routing/<task-5-bead-id>`.
  - Commit message: `escalations: claim, hand-up, held_by filter (@me), idle hand-up; gb/kd verbs; beadsapi ListEscalationsHeldBy (escalation-routing T5)`.
  - Merge lane: desk. Open the PR ready, not draft.
  - The PR body lists CONTRACT ISSUES 3, 4, 20, 24–29, and says that #756's route endpoint now moves `held_by` (D11 rule 3, with the owner-choice fallback to a coordinator that holds without approving).

### Task 6: The `/result` gate (supersedes PR #162)

**Step bead:** (minted later). Phase 2, Coordinator authority. Blocked by: Task 4. Blocks: Tasks 13 and 14.

**Who does what:**
- CREW writes the code and opens the PR as a draft. Only the OPERATOR merges it (Global Constraint 2): it decides who may close an escalation with a result.
- The gate ships OFF: `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL` unset means `false`. Task 14 flips it, after the merge desk and the laptop approve through their lanes. The merge desk never approves today, so with the gate on its closes would be refused (CONTRACT ISSUE 23).
- `BEADS_ESCALATION_RESULT_MACHINES` is wired here but stays empty at rollout. D6(d) has no live caller (CONTRACT ISSUE 21).
- PR #162 (`agent/agent-review/gd-S12o3mOtxg`, open since 2026-09-24) is superseded by Tasks 1 and 6. Its approval-field guard became Task 1, and its result authz became this task. Closing it with a "superseded by #<T1>, #<T6>" comment is Task 14's job, not this PR's.

**Files:**
- Create `server/escalation_result_gate.go`, holding `escalationResultRequiresApproval`, `SetEscalationResultMachines`, `resultMachineAllowed` and `escalationResultDenial`.
- Create `server/escalation_result_gate_test.go`.
- Modify `server/http_escalations.go`, `handleCaptureEscalationResult`, after `resultTarget, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleCaptureEscalationResult")` / `if !ok {` / `return` / `}`.
- Modify `server/server.go`: add `resultMachines map[string]struct{}` beside `coordinatorClients`.
- Modify `kbeads/internal/config/config.go`: add `EscalationResultMachines` beside `EscalationCoordinatorClients` (Task 2).
- Modify `kbeads/cmd/kd/serve.go`: after the Task 2 line `beadsServer.SetEscalationCoordinatorClients(cfg.EscalationCoordinatorClients)`.

**Interfaces:**
- Consumes:
  - from Tasks 1–4: `escalationHasServerApproval`, `parseLaneRef`, `callerInLane` and `activeEscalationPolicy`, plus the test helpers `newLaneTestServer`, `seedRoutedEscalation`, `mergeDeskEscalation`, `agentCtx`, `operatorCtx`, `callEscalationVerb`, `doStaticEscalationReq` and `escField`;
  - `isMachineWrite`, `s.auditDenial(r, *AuthzDenial, id)` (as `authorizeEscalationVerb` uses it) and `authMethodFromContext(ctx) == authMethodMachine`.
- Produces:
```go
func escalationResultRequiresApproval() bool // BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL, default false
func (s *BeadsServer) SetEscalationResultMachines(clients []string)
func (s *BeadsServer) resultMachineAllowed(client string) bool
func (s *BeadsServer) escalationResultDenial(ctx context.Context, esc *model.Bead, resultStatus string) string // "" = accepted
```

**Steps:**

- [ ] **1. Write the failing tests** in `server/escalation_result_gate_test.go`. They use the helpers listed under Interfaces. Each D6 branch has an acceptance test and a refusal test on the same fixture, and the gate-off test is the positive control for the whole feature.

```go
package server

import (
	"context"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func approvedMergeDesk(requester string) map[string]any {
	f := mergeDeskEscalation(requester)
	f["approved_by"] = "reviewer-a"
	f["approved_at"] = "2026-10-20T00:00:00Z"
	f["approval_method"] = "coordinator"
	f["approved_as"] = "coordinator:merge-desk"
	return f
}

func postResult(t *testing.T, s *BeadsServer, ctx context.Context, id, status string) int {
	t.Helper()
	return callEscalationVerb(t, ctx, s.handleCaptureEscalationResult, id, "result", map[string]any{"result_status": status, "executor": "test"}).Code
}

// CONTRACT ISSUE 23: with the flag unset nothing changes, so the merge desk's
// unapproved closes keep working until Task 14.
func TestCaptureEscalationResult_GateOffUnchanged(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "")
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := s.NewHTTPHandler("primary-token", nil)
	seedRoutedEscalation(ms, "gd-off1", "gardenio", mergeDeskEscalation("gd-crew1"))
	rec := doStaticEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-off1/result", map[string]any{"result_status": "success"})
	if rec.Code != http.StatusOK {
		t.Fatalf("gate off, unapproved static result: got %d %s, want 200 (today's behaviour)", rec.Code, rec.Body.String())
	}
	if ms.beads["gd-off1"].Status != model.StatusClosed {
		t.Fatalf("status = %s, want closed", ms.beads["gd-off1"].Status)
	}

	// The same request with the gate on is refused.
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	seedRoutedEscalation(ms, "gd-off2", "gardenio", mergeDeskEscalation("gd-crew1"))
	rec = doStaticEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-off2/result", map[string]any{"result_status": "success"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gate on, unapproved static result: got %d, want 403", rec.Code)
	}
	if ms.beads["gd-off2"].Status != model.StatusOpen {
		t.Fatal("a refused result closed the escalation")
	}
}

// D6(a): approved, and the caller is in execute_by.
func TestCaptureEscalationResult_GateA_ApprovedAndExecutor(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()

	seedRoutedEscalation(ms, "gd-a1", "gardenio", approvedMergeDesk("gd-crew1"))
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-a1", "success"); code != http.StatusForbidden {
		t.Fatalf("non-executor on an approved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-a1", "success"); code != http.StatusOK {
		t.Fatalf("executor on an approved escalation: got %d, want 200", code)
	}

	// Unapproved: even the executor is refused.
	seedRoutedEscalation(ms, "gd-a2", "gardenio", mergeDeskEscalation("gd-crew1"))
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-a2", "success"); code != http.StatusForbidden {
		t.Fatalf("executor on an unapproved escalation: got %d, want 403", code)
	}

	// Legacy (no execute_by) is executed by the operator (CONTRACT ISSUE 19).
	seedRoutedEscalation(ms, "gd-a3", "gardenio", map[string]any{"operation_type": "helm_deploy", "approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z"})
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-a3", "success"); code != http.StatusForbidden {
		t.Fatalf("agent on a legacy approved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-a3", "success"); code != http.StatusOK {
		t.Fatalf("operator on a legacy approved escalation: got %d, want 200", code)
	}
}

// D6(b): execute_by=server only takes the daemon's own result.
func TestCaptureEscalationResult_GateB_ServerExecuted(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	f := map[string]any{"operation_type": "close", "approvers": []string{"operator"}, "execute_by": "server", "held_by": "server", "approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z"}
	seedRoutedEscalation(ms, "gd-b1", "gardenio", f)
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-b1", "success"); code != http.StatusForbidden {
		t.Fatalf("operator result on a server-executed escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, withMachineWrite(operatorCtx("owner@example.com")), "gd-b1", "success"); code != http.StatusOK {
		t.Fatalf("daemon result on a server-executed escalation: got %d, want 200", code)
	}
}

// D6(c): the requester may withdraw with failure, and only with failure.
func TestCaptureEscalationResult_GateC_RequesterWithdrawal(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-c1", "gardenio", mergeDeskEscalation("gd-crew1"))
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-c1", "success"); code != http.StatusForbidden {
		t.Fatalf("requester success on an unapproved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-c1", "failure"); code != http.StatusForbidden {
		t.Fatalf("non-requester failure on an unapproved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-c1", "failure"); code != http.StatusOK {
		t.Fatalf("requester withdrawal: got %d, want 200", code)
	}
}

// D6(d): an allowlisted result machine (unused at rollout, CONTRACT ISSUE 21).
func TestCaptureEscalationResult_GateD_ResultMachine(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-d1", "gardenio", mergeDeskEscalation("gd-crew1"))
	if code := postResult(t, s, machineCtx("gardenio-gitlab-bridge"), "gd-d1", "success"); code != http.StatusForbidden {
		t.Fatalf("unlisted machine: got %d, want 403", code)
	}
	s.SetEscalationResultMachines([]string{"gardenio-gitlab-bridge"})
	if code := postResult(t, s, machineCtx("gardenio-gitlab-bridge"), "gd-d1", "success"); code != http.StatusOK {
		t.Fatalf("listed result machine: got %d, want 200", code)
	}
}
```

- [ ] **2. Run the tests and watch them fail.**
  Run: `cd kbeads && go test ./internal/server -run 'TestCaptureEscalationResult_GateOffUnchanged|TestCaptureEscalationResult_GateA_ApprovedAndExecutor|TestCaptureEscalationResult_GateB_ServerExecuted|TestCaptureEscalationResult_GateC_RequesterWithdrawal|TestCaptureEscalationResult_GateD_ResultMachine' -count=1`
  Expected: FAIL to build with `undefined: (*BeadsServer).SetEscalationResultMachines`. Without it (the call commented out), every "want 403" case fails with 200: `/result` has no approval check.

- [ ] **3. Implement.** Create `server/escalation_result_gate.go`:

```go
package server

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// escalationResultRequiresApproval reads BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL
// (default false; Task 14 flips it). Read per request like spawnPolicyEnabled,
// so the flip is a deploy-values roll with no code change.
func escalationResultRequiresApproval() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL")))
	return err == nil && v
}

// SetEscalationResultMachines sets BEADS_ESCALATION_RESULT_MACHINES: machine
// client ids whose results are accepted without an approval (D6(d), a merge
// reaper that witnessed the merge). Empty at rollout.
func (s *BeadsServer) SetEscalationResultMachines(clients []string) {
	m := make(map[string]struct{}, len(clients))
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" {
			m[c] = struct{}{}
		}
	}
	s.resultMachines = m
}

func (s *BeadsServer) resultMachineAllowed(client string) bool {
	_, ok := s.resultMachines[client]
	return ok && client != ""
}

// escalationResultDenial is the D6 gate on POST /v1/escalations/{id}/result.
// It returns "" when the result is accepted, else the 403 text. With the flag
// off it accepts everything (today's behaviour). Accepted when any of:
//
//	(d) the caller is a machine principal in BEADS_ESCALATION_RESULT_MACHINES;
//	(c) the caller is the requester and result_status=failure (a withdrawal);
//	(b) execute_by=server and the caller is the daemon itself (isMachineWrite);
//	(a) a server-stamped approval exists and the caller is in execute_by
//	    (an empty execute_by is read as operator).
func (s *BeadsServer) escalationResultDenial(ctx context.Context, esc *model.Bead, resultStatus string) string {
	if !escalationResultRequiresApproval() {
		return ""
	}
	if authMethodFromContext(ctx) == authMethodMachine && s.resultMachineAllowed(AuthIdentityFromContext(ctx).ClientID) {
		return ""
	}
	p, err := s.activeEscalationPolicy(ctx)
	if err != nil {
		slog.Warn("escalation result: policy unreadable; refusing", "escalation", esc.ID, "error", err)
		return "could not read the escalation policy; refusing the result"
	}
	if resultStatus == "failure" {
		if in, _ := s.callerInLane(ctx, laneRef{Kind: laneRequester, Raw: string(laneRequester)}, esc, p); in {
			return ""
		}
	}
	exec, _, _ := exactFieldString(esc.Fields, "execute_by")
	if exec = strings.TrimSpace(exec); exec == "" {
		exec = string(laneOperator)
	}
	lane, err := parseLaneRef(exec)
	if err != nil {
		slog.Warn("escalation result: unparseable execute_by; treating as operator", "escalation", esc.ID, "execute_by", exec, "error", err)
		lane = laneRef{Kind: laneOperator, Raw: string(laneOperator)}
	}
	if lane.Kind == laneServer {
		if isMachineWrite(ctx) {
			return ""
		}
		return "this escalation is executed by the server's own replay; a result cannot be posted for it"
	}
	if !escalationHasServerApproval(esc) {
		return "this escalation has no approval yet; approve it before recording a result, or reject it"
	}
	in, err := s.callerInLane(ctx, lane, esc, p)
	if err != nil {
		slog.Warn("escalation result: lane membership unresolvable; refusing", "escalation", esc.ID, "lane", lane.String(), "error", err)
		return "could not resolve lane membership; refusing the result"
	}
	if !in {
		return "only a member of " + lane.String() + " (execute_by) may record this escalation's result"
	}
	return ""
}
```

  In `handleCaptureEscalationResult`, before:
```go
	resultTarget, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleCaptureEscalationResult")
	if !ok {
		return
	}
```
  After:
```go
	resultTarget, ok := s.authorizeResolutionTarget(w, r, id, "escalation", "handleCaptureEscalationResult")
	if !ok {
		return
	}

	// D6 result gate (escalation_result_gate.go), behind
	// BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL. Checked before any field is
	// merged, so a refused result leaves the bead untouched.
	if denial := s.escalationResultDenial(r.Context(), resultTarget, req.ResultStatus); denial != "" {
		s.auditDenial(r, &AuthzDenial{Action: "result", BeadType: string(resultTarget.Type), Message: denial}, id)
		writeError(w, http.StatusForbidden, denial)
		return
	}
```

  In `server/server.go`, beside `coordinatorClients` (Task 2), add:
```go
	// resultMachines is BEADS_ESCALATION_RESULT_MACHINES (escalation_result_gate.go).
	resultMachines map[string]struct{}
```
  In `kbeads/internal/config/config.go`, after `EscalationCoordinatorClients []string` add:
```go
	// EscalationResultMachines (BEADS_ESCALATION_RESULT_MACHINES) are machine
	// client ids whose escalation results are accepted without an approval
	// (D6(d)). Empty at rollout.
	EscalationResultMachines []string
```
  and after `EscalationCoordinatorClients: parseCommaSeparated(os.Getenv("BEADS_ESCALATION_COORDINATOR_CLIENTS")),` add:
```go
		EscalationResultMachines: parseCommaSeparated(os.Getenv("BEADS_ESCALATION_RESULT_MACHINES")),
```
  In `kbeads/cmd/kd/serve.go`, after `beadsServer.SetEscalationCoordinatorClients(cfg.EscalationCoordinatorClients)` add:
```go
		beadsServer.SetEscalationResultMachines(cfg.EscalationResultMachines)
		logger.Info("escalation result gate",
			"requires_approval", os.Getenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL"),
			"result_machines", cfg.EscalationResultMachines)
```
  `serve.go` already reads env with `os` (check the import block; add `"os"` if it is missing).

- [ ] **4. Run the tests and watch them pass.**
  Run: `cd kbeads && go test ./internal/server -run 'TestCaptureEscalationResult|TestEscalation|TestApproveEscalation|TestMachineGates|TestScoped' -count=1`
  Expected: PASS. The existing `TestCaptureEscalationResult` passes unchanged, because the flag is unset in the test process. If the CI environment ever sets it, those tests must set `t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "")`, as `TestCaptureEscalationResult_GateOffUnchanged` does.
  Then run: `cd kbeads && go test ./internal/config ./cmd/kd -run 'TestLoad|TestServe|TestEscalation' -count=1`
  Expected: PASS.

- [ ] **5. Lint and vet.**
  Run: `gofumpt -l kbeads/internal/server kbeads/internal/config kbeads/cmd/kd` (expect empty), then `cd kbeads && go vet ./internal/server ./internal/config ./cmd/kd && staticcheck ./internal/server && golangci-lint run ./internal/server/...`.

- [ ] **6. Commit and PR (operator-merge).**
  - Branch: `agent/escalation-routing/<task-6-bead-id>`.
  - Commit message: `kbeads: D6 escalation /result gate behind BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL (default off); supersedes #162 (escalation-routing T6)`.
  - Open the PR as a draft: `gh pr create --draft …`.
  - The PR body says:
    - the gate ships off, and its acceptance and refusal matrix is (a)–(d) plus gate-off;
    - "Supersedes #162: its approval-provenance guard landed as T1 (#<T1>) and its result authz is this PR. #162 is closed in Task 14, not here."
  - Merge escalation: `gb escalation create --title "Merge PR #<n> (escalation-routing T6)" --operation-type github_pr_merge --operation-spec '{"pr":<n>,"repo":"mattbakerconsulting/gardenio"}' --label merge-desk:hold --label project:gardenio`.
  - Then file the operator decision:
```
gb decision create --prompt "Merge escalation-routing T6 (/result gate, default off)?" \
  --context "PR #<n>. Adds the D6 gate on POST /v1/escalations/{id}/result behind BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL (unset = off = today's behaviour). On: a result needs a server-stamped approval and a caller in execute_by; or execute_by=server and the daemon; or the requester withdrawing with failure; or a machine in BEADS_ESCALATION_RESULT_MACHINES (empty at rollout). Supersedes PR #162 (closed in Task 14)." \
  --option "merge:Merge it:report" --option "send-back:Send back with notes:report" --no-wait
```

---

## Phase 3: Beads viewer

### Task 7: Viewer: lane helpers, who holds it, the land-owner fold and `/up`

**Step bead:** (minted later) Phase 3 Beads viewer. Blocked by Tasks 3 and 5 (Task 1's field names via both; Task 3 serves `can-approve`; `ListEscalationsHeldBy` exists). Blocks Tasks 8, 9 and 12.

**Who does what:** CREW writes the code and opens the PR. Merge lane: desk.

**What #756 already did, and this task does not redo.** `/escalations`, the health tile and the index banner list and count only owner-audience escalations and fold the rest into "In hand: N (desk X, operator Y)" (`ownerEscalations`, `beadsapi.InHand`). Once Task 2 stamps `audience` from `held_by` (D11), those lists already follow `held_by`. This task adds only what `audience` cannot say:
- which lane holds an In-hand row ("coordinator laptop" rather than "operator");
- that an escalation held by `land-owner:<land>` is the owner audience but not the gardenio owner's: it folds for anyone who does not own that land (D9);
- `/up` leading with an escalation that needs the viewer.

**Files:**
- Create `gardenio/controller/internal/beadsapi/escalation_lanes.go` and `escalation_lanes_test.go`.
- Modify `gardenio/controller/internal/beadsapi/escalation_audience.go` (#756): re-locate by `type InHand struct {`. Add `Lands`, `AddLand` and the "land owners" part of `Line`.
- Modify `gardenio/controller/internal/beadsapi/escalation_audience_test.go` (#756): re-locate by `func TestInHandLine(t *testing.T) {`.
- Modify `gardenio/controller/internal/beadsapi/client_escalations.go`: after `ListEscalationsHeldBy` (Task 5), add `CanApproveResult` and `CanApproveEscalation`.
- Modify `gardenio/controller/internal/beadsapi/client_escalations_test.go`, in the style of `TestApproveEscalation_UsesUserBearerOnWire` (re-locate by `func TestApproveEscalation_UsesUserBearerOnWire(`).
- Create `gardenio/controller/cmd/beads-viewer/escalation_lanes.go` (`viewerLaneRefs`, `escalationOwnerRow`) and `escalation_lanes_test.go`.
- Modify `gardenio/controller/cmd/beads-viewer/escalations.go` (#756):
  - `escalationRowView`, re-locate by `	Audience string
	InHand   bool`;
  - `handleEscalations`, re-locate by `		row.InHand = row.Audience != beadsapi.AudienceOwner` and by `			tally.Add(row.Audience)`;
  - `buildEscalationsTileView`, re-locate by `	escalations, v.InHand = ownerEscalations(escalations)`;
  - `ownerEscalations`, re-locate by `func ownerEscalations(all []beadsapi.EscalationDetail) ([]beadsapi.EscalationDetail, int) {`.
- Modify `gardenio/controller/cmd/beads-viewer/server.go`, `computeIndexStats`: re-locate by `		owner, _ := ownerEscalations(escalations) // the owner's only (gd-bmOiteQi36)`.
- Modify `gardenio/controller/cmd/beads-viewer/templates/escalations.html`: re-locate by `title="who has it">{{.R.Audience}}</span>`.
- Modify `gardenio/controller/cmd/beads-viewer/up.go`, `selectUpBead` (re-locate by `func (s *Server) selectUpBead(ctx context.Context, email string) upRenderContext {`).
- Tests: `cmd/beads-viewer/escalations_test.go` (helpers `makeEscBead`, `newTestServerForEsc`) and `cmd/beads-viewer/up_test.go` (helpers `newFake`, `mkServer`; the fake's generic list response is `listResp`, and its queries are recorded in `listAllQ`).

**Interfaces:**
- Consumes: the `held_by`, `execute_by` and `audience` fields (Tasks 1, 2); `ListEscalationsHeldBy` (Task 5); `POST /v1/escalations/{id}/can-approve` (Task 3; only the client is added here, and Task 8 calls it); #756's `EscalationAudience`, `EscalationForOwner`, `AudienceOwner` and `InHand`.
- Produces (Interface contract):
  - `LaneOperator`, `LaneRequester`, `LaneServer`;
  - `EscalationHeldBy(b *BeadDetail) string`: `""` when unstamped (Global Constraint 7: read `audience` instead);
  - `HeldByFromFields(f map[string]string) string`, `EscalationExecuteBy(b *BeadDetail) string`, `LandOwnerLane(ref string) (land string, ok bool)`, `LaneLabel(ref string) string`;
  - `InHand.Lands`, `(*InHand).AddLand()`;
  - `CanApproveResult`, `(c *Client) CanApproveEscalation(ctx, id) (CanApproveResult, error)`;
  - `(s *Server) viewerLaneRefs(ctx context.Context, email string) []string`;
  - `escalationOwnerRow(fields map[string]string, lanes []string) (owner, landFold bool)`.

- [ ] **Step 1: Write the failing beadsapi tests**

`gardenio/controller/internal/beadsapi/escalation_lanes_test.go`:
```go
package beadsapi

import "testing"

func TestEscalationHeldBy_UnstampedIsEmpty(t *testing.T) {
	cases := []struct {
		name string
		b    *BeadDetail
		want string
	}{
		{"nil bead", nil, ""},
		{"no fields", &BeadDetail{}, ""},
		{"blank held_by", &BeadDetail{Fields: map[string]string{"held_by": "  "}}, ""},
		// Positive control: a stamped lane is returned as stamped.
		{"coordinator", &BeadDetail{Fields: map[string]string{"held_by": "coordinator:laptop"}}, "coordinator:laptop"},
	}
	for _, c := range cases {
		if got := EscalationHeldBy(c.b); got != c.want {
			t.Errorf("%s: EscalationHeldBy = %q, want %q", c.name, got, c.want)
		}
	}
	if got := EscalationExecuteBy(&BeadDetail{}); got != "" {
		t.Errorf("unstamped execute_by = %q, want empty (legacy execution_target path)", got)
	}
}

func TestLaneLabelAndLandOwner(t *testing.T) {
	for ref, want := range map[string]string{
		"":                       "",
		"operator":               "the owner", // the lane, not #756's operator audience
		"land-owner:acme":        "the acme owner",
		"coordinator:laptop":     "coordinator laptop",
		"coordinator:merge-desk": "coordinator merge-desk",
		"steward:acme":           "the acme steward",
		"requester":              "the requesting agent",
		"server":                 "the server",
	} {
		if got := LaneLabel(ref); got != want {
			t.Errorf("LaneLabel(%q) = %q, want %q", ref, got, want)
		}
	}
	if land, ok := LandOwnerLane("land-owner:acme"); !ok || land != "acme" {
		t.Errorf("LandOwnerLane(land-owner:acme) = %q, %v", land, ok)
	}
	for _, ref := range []string{"operator", "steward:acme", "land-owner:", ""} {
		if _, ok := LandOwnerLane(ref); ok {
			t.Errorf("LandOwnerLane(%q) = true, want false", ref)
		}
	}
}
```

At the end of `TestInHandLine` (`escalation_audience_test.go`, #756), add:
```go
	var l InHand
	l.Add(AudienceDesk)
	l.AddLand()
	l.AddLand()
	if got, want := l.Line(), "In hand: 3 (desk 1, land owners 2)"; got != want {
		t.Errorf("Line with land owners = %q, want %q", got, want)
	}
```
The test's existing assertions (`In hand: 3 (desk 2, operator 1)`, `In hand: 2 (operator 2)`) are the positive control and stay as they are.

Append to `gardenio/controller/internal/beadsapi/client_escalations_test.go`:
```go
func TestCanApproveEscalation_UsesUserBearerAndDecodes(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"allowed":false,"method":"","as":"","reason":"caller is not in the approver lane that holds this escalation (coordinator:merge-desk)"}`))
	}))
	defer srv.Close()
	c := newTestClientWithToken(t, srv, "static-tok")

	ctx := WithUserBearer(context.Background(), "user-jwt")
	got, err := c.CanApproveEscalation(ctx, "gd-e1")
	if err != nil {
		t.Fatalf("CanApproveEscalation: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/escalations/gd-e1/can-approve" || gotAuth != "Bearer user-jwt" {
		t.Fatalf("wire = %s %s auth=%q, want POST /v1/escalations/gd-e1/can-approve with the user bearer", gotMethod, gotPath, gotAuth)
	}
	if got.Allowed || got.Reason == "" {
		t.Fatalf("decoded = %+v", got)
	}
}
```
`WithUserBearer` and `newTestClientWithToken` are in `user_bearer.go` / `user_bearer_test.go`.

- [ ] **Step 2: Run them; they should FAIL**

Run: `cd gardenio/controller && go test ./internal/beadsapi -run 'TestEscalationHeldBy_UnstampedIsEmpty|TestLaneLabelAndLandOwner|TestInHandLine|TestCanApproveEscalation_UsesUserBearerAndDecodes' -count=1`
Expected: FAIL to compile, with `undefined: EscalationHeldBy`, `unknown field Lands in struct literal` and `c.CanApproveEscalation undefined`.

- [ ] **Step 3: Implement the beadsapi pieces**

`gardenio/controller/internal/beadsapi/escalation_lanes.go`:
```go
package beadsapi

import "strings"

// Escalation lane refs the controller names directly (D2 of the escalation
// routing plan). The grammar, membership and the lane → audience mapping live
// in kbeads (escalation_lane.go, escalation_policy.go laneAudience); the
// controller only READS lanes, to label them. Whether a surface shows an
// escalation is #756's audience (EscalationForOwner); whether a viewer may
// act is the daemon's can-approve, never these helpers.
const (
	LaneOperator  = "operator" // the owner (gardenio-admin), not #756's operator audience
	LaneRequester = "requester"
	LaneServer    = "server"
)

// HeldByFromFields returns an escalation's held_by, or "" when it has none
// (filed before routing shipped, or while the policy was unreadable). An
// unstamped escalation is shown where its audience puts it (Global
// Constraint 7), so "" never means "the operator".
func HeldByFromFields(f map[string]string) string {
	return strings.TrimSpace(f["held_by"])
}

// EscalationHeldBy is HeldByFromFields for a bead; a nil bead is "".
func EscalationHeldBy(b *BeadDetail) string {
	if b == nil {
		return ""
	}
	return HeldByFromFields(b.Fields)
}

// EscalationExecuteBy returns the escalation's execute_by lane, or "" when it
// has none, in which case the bridge runs the legacy execution_target path.
func EscalationExecuteBy(b *BeadDetail) string {
	if b == nil {
		return ""
	}
	return strings.TrimSpace(b.Fields["execute_by"])
}

// LandOwnerLane reports whether ref is land-owner:<land>, and which land.
func LandOwnerLane(ref string) (string, bool) {
	land, ok := strings.CutPrefix(strings.TrimSpace(ref), "land-owner:")
	return land, ok && land != ""
}

// LaneLabel is how a surface names a lane to a person; "" for no lane.
func LaneLabel(ref string) string {
	ref = strings.TrimSpace(ref)
	kind, name, _ := strings.Cut(ref, ":")
	switch kind {
	case "":
		return ""
	case LaneOperator:
		return "the owner"
	case "coordinator":
		return "coordinator " + name
	case "steward":
		return "the " + name + " steward"
	case "land-owner":
		return "the " + name + " owner"
	case LaneRequester:
		return "the requesting agent"
	case LaneServer:
		return "the server"
	}
	return ref
}
```

In `escalation_audience.go` (#756), before:
```go
type InHand struct {
	Desk     int
	Operator int
}
```
After:
```go
type InHand struct {
	Desk     int
	Operator int
	// Lands counts owner-audience escalations held by a land's owner that the
	// viewer does not own (escalation-routing D9): theirs, not this viewer's.
	Lands int
}

// AddLand counts one escalation another land's owner holds.
func (h *InHand) AddLand() { h.Lands++ }
```
`Total` becomes `h.Desk + h.Operator + h.Lands`, and `Line` gains, after the operator part:
```go
	if h.Lands > 0 {
		parts = append(parts, "land owners "+strconv.Itoa(h.Lands))
	}
```

In `client_escalations.go`, after `ListEscalationsHeldBy`:
```go
// CanApproveResult is the daemon's dry-run answer to "may the caller approve
// this escalation": the same checks the approve endpoint runs (lanes, the
// hold, operator_only, self-approval), with nothing written.
type CanApproveResult struct {
	Allowed bool   `json:"allowed"`
	Method  string `json:"method"`
	As      string `json:"as"`
	Reason  string `json:"reason"`
}

// CanApproveEscalation asks POST /v1/escalations/{id}/can-approve. Like
// ApproveEscalation it forces the per-request user bearer onto the wire when
// ctx carries one, so the daemon answers for the calling human; without one the
// static token answers, and that token is a member of no lane (allowed=false).
func (c *Client) CanApproveEscalation(ctx context.Context, id string) (CanApproveResult, error) {
	path := "/v1/escalations/" + url.PathEscape(id) + "/can-approve"
	var out CanApproveResult
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, struct{}{}, &out)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, struct{}{}, &out)
	}
	if err != nil {
		return CanApproveResult{}, fmt.Errorf("checking approval of escalation %s: %w", id, err)
	}
	return out, nil
}
```
This is `ApproveEscalation`'s shape (`doJSONAsUser` in `user_bearer.go`, `doJSON` in `client.go`), with a decoded result.

- [ ] **Step 4: Run the beadsapi tests; they should PASS**

Run: the Step 2 command again, then `cd gardenio/controller && go test ./internal/beadsapi -count=1`.
Expected: PASS, including #756's `TestEscalationAudience` and `TestDefaultEscalationAudience` unchanged.

- [ ] **Step 5: Write the failing viewer tests**

Create `gardenio/controller/cmd/beads-viewer/escalation_lanes_test.go`:
```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// oidcViewerCtx is a request context as the Keycloak path builds it: a
// verified token with roles and groups.
func oidcViewerCtx(email string, roles, groups []string) context.Context {
	ctx := oidcauth.ContextWithEmail(context.Background(), email)
	ctx = oidcauth.ContextWithOIDCVerified(ctx)
	ctx = oidcauth.ContextWithRoles(ctx, roles)
	return oidcauth.ContextWithGroups(ctx, groups)
}

func TestViewerLaneRefs(t *testing.T) {
	srv := newTestServerForEsc(t, &fakeDaemon{})
	cases := []struct {
		name  string
		ctx   context.Context
		email string
		want  []string
	}{
		{"anonymous", context.Background(), "", nil},
		{"admin", oidcViewerCtx("Op@Acme.example", []string{oidcauth.RoleAdmin}, nil), "Op@Acme.example", []string{"operator"}},
		{"land owner, not admin", oidcViewerCtx("o@acme.example", []string{oidcauth.RoleProjectMember}, []string{"/project-acme/owners", "/project-beta"}), "o@acme.example",
			[]string{"land-owner:acme"}},
		{"viewer role holds no lane", oidcViewerCtx("v@acme.example", []string{oidcauth.RoleViewer}, nil), "v@acme.example", []string{}},
		// The trusted edge (dev bypass, AVA) carries no roles and no verified
		// token; the viewer already treats it as unrestricted.
		{"edge identity", oidcauth.ContextWithEmail(context.Background(), "e@acme.example"), "e@acme.example", []string{"operator"}},
	}
	for _, c := range cases {
		if got := srv.viewerLaneRefs(c.ctx, c.email); !slices.Equal(got, c.want) || (got == nil) != (c.want == nil) {
			t.Errorf("%s: lanes = %#v, want %#v", c.name, got, c.want)
		}
	}
}

func landEscalations(now time.Time) []beadsapi.EscalationDetail {
	return []beadsapi.EscalationDetail{
		// #756 shapes: an unstamped one (audience only) and a stamped owner one.
		{Issue: makeEscBead("gd-legacy", "legacy, unstamped", "manual_owner_action", "internal", "open", now.Add(-4*time.Hour),
			map[string]string{"audience": "owner"})},
		{Issue: makeEscBead("gd-mine", "held by the owner", "manual_owner_action", "internal", "open", now.Add(-3*time.Hour),
			map[string]string{"held_by": "operator", "audience": "owner"})},
		{Issue: makeEscBead("gd-acme", "acme merge, held by its owner", "gitlab_mr_merge", "internal", "open", now.Add(-2*time.Hour),
			map[string]string{"held_by": "land-owner:acme", "audience": "owner"})},
		{Issue: makeEscBead("gd-laptop", "held by the laptop", "helm_deploy", "internal", "open", now.Add(-1*time.Hour),
			map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})},
	}
}

// TestHandleEscalations_LandOwnerFoldsForOthers pins Review Focus 6 and D9: a
// land owner's escalation is owner audience but not the gardenio owner's; it
// folds into In hand as "land owners" for anyone who does not own the land.
func TestHandleEscalations_LandOwnerFoldsForOthers(t *testing.T) {
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: landEscalations(time.Now())})
	get := func(ctx context.Context) string {
		rec := httptest.NewRecorder()
		srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil).WithContext(ctx))
		return rec.Body.String()
	}
	body := get(oidcViewerCtx("op@acme.example", []string{oidcauth.RoleAdmin}, nil))
	listed, inHand, _ := strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-mine"`) || strings.Contains(listed, `id="escalation-row-gd-acme"`) {
		t.Errorf("gardenio owner: gd-mine must be listed, gd-acme folded\n%s", body)
	}
	if !strings.Contains(inHand, `id="escalation-row-gd-acme"`) || !strings.Contains(body, "In hand: 2 (operator 1, land owners 1)") {
		t.Errorf("gardenio owner: in-hand fold wrong\n%s", body)
	}
	// Positive control: acme's owner sees it as theirs.
	body = get(oidcViewerCtx("o@acme.example", []string{oidcauth.RoleProjectMember}, []string{"/project-acme/owners"}))
	listed, _, _ = strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-acme"`) {
		t.Errorf("acme owner: gd-acme not listed as theirs\n%s", body)
	}
	// Anonymous (no lanes): #756's behaviour, every owner-audience row listed.
	body = get(context.Background())
	listed, _, _ = strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-acme"`) {
		t.Errorf("anonymous: gd-acme folded; want #756's listing\n%s", body)
	}
}

// TestHandleEscalations_UnstampedFollowsAudience pins Review Focus 5: an
// escalation with no held_by is shown where #756's audience puts it, never
// dropped; and In-hand rows name the holding lane.
func TestHandleEscalations_UnstampedFollowsAudience(t *testing.T) {
	now := time.Now()
	escs := append(landEscalations(now),
		beadsapi.EscalationDetail{Issue: makeEscBead("gd-old-desk", "legacy desk merge", "github_pr_merge", "internal", "open", now, map[string]string{"audience": "desk"})})
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: escs})
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil).WithContext(oidcViewerCtx("op@acme.example", []string{oidcauth.RoleAdmin}, nil)))
	body := rec.Body.String()
	listed, inHand, _ := strings.Cut(body, `id="escalations-inhand"`)
	if !strings.Contains(listed, `id="escalation-row-gd-legacy"`) {
		t.Errorf("unstamped owner-audience escalation not listed\n%s", body)
	}
	if !strings.Contains(inHand, `id="escalation-row-gd-old-desk"`) {
		t.Errorf("unstamped desk escalation vanished instead of folding\n%s", body)
	}
	if !strings.Contains(inHand, ">coordinator laptop</span>") {
		t.Errorf("in-hand row does not name its lane\n%s", body)
	}
	if !strings.Contains(inHand, ">desk</span>") {
		t.Errorf("an unstamped in-hand row lost #756's audience chip\n%s", body)
	}
}

func TestEscalationCounts_FoldOtherLands(t *testing.T) {
	now := time.Now()
	srv := newTestServerForEsc(t, &fakeDaemon{escalationsResp: landEscalations(now)})
	ctx := oidcViewerCtx("op@acme.example", []string{oidcauth.RoleAdmin}, nil)
	if v := srv.buildEscalationsTileView(ctx, now); v.Pending != 2 || v.InHand != 2 {
		t.Errorf("tile = pending %d in hand %d, want 2 and 2 (gd-acme folds)", v.Pending, v.InHand)
	}
	if v := srv.computeIndexStats(ctx); !v.EscalationsOK || v.EscalationsPending != 2 {
		t.Errorf("index banner = %d (ok=%v), want 2", v.EscalationsPending, v.EscalationsOK)
	}
	// No identity: #756's counts exactly.
	if v := srv.buildEscalationsTileView(context.Background(), now); v.Pending != 3 {
		t.Errorf("anonymous tile Pending = %d, want 3", v.Pending)
	}
}
```

Append to `gardenio/controller/cmd/beads-viewer/up_test.go`:
```go
// TestUp_PrefersNeedsYouEscalation: an escalation that needs the viewer
// outranks their in_progress work; one in hand never reaches their /up.
func TestUp_PrefersNeedsYouEscalation(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-up-laptop", "Held by laptop", "helm_deploy", "internal", "open", now.Add(-2*time.Hour),
			map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})},
		{Issue: makeEscBead("gd-up-esc", "Needs the owner", "manual_owner_action", "internal", "open", now.Add(-time.Hour),
			map[string]string{"held_by": "operator", "audience": "owner"})},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{ID: "gd-up-task", Title: "Task", Type: "task", Status: "in_progress"}}}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/up", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com") // edge identity: operator lane
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "gd-up-esc") || strings.Contains(body, "gd-up-laptop") || strings.Contains(body, "gd-up-task") {
		t.Fatalf("expected the owner-held escalation only; body=%s", body)
	}
}

func TestUp_SkipsEscalationInHand(t *testing.T) {
	f := newFake()
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-up-laptop", "Held by laptop", "helm_deploy", "internal", "open", time.Now(),
			map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{{ID: "gd-up-task", Title: "Task", Type: "task", Status: "in_progress"}}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/up", nil)
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if body := w.Body.String(); !strings.Contains(body, "gd-up-task") || strings.Contains(body, "gd-up-laptop") {
		t.Fatalf("want the in_progress task, not the laptop's escalation; body=%s", body)
	}
	for _, q := range f.listAllQ {
		if !slices.Contains(q.ExcludeTypes, "escalation") {
			t.Errorf("/up bead query %+v does not exclude escalations; they come only from the needs-you pick", q)
		}
	}
}
```
Add `slices` to `up_test.go`'s imports if it is not there. If `newFake`'s `listResp` answers the `in_progress` query through a narrower field in this fake (see `bead_show_test.go`, `func (f *fakeDaemon) ListBeadsFiltered(`), use that field; the assertion stays the same.

- [ ] **Step 6: Run them; they should FAIL**

Run: `cd gardenio/controller && go test ./cmd/beads-viewer -run 'TestViewerLaneRefs|TestHandleEscalations_LandOwnerFoldsForOthers|TestHandleEscalations_UnstampedFollowsAudience|TestEscalationCounts_FoldOtherLands|TestUp_PrefersNeedsYouEscalation|TestUp_SkipsEscalationInHand' -count=1`
Expected: FAIL to compile, with `srv.viewerLaneRefs undefined`.

- [ ] **Step 7: Implement the viewer lanes and the owner-row rule**

Create `gardenio/controller/cmd/beads-viewer/escalation_lanes.go`:
```go
package main

import (
	"context"
	"slices"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

// viewerLaneRefs is the set of owner-audience lanes (D11) the signed-in viewer
// holds: operator for a gardenio-admin (or an identity from the trusted edge
// with no roles and no verified token, the dev bypass and AVA, which
// scopeFromRequest already treats as unrestricted), and land-owner:<land> for
// each land whose owners group the token names. It decides what is SHOWN as
// needing the viewer, never what they may do: the buttons ask the daemon.
// Nil for an anonymous request; surfaces then behave exactly as #756 left them.
func (s *Server) viewerLaneRefs(ctx context.Context, email string) []string {
	if canonicalOIDC(email) == "" {
		return nil
	}
	lanes := []string{}
	roles := oidcauth.RolesFromContext(ctx)
	if oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin) ||
		(len(roles) == 0 && !oidcauth.OIDCVerifiedFromContext(ctx)) {
		lanes = append(lanes, beadsapi.LaneOperator)
	}
	var owned []string
	for land, role := range conversation.LandRolesFromGroups(oidcauth.GroupsFromContext(ctx)) {
		if role == conversation.LandRoleOwner {
			owned = append(owned, "land-owner:"+land)
		}
	}
	slices.Sort(owned)
	return append(lanes, owned...)
}

// escalationOwnerRow is D9 on top of #756: whether an escalation is listed as
// the viewer's own (owner) or folds into In hand, and whether it folds as
// another land owner's (landFold). #756's EscalationForOwner decides the
// audience; the only refinement is that a land owner's escalation is that
// land owner's. Nil lanes (anonymous) is #756 unchanged.
func escalationOwnerRow(fields map[string]string, lanes []string) (owner, landFold bool) {
	if !beadsapi.EscalationForOwner(fields) {
		return false, false
	}
	held := beadsapi.HeldByFromFields(fields)
	if _, ok := beadsapi.LandOwnerLane(held); ok && lanes != nil && !slices.Contains(lanes, held) {
		return false, true
	}
	return true, false
}
```

- [ ] **Step 8: Use it in the list, the counts and the template**

In `escalations.go` (#756), `escalationRowView`, before:
```go
	Audience string
	InHand   bool
```
After:
```go
	Audience string
	InHand   bool
	// HeldBy names the holding lane (escalation-routing D9), e.g.
	// "coordinator laptop"; empty for an unstamped escalation, whose chip
	// then shows Audience as #756 did. LandFold marks an In-hand row held by
	// another land's owner.
	HeldBy   string
	LandFold bool
```

In `handleEscalations`, compute the viewer's lanes once, beside `viewerEmail := oidcauth.EmailFromContext(r.Context())`:
```go
	lanes := s.viewerLaneRefs(r.Context(), viewerEmail)
```
Before:
```go
		row.InHand = row.Audience != beadsapi.AudienceOwner
```
After:
```go
		owner, landFold := escalationOwnerRow(b.Fields, lanes)
		row.InHand, row.LandFold = !owner, landFold
		row.HeldBy = beadsapi.LaneLabel(beadsapi.HeldByFromFields(b.Fields))
```
Before:
```go
			tally.Add(row.Audience)
```
After:
```go
			if row.LandFold {
				tally.AddLand()
			} else {
				tally.Add(row.Audience)
			}
```

`ownerEscalations` takes the lanes. Before:
```go
func ownerEscalations(all []beadsapi.EscalationDetail) ([]beadsapi.EscalationDetail, int) {
	owner := make([]beadsapi.EscalationDetail, 0, len(all))
	inHand := 0
	for _, ed := range all {
		if ed.Issue != nil && !beadsapi.EscalationForOwner(ed.Issue.Fields) {
			inHand++
			continue
		}
		owner = append(owner, ed)
	}
	return owner, inHand
}
```
After:
```go
func ownerEscalations(all []beadsapi.EscalationDetail, lanes []string) ([]beadsapi.EscalationDetail, int) {
	owner := make([]beadsapi.EscalationDetail, 0, len(all))
	inHand := 0
	for _, ed := range all {
		if ed.Issue != nil {
			if mine, _ := escalationOwnerRow(ed.Issue.Fields, lanes); !mine {
				inHand++
				continue
			}
		}
		owner = append(owner, ed)
	}
	return owner, inHand
}
```
Update its comment to name D9. Its two callers pass the viewer's lanes:
- `buildEscalationsTileView`: `escalations, v.InHand = ownerEscalations(escalations, s.viewerLaneRefs(ctx, oidcauth.EmailFromContext(ctx)))`;
- `computeIndexStats` (`server.go`): `owner, _ := ownerEscalations(escalations, s.viewerLaneRefs(ctx, oidcauth.EmailFromContext(ctx))) // the viewer's (gd-bmOiteQi36, escalation-routing D9)`.

In `templates/escalations.html`, the In-hand chip. Before:
```html
title="who has it">{{.R.Audience}}</span>
```
After:
```html
title="who has it">{{if .R.HeldBy}}{{.R.HeldBy}}{{else}}{{.R.Audience}}{{end}}</span>
```

- [ ] **Step 9: `/up` leads with an escalation that needs the viewer**

In `up.go`, add above `selectUpBead`:
```go
// upNeedsYouEscalation is the escalation /up leads with: the highest-priority,
// then oldest, open escalation that is the viewer's own (escalationOwnerRow)
// and held by one of their lanes, or unstamped and owner-audience for the
// operator lane. Nil when there is none, the viewer holds no lane, or the
// daemon errors (logged; /up falls through to beads).
func (s *Server) upNeedsYouEscalation(ctx context.Context, email string) *beadsapi.BeadDetail {
	lanes := s.viewerLaneRefs(ctx, email)
	if len(lanes) == 0 {
		return nil
	}
	escs, err := s.daemon.ListEscalations(ctx, "", escalationsListLimit)
	if err != nil {
		s.logger.Warn("listing escalations for /up", "email", email, "error", err)
		return nil
	}
	var best *beadsapi.BeadDetail
	for _, ed := range escs {
		b := ed.Issue
		if b == nil {
			continue
		}
		if mine, _ := escalationOwnerRow(b.Fields, lanes); !mine {
			continue
		}
		held := beadsapi.HeldByFromFields(b.Fields)
		if held == "" {
			held = beadsapi.LaneOperator // unstamped owner-audience: the gardenio owner's, as #756 lists it
		}
		if !slices.Contains(lanes, held) {
			continue
		}
		if best == nil || b.Priority < best.Priority || (b.Priority == best.Priority && b.CreatedAt.Before(best.CreatedAt)) {
			best = b
		}
	}
	return best
}
```
In `selectUpBead`, before:
```go
	if email != "" {
		// canonicalOIDC turns the bare mixed-case email into the canonical
```
After:
```go
	if email != "" {
		if b := s.upNeedsYouEscalation(ctx, email); b != nil {
			rc.Bead = b
			return rc
		}
		// canonicalOIDC turns the bare mixed-case email into the canonical
```
Add `ExcludeTypes: []string{"escalation"},` to **both** `ListBeadsQuery` literals in `selectUpBead`, with the comment: an escalation reaches /up only through the needs-you pick, so one operated by the viewer but in hand never shows as theirs. Add `slices` to `up.go`'s imports.

- [ ] **Step 10: Run the tests; they should PASS**

Run: the Step 6 command again, then the whole packages once more: `cd gardenio/controller && go test ./cmd/beads-viewer ./internal/beadsapi -count=1`.
Expected: PASS. #756's own viewer tests (`TestHandleEscalations_HappyPath` and the In-hand tests in `escalations_test.go`) still pass: they carry no identity (nil lanes) or no `held_by`, so `escalationOwnerRow` is `EscalationForOwner` for them.

- [ ] **Step 11: Vet and format**

Run: `cd gardenio/controller && gofumpt -l ./cmd/beads-viewer ./internal/beadsapi && go vet ./cmd/beads-viewer ./internal/beadsapi && staticcheck ./cmd/beads-viewer ./internal/beadsapi`
Expected: no output.

- [ ] **Step 12: Commit and PR (merge lane: desk)**

Branch `agent/escalation-routing/<task-7-bead-id>`. Commit: `viewer: In-hand rows name the holding lane; other lands' owner escalations fold; /up leads with needs-you (escalation-routing T7)`. The PR body names Review Focus 5 and 6 and links `TestHandleEscalations_UnstampedFollowsAudience` and `TestHandleEscalations_LandOwnerFoldsForOthers`.

---

### Task 8: Viewer: escalation card routing panel and daemon-checked buttons

**Step bead:** (minted later) Phase 3 Beads viewer. Blocked by Task 3 (`can-approve` is served) and Task 7 (helpers and client). Blocks Tasks 12 and 14.

**What #756 already did, and this task does not redo.** The card's head already carries #756's audience chip ("for desk", "for owner (forced)") and, for a gardenio-operator, the re-route chips (`CanRoute`, `POST /b/{id}/escalation/route`). After Task 5 that route also moves `held_by`. This task adds the routing panel (who holds it, who may approve, who does the work) under the head, and makes Approve/Reject ask the daemon.

**Who does what:** CREW writes the code and opens the PR. Merge lane: desk.

**Files:**
- Modify `gardenio/controller/cmd/beads-viewer/server.go`, the `beadAPI` interface: re-locate by `	RejectEscalation(ctx context.Context, escalationID string, req beadsapi.RejectEscalationRequest) error` and add `CanApproveEscalation`.
- Modify `server.go`, the bead-page gate: re-locate by `		can, why := s.viewerCanApproveEscalation(r.Context(), email)`.
- Modify `gardenio/controller/cmd/beads-viewer/escalation_actions.go`:
  - add a new `escalationApproveGate` next to `viewerCanApproveEscalation` (re-locate by `func (s *Server) viewerCanApproveEscalation(ctx context.Context, email string) (bool, string) {`);
  - in `escalationAction`, the precheck `	if ok, why := s.viewerCanApproveEscalation(r.Context(), email); !ok {` moves below the status check `	if bead.Status != "open" && bead.Status != "in_progress" {`;
  - `renderEscalationPartial`: re-locate by `		ev.CanApprove, ev.ApproveBlockedReason = s.viewerCanApproveEscalation(r.Context(), email)`.
- Modify `gardenio/controller/cmd/beads-viewer/escalation_card.go`:
  - `escalationPrimaryFields`: re-locate by `var escalationPrimaryFields = map[string]bool{`;
  - the `escalationView` struct: re-locate by `	RequesterChainRootHTML template.HTML`;
  - `buildEscalationView`: re-locate by `	v.Open = (bead.Status == "open" || bead.Status == "in_progress") && !v.Rejected` and `		v.PolicyRule = strings.TrimSpace(f["policy_rule"])`.
- Modify `gardenio/controller/cmd/beads-viewer/templates/partials/type_escalation.html`: the routing panel goes before the spawn block (re-locate by `  {{if .Error}}<p class="escalation-error" role="alert">{{.Error}}</p>{{end}}`), and the approval trail (re-locate by `{{if .ApprovalMethod}} <span class="escalation-method">({{.ApprovalMethod}})</span>{{end}}`).
- Tests:
  - `cmd/beads-viewer/bead_show_test.go`: fakeDaemon fields (re-locate by `	escalationsLastLim`) and the methods after `func (f *fakeDaemon) ListEscalations(`;
  - `cmd/beads-viewer/escalation_actions_test.go`: `escActionFake` and the tests named below;
  - a new `cmd/beads-viewer/escalation_card_routing_test.go`.

**Interfaces:**
- Consumes `beadsapi.CanApproveEscalation`, `CanApproveResult`, `EscalationHeldBy`, `EscalationExecuteBy` and `LaneLabel` (Task 7), and `POST /v1/escalations/{id}/can-approve` (Task 3).
- Produces `(s *Server) escalationApproveGate(ctx context.Context, id, email string) (bool, string)`, the only gate on the bead page's Approve and Reject buttons. Task 12 makes the Commons adapter call it too. The /garden Mailbox keeps #756's capability gate for drawing its buttons (`main.go`, re-locate by `		CanApproveEscalation: func(r *http.Request) bool {`); its POSTs go through `escalationAction`, which calls the new gate (D9).

- [ ] **Step 1: Add the fake's `can-approve` and point `escActionFake` at it**

In `bead_show_test.go`, add these to `fakeDaemon` after `escalationsLastLim`:
```go
	// canApproveResp answers CanApproveEscalation by escalation id; a missing id
	// is {Allowed:false}. canApproveErr fails every call; canApproveIDs records
	// which escalations were asked about.
	canApproveResp map[string]beadsapi.CanApproveResult
	canApproveErr  error
	canApproveIDs  []string
```
After the fake's `RejectEscalation` method, add:
```go
func (f *fakeDaemon) CanApproveEscalation(_ context.Context, id string) (beadsapi.CanApproveResult, error) {
	f.canApproveIDs = append(f.canApproveIDs, id)
	if f.canApproveErr != nil {
		return beadsapi.CanApproveResult{}, f.canApproveErr
	}
	return f.canApproveResp[id], nil
}
```
In `escalation_actions_test.go`, `escActionFake` now seeds the daemon's yes for `gd-esc`. The `caps` argument still seeds the user beads, which only the 404 fallback reads:
```go
		canApproveResp: map[string]beadsapi.CanApproveResult{
			"gd-esc": {Allowed: true, Method: "web_ui", As: "operator"},
		},
```

- [ ] **Step 2: Rewrite the capability tests as daemon-answer tests (these fail first)**

In `escalation_actions_test.go`, replace these tests:
- `TestEscalationApprove_WildcardCapability`;
- `TestEscalationApprove_MissingCapability`;
- `TestEscalationApprove_UnknownViewer`;
- `TestViewerCanApproveEscalation_Reasons`;
- `TestBeadShow_EscalationHidesChipsWithoutCapability`.

Their replacements:
```go
// TestEscalationApprove_DaemonSaysNo: can-approve's "no" is a 403 with its reason,
// and nothing reaches the approve endpoint. TestEscalationApprove_HappyPath is the
// positive control on the same fixture with the daemon's yes.
func TestEscalationApprove_DaemonSaysNo(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "caller is not in the approver lane that holds this escalation (coordinator:laptop)"}
	rec := escPost(t, fake, "gd-esc", "approve", "approver@acme.example", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if len(fake.approveEscCalls) != 0 {
		t.Error("a viewer the daemon refused reached ApproveEscalation")
	}
	if !strings.Contains(rec.Body.String(), "holds this escalation (coordinator:laptop)") {
		t.Errorf("403 body should carry the daemon's reason: %s", rec.Body.String())
	}
	if !slices.Contains(fake.canApproveIDs, "gd-esc") {
		t.Error("the POST did not ask can-approve")
	}
}

func TestEscalationApproveGate_Reasons(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	ctx := t.Context()

	if ok, why := srv.escalationApproveGate(ctx, "gd-esc", ""); ok || !strings.Contains(why, "Sign in") {
		t.Errorf("anonymous: ok=%v why=%q", ok, why)
	}
	if ok, _ := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); !ok {
		t.Error("daemon yes was refused")
	}
	fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "operator_only"}
	if ok, why := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); ok || !strings.Contains(why, "operator_only") {
		t.Errorf("daemon no: ok=%v why=%q", ok, why)
	}
	// Fail closed on a daemon error ...
	fake.canApproveErr = &beadsapi.APIError{StatusCode: 500, Message: "boom"}
	if ok, why := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); ok || !strings.Contains(why, "Couldn't check") {
		t.Errorf("daemon error: ok=%v why=%q", ok, why)
	}
	// ... except a daemon that predates can-approve (404): the legacy capability
	// gate, which that daemon's approve endpoint also expects.
	fake.canApproveErr = &beadsapi.APIError{StatusCode: 404, Message: "not found"}
	if ok, _ := srv.escalationApproveGate(ctx, "gd-esc", "approver@acme.example"); !ok {
		t.Error("404 fallback refused a viewer holding approve:escalation")
	}
	if ok, _ := srv.escalationApproveGate(ctx, "gd-esc", "stranger@acme.example"); ok {
		t.Error("404 fallback admitted a viewer without approve:escalation")
	}
}

func TestBeadShow_EscalationHidesChipsWhenDaemonSaysNo(t *testing.T) {
	fake := escActionFake("open", `["approve:escalation"]`)
	fake.canApproveResp["gd-esc"] = beadsapi.CanApproveResult{Reason: "held by coordinator:merge-desk"}
	srv := NewServer(fake, slog.Default(), "", "http://test", "", nil)
	req := httptest.NewRequest(http.MethodGet, "/gd-esc", nil)
	req.Header.Set("X-Auth-Request-Email", "approver@acme.example")
	rec := httptest.NewRecorder()
	edgeAuthMux(srv).ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "escalation-approve-chip") {
		t.Error("approve chip rendered although the daemon said no")
	}
	if !strings.Contains(body, "held by coordinator:merge-desk") {
		t.Errorf("blocked reason not shown: %s", body)
	}
	if !strings.Contains(body, "site-deployment!1274") {
		t.Error("MR target hidden from a viewer who cannot approve")
	}
}
```
`TestBeadShow_EscalationRendersRichCard` stays, unchanged, as the positive control.

`TestEscalationApprove_DuplicateUserBeadsFailClosed` (renamed from #726's `…DuplicateUserBeadsUnion` on main) still guards the duplicate-user-bead rule, which now only runs on the fallback path. Its fake has no `canApproveResp`, so without a change the daemon's zero answer would refuse it and the test would pass for the wrong reason. Add `fake.canApproveErr = &beadsapi.APIError{StatusCode: 404}` before its `escPost` so that the fallback is what refuses it.

Add `"slices"` to the file's imports.

Create `gardenio/controller/cmd/beads-viewer/escalation_card_routing_test.go`:
```go
package main

import (
	"slices"
	"testing"
	"time"
)

func TestBuildEscalationView_RoutingPanel(t *testing.T) {
	b := escBead2("open")
	b.Fields["held_by"] = "coordinator:merge-desk"
	b.Fields["approvers"] = `["coordinator:merge-desk","coordinator:laptop","operator"]`
	b.Fields["execute_by"] = "coordinator:merge-desk"
	b.Fields["policy_rule"] = "escalation#1"
	b.Fields["claimed_by"] = "role:reviewer"
	v := buildEscalationView(b, nil, "", time.Now())

	if v.HeldBy != "coordinator:merge-desk" || v.HeldByLabel != "coordinator merge-desk" {
		t.Errorf("held = %q / %q", v.HeldBy, v.HeldByLabel)
	}
	if want := []string{"coordinator merge-desk", "coordinator laptop", "the owner"}; !slices.Equal(v.Approvers, want) {
		t.Errorf("approvers = %v, want %v", v.Approvers, want)
	}
	if v.ExecuteByLabel != "coordinator merge-desk" || v.RoutingRule != "escalation#1" || v.ClaimedBy != "role:reviewer" {
		t.Errorf("routing = %+v", v)
	}
	// The spawn block's PolicyRule is the spawn_policy evaluator's, not routing's.
	if v.PolicyRule != "" {
		t.Errorf("routing policy_rule leaked into the spawn block: %q", v.PolicyRule)
	}
	for _, row := range v.Fields {
		switch row.Key {
		case "held_by", "approvers", "execute_by", "claimed_by", "policy_rule":
			t.Errorf("routing field %q double-rendered in the tail", row.Key)
		}
	}
}

// Positive control: an unstamped escalation has no routing panel rows (Global
// Constraint 7); #756's audience chip in the head is what it shows.
func TestBuildEscalationView_UnstampedHasNoLane(t *testing.T) {
	v := buildEscalationView(escBead2("open"), nil, "", time.Now())
	if v.HeldBy != "" || v.HeldByLabel != "" || v.Approvers != nil || v.ExecuteByLabel != "" {
		t.Errorf("unstamped routing = %+v", v)
	}
	if v.Audience == "" {
		t.Error("#756's audience chip lost on an unstamped escalation")
	}
}
```

- [ ] **Step 3: Run them; they should FAIL**

Run: `cd gardenio/controller && go test ./cmd/beads-viewer -run 'TestEscalationApprove|TestEscalationReject|TestEscalationApproveGate_Reasons|TestBeadShow_Escalation|TestBuildEscalationView_' -count=1`
Expected: FAIL to compile, with `srv.escalationApproveGate undefined` and `v.HeldBy undefined`. The fake also stops satisfying `beadAPI` once Step 4 adds the method.

- [ ] **Step 4: Implement the gate and use it at all three call sites**

In `server.go`, add to `beadAPI` after `RejectEscalation`:
```go
	// CanApproveEscalation is the daemon's dry run of its approve checks for the
	// caller (POST /v1/escalations/{id}/can-approve). It alone decides whether
	// Approve/Reject render (escalationApproveGate).
	CanApproveEscalation(ctx context.Context, escalationID string) (beadsapi.CanApproveResult, error)
```
In `escalation_actions.go`, below `viewerCanApproveEscalation`:
```go
// escalationApproveGate decides whether the viewer is offered Approve/Reject on
// one escalation, and why not. It asks the daemon (can-approve: lanes,
// operator_only, self-approval), so the buttons and the POST agree with what
// the daemon's approve will accept; the viewer keeps no list of approvers.
//
// Fails closed: a daemon error hides the buttons with a reason. The one
// exception is a 404 from a daemon rolled before can-approve existed: then the
// legacy approve:escalation capability check applies, which is what that
// daemon's approve endpoint expects. Remove the fallback once every daemon
// serves can-approve (escalation routing Task 14).
func (s *Server) escalationApproveGate(ctx context.Context, id, email string) (bool, string) {
	if strings.TrimSpace(email) == "" {
		return false, "Sign in to approve or reject this escalation."
	}
	res, err := s.daemon.CanApproveEscalation(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return s.viewerCanApproveEscalation(ctx, email)
		}
		s.logger.Warn("can-approve check failed; hiding escalation actions", "id", id, "error", err)
		return false, "Couldn't check your approval rights with the daemon."
	}
	if !res.Allowed {
		if reason := strings.TrimSpace(res.Reason); reason != "" {
			return false, "You can't approve this escalation: " + reason
		}
		return false, "You are not an approver for this escalation."
	}
	return true, ""
}
```
In `escalationAction`, delete the `viewerCanApproveEscalation` precheck (four lines). Put it back **after** the `bead.Status` check, so a missing, wrong-type or closed bead still answers 404, 400 or 409:
```go
	if ok, why := s.escalationApproveGate(r.Context(), id, email); !ok {
		s.renderEscalationError(w, r, id, email, why, http.StatusForbidden)
		return
	}
```
In `renderEscalationPartial`, before:
```go
		ev.CanApprove, ev.ApproveBlockedReason = s.viewerCanApproveEscalation(r.Context(), email)
		ev.CanApprove = ev.CanApprove && ev.Open
```
After:
```go
		if ev.Open {
			ev.CanApprove, ev.ApproveBlockedReason = s.escalationApproveGate(r.Context(), ev.BeadID, email)
		}
```
The `ev.CanRoute = …` line below it (#756) is unchanged.

In `server.go`, before:
```go
	if ev, _ := data["Escalation"].(*escalationView); ev != nil {
		// Gate the approve/reject chips on the viewer's OWN approve:escalation
		// capability (the same one the bridge SPA enforces) — never on mere
		// authentication. Needs the request context, so it happens here rather
		// than in injectTypeView.
		can, why := s.viewerCanApproveEscalation(r.Context(), email)
		ev.CanApprove = can && ev.Open
		ev.ApproveBlockedReason = why
		ev.CanRoute = viewerMayRouteEscalation(r) && ev.Open && !ev.AudienceForced
	}
```
After:
```go
	if ev, _ := data["Escalation"].(*escalationView); ev != nil {
		// Approve/Reject render only when the daemon's can-approve says this
		// viewer may approve THIS escalation (D9). Needs the request context
		// (the viewer's bearer), so it happens here rather than in injectTypeView.
		if ev.Open {
			ev.CanApprove, ev.ApproveBlockedReason = s.escalationApproveGate(r.Context(), ev.BeadID, email)
		}
		ev.CanRoute = viewerMayRouteEscalation(r) && ev.Open && !ev.AudienceForced
	}
```
Update the doc comment on `escalationView.CanApprove` to say "the daemon's can-approve allowed it and the escalation is open". `viewerCanApproveEscalation` stays for the 404 fallback, for the /garden Mailbox's button gate (`main.go`), and for the Commons adapter until Task 12.

- [ ] **Step 5: Add the routing panel to the card**

In `escalation_card.go`, add to `escalationPrimaryFields`:
```go
	// Routing (escalation routing D1): rendered by the card's routing panel.
	"held_by":        true,
	"execute_by":     true,
	"approvers":      true,
	"operator_only":  true,
	"approved_as":    true,
	"claimed_by":     true,
	"claimed_at":     true,
	"handed_up_by":   true,
	"handed_up_at":   true,
	"hand_up_reason": true,
```
Add to `escalationView` after `RequesterChainRootHTML`:
```go
	// Routing (D1): who may approve, who does the work, who it waits on now.
	// All server-stamped. An unstamped escalation has none of them; the head's
	// #756 audience chip is what it shows (Global Constraint 7).
	HeldBy         string
	HeldByLabel    string
	Approvers      []string // labels, in policy order; nil when unstamped
	OperatorOnly   bool
	ExecuteByLabel string
	RoutingRule    string // policy_rule: "escalation#<i>" or "floor:<reason>"
	ApprovedAs     string // label of the lane an approval was made as
	ClaimedBy      string
	ClaimedAt      string
	HandedUpBy     string
	HandedUpAt     string
	HandUpReason   string
```
In `buildEscalationView`, after `v.Open = …`:
```go
	v.HeldBy = beadsapi.EscalationHeldBy(bead)
	v.HeldByLabel = beadsapi.LaneLabel(v.HeldBy)
	var approvers []string
	if raw := strings.TrimSpace(f["approvers"]); raw != "" && json.Unmarshal([]byte(raw), &approvers) == nil {
		for _, a := range approvers {
			v.Approvers = append(v.Approvers, beadsapi.LaneLabel(a))
		}
	}
	v.OperatorOnly = strings.TrimSpace(f["operator_only"]) == "true"
	if ex := beadsapi.EscalationExecuteBy(bead); ex != "" {
		v.ExecuteByLabel = beadsapi.LaneLabel(ex)
	}
	if as := strings.TrimSpace(f["approved_as"]); as != "" {
		v.ApprovedAs = beadsapi.LaneLabel(as)
	}
	v.ClaimedBy, v.ClaimedAt = strings.TrimSpace(f["claimed_by"]), strings.TrimSpace(f["claimed_at"])
	v.HandedUpBy, v.HandedUpAt = strings.TrimSpace(f["handed_up_by"]), strings.TrimSpace(f["handed_up_at"])
	v.HandUpReason = strings.TrimSpace(f["hand_up_reason"])
```
In the `if v.IsSpawn {` block, `v.PolicyRule = strings.TrimSpace(f["policy_rule"])` becomes a read of routing's rule, and only when it is not one:
```go
	rule := strings.TrimSpace(f["policy_rule"])
	if strings.HasPrefix(rule, "escalation#") || strings.HasPrefix(rule, "floor:") {
		v.RoutingRule = rule
		rule = ""
	}
```
Place that above the `if v.IsSpawn {` block, and inside it write `v.PolicyRule = rule`. Add `"encoding/json"` to the imports if the file lacks it.

In `type_escalation.html`, before `{{if .Error}}…{{end}}` (so the panel sits right under #756's audience chip in the head):
```html
  {{if .HeldByLabel}}
  <dl class="field-list field-list-rich escalation-routing" aria-label="Routing">
    <dt title="held_by">Waiting on</dt>
    <dd><span class="chip chip-type escalation-held-by">{{.HeldByLabel}}</span>{{if .ClaimedBy}} <span class="meta">claimed by {{.ClaimedBy}}{{if .ClaimedAt}} &middot; {{.ClaimedAt}}{{end}}</span>{{end}}</dd>
    {{if .Approvers}}<dt title="approvers">May approve</dt><dd>{{range $i, $a := .Approvers}}{{if $i}}, {{end}}{{$a}}{{end}}{{if .OperatorOnly}} <span class="chip escalation-operator-only" title="operator_only">operator only</span>{{end}}</dd>{{end}}
    {{if .ExecuteByLabel}}<dt title="execute_by">Does the work</dt><dd>{{.ExecuteByLabel}}</dd>{{end}}
    {{if .HandedUpBy}}<dt title="handed_up_by">Handed up</dt><dd>by {{.HandedUpBy}}{{if .HandedUpAt}} &middot; {{.HandedUpAt}}{{end}}{{if .HandUpReason}}<div class="escalation-reason">{{.HandUpReason}}</div>{{end}}</dd>{{end}}
    {{if .RoutingRule}}<dt title="policy_rule">Rule</dt><dd><code>{{.RoutingRule}}</code></dd>{{end}}
  </dl>
  {{end}}
```
In the approval trail, after `{{if .ApprovalMethod}} <span class="escalation-method">({{.ApprovalMethod}})</span>{{end}}`, add `{{if .ApprovedAs}} as {{.ApprovedAs}}{{end}}`.

- [ ] **Step 6: Run the tests; they should PASS**

Run: the Step 3 command again, then `cd gardenio/controller && go test ./cmd/beads-viewer -count=1`.
Expected: PASS.

- [ ] **Step 7: Vet and format**

Run: `cd gardenio/controller && gofumpt -l ./cmd/beads-viewer && go vet ./cmd/beads-viewer && staticcheck ./cmd/beads-viewer`
Expected: no output.

- [ ] **Step 8: Commit and PR (merge lane: desk)**

Branch `agent/escalation-routing/<task-8-bead-id>`. Commit: `viewer: escalation card shows routing; Approve/Reject ask the daemon's can-approve (escalation-routing T8)`.

The PR body must say three things:
- the `approve:escalation` capability is now a fallback only, used against a daemon from before Task 3;
- the merge order does not matter, thanks to that fallback;
- this PR does not change who may approve, so it is not an operator-merge PR.

---

## Phase 4: Notifications and execution (Slack bridge)

### Task 9: Bridge: DMs, hand-up, pingbacks and executors follow `held_by` / `execute_by`

**Step bead:** (minted later) Phase 4 Notifications. Blocked by Task 1 (fields), Task 5 (`hand-up` stamps `handed_up_*`, the audience and a pingback) and Task 7 (beadsapi lane helpers). Blocks Tasks 12 (which reuses `conversation.EscalationAddressees`) and 14.

**Who does what:** CREW writes the code and opens the PR. Merge lane: desk.

**What #756 already did, and this task does not redo.** The operator DM (`EscalationCreated`) and the cross-operator notifier (`cross_operator_notify.go`) already return early unless the escalation is owner audience (`beadsapi.EscalationForOwner`). Since Task 2 stamps `audience` from `held_by`, and Tasks 4 and 5 restamp it on approve and hand-up, a coordinator-held or coordinator-approved escalation already gets no DM and no cross-operator notice. #726's cross-operator step (and its contract note 13) is dropped. What is left:
- within the owner audience, `held_by` picks who: the operator lane DMs `operated_by` as today; a `land-owner:<land>` lane gets no fleet DM (its owners are reached in the Commons, Task 12);
- a DM on hand-up, which #756 has no event for;
- the pingback ladder and the bridge's pingback posts warm only owner-audience escalations (#756 did not touch `escalation_pingback_loop.go`), and a hand-up restarts the ladder;
- executors follow `execute_by`.

**Files:**
- Create `gardenio/controller/internal/conversation/escalation_addressees.go` and `escalation_addressees_test.go`. `conversation` already imports `beadsapi`, and `bridge` already imports `conversation` (`internal/bridge/agent_cards.go`), so this adds no cycle.
- Modify `gardenio/controller/internal/bridge/escalation_operator_dm.go`:
  - `EscalationCreated` (re-locate by `func (e *EscalationOperatorDM) EscalationCreated(ctx context.Context, bead BeadEvent) {`), from #756's owner-audience gate to the `operator_dm_at` stamp: split it into a recipient check plus `deliver`;
  - `escalationOperatorDMMessage` and `buildEscalationOperatorDM`: add a `Headline`;
  - add a new `EscalationHandedUp`.
- Modify `gardenio/controller/internal/bridge/escalations.go`:
  - `handleUpdated`: the `approvedBy == ""` branch (re-locate by `		// Only unapproved escalations are re-surfaced — an approved one is`);
  - `triggerExecution` (re-locate by `func (e *Escalations) triggerExecution(ctx context.Context, bead BeadEvent) {`);
  - add a new `nudgeRequesterToExecute` and a `nudge` seam on `Escalations` (re-locate by `type Escalations struct {` and `func NewEscalations(`).
- Modify `gardenio/controller/internal/bridge/escalation_pingback_loop.go`:
  - `evaluateBead` (re-locate by `func (l *EscalationPingbackLoop) evaluateBead(ctx context.Context, b *beadsapi.BeadDetail) {`);
  - `pingbackAnchor` (re-locate by `func pingbackAnchor(fields map[string]string, createdAt time.Time) time.Time {`).
- `cross_operator_notify.go` is not touched (#756's owner-audience gate covers coordinator approvals; see above). `TestCrossOperatorNotify_EscalationApprovedByOther` is the regression check, unchanged.
- Tests:
  - `internal/bridge/escalation_operator_dm_test.go`: `escalationDMHarness`, `mergeEscalation`, `escalationEvent`;
  - `internal/bridge/escalations_test.go`: `newMockEscalationBeadClient`; the pattern of `TestEscalations_HandleUpdated_TriggersExecution`;
  - `internal/bridge/escalation_pingback_loop_test.go`: `addOpenEscalation`, `newPingbackLoopForTest`.

**Interfaces:**
- Consumes `beadsapi.HeldByFromFields`, `EscalationHeldBy`, `EscalationExecuteBy`, `LaneOperator`, `LaneRequester`, `LaneServer` (Task 7), #756's `beadsapi.EscalationForOwner` and the bridge's `escalationAudienceFields`, and the fields `held_by`, `execute_by`, `audience`, `handed_up_by`, `handed_up_at` and `hand_up_reason` (Tasks 1, 2 and 5).
- Produces:
  ```go
  // package conversation (Task 12 reuses this; it does not redefine it)
  type EscalationAddressee struct {
      Lane string // "operator" | "land-owner:<land>"
      Land string // set for a land-owner lane
  }
  func EscalationAddressees(heldBy, land string) []EscalationAddressee
  // package bridge
  func (e *EscalationOperatorDM) EscalationHandedUp(ctx context.Context, bead BeadEvent)
  ```
  plus the metric `escalation_operator_dm_skipped_total{lane}`.

- [ ] **Step 1: Write the failing addressee tests**

`gardenio/controller/internal/conversation/escalation_addressees_test.go`:
```go
package conversation

import (
	"slices"
	"testing"
)

func TestEscalationAddressees(t *testing.T) {
	cases := []struct {
		held, land string
		want       []EscalationAddressee
	}{
		// Unstamped: the caller already applied #756's audience gate, so an
		// unstamped owner-audience escalation is the operator's (Global
		// Constraint 7).
		{"", "", []EscalationAddressee{{Lane: "operator"}}},
		{"operator", "gardenio", []EscalationAddressee{{Lane: "operator"}}},
		{"land-owner:acme", "acme", []EscalationAddressee{{Lane: "land-owner:acme", Land: "acme"}}},
		// Legacy entries never hold an escalation (Task 2 validation).
		{"oidc:a@b.example", "", nil},
		// Agents and machines are nobody's personal ping.
		{"coordinator:laptop", "gardenio", nil},
		{"steward:acme", "acme", nil},
		{"requester", "", nil},
		{"server", "", nil},
		// A land-owner lane for another land than the escalation's own is a
		// stale or forged stamp: address nobody rather than the wrong owners.
		{"land-owner:acme", "beta", nil},
	}
	for _, c := range cases {
		if got := EscalationAddressees(c.held, c.land); !slices.Equal(got, c.want) {
			t.Errorf("EscalationAddressees(%q, %q) = %+v, want %+v", c.held, c.land, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run it; it should FAIL**

Run: `cd gardenio/controller && go test ./internal/conversation -run 'TestEscalationAddressees' -count=1`
Expected: FAIL, with `undefined: EscalationAddressees`.

- [ ] **Step 3: Implement `EscalationAddressees`**

`gardenio/controller/internal/conversation/escalation_addressees.go`:
```go
package conversation

import (
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// EscalationAddressee is one set of people an escalation is addressed to: the
// operator (the gardenio owner) or one land's owners.
type EscalationAddressee struct {
	Lane string // "operator" | "land-owner:<land>"
	Land string // set for a land-owner lane
}

// EscalationAddressees says which people an owner-audience escalation's
// held_by addresses (escalation routing D9): who, once #756's audience has
// said "the owner's". Used for DMs (bridge) and Commons placement (Lands).
// Callers check beadsapi.EscalationForOwner first; this does not. A
// coordinator, steward, requester or server lane addresses nobody: it is an
// agent's queue (and is never owner audience once stamped, D11).
//
// land is the escalation's own project. A land-owner lane naming any other
// land addresses nobody and is logged: it is stale or forged, and the wrong
// land's owners must never hear about it. The tenant/operator kind of a land
// (land_kind) is not consulted here; it is already decided by whoever stamped
// held_by.
func EscalationAddressees(heldBy, land string) []EscalationAddressee {
	held := strings.TrimSpace(heldBy)
	if held == "" {
		held = beadsapi.LaneOperator
	}
	switch kind, name, _ := strings.Cut(held, ":"); {
	case held == beadsapi.LaneOperator:
		return []EscalationAddressee{{Lane: beadsapi.LaneOperator}}
	case kind == "land-owner":
		if name == "" || name != strings.TrimSpace(land) {
			slog.Warn("escalation held_by names another land's owners; addressing nobody",
				"held_by", held, "land", land)
			return nil
		}
		return []EscalationAddressee{{Lane: held, Land: name}}
	}
	return nil
}
```
Run the Step 2 command again. Expected: PASS.

- [ ] **Step 4: Write the failing bridge tests**

Append to `internal/bridge/escalation_operator_dm_test.go`:
```go
// TestEscalationOperatorDM_FollowsHeldBy: #756's audience gate decides
// whether; held_by decides who (D9). Fixtures carry the audience Task 2
// stamps for each held_by (D11). The operator-held and unstamped-owner cases
// are the positive controls.
func TestEscalationOperatorDM_FollowsHeldBy(t *testing.T) {
	for _, c := range []struct {
		held, audience string
		dms            int
	}{
		{"", "owner", 1},
		{"", "", 0}, // unstamped merge: #756's default audience is desk
		{"operator", "owner", 1},
		{"land-owner:acme", "owner", 0}, // its owners are reached in the Commons (Task 12)
		{"coordinator:laptop", "operator", 0},
		{"coordinator:merge-desk", "desk", 0},
		{"steward:acme", "desk", 0},
		{"requester", "operator", 0},
	} {
		detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
		if c.held != "" {
			detail.Fields["held_by"] = c.held
		}
		if c.audience != "" {
			detail.Fields["audience"] = c.audience
		}
		e, sender, _ := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
		e.EscalationCreated(context.Background(), escalationEvent(detail))
		if n := len(sender.all()); n != c.dms {
			t.Errorf("held_by=%q audience=%q: %d DMs, want %d", c.held, c.audience, n, c.dms)
		}
	}
}

// TestEscalationOperatorDM_HandUpDMsOnce: a coordinator hands an escalation up;
// the operator gets one DM saying who handed it up and why, and a redelivered
// event does not re-send it.
func TestEscalationOperatorDM_HandUpDMsOnce(t *testing.T) {
	detail := mergeEscalation("oidc:alex.novak@contractor-b.example", i64(370))
	detail.Fields["held_by"] = "coordinator:laptop"
	detail.Fields["audience"] = "operator"
	e, sender, daemon := escalationDMHarness(t, map[string]*beadsapi.BeadDetail{detail.ID: detail})
	e.EscalationCreated(context.Background(), escalationEvent(detail))
	if n := len(sender.all()); n != 0 {
		t.Fatalf("coordinator-held create sent %d DMs", n)
	}

	// The daemon's hand-up (Task 5) moves held_by, its audience (D11) and
	// stamps the hand-up fields.
	daemon.beads[detail.ID].Fields["held_by"] = "operator"
	daemon.beads[detail.ID].Fields["audience"] = "owner"
	daemon.beads[detail.ID].Fields["handed_up_by"] = "machine:gardenio-laptop-operator"
	daemon.beads[detail.ID].Fields["handed_up_at"] = "2026-10-20T05:00:00Z"
	daemon.beads[detail.ID].Fields["hand_up_reason"] = "needs the owner's GitHub session"
	ev := escalationEvent(daemon.beads[detail.ID])

	e.EscalationHandedUp(context.Background(), ev)
	dms := sender.all()
	if len(dms) != 1 || dms[0].UserID != "U0PERSON003" {
		t.Fatalf("hand-up DMs = %+v, want one to U0PERSON003", dms)
	}
	for _, want := range []string{"handed this escalation up to you", "needs the owner's GitHub session", "gd-B07MJ9scWG"} {
		if !strings.Contains(dms[0].Text, want) {
			t.Errorf("hand-up DM missing %q:\n%s", want, dms[0].Text)
		}
	}
	e.EscalationHandedUp(context.Background(), ev)
	if n := len(sender.all()); n != 1 {
		t.Errorf("redelivered hand-up sent %d DMs, want 1", n)
	}
}
```
Append to `internal/bridge/escalation_pingback_loop_test.go`:
```go
// In-hand escalations are a coordinator's queue: the ladder warms only the
// owner's (#756 audience). The owner-audience escalation in the same sweep is
// the positive control.
func TestPingbackSweep_SkipsInHand(t *testing.T) {
	now := time.Now()
	client := newMockEscalationBeadClient()
	addOpenEscalation(client, "gd-hb1", 30*time.Hour, now, map[string]string{"held_by": "coordinator:laptop", "audience": "operator"})
	addOpenEscalation(client, "gd-hb2", 30*time.Hour, now, map[string]string{"audience": "owner"})

	newPingbackLoopForTest(client, now).sweepOnce(context.Background())

	if _, ok := client.updatedFields["gd-hb1"]; ok {
		t.Error("coordinator-held escalation was pinged")
	}
	if _, ok := client.updatedFields["gd-hb2"]; !ok {
		t.Error("owner-audience escalation was not pinged")
	}
}

// After a hand-up the ladder restarts from handed_up_at, so the operator gets
// the hand-up's own pingback, not an immediate catch-up burst for the hours a
// coordinator held it.
func TestPingbackAnchor_PrefersLaterHandUp(t *testing.T) {
	created := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	f := map[string]string{"notified_at": "2026-10-19T00:00:00Z", "handed_up_at": "2026-10-20T05:00:00Z"}
	if got := pingbackAnchor(f, created); !got.Equal(time.Date(2026, 10, 20, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("anchor = %v, want handed_up_at", got)
	}
}
```
Append to `internal/bridge/escalations_test.go`:
```go
func approvedEvent(id string, fields map[string]string) []byte {
	f := map[string]string{
		"operation_type":   "github_pr_merge",
		"operation_spec":   `{"repo":"mattbakerconsulting/gardenio","pr":700}`,
		"approved_by":      "machine:gardenio-laptop-operator",
		"execution_target": "local",
		"requesting_agent": "gd-agent-xyz",
	}
	for k, v := range fields {
		f[k] = v
	}
	data, _ := json.Marshal(map[string]any{"bead": BeadEvent{ID: id, Type: "escalation", Labels: []string{"project:gardenio"}, Fields: f}})
	return data
}

// TestEscalations_ExecuteByCoordinatorSpawnsNoExecutor: the coordinator that
// holds execute_by does the work itself (it now holds the escalation), so the
// bridge must not spawn an "escalation-<id>" executor. TestEscalations_HandleUpdated_
// TriggersExecution is the positive control: execute_by empty → legacy spawn.
func TestEscalations_ExecuteByCoordinatorSpawnsNoExecutor(t *testing.T) {
	for _, lane := range []string{"coordinator:merge-desk", "coordinator:laptop", "server"} {
		client := newMockEscalationBeadClient()
		esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})
		esc.handleUpdated(context.Background(), approvedEvent("gd-x1", map[string]string{"execute_by": lane}))
		client.mu.Lock()
		if len(client.spawnedAgents) != 0 || client.updatedFields["gd-x1"]["executor"] != "" {
			t.Errorf("execute_by=%s spawned %v / executor %q", lane, client.spawnedAgents, client.updatedFields["gd-x1"]["executor"])
		}
		client.mu.Unlock()
	}
}

// execute_by=requester: the requesting agent is told to proceed; no executor.
func TestEscalations_ExecuteByRequesterNudgesRequester(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-xyz"] = &beadsapi.BeadDetail{ID: "gd-agent-xyz", Type: "agent", Fields: map[string]string{"agent": "crew-asker"}}
	esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})
	var nudged []string
	esc.nudge = func(_ context.Context, agent, msg string) error {
		nudged = append(nudged, agent+": "+msg)
		return nil
	}
	esc.handleUpdated(context.Background(), approvedEvent("gd-x2", map[string]string{"execute_by": "requester"}))

	if len(nudged) != 1 || !strings.HasPrefix(nudged[0], "crew-asker: ") || !strings.Contains(nudged[0], "gb escalation close gd-x2") {
		t.Fatalf("nudges = %v", nudged)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, n := range client.spawnedAgents {
		if strings.HasPrefix(n, "escalation-") {
			t.Errorf("requester-executed escalation spawned executor %q", n)
		}
	}
}

// An in-hand, unapproved escalation's pingback request does not post to
// Slack: hand-up (which makes it the owner's) is its way to a human.
func TestEscalations_PingbackSkipsInHand(t *testing.T) {
	client := newMockEscalationBeadClient()
	notifier := &mockEscalationNotifier{} // escalation_pingback_loop_test.go
	esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default(), EscalationNotifier: notifier})
	for _, c := range []struct {
		id, held, audience string
		posts              int
	}{{"gd-p1", "coordinator:laptop", "operator", 0}, {"gd-p2", "operator", "owner", 1}} {
		data, _ := json.Marshal(map[string]any{"bead": BeadEvent{ID: c.id, Type: "escalation", Status: "open", Fields: map[string]string{
			"held_by": c.held, "audience": c.audience, "pingback_requested_at": "2026-10-20T05:00:00Z",
		}}})
		notifier.mu.Lock()
		before := len(notifier.pingbacks)
		notifier.mu.Unlock()
		esc.handleUpdated(context.Background(), data)
		notifier.mu.Lock()
		got := len(notifier.pingbacks) - before
		notifier.mu.Unlock()
		if got != c.posts {
			t.Errorf("held_by=%s: %d pingback posts, want %d", c.held, got, c.posts)
		}
	}
}
```
No cross-operator test is added: a coordinator's approval moves `held_by` to `execute_by` and `audience` with it (Task 4), so #756's owner-audience gate already keeps it silent, and #756's own `cross_operator_notify_test.go` cases cover that gate.

- [ ] **Step 5: Run them; they should FAIL**

Run: `cd gardenio/controller && go test ./internal/bridge -run 'TestEscalationOperatorDM_|TestPingbackSweep_|TestPingbackAnchor_PrefersLaterHandUp|TestEscalations_|TestCrossOperatorNotify_' -count=1` (the cross-operator tests are #756's, run as the regression check)
Expected: FAIL to compile, with `e.EscalationHandedUp undefined` and `esc.nudge undefined`.

- [ ] **Step 6: DMs follow `held_by`, plus the hand-up DM**

In `escalation_operator_dm.go`, add next to `escalationOperatorDMUnresolvedTotal`:
```go
// escalationOperatorDMSkippedTotal counts owner-audience escalations that got
// no fleet DM because held_by addresses a land's owners (reached through the
// Commons, Lands) or, for a stale stamp, nobody. In-hand escalations never
// get here: #756's audience gate returns first.
var escalationOperatorDMSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "escalation_operator_dm_skipped_total",
	Help: "Owner-audience escalation DMs not sent because held_by addresses no fleet operator, by lane kind.",
}, []string{"lane"})

// escalationDMRecipient is the operated_by identity to DM for an owner-audience
// escalation (the caller has applied #756's gate), from held_by via
// conversation.EscalationAddressees; ok=false means held_by addresses no fleet
// operator. The operator lane, and an unstamped escalation, DM operated_by
// exactly as #756 does (Global Constraint 7).
func escalationDMRecipient(detail *beadsapi.BeadDetail) (string, bool) {
	held := beadsapi.EscalationHeldBy(detail)
	for _, a := range conversation.EscalationAddressees(held, projectFromLabels(detail.Labels)) {
		if a.Lane == beadsapi.LaneOperator {
			return strings.TrimSpace(detail.OperatedBy), true
		}
	}
	kind, _, _ := strings.Cut(held, ":")
	escalationOperatorDMSkippedTotal.WithLabelValues(kind).Inc()
	return "", false
}
```
In `EscalationCreated`, #756's owner-audience gate (`if !beadsapi.EscalationForOwner(escalationAudienceFields(detail.Fields, bead.Fields)) {`) stays where it is. Below it, before:
```go
	operatedBy := strings.TrimSpace(detail.OperatedBy)
	if operatedBy == "" || !IsHumanOperator(operatedBy) {
```
After:
```go
	// #756's gate (above) said the owner's; held_by says which owner (D9). A
	// land's owners are reached in the Commons, not by a fleet DM.
	operatedBy, ok := escalationDMRecipient(detail)
	if !ok {
		e.logger.Info("escalation operator DM: held by a land's owners, not sent",
			"bead", bead.ID, "held_by", beadsapi.EscalationHeldBy(detail))
		return
	}
	if operatedBy == "" || !IsHumanOperator(operatedBy) {
```
Move everything from `op := e.index.Resolve(...)` to the end of `EscalationCreated` into a new method:
```go
// deliver resolves operatedBy's Slack id and sends one DM, deduped by seenKey,
// then stamps operator_dm_at. checkSelf skips the DM when the creator is that
// same human (a create, not a hand-up). headline "" is the create wording.
func (e *EscalationOperatorDM) deliver(ctx context.Context, bead BeadEvent, detail *beadsapi.BeadDetail, operatedBy, seenKey, headline string, checkSelf bool)
```
The moved code changes in three ways:
- `e.seen.markSeen(bead.ID)` and `e.seen.release(bead.ID)` take `seenKey`;
- the creator/`sameOperator` check runs only when `checkSelf`;
- the `buildEscalationOperatorDM` call gets `Headline: headline`.

`EscalationCreated` then ends with `e.deliver(ctx, bead, detail, operatedBy, bead.ID, "", true)`. That keeps every existing test's behaviour, since the dedup key is still `bead.ID`.

Add `Headline string` to `escalationOperatorDMMessage`. In `buildEscalationOperatorDM`, before:
```go
	fmt.Fprintf(&b, ":rotating_light: %s — a new escalation is addressed to you.\n", m.Mention)
```
After:
```go
	headline := m.Headline
	if headline == "" {
		headline = "a new escalation is addressed to you."
	}
	fmt.Fprintf(&b, ":rotating_light: %s — %s\n", m.Mention, headline)
```
Add the hand-up entry point below `EscalationCreated`:
```go
// EscalationHandedUp is called from Escalations.handleUpdated when an
// unapproved escalation carries handed_up_at (escalation routing D5: a
// coordinator or steward gave it to a human). One DM per hand-up: the seen key
// carries handed_up_at, and operator_dm_at at or after it means this hand-up
// was already delivered (restart-safe). Safe on a nil receiver.
func (e *EscalationOperatorDM) EscalationHandedUp(ctx context.Context, bead BeadEvent) {
	if e == nil || bead.ID == "" {
		return
	}
	at := strings.TrimSpace(bead.Fields["handed_up_at"])
	if at == "" || strings.TrimSpace(bead.Fields["approved_by"]) != "" {
		return
	}
	detail, err := e.daemon.GetBead(ctx, bead.ID)
	if err != nil {
		e.logger.Warn("escalation hand-up DM: cannot read bead", "bead", bead.ID, "error", err)
		return
	}
	if strings.TrimSpace(detail.Fields["handed_up_at"]) != at {
		return // a newer hand-up has its own event
	}
	if dm := strings.TrimSpace(detail.Fields["operator_dm_at"]); dm != "" && dm >= at {
		return // RFC3339 UTC strings order as times
	}
	// The hand-up restamped audience from the new held_by (D11); the same
	// #756 gate as a create.
	if !beadsapi.EscalationForOwner(escalationAudienceFields(detail.Fields, bead.Fields)) {
		return
	}
	operatedBy, ok := escalationDMRecipient(detail)
	if !ok || operatedBy == "" || !IsHumanOperator(operatedBy) {
		return
	}
	by := strings.TrimSpace(detail.Fields["handed_up_by"])
	if by == "" {
		by = "a coordinator"
	}
	headline := escapeSlackMrkdwn(truncateRunes(by, 80)) + " handed this escalation up to you"
	if reason := strings.TrimSpace(detail.Fields["hand_up_reason"]); reason != "" {
		headline += ": " + escapeSlackMrkdwn(truncateRunes(reason, 300))
	} else {
		headline += "."
	}
	e.deliver(ctx, bead, detail, operatedBy, bead.ID+"#hu:"+at, headline, false)
}
```
Add the import `gardenio/controller/internal/conversation` (the file already imports `beadsapi` since #756). `projectFromLabels` is the bridge's own (`internal/bridge/bot_commands.go`, re-locate by `func projectFromLabels(`; six packages define one, the bridge's is the one in that file).

The hand-up stamps `operator_dm_at` again. The schema already declares that key (see the comment above the stamp), so no schema change is needed.

- [ ] **Step 7: Pingbacks and queue alerts warm only the owner's**

In `escalations.go` `handleUpdated`, before:
```go
	approvedBy := bead.Fields["approved_by"]
	if approvedBy == "" {
		// Only unapproved escalations are re-surfaced — an approved one is
		// already moving through execution.
		e.maybeNotifyPingback(ctx, *bead)
		e.maybeNotifyQueueAlert(ctx, *bead)
		return
	}
```
After:
```go
	approvedBy := bead.Fields["approved_by"]
	if approvedBy == "" {
		// A hand-up (D5) is a personal ask of the human who now holds it.
		e.operatorDM.EscalationHandedUp(ctx, *bead)
		// Only unapproved owner-audience escalations are re-surfaced (#756
		// audience, which follows held_by, D11): an in-hand one is a
		// coordinator's queue, and its way to a human is hand-up, which makes
		// it the owner's and stamps its own pingback.
		if beadsapi.EscalationForOwner(bead.Fields) {
			e.maybeNotifyPingback(ctx, *bead)
			e.maybeNotifyQueueAlert(ctx, *bead)
		}
		return
	}
```
In `escalation_pingback_loop.go` `evaluateBead`, after the `approved_by` early return:
```go
	// Only an owner-audience escalation is warmed (#756 audience, which follows
	// held_by, D11). An in-hand one is a coordinator's queue; it reaches the
	// owner by hand-up (Task 5) or the idle hand-up.
	if !beadsapi.EscalationForOwner(fields) {
		return
	}
```
`pingbackAnchor` before:
```go
func pingbackAnchor(fields map[string]string, createdAt time.Time) time.Time {
	if v := fields["notified_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return createdAt
}
```
After:
```go
func pingbackAnchor(fields map[string]string, createdAt time.Time) time.Time {
	anchor := createdAt
	if v := fields["notified_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			anchor = t
		}
	}
	// A hand-up restarts the clock: the hours a coordinator held it are not
	// hours the human ignored it.
	if t, err := time.Parse(time.RFC3339, fields["handed_up_at"]); err == nil && t.After(anchor) {
		anchor = t
	}
	return anchor
}
```
This is a behaviour change for in-hand escalations, which the ladder warmed before (#756 did not touch it). Today's `TestPingbackSweep_*` fixtures carry no `audience`; if any is an in-hand operation type by #756's default (a merge is `desk`), give it `"audience": "owner"` so it still tests the ladder, and say so in the PR.

- [ ] **Step 8: Executors follow `execute_by`**

Add a seam to `Escalations` after `httpClient`:
```go
	// nudge delivers a nudge to a named agent; NudgeAgent in production, a fake
	// in tests.
	nudge func(ctx context.Context, agentName, message string) error
```
In `NewEscalations`, after building the struct as `e := &Escalations{…}`:
```go
	e.nudge = func(ctx context.Context, agentName, message string) error {
		return NudgeAgent(ctx, e.daemon, e.httpClient, e.logger, agentName, message)
	}
	return e
```
`triggerExecution` before:
```go
func (e *Escalations) triggerExecution(ctx context.Context, bead BeadEvent) {
	target := bead.Fields["execution_target"]
	switch target {
```
After:
```go
// triggerExecution dispatches the approved escalation by its execute_by lane
// (escalation routing D1). Empty is the legacy execution_target path,
// unchanged (Global Constraint 7).
func (e *Escalations) triggerExecution(ctx context.Context, bead BeadEvent) {
	switch exec := strings.TrimSpace(bead.Fields["execute_by"]); {
	case exec == "":
	case strings.HasPrefix(exec, "coordinator:"), strings.HasPrefix(exec, "steward:"), exec == beadsapi.LaneOperator:
		// The lane holds the escalation now (held_by = execute_by on approve)
		// and does the work itself; it records the result with gb.
		e.logger.Info("escalation approved; its execute_by lane does the work, no executor spawned",
			"id", bead.ID, "execute_by", exec)
		return
	case exec == beadsapi.LaneRequester:
		e.nudgeRequesterToExecute(ctx, bead)
		return
	case exec == beadsapi.LaneServer:
		// The daemon's own replay already ran (bead create/update/close/grant).
		return
	default:
		// Fail closed (Global Constraint 4): an unknown lane spawns nothing.
		e.logger.Warn("escalation approved with an unknown execute_by; nothing executed",
			"id", bead.ID, "execute_by", exec)
		return
	}
	target := bead.Fields["execution_target"]
	switch target {
```
Delete the old one-line doc comment above it.

Add below `nudgeRequestingAgent`:
```go
// nudgeRequesterToExecute tells the requesting agent its escalation is approved
// and it should do the work itself (execute_by=requester, e.g. a land's merge
// approved by its owner). If the agent cannot be nudged (its pod is gone), the
// usual follow-through respawn carries the same message.
func (e *Escalations) nudgeRequesterToExecute(ctx context.Context, bead BeadEvent) {
	agentBeadID := strings.TrimSpace(bead.Fields["requesting_agent"])
	if agentBeadID == "" {
		e.logger.Warn("execute_by=requester but no requesting_agent; nothing executed", "id", bead.ID)
		return
	}
	agentBead, err := e.daemon.GetBead(ctx, agentBeadID)
	if err != nil {
		e.logger.Warn("could not resolve requesting agent bead", "escalation", bead.ID, "agent_bead", agentBeadID, "error", err)
		return
	}
	agentName := agentBead.Fields["agent"]
	if agentName == "" {
		agentName = agentBead.Assignee
	}
	if agentName == "" {
		e.logger.Warn("requesting agent bead has no agent name", "escalation", bead.ID, "agent_bead", agentBeadID)
		return
	}
	message := fmt.Sprintf("Escalation %s (%s) is approved. Do the operation yourself now, then record the outcome with `gb escalation close %s`.",
		bead.ID, bead.Fields["operation_type"], bead.ID)
	if err := e.nudge(ctx, agentName, message); err != nil {
		e.logger.Warn("could not nudge requester to execute; respawning", "escalation", bead.ID, "agent", agentName, "error", err)
		e.respawnRequestingAgent(ctx, agentName, bead, agentBead, message)
		return
	}
	e.logger.Info("nudged requester to execute its approved escalation", "escalation", bead.ID, "agent", agentName)
}
```
Change the `NudgeAgent(…)` call in `nudgeRequestingAgent` to `e.nudge(ctx, agentName, message)`, so one seam serves both nudges.

`gb escalation close <id>` is the real verb (`cmd/gb/escalation_lifecycle.go`, re-locate by `Use:   "close <id>",`).

- [ ] **Step 9: Run the tests; they should PASS**

Run: the Step 5 command again, then `cd gardenio/controller && go test ./internal/bridge ./internal/conversation -count=1`.
Expected: PASS. `TestEscalations_HandleUpdated_TriggersExecution` still spawns `escalation-gd-esc2`, because its fixture has no `execute_by`. `TestEscalationOperatorDM_AddressedHumanGetsExactlyOneDM` still sends exactly one DM, because its fixture has no `held_by` and #756 already made it owner audience. #756's cross-operator tests pass unchanged.

- [ ] **Step 10: Vet and format**

Run: `cd gardenio/controller && gofumpt -l ./internal/bridge ./internal/conversation && go vet ./internal/bridge ./internal/conversation && staticcheck ./internal/bridge ./internal/conversation`
Expected: no output.

- [ ] **Step 11: Commit and PR (merge lane: desk)**

Branch `agent/escalation-routing/<task-9-bead-id>`. Commit: `bridge: escalation DMs, hand-up, pingbacks and executors follow held_by/execute_by (escalation-routing T9)`.

The PR body must:
- state that `conversation.EscalationAddressees` is defined here and that Task 12 reuses it;
- call out the pingback-ladder change (in-hand escalations are no longer warmed);
- record that a coordinator lane with no live member reaches the owner only through the idle hand-up (Task 5, `BEADS_ESCALATION_LANE_IDLE_HANDUP`).

---

## Phase 5: Lands

### Task 10: Lands spec amendment: steward and land-owner approvals

**Phase:** 5 Lands · **Lane:** CREW (docs only) · **Merge:** desk (self-merge rules; docs are not a human path) · **Blocked by:** none

All anchors below were grepped against `399f1ecfe` and appear exactly once in their file.

**Who does what:** a crew edits two spec files and opens one PR. No code changes.

**Files:**
- Modify: `gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md` (B-D11 row; §4.2 steward closing paragraph; §5.1 tier table; the 10.1 row of the step table)
- Modify: `gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md` (§2.4 DMs; §2.5 card placement; §6.4 action table)

**Interfaces:** none (prose). The prose must use the contract names exactly: `held_by`, `land_steward_approves`, `steward_delegate`, `steward:<land>`, `land-owner:<land>`, `POST /v1/escalations/{id}/can-approve`, `POST /v1/lands/{land}/steward-delegation`.

- [ ] **Step 1: Branch.**
  ```bash
  git fetch origin && git switch -c agent/escalation-routing/<task-bead-id> origin/main
  ```

- [ ] **Step 2: Track B, B-D11.** Re-locate the row by its text `| B-D11 | Tenant land conversation never reaches the fleet Slack |`. In its decision cell, replace `and only operator-addressed escalations about a land reach the operator's Slack.` with:
  > and only owner-audience escalations (#756's `audience`) held by `operator` reach the operator's Slack. A tenant land's merge escalations route to `steward:<land>` (when the land delegates, `desk` audience) or `land-owner:<land>` (`owner` audience, that land's owners) by the escalation policy (escalation-routing plan, D7 rule 2 and D11), so they never do.

- [ ] **Step 3: Track B §4.2, steward closing paragraph.** Re-locate it by `It does not merge PRs: the tenant merges on GitHub.` and replace the whole paragraph with:
  > It never merges a PR itself. A `lands-gardener` merges its own PR only after the PR's merge escalation (`github_pr_merge` or `gitlab_mr_merge`) is approved, either by the steward or by a land owner.
  >
  > The steward may approve one only when all of these hold:
  > - the land owner has delegated that operation type to it (`land_steward_approves` on the land's project bead, written through `steward_delegate`);
  > - the escalation is not `operator_only`;
  > - the requester is a different agent bead from the steward.
  >
  > Without delegation the escalation waits on the land owner (`held_by=land-owner:<land>`), who approves or rejects it in Commons. A land owner who merges on GitHub themselves rejects the escalation with the reason `merged on GitHub`.
  >
  > The steward does not talk outside its land and never sees another land. It does not file escalations to the operator; its own needs go through `#support`.

- [ ] **Step 4: Track B §5.1 table.** Re-locate the row `| Run tools inside its own pod | File escalations, approve anything, write grants |` and replace it with these two rows:
  ```markdown
  | Run tools inside its own pod; file a merge escalation (`github_pr_merge`, `gitlab_mr_merge`) for a PR in its own land's repo | File any other escalation; write grants |
  | (steward only) approve a merge escalation in its own land whose operation type the land owner delegated (`land_steward_approves`), requested by another agent bead | Approve anything `operator_only`, anything undelegated, anything in another land, or its own request |
  ```
  Below the table, add one sentence: "The approve check is the daemon's coordinator path (escalation-routing D4/D8); `POST /v1/escalations/{id}/can-approve` answers it dry-run for any surface."

- [ ] **Step 5: Track B step table, row 10.1.** Re-locate `| **10.1** | \`lands-tenant\` tier:`. Replace `escalations from tenant agents refused with a structured denial pointing at \`#support\`.` with:
  > escalations from tenant agents refused with a structured denial pointing at `#support`, **except** a merge escalation (`github_pr_merge`, `gitlab_mr_merge`) about the agent's own land, which the escalation policy routes to the steward or land owner (escalation-routing plan Task 11).

- [ ] **Step 6: Commons §2.4 DMs.** Re-locate the DM-first paragraph by `targets the addressee's DM channel in the Commons and the Slack IM` (the shorter `the addressee's DM channel` appears twice). After the paragraph, add:
  > For an escalation, #756's `audience` says whether it is a person's at all (only `owner` is), and its `held_by` lane says whose, mapped by `conversation.EscalationAddressees`:
  > - `operator` → the operator seat;
  > - `land-owner:<land>` → each owner of that land;
  > - `steward:<land>`, `coordinator:<name>`, `requester` and `server` are agents or the daemon, so no person gets a DM; the card stays in `#escalations`.
  >
  > The DM is never addressed by `operated_by`. When `held_by` changes (approve, hand-up), the new addressee gets the DM card and the old DM card re-renders as "now waiting on …".

- [ ] **Step 7: Commons §2.5.** In the sentence that starts on the line `Decisions and escalations get a second card in the addressee's DM channel when the addressee is` (it wraps onto the next line, `a person (DM-first); …`), append after that sentence: "For escalations, 'the addressee' is defined in §2.4 by `held_by`." In the card-face table, replace the escalation row's face cell `operation, creds summary, approve/reject, result, pingback` with:
  > operation, creds summary, **"Waiting on <lane>"** (from `held_by`), approve/reject only when `can-approve` allows the viewer, result, pingback

- [ ] **Step 8: Commons §6.4 action table.** Replace the two rows found by `` `escalation_answer`, `channel_join` | no | yes | yes | `` and `` `channel_manage`, `member_manage` | no | no | yes | `` with:
  ```markdown
  | `done`, `spawn` (Q8c), `agent_control`, `decision_respond`, `decision_dismiss`, `decision_promote`, `permission_answer`, `escalation_answer`, `channel_join` | no | yes | yes |
  | `channel_manage`, `member_manage`, `steward_delegate` | no | no | yes |
  ```
  Under the table add:
  > `escalation_answer` is the role gate only. The button also needs the daemon's `can-approve` yes for that viewer and escalation, so a member sees Approve only on an escalation whose `approvers` include `land-owner:<land>` and whose tier allows it.
  >
  > `steward_delegate` sets `land_steward_approves` through `POST /v1/lands/{land}/steward-delegation`. It is owner-only, and it can never include an operator-only operation type.

- [ ] **Step 9: Self-check, then open the PR.**
  ```bash
  grep -n -E 'held_by|land_steward_approves|steward_delegate|can-approve|steward-delegation' gardenio/docs/superpowers/specs/2026-10-19-lands-*.md
  ```
  Expected: hits in both files, each name spelled as in the Interface contract. `grep -n 'operated_by' gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md` must show no DM-addressing use. Commit `docs(lands): escalation approvals follow held_by; steward delegation (escalation-routing T10)`, push, then open the PR non-draft. The merge desk takes docs PRs only when the crew cannot self-merge; docs are not a human path, so self-merge on green.

---

### Task 11: Land delegation: `land_steward_approves`, `steward_delegate`, land lanes

**Phase:** 5 Lands · **Lane:** CREW · **Merge:** operator (changes who may approve; Global Constraint 2) · **Blocked by:** Tasks 10 and 4

**Who does what:** a crew writes code and tests and opens a **draft** PR. The merge escalation gets `merge-desk:hold`. The crew files one `type=decision` bead to the operator with the diff summary (template in Step 9). The operator merges.

**Files:**
- Modify: `kbeads/internal/server/config.go`: the `"type:project"` field list gets `land_steward_approves`.
- Create: `kbeads/internal/server/land_steward_delegation.go`: the field guard, validation and endpoint. Reading the field is Task 2's `landDelegatesToSteward`; reuse it.
- Create: `kbeads/internal/server/land_steward_delegation_test.go`.
- Modify: `kbeads/internal/server/action_table.go`: `actionStewardDelegate`, `ownerActions`.
- Modify: `kbeads/internal/server/action_table_test.go`: reviewed rows.
- Modify: `kbeads/internal/server/escalation_lane_members.go` (Task 3's file): real `steward:` and `land-owner:` membership.
- Modify: `kbeads/internal/server/escalation_policy.go` (Task 2's file): `escalationFactsFor` sets `Land` only for `land_kind == landKindTenant`.
- Modify: `kbeads/internal/server/escalation_scope.go`: a `scopedApproverAllowed` carve-out for land-owner merge approvals.
- Modify: `kbeads/internal/server/http.go`: register the route.
- Modify: `gardenio/controller/internal/conversation/landrole.go`, `landrole_test.go`: `ActionStewardDelegate`.
- Modify: `gardenio/controller/internal/beadsapi/projects.go`: `SetStewardDelegation`.
- Modify: `gardenio/controller/internal/bridge/init.go`: mirror the project field (re-locate by `{Name: "land_owner_sub", Type: "string"}`).

**Interfaces (new; add to the contract in this PR):**
```go
// kbeads, package server
const actionStewardDelegate = "steward_delegate"
const landStewardApprovesField = "land_steward_approves"
func validateLandStewardApproves(ops []string) error
func (s *BeadsServer) handleSetStewardDelegation(w http.ResponseWriter, r *http.Request) // POST /v1/lands/{land}/steward-delegation

// controller
const conversation.ActionStewardDelegate conversation.Action = "steward_delegate"
func (c *beadsapi.Client) SetStewardDelegation(ctx context.Context, land string, ops []string) error
```

- [ ] **Step 1: Branch and re-read what Tasks 2–4 landed.**
  ```bash
  git fetch origin && git switch -c agent/escalation-routing/<task-bead-id> origin/main
  grep -n -E 'func \(s \*BeadsServer\) (callerInLane|firstStaffedLane|checkMayApprove|escalationFactsFor)' kbeads/internal/server/*.go
  grep -n -E 'laneSteward|laneLandOwner|"steward"|"land-owner"' kbeads/internal/server/escalation_lane*.go
  ```
  Expected: all four functions exist (Tasks 2–3). Note how Task 3 handled `steward:`/`land-owner:`. If it returned `false, nil` (fail closed) for both, this task replaces that. If it already resolved them, keep its code and add only the delegation check in Step 5.

- [ ] **Step 2: Write the failing tests first.** In `land_steward_delegation_test.go`, on `newScopedFixture(t)` (`scoped_test_server_test.go`):
  1. Seed land `claimlayer`'s project bead with `land_kind=tenant`, using the fixture's project seeding. Re-locate it with `grep -n 'FindProjectByName' http_test.go`; `mockStore.FindProjectByName` is the lookup.
  2. Seed a steward agent bead `cl-steward-1` with fields `role=steward`, `project=claimlayer`, and its per-agent token: `RegisterTokenFull(tok, "gardenio", "claimlayer", nil, nil, time.Time{}, "agent:cl-steward-1", false)` (`fleet.go`, re-locate by `func (fc *FleetConfig) RegisterTokenFull(`), or `agentFleetCtx("cl-steward-1", "steward-1")` (`agent_identity_equivalence_test.go`, re-locate by `func agentFleetCtx(beadID, name string) context.Context {`).
  3. Seed a gardener agent bead `cl-gardener-1` (`role=lands-gardener`, `project=claimlayer`).

  Then write these tests:
  ```go
  // Review Focus 5. Positive control in the same test: 403 before, 200 after.
  func TestStewardApprove_RequiresLandDelegation(t *testing.T) {
      f := newScopedFixture(t)
      f.seedTenantLand("claimlayer")
      esc := f.seedEscalationAs("cl-gardener-1", map[string]any{"operation_type": "github_pr_merge"}, "project:claimlayer")
      if got := f.field(esc, "held_by"); got != "land-owner:claimlayer" {
          t.Fatalf("held_by = %q before delegation, want land-owner:claimlayer", got)
      }
      if got := f.field(esc, "audience"); got != "owner" {
          t.Fatalf("audience = %q before delegation, want owner (D11: the land's owner)", got)
      }
      if code := f.approveAs(f.stewardTok, esc); code != http.StatusForbidden {
          t.Fatalf("steward approve before delegation = %d, want 403", code)
      }
      if code, body := f.do(t, tokScopedOwner, http.MethodPost, "/v1/lands/claimlayer/steward-delegation",
          map[string]any{"operation_types": []string{"github_pr_merge"}}); code != http.StatusOK {
          t.Fatalf("owner delegate = %d %s", code, body)
      }
      esc2 := f.seedEscalationAs("cl-gardener-1", map[string]any{"operation_type": "github_pr_merge"}, "project:claimlayer")
      if got := f.field(esc2, "held_by"); got != "steward:claimlayer" {
          t.Fatalf("held_by after delegation = %q, want steward:claimlayer", got)
      }
      if got := f.field(esc2, "audience"); got != "desk" {
          t.Fatalf("audience after delegation = %q, want desk (D11: steward)", got)
      }
      if code := f.approveAs(f.stewardTok, esc2); code != http.StatusOK {
          t.Fatalf("steward approve after delegation = %d, want 200", code)
      }
      if f.field(esc2, "approval_method") != "coordinator" || f.field(esc2, "approved_as") != "steward:claimlayer" {
          t.Fatalf("approval stamp = %q/%q", f.field(esc2, "approval_method"), f.field(esc2, "approved_as"))
      }
      if got := f.field(esc2, "held_by"); got != "requester" {
          t.Fatalf("held_by after approve = %q, want requester (execute_by)", got)
      }
  }
  ```
  `seedEscalationAs`, `approveAs`, `field` and `seedTenantLand` are small helpers. Reuse `f.seedEscalation`, `f.approve` and `f.wantStatus` from `escalation_scope_test.go`; only the token differs.

  Write the siblings on the same fixture:
  - `TestStewardApprove_RefusesOwnRequest`: the steward files an escalation and approves it → 403. **Control:** the land owner (`tokScopedOwner`) approves the same escalation → 200.
  - `TestStewardApprove_RefusesOtherLand`: a steward of `claimlayer` approves an `acme` escalation → 403. **Control:** an `acme` steward → 200.
  - `TestStewardApprove_RefusesOperatorOnly`: delegation is set, but the escalation has `sensitivity=secret` → `operator_only=true`, `held_by=operator`, `audience=owner` (the one floor, D3), and the steward's approve → 403. **Control:** the same steward on a non-secret merge → 200.
  - `TestLandOwnerApprove_TenantMerge`: `tokScopedOwner` approves a `github_pr_merge` in a tenant land with no delegation → 200, `approved_as=land-owner:claimlayer`. **Refusal sibling:** `tokScopedWriter` (member, not owner) → 403.
  - `TestStewardDelegation_OwnerOnly`:
    - `tokScopedWriter` → 403;
    - `tokScopedGuest` → 403;
    - `tokLandClaimlayer` → 403;
    - `tokScopedOwner` on another land → 403;
    - **controls:** `tokScopedOwner` on claimlayer → 200; `tokOwnerAdmin` → 200.
  - `TestStewardDelegation_RejectsFloorTypes`: owner posts `["github_pr_merge","credential_rotation"]` → 400, field unchanged. Owner posts `["secret_write"]` and `["credential_x"]` → 400. **Control:** `["github_pr_merge","gitlab_mr_merge"]` → 200.
  - `TestStewardDelegation_RejectsUnknownType`: `["drop_database"]` → 400. Allowed types are the `scopedApprovableOperations` keys (`escalation_scope.go`) plus `github_pr_merge` and `gitlab_mr_merge`.
  - `TestLandStewardApprovesField_NotWritableByPatch`: `tokOwnerAdmin` PATCHes `fields.land_steward_approves` on the project bead → 400 naming the endpoint, field unchanged. **Control:** the same admin PATCHes `fields.budget_usd` → 200.
  - `TestEscalationFacts_LandOnlyForTenantKind`: a project with `land_default_channel` set and `land_kind=operator` → `escalationFactsFor(...).Land == ""`, so rule 2 does not match. **Control:** `land_kind=tenant` → `Land == "claimlayer"`.

  Run them (CI runs them too; no local go test on the laptop. A crew pod may run them):
  ```bash
  cd kbeads && go test ./internal/server -run 'TestStewardApprove_|TestLandOwnerApprove_|TestStewardDelegation_|TestLandStewardApprovesField_|TestEscalationFacts_LandOnly' -count=1
  ```
  Expected: FAIL (undefined handlers and helpers, or wrong `held_by`).

- [ ] **Step 3: Field and validation.** Add the project field in `config.go`. Re-locate the field list by `{"name": "land_owner_sub",       "type": "string"},`:
  ```go
  // land_steward_approves: operation types the land owner lets the land's
  // steward approve (escalation-routing D8). Written only through
  // POST /v1/lands/{land}/steward-delegation; a PATCH that names it is a 400.
  {"name": "land_steward_approves", "type": "json"},
  ```
  In `land_steward_delegation.go`:
  ```go
  // validateLandStewardApproves accepts a set of operation types a land may
  // delegate to its steward: approvable ones only, never a D3 floor operation
  // type (escalationFloorOperation, Task 2: the one floor).
  func validateLandStewardApproves(ops []string) error {
      seen := map[string]bool{}
      for _, op := range ops {
          if seen[op] {
              return inputError(fmt.Sprintf("land_steward_approves: duplicate %q", op))
          }
          seen[op] = true
          if floor := escalationFloorOperation(op); floor != "" {
              return inputError(fmt.Sprintf("land_steward_approves: %q is operator-only (%s)", op, floor))
          }
          if !stewardDelegableOperation(op) {
              return inputError(fmt.Sprintf("land_steward_approves: %q is not a delegable operation type", op))
          }
      }
      return nil
  }

  func stewardDelegableOperation(op string) bool {
      if op == "github_pr_merge" || op == "gitlab_mr_merge" {
          return true
      }
      _, ok := scopedApprovableOperations[op]
      return ok
  }
  ```
  `inputError` is a string type (`server.go`, re-locate by `type inputError string`), so a formatted message is `inputError(fmt.Sprintf(…))`. Add `land_steward_approves` to the Task 1 server-only guard for **`type=project`** updates, at the same call site as the route-field guard. It returns 400 with `use POST /v1/lands/{land}/steward-delegation`. Writes through `withMachineWrite(ctx)` (`write_authz.go`, re-locate by `func withMachineWrite(ctx context.Context) context.Context {`) are exempt, which is how the endpoint itself writes.

- [ ] **Step 4: Action tables.** In `action_table.go`:
  ```go
  // actionStewardDelegate: the land owner sets land_steward_approves
  // (escalation-routing D8). Owner tier only.
  const actionStewardDelegate = "steward_delegate"

  // ownerActions is memberActions plus the owner-only acts.
  var ownerActions = withActions(memberActions, map[model.BeadType]map[string]bool{
      model.TypeProject: {actionStewardDelegate: true},
  })
  ```
  Change `tierOwner:  memberActions,` (re-locate by that line; two spaces, gofmt-aligned) to `tierOwner:  ownerActions,`. `withActions` deep-copies, so `memberActions` is never mutated; write it next to `tierAllows` if no copy helper exists.

  In `action_table_test.go`:
  - add `actionStewardDelegate` to `reviewedActions`;
  - give `tierOwner` its own row: `reviewedMemberDecisions` plus `model.TypeProject: {actionStewardDelegate}`. Build it with `maps.Clone` and set the one key.

  `TestActionTable_AllTypesAllTiersHTTP` drives only create/update/close/delete, so it is unaffected. The project-type write policy still 403s a land owner's PATCH.

  Controller, `internal/conversation/landrole.go`: add `ActionStewardDelegate Action = "steward_delegate"` with row `LandRoleOwner`. Add it to `allActions` in `landrole_test.go`, and add `ActionStewardDelegate: "O"` to `TestActionTable_Matrix`'s `want`.

- [ ] **Step 5: Land lanes in membership.** In `escalation_lane_members.go`, inside `callerInLane`'s switch on `lane.Kind`:
  ```go
  case laneSteward:
      // D8: a steward lane has members only for an operation type the land
      // delegates; otherwise nobody is in it (and firstStaffedLane skips it).
      delegated, err := s.landDelegatesToSteward(ctx, lane.Name, fieldStr(esc, "operation_type")) // Task 2
      if err != nil || !delegated {
          return false, err
      }
      agentID, ok := agentBeadIDFromContext(ctx) // per-agent token only
      if !ok || agentID == fieldStr(esc, "requesting_agent") {
          return false, nil
      }
      agent, err := s.store.GetBead(ctx, agentID)
      if err != nil || agent == nil {
          return false, err
      }
      role, _, _ := exactFieldString(agent.Fields, agentRoleField)
      project, _, _ := exactFieldString(agent.Fields, "project")
      return role == "steward" && project == lane.Name, nil
  case laneLandOwner:
      sc := scopeFromContext(ctx)
      return sc.Tiers[lane.Name] == tierOwner, nil
  ```
  Use Task 3's actual kind constants and field helper names; `fieldStr` stands for whatever Task 3 used. `firstStaffedLane` needs to know whether a lane has *any* member, not only whether the caller is one. For `steward:<land>` that is "delegated and the land has a steward role bead", so add that branch to Task 3's staffed check. For `land-owner:<land>` it is "the project bead has `land_owner_sub`" (provisioner-written; `config.go` comment at the `land_owner_sub` field).

  `landDelegatesToSteward` (Task 2) already requires `land_kind == tenant` and fails closed on a malformed `land_steward_approves`. Callers treat its error as "not a member" and log at WARN with the escalation id (Global Constraint 4).

  Check that Task 2's `escalationFactsFor` sets `Land` only when the escalation's project has `land_kind == tenant` (Contract Issue 1). Fix it here if not.

- [ ] **Step 6: The scoped-approver carve-out.**
  - `scopedApproverAllowed` (`escalation_scope.go`) refuses a scoped human on any op missing from `scopedApprovableOperations`, and that map has `github_pr_merge`/`gitlab_mr_merge` set to `false`. So today a land owner can never approve a merge.
  - Allow it only when all three hold: the caller's tier in the escalation's project is `tierOwner`; the op is `github_pr_merge` or `gitlab_mr_merge`; and the escalation's server-stamped `approvers` contain `land-owner:<project>`.
  - Keep every other check (project denial, tier denial, fields denial, privileged spawn) ahead of the carve-out.
  - The map entries stay `false`, so a member, or an owner on a non-tenant project, is still refused (`TestLandOwnerApprove_TenantMerge`'s refusal sibling).

- [ ] **Step 7: Endpoint.** Register `POST /v1/lands/{land}/steward-delegation` in `http.go`'s route table at `RoleViewer`, the same gate as `can-approve`, so scoped OIDC owners reach the handler. The handler, in order:
  1. Decode `{"operation_types": [...]}`. Bad JSON → 400.
  2. `validateLandStewardApproves`. Error → 400.
  3. `FindProjectByName(land)`. Missing → 404. `land_kind != tenant` → 400 `"steward delegation is for tenant lands"`.
  4. Authorize: `callerMayApproveAsAdmin(ctx)` (`escalation_approval_authz.go`, re-locate by `func callerMayApproveAsAdmin(ctx context.Context) bool {`), or the caller is scoped and `scopedTierDenial(ctx, actionStewardDelegate, model.TypeProject, []string{"project:" + land}) == nil`. Anything else → 403 with the denial body. A machine or the shared static token is never allowed: `isScopedPrincipal` is false for them and so is the admin check.
  5. `s.mergeBeadFields(withMachineWrite(ctx), project.ID, map[string]any{landStewardApprovesField: ops})`.
  6. `s.recordAndPublish(ctx, events.TopicBeadUpdated, project.ID, actorFromContext(ctx), ...)`, plus an audit line naming the actor and the new list.
  7. 200 `{"land": land, "operation_types": ops}`.

  An empty list is valid and is how the owner revokes delegation. Revoking does not re-route open escalations, and that is safe: membership is checked at approve time, so the steward's next approve is refused at once, and the idle-lane hand-up (D5) moves the escalation to `land-owner:<land>` within `BEADS_ESCALATION_LANE_IDLE_HANDUP`. Pin it with `TestStewardDelegation_RevokeRefusesNextApprove`: revoke, then the steward's approve is 403; positive control: re-grant, then 200.

- [ ] **Step 8: Controller client.** In `internal/beadsapi/projects.go`, re-locate by `func (c *Client) GetProjectBead`:
  ```go
  // SetStewardDelegation sets the land's land_steward_approves list through
  // the daemon's owner-only endpoint, on the caller's bearer (the
  // ApproveEscalation pattern: user bearer when the context carries one).
  func (c *Client) SetStewardDelegation(ctx context.Context, land string, ops []string) error {
      path := "/v1/lands/" + url.PathEscape(land) + "/steward-delegation"
      body := map[string]any{"operation_types": ops}
      var err error
      if bearer := UserBearerFromContext(ctx); bearer != "" {
          err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, body, nil)
      } else {
          err = c.doJSON(ctx, http.MethodPost, path, body, nil)
      }
      if err != nil {
          return fmt.Errorf("setting steward delegation for %s: %w", land, err)
      }
      return nil
  }
  ```
  The client has no `post` helper; `doJSON` (`client.go`) and `doJSONAsUser` (`user_bearer.go`) are the request helpers. Add a test using the package's fake-server pattern that asserts the path, the body, and that the user bearer from `beadsapi.WithUserBearer` is sent. Mirror `land_steward_approves` into `internal/bridge/init.go`'s project field list (`{Name: "land_steward_approves", Type: "json"}`).

- [ ] **Step 9: Verify, open as draft, file the operator decision.**
  ```bash
  cd kbeads && gofumpt -l ./internal/server && go vet ./internal/server && staticcheck ./internal/server && golangci-lint run ./internal/server/...
  go test ./internal/server -run 'TestStewardApprove_|TestLandOwnerApprove_|TestStewardDelegation_|TestLandStewardApprovesField_|TestEscalationFacts_LandOnly|TestActionTable_' -count=1
  cd ../gardenio/controller && gofumpt -l ./internal/conversation ./internal/beadsapi && go test ./internal/conversation ./internal/beadsapi -run 'TestActionTable_|TestSetStewardDelegation' -count=1
  ```
  Expected: `gofumpt -l` prints nothing; every listed test PASS.
  - Commit `kbeads: land steward delegation + land lanes (escalation-routing T11)`.
  - Push and open the PR as a **draft**.
  - File the merge escalation, then `kd label add <esc> merge-desk:hold`.
  - File the operator decision:
  ```bash
  kd create "PR #<N> needs operator ruling: land owners and stewards gain merge approval" --type decision \
    --field prompt=@- --field options='["merge","send back"]' \
    --label project:gardenio --label operator-decision <<'EOF'
  PR #<N> (escalation-routing Task 11) changes who may approve escalations:
  - land owners may approve github_pr_merge/gitlab_mr_merge in their own tenant land when the policy names land-owner:<land>;
  - a land's steward may approve operation types the owner delegated (land_steward_approves), never operator-only ones, never its own request;
  - POST /v1/lands/{land}/steward-delegation is owner-or-admin only; the field is not PATCH-writable.
  Refusal tests and positive controls: <list test names>. CI: <run url>.
  EOF
  ```
  - Comment on gd-g77WcV22SK (10.1): `escalation-routing T11: tenant agents must be able to file github_pr_merge/gitlab_mr_merge escalations about their own land (Track B §5.1 as amended by T10); please carve these out of the tenant-escalation refusal.`

---

### Task 12: Commons escalation cards: who holds it, buttons by `can-approve`

**Phase:** 5 Lands · **Lane:** CREW · **Merge:** desk (no authz change: the buttons only reflect the daemon's answer, and the daemon refuses anyway) · **Blocked by:** Tasks 8, 9 and 11 (so the card's lane names cover stewards and land owners)

**What is already on main, and this task does not redo.**
- #729 (`42d338a52`) and #734 (`77da56b1f`) made the Commons escalation card's Approve and Reject live. `cardAction` (`internal/commons/card_actions.go`) gates the button on the land-role table (`Gate.MayAct`), then calls `Handler.Escalations`. In the viewer that is `commonsEscalations` (`cmd/beads-viewer/commons_wiring.go`), which checks the viewer's `approve:escalation` capability and then posts to the daemon on the viewer's bearer with `approval_method=commons`. It answers 501 `errActionsOff` only when `Escalations` is nil.
- #756 gave the card its `Audience`, the `InHand()` fold and the "who has it" chip.

What is left: the card names the holding lane (`held_by`), and Approve/Reject render, and are accepted, only when the daemon's `can-approve` says this viewer may approve this escalation (D9), the same gate as the bead page (Task 8).

**Who does what:** a crew writes code, tests and the updated golden, and opens one PR.

**Files:**
- Modify `gardenio/controller/internal/commons/escalation_cards.go`: `EscalationCard` gains `HeldBy`, `WaitingOn` and `CanApprove` (re-locate by `type EscalationCard struct {`); `escalationCardFrom` sets the first two (re-locate by `func escalationCardFrom(b *beadsapi.BeadDetail, land string) (EscalationCard, bool) {`); add `EscalationActionsFor`.
- Modify `gardenio/controller/internal/commons/commons.go`: the `Approvals` interface and the `Handler.Approvals` field, beside `Escalations` (re-locate by `	Escalations Escalations`).
- Modify `gardenio/controller/internal/commons/cards.go`: hydration asks `can-approve` (re-locate by `					c.Asker = askerName(ctx, h.Beads, b.Fields["requesting_agent"], r.M.Land)`).
- Modify `gardenio/controller/internal/commons/render.go`, the `escalationcard` template: the in-hand chip (re-locate by `title="Who has it">{{.Audience}}</span>`), and the Approve form and Reject details (re-locate by `<form method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act"><button class="btn cm-allow" type="submit" name="action" value="approve">Approve</button></form>`).
- Modify `gardenio/controller/internal/commons/commonstest/fixture.go`: `held_by` and `audience` on `cl-esc1`, `cl-esc2` and `cl-esc5` as Task 2 would stamp them (re-locate by `d.Put(&beadsapi.BeadDetail{ID: "cl-esc1", Type: "escalation"`).
- Modify `gardenio/controller/internal/commons/escalation_cards_test.go` and `commons_test.go` (a fake `Approvals`).
- Modify `gardenio/controller/internal/commons/testdata/escalations.golden.html` (regenerated; read the diff).
- Modify `gardenio/controller/cmd/beads-viewer/commons_wiring.go`:
  - `newCommonsHandler` sets `Approvals` (re-locate by `	cursors, _ := beads.(commons.Cursors)`);
  - `commonsEscalations.act` uses `escalationApproveGate` (Task 8) instead of `viewerCanApproveEscalation`, after its status check (re-locate by `	if ok, why := e.s.viewerCanApproveEscalation(r.Context(), email); !ok {`).
- Create `gardenio/controller/cmd/beads-viewer/commons_escalations_test.go`: the adapter refuses on the daemon's no. No test exercises `commonsEscalations` on main today (grepped at `399f1ecfe`).
- Modify `gardenio/controller/internal/conversation/placement.go`: the DM-first comment names `EscalationAddressees` (Task 9). Reuse it; never redefine it.

**Interfaces (new):**
```go
// internal/commons
type Approvals interface {
    CanApproveEscalation(ctx context.Context, id string) (beadsapi.CanApproveResult, error)
}
// Handler gains: Approvals Approvals // nil: no Approve/Reject buttons (fail closed)
func EscalationActionsFor(ctx context.Context, a Approvals, card *EscalationCard) bool

// internal/conversation (Task 9; consumed here, not defined)
type EscalationAddressee struct{ Lane, Land string }
func EscalationAddressees(heldBy, land string) []EscalationAddressee
```
`*beadsapi.Client` already satisfies `Approvals` after Task 7 (`CanApproveEscalation`).

- [ ] **Step 1: Branch, then confirm what Tasks 7–9 landed.**
  ```bash
  git fetch origin && git switch -c agent/escalation-routing/<task-bead-id> origin/main
  grep -n -E 'func \(c \*Client\) CanApproveEscalation|func EscalationHeldBy|func LaneLabel|type CanApproveResult' gardenio/controller/internal/beadsapi/*.go
  grep -n 'func (s \*Server) escalationApproveGate' gardenio/controller/cmd/beads-viewer/*.go
  grep -n 'func EscalationAddressees' gardenio/controller/internal/conversation/*.go
  ```
  Expected: all six.

- [ ] **Step 2: Fixture.** In `commonstest/fixture.go`, stamp the open and approved escalations as Task 2 (and Task 4's approve) would:
  - `cl-esc1` (credential_requirements: forced to the owner, D3): `"held_by": "operator"`, `"audience": "owner"`;
  - `cl-esc2` (approved, executed by the server): `"held_by": "server"`, `"audience": "operator"`;
  - `cl-esc5` (a merge #756 gave to the desk): `"held_by": "coordinator:merge-desk"` beside its `"audience": "desk"`;
  - leave `cl-esc3`/`cl-esc4` (closed) as they are.
  Update the fixture's audience comment (`// Audience (gd-bmOiteQi36): …`) to say the stamps are what Task 2 writes; the In-hand line it predicts (`In hand: 3 (desk 1, operator 2)`) does not change.

- [ ] **Step 3: Failing tests.** In `commons_test.go`, add a fake:
  ```go
  type fakeApprovals struct {
      ans map[string]beadsapi.CanApproveResult
      err map[string]error
  }

  func (f fakeApprovals) CanApproveEscalation(_ context.Context, id string) (beadsapi.CanApproveResult, error) {
      if err := f.err[id]; err != nil {
          return beadsapi.CanApproveResult{}, err
      }
      if r, ok := f.ans[id]; ok {
          return r, nil
      }
      return beadsapi.CanApproveResult{Reason: "not an approver"}, nil
  }
  ```
  Then in `escalation_cards_test.go`, using `commonsServer(t, d, func(h *Handler) { h.Approvals = … })`, `get`, `mustContain` and `mustNotContain`, on the `#escalations` channel page (the path the existing escalation-card tests use):
  - `TestEscalationCard_WaitingOnNamesTheLane`: the `cl-esc1` card contains `Waiting on the owner`, and the in-hand `cl-esc5` card's "who has it" chip reads `coordinator merge-desk` (not `desk`).
  - `TestEscalationCard_UnstampedKeepsAudience`: remove `held_by` from `cl-esc5` in the test's copy of the fixture; its chip reads `desk`, as #756 drew it, and no card says `Waiting on` an empty lane (Global Constraint 7).
  - `TestEscalationCard_ButtonsOnlyWhenDaemonAllows`: with `fakeApprovals{ans: {"cl-esc1": {Allowed: true, Method: "web_ui", As: "operator"}}}`, the `cl-esc1` card has the approve form (`value="approve"`) and the reject button. **Same fixture, refusal:** the `cl-esc5` card (the fake says no) has neither, and still has the Review link.
  - `TestEscalationCard_NoApprovalsMeansNoButtons`: `Approvals` is nil, so no card has the approve form. **Control:** the same page with the allowing fake has it on `cl-esc1`.
  - `TestEscalationCard_CanApproveErrorHidesButtons`: the fake returns an error for `cl-esc1`, so there is no form and the page still answers 200 (fail closed, logged at WARN).
  ```bash
  cd gardenio/controller && go test ./internal/commons -run 'TestEscalationCard_' -count=1
  ```
  Expected: FAIL to compile, with `unknown field Approvals in struct literal`.

- [ ] **Step 4: Card data.** In `escalation_cards.go`, add to `EscalationCard` after `Audience`:
  ```go
  	// HeldBy is the lane that holds it (escalation-routing D1), WaitingOn its
  	// plain-words label (beadsapi.LaneLabel). Both empty for an unstamped
  	// escalation, which then shows only Audience (Global Constraint 7).
  	HeldBy    string
  	WaitingOn string
  	// CanApprove is the daemon's can-approve for this viewer (D9); only then
  	// do Approve and Reject render. Set by hydration, never by escalationCardFrom.
  	CanApprove bool
  ```
  In `escalationCardFrom`, beside `Audience: beadsapi.EscalationAudience(f),`:
  ```go
  		HeldBy:      beadsapi.EscalationHeldBy(b),
  		WaitingOn:   beadsapi.LaneLabel(beadsapi.EscalationHeldBy(b)),
  ```
  and below the type:
  ```go
  // EscalationActionsFor reports whether the viewer on ctx may approve (and so
  // reject) card's escalation: the daemon's can-approve on the viewer's own
  // bearer (D9). False for a closed card, a nil Approvals or any error (fail
  // closed; the error is logged). Whoever may approve may reject.
  func EscalationActionsFor(ctx context.Context, a Approvals, card *EscalationCard) bool {
  	if a == nil || card == nil || !card.Open() {
  		return false
  	}
  	res, err := a.CanApproveEscalation(ctx, card.ID)
  	if err != nil {
  		slog.Warn("commons: can-approve failed; hiding escalation buttons", "escalation", card.ID, "error", err)
  		return false
  	}
  	return res.Allowed
  }
  ```
  Add `context` and `log/slog` to the file's imports if it lacks them. In `commons.go`, beside `Escalations Escalations`:
  ```go
  	// Approvals answers can-approve for the viewer (escalation-routing D9), so
  	// an escalation card shows Approve and Reject only when the daemon would
  	// accept them. Nil shows no buttons.
  	Approvals Approvals
  ```
  and the interface next to `Escalations`'s (that one is in `card_actions.go`; put `Approvals` beside it there):
  ```go
  // Approvals is the daemon's dry run of its approve checks for the viewer.
  type Approvals interface {
  	CanApproveEscalation(ctx context.Context, id string) (beadsapi.CanApproveResult, error)
  }
  ```

- [ ] **Step 5: Hydration asks the daemon.** In `cards.go`, the escalation case. Before:
  ```go
  				if c, ok := escalationCardFrom(b, r.M.Land); ok {
  					c.Asker = askerName(ctx, h.Beads, b.Fields["requesting_agent"], r.M.Land)
  					r.Escalation = &c
  				}
  ```
  After:
  ```go
  				if c, ok := escalationCardFrom(b, r.M.Land); ok {
  					c.Asker = askerName(ctx, h.Beads, b.Fields["requesting_agent"], r.M.Land)
  					// ctx is the request's: it carries the viewer's bearer, so the
  					// daemon answers for this viewer, never for the viewer process.
  					c.CanApprove = EscalationActionsFor(ctx, h.Approvals, &c)
  					r.Escalation = &c
  				}
  ```
  Confirm in the surrounding function that `ctx` derives from `r.Context()` (it is the one `askerName` reads with); if it is a detached context, pass the request's instead and say so in the PR.

- [ ] **Step 6: Template.** In `render.go`'s `escalationcard`:
  - the in-hand chip names the lane when there is one. Before: `title="Who has it">{{.Audience}}</span>`. After: `title="Who has it">{{if .WaitingOn}}{{.WaitingOn}}{{else}}{{.Audience}}{{end}}</span>`;
  - in the `{{- if .Open}}` branch, before `<div class="cm-ecard-acts">`, add `{{- if and .WaitingOn (not .InHand)}}<p class="cm-ecard-waiting">Waiting on {{.WaitingOn}}</p>{{- end}}`;
  - wrap the Approve `<form …value="approve">Approve</button></form>` and the whole `<details class="cm-switch cm-kill">…</details>` in `{{if .CanApprove}}…{{end}}`. The Review link stays unconditional.

  Regenerate the golden, and read the diff before staging it:
  ```bash
  cd gardenio/controller && go test ./internal/commons -run TestFragmentGoldens -update -count=1 && git diff --stat internal/commons/testdata/
  git diff internal/commons/testdata/escalations.golden.html
  ```
  Expected: only `escalations.golden.html` changes. The diff adds `Waiting on the owner` to `cl-esc1`, renames `cl-esc5`'s chip to `coordinator merge-desk`, and drops the Approve/Reject controls from every card, because the golden handler has no `Approvals`. If other goldens change, stop and find out why.

- [ ] **Step 7: Viewer wiring and the POST re-check.** In `cmd/beads-viewer/commons_wiring.go` `newCommonsHandler`, beside `cursors, _ := beads.(commons.Cursors)`:
  ```go
  	approvals, _ := beads.(commons.Approvals)
  ```
  and `Approvals: approvals,` in the `commons.Handler` literal. A daemon client without the method leaves it nil and shows no buttons. Add a compile-time check in a viewer test file:
  ```go
  var _ commons.Approvals = (*beadsapi.Client)(nil)
  ```
  In `commonsEscalations.act`, delete the `viewerCanApproveEscalation` precheck (three lines) and put the gate back **after** the status check, so a wrong-type or closed bead still answers 400 or 409 (as Task 8 does for the bead page):
  ```go
  	if ok, why := e.s.escalationApproveGate(r.Context(), id, email); !ok {
  		return &commons.CardRefusal{Status: http.StatusForbidden, Message: why}
  	}
  ```
  Update the type's comment: "the daemon's can-approve (escalationApproveGate), then the daemon's own escalation route on the viewer's bearer". In the new `commons_escalations_test.go`, using `escActionFake` (Task 8 seeds its `canApproveResp`) and a request whose context carries the viewer's email (`oidcauth.ContextWithEmail`): the fake daemon's `canApproveResp` says no → `CardRefusal{Status: 403}` and no `ApproveEscalation` call; **control:** it says yes → the approve call is made with `ApprovalMethod: "commons"`. `cardAction` itself is not changed: its land-role gate stays, and the adapter is the re-check.

- [ ] **Step 8: Placement comment.** In `internal/conversation/placement.go`, update the comment that says DM-first is "placed by the DM step, not here" to name `EscalationAddressees` (Task 9) as how an escalation's DM addressee is chosen. No code change in placement.

- [ ] **Step 9: Verify and open the PR.**
  ```bash
  cd gardenio/controller && gofumpt -l ./internal/commons ./internal/conversation ./cmd/beads-viewer && go vet ./internal/commons ./internal/conversation ./cmd/beads-viewer && staticcheck ./internal/commons ./internal/conversation ./cmd/beads-viewer
  go test ./internal/commons ./internal/conversation ./cmd/beads-viewer -count=1
  ```
  Expected: clean and PASS. Commit `commons: escalation cards name the holding lane; Approve/Reject ask can-approve (escalation-routing T12)`, then push and open the PR. Self-merge on green: `internal/commons` is not a human path, and the adapter change only narrows who sees a button the daemon would refuse.

---

## Phase 6: Identities and rollout

### Task 13: Identities, env and policy seed

**Phase:** 6 Rollout · **Lane:** OWNER + OPERATOR · **Merge:** the chart/values PR (13a) is **operator-merge** (it sets who may approve: Contract Issue 6) · **Blocked by:** Tasks 4 and 6

**Who does what:**
- **OWNER:**
  - answers one decision bead approving the laptop coordinator identity and the seeded policy (Step 1);
  - nothing else. The secret is generated without anyone seeing it.
- **OPERATOR** (laptop session, owner's AWS profile `gardenio`, region `us-east-1`, cluster via `scripts/k3s/kubectl-ssm.sh` / `--context gardenio-ssm`):
  - everything else;
  - reports what was created after each step.
- **CREW:** writes PR 13a (chart values + deploy-values). Any crew can take it once Tasks 4 and 6 are merged.

**Files (PR 13a):**
- Modify: `gardenio/helm/gardenio/values.yaml`: re-locate by `escalationApproverClients: ""` and add `escalationCoordinatorClients: ""`, `escalationResultMachines: ""` and `escalationResultRequiresApproval: false`, each with a one-line comment.
- Modify: `gardenio/helm/gardenio/templates/beads/deployment.yaml`: next to the `BEADS_ESCALATION_APPROVER_CLIENTS` block (re-locate by that name; it appears once), render `BEADS_ESCALATION_COORDINATOR_CLIENTS` and `BEADS_ESCALATION_RESULT_MACHINES` when non-empty, and `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL` always (`{{ .Values.beads.escalationResultRequiresApproval | quote }}`).
- Modify: `deploy-values/gardenio-k3s.yaml`:
  - a new KeycloakClient `gardenio-laptop-operator` next to the slack-bridge entry (re-locate by `clientId: gardenio-slack-bridge`);
  - the `beads.machineClients` list (re-locate by `  machineClients:`, two-space indent; the other two mentions are comments);
  - `beads.escalationCoordinatorClients: "gardenio-laptop-operator"`, `beads.escalationResultMachines: ""`, `beads.escalationResultRequiresApproval: false`.
- Modify: `gardenio/helm/gardenio/tests/machine_principal_test.go`: the expected client list gets `gardenio-laptop-operator` (it asserts `machineClients` equals the serviceAccount clients). Add a render test asserting the three env vars appear with the deploy-values values.

- [ ] **Step 1 (OPERATOR → OWNER): one decision bead before anything is created.** First check the open decisions (`kd list --type decision --status open`) so this isn't a duplicate. Then:
  ```bash
  kd create "Escalation routing: create the laptop coordinator identity and seed the escalation policy" --type decision \
    --field prompt=@- --field options='["approve as written","change it (say how)"]' \
    --label project:gardenio --label operator-decision <<'EOF'
  What changes:
  1. A new Keycloak service-account client, gardenio-laptop-operator, used only by the laptop agent session. It can approve escalations whose policy lists coordinator:laptop, never operator-only ones (secrets, credentials, approval-rule changes) and never its own. Its secret lives only in AWS Secrets Manager gardenio/laptop-operator-client and a 0600 file on the laptop.
  2. The daemon's escalation_policy bead, seeded with the plan's default rules (D7): merge-desk then laptop then you for gardenio merges; steward or land owner for tenant-land merges; you for owner actions; everything else approved by you, executed by the laptop.
  3. The merge desk prompt (trigger gd-WksO1FSmvV) gains one step: approve the escalation before merging or closing it.
  Why: today every surface shows you all 13 open escalations; after this, only the ~4 that need your hands or judgement.
  Nothing changes who executes until Task 14 switches the laptop over.
  EOF
  ```
  Wait for the owner's answer; it arrives as a respawn/answer on the decision. Don't proceed on "change it" without re-filing.

- [ ] **Step 2 (CREW): PR 13a.** The KeycloakClient entry mirrors `gardenio-slack-bridge` field for field. Re-locate that block by `clientId: gardenio-slack-bridge`.
  - `clientId: gardenio-laptop-operator`
  - `public: false`, `standardFlowEnabled: false`, `directAccessGrantsEnabled: false`
  - `secret: "$gardenio-machine-gardenio-laptop-operator:clientSecret"`
  - `access.token.lifespan: "1800"`
  - `serviceAccountsEnabled: true` with realmRoles `[gardenio-operator]` only. It needs the operator role to reach `RoleOperator`-gated escalation routes (`kd escalation` long help, `kbeads/cmd/kd/escalation.go`, re-locate by `which are RoleOperator-gated on`). It is *not* `gardenio-admin`, so it is never the `operator` lane (D2).
  - protocolMappers: the same `realm-roles` (claim `realm_roles`) and `audience-self` (included.client.audience `gardenio-traefik-forwardauth`) as slack-bridge.

  Append `gardenio-laptop-operator` to `beads.machineClients`.

  Run the chart tests and a template render, then open the PR as a **draft**:
  ```bash
  cd gardenio/helm/gardenio && go test ./tests -run 'TestMachinePrincipal|TestEscalationEnv' -count=1
  helm template t . -f ../../../deploy-values/gardenio-k3s.yaml | grep -n -A1 -E 'BEADS_ESCALATION_(COORDINATOR_CLIENTS|RESULT_MACHINES|RESULT_REQUIRES_APPROVAL|APPROVER_CLIENTS)'
  ```
  Expected:
  - `BEADS_ESCALATION_COORDINATOR_CLIENTS` = `gardenio-laptop-operator`;
  - `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL` = `"false"`;
  - no `BEADS_ESCALATION_RESULT_MACHINES` line (empty);
  - `BEADS_ESCALATION_APPROVER_CLIENTS` unchanged.

  The PR body says:
  - the deploy-values change takes effect only at an admin values roll (the deployer parks on values drift);
  - `escalationResultMachines` is empty on purpose (Contract Issue 4);
  - the gate stays off until Task 14.

  Then the merge-escalation `merge-desk:hold` and the operator decision bead, as in Global Constraint 2.

- [ ] **Step 3 (OPERATOR): generate the secret where nobody sees it.** Do this after the owner's yes and before the roll.
  ```bash
  umask 077; S=$(mktemp -d ~/ws/scratch/laptop-op.XXXX)
  openssl rand -base64 48 | tr -d '\n' > "$S/secret"
  AWS_PROFILE=gardenio aws --region us-east-1 secretsmanager create-secret \
    --name gardenio/laptop-operator-client --secret-string "file://$S/secret" \
    --description "Keycloak client secret for gardenio-laptop-operator (escalation-routing T13)"
  install -m 0600 "$S/secret" ~/.config/gardenio/laptop-operator.secret
  KUBECONFIG=<ssm kubeconfig> kubectl --context gardenio-ssm -n gardenio create secret generic \
    gardenio-machine-gardenio-laptop-operator --from-file=clientSecret="$S/secret"
  shred -u "$S/secret"; rmdir "$S"
  ```
  - This uses `create`, not `apply`, and never templates or prints Secrets (memory rule). Re-check the namespace against where `gardenio-machine-gardenio-slack-bridge` lives with `kubectl get secret -n <ns> gardenio-machine-gardenio-slack-bridge -o name`, which prints names only.
  - Report what was created: the SM secret ARN, the k8s Secret name/namespace and the laptop file path. Never report the value.

- [ ] **Step 4 (OPERATOR): admin values roll.** Once PR 13a is merged by the operator:
  1. Run the reviewed-diff recipe (`~/ws/make-local-roll.py <worktree-at-live-tag> <calver>`): dry-run, then review `~/ws/scratch-roll-<tag>.live-drift.diff`.
  2. The diff must show exactly: the new KeycloakClient, the `machineClients` env and the escalation env vars. Drop `kind: Secret` from both sides before diffing.
  3. Copy the diff to a reviewed file and roll with `REVIEWED_DIFF_FILE`, exporting `GH_CONFIG_DIR=$HOME/.config/gh-ja`. If the theme ConfigMap is deployer-applied, apply it first, as in the admin-roll recipe.
  4. Leave the deployer running: no re-suspend after the roll (P-R4 gd-iK8RnuLG3W).
  5. File the `release-deploy <calver>` event bead by hand.

  Then confirm:
  ```bash
  kubectl --context gardenio-ssm -n operators get keycloakclient -o name | grep laptop-operator
  kubectl --context gardenio-ssm -n gardenio get deploy gardenio-beads -o json | python3 -I -c 'import sys,json
  d=json.load(sys.stdin); e={x["name"]:x.get("value") for c in d["spec"]["template"]["spec"]["containers"] for x in c.get("env",[]) if x["name"].startswith("BEADS_ESCALATION")}; print(e)'
  ```
  Expected: the KeycloakClient exists and reports OK; env shows `BEADS_ESCALATION_COORDINATOR_CLIENTS=gardenio-laptop-operator` and `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL=false`. Re-locate the KeycloakClient namespace and the beads Deployment name by `kubectl get keycloakclient -A -o name` and `kubectl -n gardenio get deploy -o name | grep beads`; the commands above show the expected shape.

- [ ] **Step 5 (OPERATOR): token positive and negative controls.** Mint a token by client credentials, with the secret going to curl by file and never argv:
  ```bash
  T=$(mktemp ~/ws/scratch/lop-tok.XXXX); chmod 600 "$T"
  curl -sS https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id=gardenio-laptop-operator \
    --data-urlencode "client_secret@$HOME/.config/gardenio/laptop-operator.secret" \
    | python3 -I -c 'import sys,json; print(json.load(sys.stdin)["access_token"])' > "$T"
  printf 'Authorization: Bearer %s\n' "$(cat "$T")" > "$T.h"; chmod 600 "$T.h"
  ```
  Pick three escalations, from `kd escalation list --json` plus `kd show <id> --json` (the `approvers`/`operator_only` fields):
  - (a) an open gardenio `github_pr_merge` or `gitlab_mr_merge` escalation, requested by an agent;
  - (b) an open `operator_only=true` one;
  - (c) one whose approvers don't include `coordinator:laptop`, e.g. a `manual_owner_action`.

  If no (b) exists, create a throwaway `credential_rotation` escalation with `kd create`, a title starting `[routing-control]`, and reject it after.
  ```bash
  for id in <a> <b> <c>; do curl -sS -H @"$T.h" -X POST https://api.clonk.tv/v1/escalations/$id/can-approve; echo; done
  ```
  Expected:
  - (a) `{"allowed":true,"method":"coordinator","as":"coordinator:laptop",...}`. It is the **positive control**: if (a) is false, everything below proves nothing; stop and debug lane membership.
  - (b) `allowed:false` with a reason naming the floor.
  - (c) `allowed:false`, not an approver.

  The same three with the owner's `kd login` bearer: (a), (b) and (c) are all `allowed:true, method:web_ui, as:operator`. That confirms the human path is unchanged. Also check a static-token call (`GARDENIO_TOKEN`) to (a): `allowed:false`, since a static token is in no lane. Then `shred -u "$T" "$T.h"`.

- [ ] **Step 6 (OPERATOR): seed the policy.** After the owner's yes in Step 1, write the D7 policy as an `escalation_policy` bead, as the owner via `kd login`. That is the only identity whose write passes the `escalation_policy` type's write policy (Task 2) and isn't itself a floor operation.
  ```bash
  cat > ~/ws/scratch/escalation-policy.rules.json <<'EOF'
  [
    {"when": {"operation_types": ["github_pr_merge", "gitlab_mr_merge"], "projects": ["gardenio"]},
     "approvers": ["coordinator:merge-desk", "coordinator:laptop", "operator"], "execute_by": "coordinator:merge-desk"},
    {"when": {"operation_types": ["github_pr_merge", "gitlab_mr_merge"], "lands": ["*"]},
     "approvers": ["steward:{land}", "land-owner:{land}"], "execute_by": "requester"},
    {"when": {"operation_types": ["manual_owner_action", "soak_verification"]},
     "approvers": ["operator"], "execute_by": "operator"},
    {"when": {"operation_types": ["create", "update", "close", "grant"]},
     "approvers": ["operator"], "execute_by": "server", "hold_by": "coordinator:laptop"},
    {"when": {}, "approvers": ["operator"], "execute_by": "coordinator:laptop", "hold_by": "coordinator:laptop"}
  ]
  EOF
  cat > ~/ws/scratch/escalation-policy.coordinators.json <<'EOF'
  {"merge-desk": {"principals": ["role:reviewer"], "audience": "desk"},
   "laptop": {"principals": ["machine:gardenio-laptop-operator"], "audience": "operator"}}
  EOF
  kd create "escalation_policy (escalation-routing D7 seed)" --type escalation_policy \
    --field rules=@$HOME/ws/scratch/escalation-policy.rules.json \
    --field coordinators=@$HOME/ws/scratch/escalation-policy.coordinators.json \
    --label project:gardenio
  ```
  - `hold_by` on rules 4 and 5 and each coordinator's `audience` are D7 and D11 **(owner choice, asked in Step 1)**. With them, everything that is not a merge and not a floor stays in #756's `operator` audience, held by the laptop; without `hold_by`, `stampEscalationRoute` would hold those by `operator` and stamp `audience=owner`. Task 2's validation refuses a coordinator `audience` of `owner`.
  - Rule 0 (the floor) is code, not config, so it isn't in `rules`. Rule indices in `policy_rule` count from the bead's list. If Task 2 numbers the floor as `#0`, match Task 2's convention; re-check with `grep -n 'escalation#' kbeads/internal/server/escalation_policy.go` on main.
  - If `kd create --field k=@file` isn't supported for json fields, use `--field rules="$(cat …)"`; neither file holds a secret.

  Verify:
  ```bash
  kd list --type escalation_policy --status open --json | python3 -I -c 'import sys,json; b=json.load(sys.stdin); print(len(b), [x["id"] for x in b])'
  ```
  Expected: exactly one open `escalation_policy` bead. Then file a fresh `[routing-control]` `gitlab_mr_merge` escalation in project gardenio with the operator's own kd login and check its stamp: `policy_rule=escalation#<rule 1's index>`, `held_by=coordinator:merge-desk`, `audience=desk`, `execute_by=coordinator:merge-desk`. **Second control:** a `[routing-control]` `manual_check` escalation stamps `held_by=coordinator:laptop`, `audience=operator` (rule 5), and `gb escalation route <id> owner` moves it to `held_by=operator`, `audience=owner` (D11 rule 3). Reject both after. Comment on the policy bead: `laptop-claude: seeded per decision <id>; control escalation <id> stamped held_by=coordinator:merge-desk.`

- [ ] **Step 7 (OPERATOR): the merge desk approves first.** Edit trigger `gd-WksO1FSmvV`'s `prompt` (`kd show gd-WksO1FSmvV --json` → `fields.prompt`) in three places. Re-locate each by its text.
  1. In **MERGEABLE**, insert a new step 1 before `Post a PR comment whose first line is \`Verdict: mergeable`:
     > 1. Approve the escalation as the merge desk: `gb escalation approve <id>`. The daemon records `approval_method=coordinator`, `approved_as=coordinator:merge-desk`. A 403 here means the policy does not list you for this escalation. Do not merge; comment the 403 body on the escalation, label it `merge-desk:seen` and `merge-desk:close-denied`, and squawk it.

     Renumber the following steps.
  2. In **Step 0.5**, change `(if merged: close the escalation success with the merge sha, and stop there)` to `(if merged: approve it (\`gb escalation approve <id>\`), then close it success with the merge sha, and stop there)`.
  3. In **SENT BACK**, leave the `failure` close as is. A reject is the D5 path and needs no approval: replace `gb escalation close <id> --status failure --result '{...}'` with `gb escalation reject <id> --reason '<one line>'`, followed by the existing `kd comment add` carrying the full `required_changes` JSON.

  Write the edited prompt to a scratch file and diff it against the original (`diff` must show only those three hunks). Then:
  ```bash
  kd update gd-WksO1FSmvV --field prompt=@$HOME/ws/scratch/merge-desk-prompt.v2.txt
  ```
  - The trigger carries `spawn_lineage` with a digest. If the update re-stamps or invalidates it, the next cron run (`7,22,37,52 * * * *`) won't spawn.
  - Confirm `last_run` advances past the edit time within 20 minutes. If it doesn't, read `last_outcome` and fix the lineage before Task 14.
  - Comment on gd-WksO1FSmvV: `laptop-claude: escalation-routing T13: desk approves before merge/close (decision <id>).`

- [ ] **Step 8: Report.** Post one comment on the plan bead. It lists what was created (KeycloakClient, SM secret ARN, k8s Secret, policy bead id, the prompt edit), the three can-approve control results, and the calver rolled.

---

### Task 14: Laptop switch-over, gate flip, supersede old beads, acceptance

**Phase:** 6 Rollout · **Lane:** OPERATOR · **Merge:** n/a (the laptop1 repo is pushed straight to main; the gate flip is a values PR plus an admin roll) · **Blocked by:** Tasks 7, 8, 9 and 13

**Who does what:**
- **OPERATOR** (laptop session) does everything.
- **OWNER:** no action, except to read the census report at the end. If the owner sees a wrong "needs you" count, that's a bug report against Task 7 (the `/up` and needs-you counts), not a reason to roll back.

**Files:**
- Modify (laptop1 repo, `~/ws/claude`): `skills/operating-gardenio/SKILL.md`, `skills/operating-gardenio/scripts/watch-queue.py`.
- Create (laptop1 repo): `skills/operating-gardenio/scripts/coordinator.sh`.
- Modify (not in git; dev-mod): `~/.claude/dev-mods/<id>/gardenio-dock/bin/queue.py`, `hooks/register.js`.
- Modify (gardenio repo PR 14a): `deploy-values/gardenio-k3s.yaml`: `beads.escalationResultRequiresApproval: true`.

- [ ] **Step 1: The coordinator wrapper.** Create `skills/operating-gardenio/scripts/coordinator.sh`.
  - It mints a client-credentials token to a 0600 temp file and calls the daemon with `curl -H @file`, so the secret and the token never reach argv.
  - It implements `approve <id>`, `can-approve <id>`, `claim <id>`, `hand-up <id> <reason>`, `result <id> <status> <result-json-file>`, and `list` (open escalations `held_by=coordinator:laptop`).
  ```bash
  #!/usr/bin/env bash
  # Acts as coordinator:laptop (Keycloak client gardenio-laptop-operator) against
  # https://api.clonk.tv. The owner's kd login stays the OPERATOR lane; this is
  # the laptop agent's own identity (escalation-routing T14).
  set -euo pipefail
  API=${GARDENIO_API:-https://api.clonk.tv}
  SECRET=${LAPTOP_OPERATOR_SECRET_FILE:-$HOME/.config/gardenio/laptop-operator.secret}
  H=$(mktemp); trap 'shred -u "$H" 2>/dev/null || rm -f "$H"' EXIT; chmod 600 "$H"
  tok=$(curl -sS --fail https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id=gardenio-laptop-operator \
    --data-urlencode "client_secret@$SECRET" | python3 -I -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')
  printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$tok" > "$H"; unset tok
  call() { curl -sS --fail-with-body -H @"$H" -X "$1" "$API$2" ${3:+--data-binary @"$3"}; echo; }
  case "${1:-}" in
    can-approve) call POST "/v1/escalations/$2/can-approve" ;;
    approve)     call POST "/v1/escalations/$2/approve" ;;
    claim)       call POST "/v1/escalations/$2/claim" ;;
    hand-up)     b=$(mktemp); python3 -I -c 'import sys,json; print(json.dumps({"reason":sys.argv[1]}))' "$3" > "$b"; call POST "/v1/escalations/$2/hand-up" "$b"; rm -f "$b" ;;
    result)      call POST "/v1/escalations/$2/result" "$4" ;;  # $3 is the status, also inside the JSON file
    list)        call GET "/v1/escalations?status=open,in_progress&held_by=coordinator:laptop&limit=200" ;;  # Task 5's held_by filter
    *) echo "usage: coordinator.sh can-approve|approve|claim|hand-up|result|list ..." >&2; exit 2 ;;
  esac
  ```
  - Check the approve body shape against Task 4's handler before relying on an empty body. If it needs `{"approved_by":…}`, the server ignores that for coordinators (it stamps the principal), so send `{}`.
  - Run `./coordinator.sh can-approve <a>` against Step 5 (a) of Task 13 again: expected `allowed:true`.

- [ ] **Step 2: SKILL.md.**
  - In the **Quick reference** table, replace the `Resolve` and `Open queue` rows and add one:
    ```markdown
    | My queue (coordinator:laptop) | `scripts/coordinator.sh list` |
    | Owner's queue (operator) | `gb escalation list --held-by operator` (Task 5 flag; `kd` has no `--held-by`); never approve these as the laptop |
    | Approve as the laptop | `scripts/coordinator.sh can-approve <id>` then `scripts/coordinator.sh approve <id>`; never `kd escalation approve` (that is the owner's identity) |
    | Resolve | `scripts/coordinator.sh result <id> <status> <file.json>` once approved; `kd escalation resolve` is the owner's identity and only for `execute_by=operator` |
    | Can't do it | `scripts/coordinator.sh hand-up <id> "<plain-words reason>"` routes it to the owner with a pingback |
    ```
  - In **Owner-only actions**, add: "An escalation whose `held_by` is `operator` is the owner's. The laptop may prepare it (comment the exact command, the evidence) but never approves it with the owner's kd login. That is the habit this switch-over retires."
  - In **Common mistakes**, add a row: "Approving with `kd escalation approve`: that records the OWNER as approver (`approval_method=web_ui`), and the owner's surfaces then show work they never saw. Use `coordinator.sh`."

- [ ] **Step 3: The watcher wakes on the laptop's lane.** In `watch-queue.py`:
  - Replace the `kd_json("escalation", "list")` loop body's filter. Wake on an open escalation outside the known set whose `held_by` is `coordinator:laptop`, **or** whose `held_by` is `operator` and whose priority is ≤ 1. The owner's P0/P1 items still wake the laptop, which tells the owner in chat; it never approves them.
  - Read `held_by` from `gb escalation list --held-by coordinator:laptop,operator --json` (Task 5's flag, on `gb` only), so there's no per-id `kd show`.
  - Keep `SKIP_TITLE`/`SKIP_OPTYPES` for one release, then delete them. `held_by=coordinator:merge-desk` already removes ordinary merges.
  - Update the docstring to say what wakes it.
  - The printed line becomes `ESCALATION <id> P<n> held_by=<lane>: <title>`.

- [ ] **Step 4: The dock counts only the owner's lane.** In `gardenio-dock/bin/queue.py` `snapshot()`, change the escalation query to `/v1/escalations?status=open,in_progress&held_by=operator&limit=200` (Task 5's `held_by` filter; `GET /v1/escalations` has no `audience` filter, and `held_by=operator` is exactly the gardenio owner's owner-audience set, D11). Add `"held_by": f.get("held_by") or "operator"` to `slim()`, so an unstamped bead still counts (Global Constraint 7).

  In `hooks/register.js` `oneLine()`, label the count `needs you`: `plural(esc.length, 'escalation') + ' need you'`. Add a second, dim count from a second query, `held_by=coordinator:laptop`, rendered as `· laptop: N`.

  These files are a dev-mod outside git, so copy the originals to `~/ws/scratch/gardenio-dock.bak/` first. Then check: `python3 -I bin/queue.py --once` prints a snapshot whose `escalations` all carry `held_by == "operator"`.

- [ ] **Step 5: Drain the existing queue onto the right lanes.** For each open escalation, `kd escalation list --json`, then `kd show <id> --json` for `held_by`/`approvers`:
  - `held_by=coordinator:laptop` and `can-approve` true: approve or hand it up on the evidence, as the laptop, via `coordinator.sh`.
  - Already approved and waiting on an executor (census row 3: gd-YjL6XkDlqU, gd-5mFCwUapcL, gd-3aYqRdf1SO, gd-fWeCNSRdCd): if `execute_by` is `coordinator:laptop`, claim it, do it or hand it up. If it was approved before Task 4 (`approval_method=web_ui` by the owner), the sweep has set `held_by=execute_by`; leave the approval alone.
  - Owner items (census row 1: gd-0lWOG2ZT09, gd-bH4HHHzucm, gd-C9kdXMyDWl, gd-1nDlKw5PWA): confirm `held_by=operator`, then leave them.

  Comment `laptop-claude: escalation-routing T14: <what was done>` on each one touched.

- [ ] **Step 6: Flip the `/result` gate (PR 14a and an admin roll).** Before the flip, confirm all three:
  1. Task 13 Step 7's desk prompt is live and one desk run has approved before closing: `kd list --type escalation -s closed --closed-after <T13 time> --json`, then `approval_method=coordinator`, `approved_as=coordinator:merge-desk` on a desk-closed escalation.
  2. The laptop's `coordinator.sh result` has closed one approved escalation.
  3. No open escalation has `execute_by` empty: `kd escalation list --json`, then `kd show` per id. An empty one means the Task 2 sweep missed it; fix that first.

  Then:
  - The CREW or the operator opens PR 14a: `beads.escalationResultRequiresApproval: true` in `deploy-values/gardenio-k3s.yaml`, with a body citing the three checks.
  - It changes who may close escalations, so treat it as **operator-merge** (draft, `merge-desk:hold`, a decision bead).
  - Roll it with the reviewed-diff recipe (Task 13 Step 4). The live-drift diff must be exactly the one env var.

  After the roll, run the controls:
  - **Refusal:** with the owner's kd login, create `[routing-control]` escalation X (`gitlab_mr_merge`, project gardenio). Call `coordinator.sh result X success <file>` without approving: expected 403 naming D6.
  - **Execute-by refusal** on the same X: approve it as the laptop (`coordinator:laptop` is in rule 1's approvers, so `approve` returns 200). Then `coordinator.sh result X success <file>` still returns 403, because `execute_by=coordinator:merge-desk`. That's D6(a)'s execute_by check.
  - **Positive control:** a fresh `[routing-control]` escalation Z with `operation_type=manual_check` (rule 5: approvers `operator`, execute_by `coordinator:laptop`). The owner approves it with `kd escalation approve Z`, then `coordinator.sh result Z success <file>` returns 200. If this fails, the gate is refusing everything; roll back at once.
  - **Withdrawal:** the requester posts `failure` on a fresh Y (D6(c)) and gets 200.
  - Record all three codes in the plan bead comment.
  - If the merge desk's next run hits `merge-desk:close-denied`, roll the flag back with the same recipe (`false`) and read the desk's 403 body before retrying.

- [ ] **Step 7: Supersede the old work.**
  - PR #162: `GH_CONFIG_DIR=$HOME/.config/gh-ja gh pr close 162 -R joyousaversion/gardenio --comment "Superseded by escalation-routing Task 6 (/result gate, merged as <sha>) and Task 14 (gate on, <calver>)."` Re-check the repo slug with `gh pr view 162 --json url` first; the repo moved to mattbakerconsulting on 10-13.
  - `kd comment add gd-S12o3mOtxg "laptop-claude: superseded by escalation-routing Task 6 (<PR>) + Task 14 gate flip (<calver>); closing."`, then `kd close gd-S12o3mOtxg --reason "superseded by escalation-routing T6/T14"`.
  - `kd comment add gd-y5Vqo12u7z "laptop-claude: delivered as escalation-routing Task 2 (<PR>); policy bead <id>."`, then `kd close gd-y5Vqo12u7z --reason "delivered by escalation-routing T2"`.
  - Comment on epic gd-3UQKfj0nA8 that the merge desk now approves as `coordinator:merge-desk`, with a link to one desk-approved escalation.

- [ ] **Step 8: Acceptance.** Do this 24 h after Step 6.
  1. **Census:** list open/in_progress escalations grouped by `held_by` (`kd escalation list --json`, then `kd show` per id).
     - **Pass:** `held_by=operator` ≤ 4 and every one of them is a census-row-1 kind (owner hands or judgement).
     - **Fail:** any merge or `helm_deploy_values` held by `operator`. Find the rule that stamped it (`policy_rule`) and fix the policy bead or the floor. Don't hand-edit `held_by`; it's server-only.
  2. **Surfaces, in Chrome:** the owner signs in and the laptop never submits the Keycloak form.
     - The beads viewer "Needs you" count equals the census `operator` count (plus any `land-owner:<land>` held ones for a land the owner owns, D9).
     - `/garden` Mailbox shows the owner-audience set (#756's gate, unchanged by this plan).
     - A gardenio merge escalation is folded into In hand (audience `desk`), not "Needs you", and its In-hand row and Commons card name `coordinator merge-desk` (`LaneLabel`). Its Approve button *does* show for the owner, because `operator` is in rule 1's approvers. That's correct: the owner can still overrule. A missing button is the bug here.
  3. **Slack:** in a 24 h window, no operator DM for an escalation whose `audience` was not `owner` or whose `held_by` was not `operator`. Read the bridge log for `escalation_operator_dm` lines and cross-check each id's `audience` and `held_by`.
  4. **Lands:** in a tenant land with a steward, the owner calls `SetStewardDelegation`; the Commons owner menu, or `curl` with the owner's bearer, delegates `["github_pr_merge"]`. A lands-gardener's merge escalation then shows `held_by=steward:<land>`, and the operator's Slack never sees it. If no tenant land exists yet, record "not exercised: no tenant land" in the report instead of faking one.
  5. **End-to-end merge walkthrough:** one human-path gardenio PR goes:
     - crew files the escalation (`held_by=coordinator:merge-desk`);
     - the desk approves (`approved_as=coordinator:merge-desk`, `held_by=coordinator:merge-desk`);
     - the desk merges and closes `success` (200 under the gate);
     - the task auto-closes.

     Link each bead state in the report.

  Report through an Artifact (memory rule: report via Artifacts). It covers the census table before and after, the control codes, the walkthrough links and anything not exercised. Comment its link on the plan bead and set the plan's `plan_state` per the minting recipe.

---

### Task 15: In-cluster coordinator for bead-only escalations

**Phase:** 6 Rollout · **Lane:** CREW (Steps 1–3) + OPERATOR (Steps 4–5) · **Merge:** Steps 1 and 2 are **operator-merge** (they change who may write and which pod holds a credential, Global Constraint 2); Step 3 is desk · **Blocked by:** nothing in Tasks 1–14 (Steps 1–3 can start now; Step 5's result controls need Task 14 Step 6's gate flip to prove a 403)

**Why (owner decision gd-B71zgAalvu, option a, 2026-11-01 18:09Z):** "In-cluster coordinator for bead-only ops + laptop wakes for the release windows". On 11-01 the laptop slept for hours and 27 open and 62 in_progress escalations piled up. On 11-02 at about 09:10Z, the census was 81 open or in_progress escalations. 55 were held by `operator`, 23 by `coordinator:laptop` and 3 by `coordinator:merge-desk`. 53 of the 55 `operator` ones got there by the idle hand-up (D5, `handed_up_by=system:escalation-route`), and 44 of those 53 were already approved. Each was waiting on its executor, the laptop, not on the owner. Some needed only bead writes. For example, gd-bAuQ3NhJEP (close 4 fixed bugs) was approved with `web_ui` and handed up after "no coordinator:laptop activity for 6h0m0s". This task covers the in-cluster half. The laptop wake schedule is laptop-side and not in this repo.

**What it is:** a third coordinator lane, `coordinator:cluster`. It **executes** owner-approved escalations whose work is only bead writes. It **prepares and hands up** unapproved ones. It **never approves**: the decision text says it "could not approve escalations, roll helm or run terraform". Approval stays with the `operator` lane. Helm, kubectl, terraform, trigger reseals and anything on the D3 floor stay with the laptop and the owner.

**Who does what:**
- **CREW:** Steps 1–3 (kbeads, controller, kd/gb, the prompt doc).
- **OPERATOR** (laptop session, with the owner's `kd login` where a floor type is written): Steps 4–5. Step 4 is a `helm_deploy_values` escalation, never a crew edit of `gardenio/helm/**` or `deploy-values/**`.
- **OWNER:** answers Step 4's merge and roll escalation. The secret is generated where nobody sees it, as in Task 13 Step 3.

#### Design (settled here; reopen only via `gb decision create`)

**C1. The lane.** `coordinators.cluster = {"principals": ["machine:gardenio-cluster-coordinator"], "audience": "operator"}`. `operator` keeps #756's "In hand" display (D11). The lane holds nothing until Step 5 adds the policy rule, so Steps 1–4 change no live routing.

**C2. Bead-only operation types.** The work is only daemon writes (update, close, label, dependency, comment, deployment record) to beads that are not on the D3 floor and not authz config. The class is `beadOnlyOperationTypes` in `escalation_op_classes.go`:
- `kbeads_admin_write`, `kbeads_config_write`: the free-text types filers use today (9 and 1 in the 11-02 census);
- `release_record`, `release_record_backfill`: deployment records on `release` beads;
- `bead_close`, `bead_label`: new, narrower names for filers to prefer.
- `kbeads_bead_close`: the spelling `gb escalation create` files today (added in Step 1 review, PR #1972).

*Amended 11-05 (P9.5, gd-q4ebFp0p4f):* bead-only is now every type and alias of the registry classes `bead_admin` and `bead_close`, the classes D7 rule 4 sends to `coordinator:cluster` to execute, plus `release_record` and `bead_label`, less the floor types among them (`kbeads_role_update`, `user_identity_pin`). Before this, rule 4 routed `kbeads_plan_lifecycle`, `kbeads_dep_edit` and `bead_admin_close` to the lane while the grant refused them.

These are **not** bead-only, even though their work lands in beads: `trigger_*` (a reseal needs the owner's session), `kbeads_authz_*` and `beads_rbac` (who may write), and every D3 floor type. A filer can still pick the wrong type. The write grant (C4) is what actually bounds the coordinator, not the type name.

**C3. The policy rule.** It goes in after the `helm_deploy_values` rule and before the catch-all, so it becomes `escalation#6` and the catch-all moves to `#7`:
```json
{"when": {"operation_types": ["kbeads_admin_write", "kbeads_config_write", "release_record", "release_record_backfill", "bead_close", "kbeads_bead_close", "bead_label"]},
 "approvers": ["operator"], "execute_by": "coordinator:cluster", "hold_by": "coordinator:cluster"}
```
- **The D3 floor is untouched.** Floor reasons are code (`escalationForcedReason`) and match before any rule (rule 0), so a floor escalation of one of these types still goes to `operator` with `execute_by=coordinator:laptop`.
- **`approvers` is `["operator"]` only.** `coordinator:cluster` is never an approver, so Task 4's coordinator approve path refuses it (D4: the holding lane must be in `approvers`). That is the positive check that it cannot approve.
- **Existing escalations keep their stamps.** `execute_by` is frozen, and nothing re-stamps an escalation filed before Step 5. The backlog's approved items stay `coordinator:laptop`'s to execute.

**C4. Write authority: scoped to one escalation.** The machine client gets no blanket ownership. Decision gd-ZsfxvXRiyv ("tight") refused one for machine principals. The authority has three parts:
- **The stamp.** At the first approve, the server stamps `approved_targets`: the beads the escalation names in its immutable fields (`operation_spec.bead_ids`, `target_bead_id`) whose type is in `escalationExecutorTypes` (`task`, `bug`, `feature`, `chore`, `epic`) and whose project is the escalation's. A later approve never re-stamps it. *Amended in Step 1 review (PR #1972):* the first draft read untyped mention edges, but description and context stay editable until approval, so a filer could add a bead between the owner opening the card and approving it. Filers name their targets in `operation_spec.bead_ids`; the coordinator's hand-up reason repeats them. The stamp freezes them, and the escalation card can show them.
- **The grant.** A new `gardenio-machine-escalation-executor` grant (`machine_grants.go`) lets the principal update, close, label, add or remove a dependency on, or comment on bead B that it does not own, only while all of these hold:
  - it holds the claim on an open, approved escalation;
  - that escalation's `execute_by` is a coordinator lane the principal is a member of;
  - that escalation's `approved_targets` lists B.
  - B carries that escalation's one `project:` label.
- **What it never reaches.** `config`, `role`, `agent`, `trigger`, `escalation`, `decision`, `escalation_policy`, `spawn_policy`, `user`, `account` and `project` beads are never in `approved_targets`. Deployment records ride the existing `gardenio-machine-release-deploy` grant (deploy/release only).

A leaked token can therefore claim, hand up and post results only for escalations held by `coordinator:cluster`, and write only beads the owner approved by name. To stop it at once, the operator removes the client from `BEADS_ESCALATION_COORDINATOR_CLIENTS` or disables the Keycloak client. The owner can also delete the C3 rule.

**C5. Principal and in-cluster authentication.**
- **The client.** A Keycloak service-account client, `gardenio-cluster-coordinator`, mirrors `gardenio-laptop-operator` field for field (Task 13 Step 2). Its realm roles are `gardenio-operator` (to reach the `RoleOperator` escalation routes), `gardenio-machine-escalation-executor` and `gardenio-machine-release-deploy`. It is never `gardenio-admin`, so it is never the `operator` lane (D2).
- **The secret.** It lives only in the k8s Secret `gardenio-machine-gardenio-cluster-coordinator` (key `clientSecret`), generated in-cluster with `kubectl create` as in Task 13 Step 3. No SM copy is needed: nothing off-cluster uses it.
- **How it reaches a pod.** It reaches exactly one kind of pod: an agent whose role bead carries `machine_client: gardenio-cluster-coordinator`. The controller honours that field only when the client id is in the chart allowlist `agents.machineClientMounts` (env `GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS`). It mounts the secret as a 0400 file at `/run/gardenio/machine-client/secret`, never as env. This is the same pattern as `service_account` and `needs_k8s_api` (`cmd/controller/podspec_k8s_identity.go`).
- **How the agent uses it.** `kd --as-machine` and `gb --as-machine` read `GARDENIO_MACHINE_CLIENT_ID` and `GARDENIO_MACHINE_CLIENT_SECRET_FILE` and mint a client-credentials token in memory for that one invocation. The secret and the token never reach argv or a log. Without the flag, the agent's own per-agent token does its bookkeeping (`gb stop`, its task bead).

*Considered and not chosen:* `role:escalation-coordinator` as the principal (D2 allows it). It needs no Keycloak client or secret. But anyone able to mint an agent bead with that role would join the lane, and it has no equivalent of `BEADS_ESCALATION_COORDINATOR_CLIENTS` (D4's defence in depth) as a kill switch. The decision text also names a machine client.

**C6. What runs it.**
- **The role and trigger.** A role, `escalation-coordinator`, spawned only by one owner-sealed trigger. A `spawn_policy` rule names that trigger's lineage; no `gb spawn` path reaches the role.
- **When it runs.** The trigger fires on lane work, not on a clock:
  - the bridge fires it (the `gb gardener fire-now` spawn path) on an approval transition whose `execute_by` is `coordinator:cluster` (`approvalTransition`, `internal/bridge/escalations.go`), and on a create held by `coordinator:cluster`;
  - a backstop cron runs every 2 h, with a condition (`internal/scheduler/conditions`) that skips the fire when the lane holds nothing. A sleeping laptop therefore costs at most one empty-queue check every 2 h.
- **One run.** The prompt is `gardenio/docs/escalation-coordinator-prompt.md` (Step 3):
  1. `gb --as-machine escalation list --held-by coordinator:cluster --json`.
  2. **Unapproved:** check that the ask is bead-only and that every bead it names exists and is an executor type. Comment the exact writes it will make. Then `hand-up` with a plain reason, e.g. "needs your approval: closes gd-a, gd-b as fixed; on approval the cluster coordinator does it". If it is not bead-only after all, hand up with "needs the laptop: <why>".
  3. **Approved:** `claim`, then make the writes with `kd --as-machine`, only on `approved_targets`. Then `result success` with a result JSON listing each write. A refused write (403) means the approval did not cover it: post `result failure` naming the 403, and nothing else.
  4. **Digest:** one squawk listing `coordinator:laptop`-held escalations with `waiting on laptop since <held_at>`, only when that set changed since the last run. This is the "visible, not silent" part of option b.
- **Handing up to the operator.**
  - Every hand-up goes to `operator` (D5) with a pingback, so the owner's surfaces show it at once.
  - If the coordinator is down, an approved escalation it never claimed hands itself up after `BEADS_ESCALATION_LANE_IDLE_HANDUP` (D5). Nothing is hidden for longer than that.

**C7. Non-goals.** No approvals by `coordinator:cluster`. No helm, kubectl, terraform or trigger reseals. No re-stamping of existing escalations. No change to the laptop lane, the merge desk or the floor. No laptop wake schedule; that is laptop-side, per the same decision.

#### Contract additions (Global Constraint 11)

- **Escalation field:** `approved_targets`, a json list of bead ids, frozen, written by the server at approve (Step 1). It is added to Global Constraint 5's server-only list.
- **Policy:** coordinator `cluster`, lane `coordinator:cluster`.
- **kbeads Go:** `var beadOnlyOperationTypes map[string]bool`, `func isBeadOnlyOperation(op string) bool`, `var escalationExecutorTypes map[model.BeadType]bool`, the machine role `gardenio-machine-escalation-executor`.
- **Controller:** the role-bead field `machine_client`, env `GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS`, and on agent pods `GARDENIO_MACHINE_CLIENT_ID` and `GARDENIO_MACHINE_CLIENT_SECRET_FILE`.
- **CLI:** `kd --as-machine`, `gb --as-machine`.
- **Chart:** `agents.machineClientMounts` (list).

#### Steps

- [ ] **Step 1 (CREW, kbeads, operator-merge) — gd-sZNUPjzygT.**
  - Add `beadOnlyOperationTypes` (C2).
  - Add the `approved_targets` stamp in the approve path, for the human and coordinator approvals alike.
  - Add the `gardenio-machine-escalation-executor` grant (C4).
  - Tests, each with its positive control on the same fixture (Global Constraint 6):
    - a bead listed in `approved_targets` vs not listed;
    - claim held vs not held;
    - approved vs unapproved;
    - an executor type vs a `config`/`role` bead;
    - `can-approve` for a `coordinator:cluster` principal is `allowed:false` (not in `approvers`).
- [ ] **Step 2 (CREW, controller + kd/gb, operator-merge) — gd-EwfINsGbxD.**
  - Add the `machine_client` role-bead mount, gated by `GARDENIO_AGENT_MACHINE_CLIENT_MOUNTS`, with an ignored-field metric like `agentK8sIdentityIgnoredTotal`.
  - Add `--as-machine` on kd and gb (C5).
  - Tests: no mount unless the client is allowlisted; the flag sends the machine bearer; the secret is never in argv.
- [ ] **Step 3 (CREW, controller + docs, desk) — gd-Khn7cXyokc.**
  - Write `gardenio/docs/escalation-coordinator-prompt.md` (C6).
  - Add the bridge wake on the approval transition and on a create held by `coordinator:cluster`, plus the backstop condition. If the bead-query leaf cannot filter on `held_by`, add that filter.
  - Test: a cosmetic update to an approved escalation does not fire the wake (the gd-TMYvBxNY6h case).
  - *As built:* the wake is in the trigger engine, not the slack-bridge. Only `machine:gardenio-trigger-engine` may spawn on a sealed trigger's lineage (`spawn_lineage_authz.go`), so a bridge-side spawn would be refused. The trigger is `trigger:multi`: two `bead_event` entries match on an `approval_transition` payload key, which the engine derives with `approvalTransition`, and on `held_by`. The cron backstop's preflight is a `bead_query` with the new `held_by` filter. The `triggers` JSON is in the prompt doc.
- [ ] **Step 4 (OPERATOR, by escalation) — gd-yT5xAPB0uJ.** Once Steps 1 and 2 are merged, file one `helm_deploy_values` escalation, `--requesting-task gd-yT5xAPB0uJ`. It covers:
  - the KeycloakClient (C5);
  - `beads.machineClients` += `gardenio-cluster-coordinator`;
  - `beads.escalationCoordinatorClients` = `gardenio-laptop-operator,gardenio-cluster-coordinator`;
  - `agents.machineClientMounts: [gardenio-cluster-coordinator]`;
  - the in-cluster Secret.

  It is operator-merge, rolled with the reviewed-diff recipe (Task 13 Step 4) in a release window. The live-drift diff must be exactly those values, the new KeycloakClient and the two env vars.
- [ ] **Step 5 (OPERATOR, owner's `kd login` for the floor-type writes) — gd-8gfzBvgFrO.** In this order, so the lane gets no work before its runner exists:
  1. the role bead `escalation-coordinator` (`machine_client: gardenio-cluster-coordinator`);
  2. the `spawn_policy` rule for the role;
  3. the trigger, with Step 3's prompt;
  4. the C3 rule and `coordinators.cluster` on the policy bead gd-r8WKkZb7H1.

  Controls:
  - **Positive, end to end.** This one comes first; if it fails, the negatives prove nothing. File a `[routing-control]` `bead_close` escalation naming a throwaway task. Expected:
    - it stamps `policy_rule=escalation#6`, `held_by=coordinator:cluster`, `execute_by=coordinator:cluster`, `audience=operator`;
    - the coordinator hands it up;
    - the owner approves it, which stamps `approved_targets=[<task>]`;
    - the coordinator claims it, closes the task, and its result returns 200.
  - **Negatives:**
    - `can-approve` as the cluster client: `allowed:false`;
    - a write to a bead outside `approved_targets`: 403;
    - with the Task 14 gate on, a result on a `coordinator:laptop`-executed escalation: 403;
    - a `credential_rotation` escalation: floor, `held_by=operator`, never `coordinator:cluster`.

  Record the codes on gd-8gfzBvgFrO and the plan bead. The rule is in force as soon as the positive control passes. If a later run makes an unexpected write, delete the C3 rule (owner) and read the coordinator's result JSON.

---

## Appendix: contract notes from task writing

The task writers checked the contract above against `origin/main` and recorded every correction here. The tasks already use the corrected names. Where a note and the contract disagree, the note wins.


The three sets of notes below were written against `origin/main` `85dd8344`, before #756. Their line numbers are history: re-locate by the quoted snippet, never by the number. Where the revision onto #756 overturned a note, the note is struck through and the replacement is named; nothing else in them was re-checked. Paths are relative to the repo root. In this part, `kbeads/internal/server/` is written `server/`.

### Contract notes (revision onto #756, checked at `399f1ecfe`)

Each item names what #726's plan said, what #756 put on main, and what the tasks now do. Every "re-locate by" snippet in the tasks was grepped to appear exactly once in its file at `399f1ecfe`.

- **R1. Admin PATCH of a routing field is refused.**
  - *#726:* the admin exemption (`escalationFieldsExempt`) stayed as the repair path for routing fields (Tasks 1–6 note 9).
  - *Now:* the exemption also requires `escalationRouteKey`, so only the server's own routing writes pass. An admin repairs routing with the route endpoint (`gb escalation route`), which re-derives `held_by` and `audience` together. Pinned by `TestEscalationRouteFields_AdminPatchRefusedRouteRepairs` (Task 1).
- **R2. `approversNamePerson` misread lane refs as people.**
  - *#756:* `approversNamePerson` (in both `server/` and `beadsapi`) treats every non-`team:` entry as a person, so once Task 2 stamps `approvers=["coordinator:merge-desk",…]` every merge would be forced to the owner.
  - *Now:* both copies call `isLegacyPersonApprover` (`oidc:`, `slack:`, bare email; never a lane ref or `team:`). Pinned by `TestApproversNamePerson_LaneRefsAreNotPeople` (Task 1).
- **R3. One floor.** #726 had its own `operatorOnlyFloor` beside #756's `escalationAudienceForced`. Now there is one: `escalationForcedReason` (Task 2) adds D3's operation types to #756's forced-owner reasons; `operator_only` is stamped from it, and the read-side `EscalationAudienceForced` gains the same operation types plus `operator_only == "true"`. `escalationFloorOperation(op)` is the operation-type half, used by Task 11's delegation validation.
- **R4. Every `held_by` write stamps `audience` (D11).** One mapping (`laneAudience`), one stamping rule, enforced in the server on create, sweep, approve, hand-up, idle hand-up and route. The read side never recomputes `audience` from `held_by`.
- **R5. `hold_by` on rules 4 and 5 (D7, owner choice).** Without it every non-merge escalation would be held by `operator` and move back onto the owner's list, undoing #756's default. The laptop holds them staffed or not; an unstaffed holder keeps #756's `operator` audience and the operator lane can still approve.
- **R6. The sweep keeps #756's audience.** An unstamped escalation that already carries an `audience` (filed or re-routed under #756) is swept to a lane of that audience, not to the policy's lane (D11 rule 4).
- **R7. The coordinator hold rule (D4, owner choice).** A coordinator approves only what its lane (or the steward lane) holds. A laptop approval of a gardenio merge therefore needs the escalation routed to it first (`gb escalation route <id> operator`, which picks `coordinator:laptop` by D11 rule 3).
- **R8. Route on an unstamped escalation changes `audience` only**, as #756 does; the sweep picks its lane later. #756's `TestEscalationRouteEndpoint` is unchanged.
- **R9. `approval_method=commons`.** #729 writes it from the Commons card. Task 1 adds it to both `approval_method` enum lists, which #726 did not know about.
- **R10. Commons actions are live (#729, #734).** #726's Task 13 assumed `cardAction` answered 501. Task 12 now only adds the holding lane and the `can-approve` gate on the live buttons, and switches `commonsEscalations.act` to `escalationApproveGate`.
- **R11. Dropped.** #726's Task 9 (/garden Mailbox scoped by `held_by`): #756's owner-audience Mailbox already is that scope, and `held_by` adds only the land-owner fold, which Task 7 does on `/up`. #726's Task 10 Step 7 (no cross-operator DM for a coordinator approval) and its test: with #756 the DM already goes only to owner-audience escalations.
- **R12. Behaviour change: the pingback ladder warms only owner-audience escalations** (Task 9, `beadsapi.EscalationForOwner` instead of `LaneIsHuman`). A desk- or operator-audience escalation gets no automatic pingbacks; the idle hand-up (D5) is its timeout. The PR must say so.
- **R13. `LaneIsHuman` is gone.** Every "is this a person's?" question is #756's `EscalationForOwner`; `held_by` only says whose.

### Contract notes (Tasks 1–6)

Each item names what the header says, what the code says, and the fix these tasks use. Items 1–3 and 21–23 came from the Tasks 10–14 writer.

1. **Tenant land detection.**
   - *Header:* D7 says a land is a `type:project` bead "carrying `land_default_channel`".
   - *Code:* the tenant marker is `land_kind == "tenant"` (`server/lands_ledger.go:45` `const landKindTenant = "tenant"`, declared at `server/config.go:1021` `{"name": "land_kind", "type": "enum", "values": ["operator", "tenant"]}`). Operator projects can carry `land_default_channel` too.
   - *Fix:* a land is a project bead whose `land_kind == "tenant"`. `lands: ["*"]` matches only those.
2. **Tenant agents filing escalations.** Tenant agents cannot file escalations today (Track B §5.1, step 10.1 gd-g77WcV22SK). Tasks 10/11 carve out merge escalations about the agent's own land. Task 2 doesn't handle that carve-out, but it doesn't assume a tenant escalation is refused at create either: routing runs on whatever `createBead` accepts.
3. **`held_by` filter endpoint.**
   - *Header:* `GET /v1/beads?type=escalation&held_by=<ref>[,<ref>]`.
   - *Code:* `model.BeadFilter.Fields` is an exact single-value JSONB match. It can't express OR, and it can't say "absent means `operator`" (Global Constraint 7).
   - *Fix:* the filter lives on `GET /v1/escalations?held_by=<ref>[,<ref>]` (`handleListEscalations`). It filters in Go after the scope filter and treats an empty `held_by` as `operator`. Task 7's `ListEscalationsHeldBy` calls `/v1/escalations`.
4. **`ListEscalationsHeldBy` return type and owning task.**
   - *Header:* returns `[]*Bead`, and the function belongs to Task 7.
   - *Code:* beadsapi has no `Bead` type (`gardenio/controller/internal/beadsapi/client.go:1396` `type BeadDetail struct`). `gb escalation list --held-by` (Task 5) needs the call.
   - *Fix:*
     - The signature is `func (c *Client) ListEscalationsHeldBy(ctx context.Context, heldBy []string, statuses []string, limit int) ([]*BeadDetail, error)`, and it moves to Task 5 together with `ClaimEscalation` and `HandUpEscalation`.
     - Task 7 keeps `CanApproveEscalation` and `EscalationHeldBy`, typed `func EscalationHeldBy(b *BeadDetail) string`.
5. **A stamped `approvers` list would lock every human out.**
   - *Code:* `escalationApproverListed` (`server/escalation_approval_authz.go:243`) treats every non-`team:` entry as a human identity. So `"operator"` reads as a bare email, and once Task 2 stamps `approvers=["operator",…]` no human could approve anything.
   - *Fix (Task 3):* `humanSatisfiesApprovers` splits the list:
     - `operator` and `land-owner:<land>` entries go to `callerInLane`;
     - `coordinator:`, `steward:`, `requester` and `server` are never human lanes;
     - everything else, including unparseable entries (today's behaviour), keeps going to `escalationApproverListed`.
6. **Human approval narrows from operator to admin.** Today an OIDC `gardenio-operator` may approve an escalation that has no `approvers`. D2's `operator` lane is `gardenio-admin`, so after Task 2 stamps `approvers=["operator"]`, an operator-but-not-admin human loses approval on newly routed escalations. This is intended by D2; flagged so the owner knows. Unstamped legacy escalations keep today's rule until the sweep stamps them.
7. **Spawn-rule approvers are kept.**
   - *Code:* `write_authz.go` `handleEscalation` already stamps `approvers` from the matching human spawn rule (`fields["approvers"] = rule.Approvers`) and creates with `withServerStampedEscalation`.
   - *Fix:* `stampEscalationRoute` keeps a non-empty `approvers` already on the bead, unless the D3 floor applies. It still stamps `execute_by`, `held_by`, `operator_only` and `policy_rule`.
   - Spawn auto-approve later overwrites `policy_rule` with `<role>#<i>` (`server/spawn_policy.go:574` `"policy_rule":     ruleLabel,`). That value is approval provenance. Surfaces read a `policy_rule` that starts with neither `escalation#` nor `floor:` as spawn provenance.
8. **`approved_by` on `/result`.**
   - *Code:* `EscalationReaper` sends `approved_by` in `/result` `Fields` (`gardenio/controller/internal/bridge/escalation_reaper.go:833` `for k, v := range audit.fields {`). The reaper lives in the disabled `cmd/gitlab-bridge`, but it would 400 if revived.
   - *Fix (Task 1):* `/result` accepts `approved_by` in `Fields` only while the bead carries no server-stamped approval (`approved_at` empty). Every other routing or provenance field is refused there. D6 trusts `approved_at`, which no client can write after Task 1, and never trusts `approved_by`.
9. ~~**The admin exemption stays.**~~ *Superseded by R1: the exemption no longer covers routing fields.* `escalationFieldsExempt` (`server/escalation_approval_authz.go`) exempts the server-stamped context, machine replays and `callerMayApproveAsAdmin` (OIDC admin, edge, auth-off) from the stamped and immutable guards. It stays as the repair path, so Global Constraint 5's "clients can never write" means non-admin clients. The forgery tests use the static token and per-agent tokens. A test pins the admin exemption so it is a visible, deliberate choice.
10. **Coordinator clients env lands in Task 2.**
    - *Header:* `BEADS_ESCALATION_COORDINATOR_CLIENTS` belongs to Task 4.
    - *Code:* `firstStaffedLane` (create-time `held_by`, Task 2) and `callerInLane` (Task 3) both need it.
    - *Fix:* the setter `SetEscalationCoordinatorClients`, the config field and the `serve.go` wiring land in Task 2. Empty means no machine coordinators, which is inert until Task 13.
11. **`firstStaffedLane` moves to Task 2.**
    - *Header:* the function belongs to Task 3.
    - *Code:* create-time stamping (Task 2, which Task 3 depends on) needs it.
    - *Fix:* Task 2 creates `server/escalation_lane_members.go` with `firstStaffedLane`, `laneStaffed`, `tenantLandBead`, `isTenantLand` and `landDelegatesToSteward`. Task 3 adds `callerInLane`, `approveCheck` and `checkMayApprove` to the same file.
12. **Coordinator approvals and server replays.**
    - *Code:* `replayEscalation` runs an approved `create`, `update` or `close` as `withMachineWrite`, and runs `grant` with no write-policy check at all (`replayWritePolicyDenial`'s `default:` arm returns `""`).
    - *Fix (Task 4):* `coordinatorReplayDenial`:
      - refuses a coordinator outright on `grant` and on privileged spawns (`isNeverAutoApproveRole` or `roleIsPrivileged`);
      - runs `replayWritePolicyDenial` and `scopedReplayDenial` for coordinators as it does for humans. A non-OIDC caller has no ladder roles, so any `min_role` type is refused.
13. **Land-owners and the scoped-approver gate.**
    - *Code:* `authorizeEscalationVerb` runs `scopedApproverAllowed` first, and `scopedApprovableOperations` (`server/escalation_scope.go`) has `"github_pr_merge":         false`. So a land-owner, who is a scoped principal, gets 403 on D7 rule 2 whatever the lanes say.
    - *Fix:* out of scope for Tasks 1–6; Task 11 must relax this for an escalation whose `approvers` name `land-owner:<land>` for the caller's own land. Until then, Task 3's `can-approve` reports that denial honestly.
14. **The steward delegation read is built once.** `landDelegatesToSteward(ctx, land, opType) (bool, error)` (Task 2) reads the project field `land_steward_approves`; absent means `[]`. So `steward:<land>` is never staffed and never a member until Task 11 declares the field and lets the owner write it. Task 11 should call this helper, not re-implement it.
15. **Unmatched policy.** Validation requires the last rule's `when` to be empty. A policy that still matches nothing (it would have to bypass validation) fails closed: `Rule "floor:no_matching_rule"`, `approvers ["operator"]`, `execute_by "operator"`, `OperatorOnly true`.
16. **Rule numbering.** `escalation#<i>` is 1-based, so the stamped label matches D7's table (rule 1 = gardenio merges). `#0` is never stamped; the floor stamps `floor:<reason>`.
17. **Floor `execute_by`.** D7 row 0 gives `coordinator:laptop`. `evaluateEscalationPolicy` stamps it only when the policy defines a `laptop` coordinator; otherwise it stamps `operator`, so a floor escalation is never executable by nobody.
18. **D6(b), "the caller is the daemon".** The daemon's replay closes through store `CloseBead` (`replayEscalation`), never through `/result`. So (b) is implemented as `isMachineWrite(ctx)`: it can't be reached over HTTP and is kept for internal callers.
19. **D6 and unswept legacy escalations.** An escalation with an empty `execute_by` (filed before Task 2 and not yet swept) is gated as if `execute_by=operator`.
20. **`held_by=@me`.**
    - *Problem:* `gb escalation list --needs-me` needs the caller's lanes, and only the server can compute them (policy coordinators, agent role, Keycloak groups).
    - *Fix:* `held_by=@me` on `/v1/escalations` keeps the escalations whose `held_by` lane the caller is a member of (`callerInLane` per escalation, so `requester` works too). This is a new shared name, added here.
21. **D6(d) has no live caller.** `EscalationReaper` lives in the disabled `cmd/gitlab-bridge` and uses `BEADS_DAEMON_TOKEN`. The (d) branch and `BEADS_ESCALATION_RESULT_MACHINES` are kept but are unused at rollout: the env var is empty.
22. **The machine-gate inventory changes meaning.** `server/machine_gates_test.go` enumerates machine-principal gates. Row `{"http_escalations.go", `if cid := AuthIdentityFromContext(r.Context()); cid.Type == "oidc" {`, …, "only shapes the refusal message; machine is never an approver"}` stops being true in Task 4, so Task 4 rewrites that row's note and adds a machine-coordinator check. New code in Tasks 1–6 uses no `machineGatePattern` token (`authMethodOIDC|authMethodLand|roleBearing\(|Type (==|!=) "(oidc|static|fleet)"`). It uses `authMethodFromContext(ctx) != authMethodMachine`, `agentBeadIDFromContext`, `escalationApprover` and `callerMayApproveAsAdmin` instead, so `TestMachineGates_TableCoversEverySite` stays in sync.
23. **The merge desk and the result gate.** The merge desk never approves today. With Task 6's gate on, its closes would be refused, so the gate ships off (`BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL` unset = `false`), and Task 6 pins "flag off = today's behaviour" with a positive-control test.
24. **Claim and hand-up route role.** Land-owners are `RoleProjectMember` OIDC humans, and `requireRole(RoleOperator, …)` would 403 them. `claim` and `hand-up` are therefore `requireRole(RoleViewer, …)` (as `can-approve` is), with lane membership as the real gate.
25. **Hand-up at the top of the chain.** D5 doesn't say what a hand-up from `operator`, or from `land-owner:<land>` on a tenant land, does. The fix is a 409: "nothing above `<lane>`". A hand-up also clears `claimed_by` and `claimed_at`.
26. **Idle hand-up (new requirement from the Tasks 7–9 writer).** A coordinator lane can go offline. So the Task 2 sweep (extended in Task 5) automatically hands up an open escalation that meets all of these:
    - its `held_by` is a `coordinator:*` or `steward:*` lane;
    - it is not claimed;
    - its `held_by` was set more than `BEADS_ESCALATION_LANE_IDLE_HANDUP` ago (a Go duration; default `6h`; `0` disables).

    The hand-up works exactly like the manual one: it goes to `operator`, or to `land-owner:<land>` on a tenant land, with `handed_up_by="system:escalation-route"` and `hand_up_reason="no <lane> activity for <dur>"`. This needs a new server-only field, `held_at` (RFC3339 UTC). It is in Task 1's `escalationRouteFields` and is restamped every time `held_by` changes: create and sweep (Task 2), approve (Task 4), hand-up (Task 5). A coordinator-held escalation with no `held_at` gets `held_at=now` from the sweep, which starts its clock; it is not handed up. `BEADS_ESCALATION_LANE_IDLE_HANDUP` is a new env name, added to the contract here.
27. **The `held_by` filter on the client side.** The Tasks 7–9 writer asked for `ListBeadsQuery.HeldBy []string` (`gardenio/controller/internal/beadsapi`).
    - The server filter lives on `/v1/escalations` (item 3), so `ListBeadsFiltered` with `HeldBy` set delegates to `ListEscalationsHeldBy`. It refuses any `Types` other than `["escalation"]` and returns `ListBeadsResult{Beads, Total: len(Beads)}`.
    - An empty `held_by` matches `held_by=operator` (Global Constraint 7) on the server.
    - Both functions land in Task 5; Task 7 consumes them.
28. **Claim leaves `status` alone.** gardenhub reads `in_progress` as "approved", so `claim` stamps only `claimed_by`/`claimed_at`. A test pins it.
29. **Timestamps are RFC3339 UTC.** `claimed_at`, `handed_up_at` and `held_at` are `time.Now().UTC().Format(time.RFC3339)` strings, because the bridge's hand-up DM dedup compares `handed_up_at` with `operator_dm_at`.

---

### Contract notes (Tasks 7–9)

Every anchor below was checked on `origin/main` at 85dd8344. The coordinator should fold these into the Interface contract before the plan is minted.

1. **No `beadsapi.Bead` type.** The package has only `BeadDetail` (`internal/beadsapi/client.go:1396`). The contract lines change to:
   ```go
   func (c *Client) ListEscalationsHeldBy(ctx context.Context, heldBy []string, statuses []string, limit int) ([]*BeadDetail, error)
   func EscalationHeldBy(b *BeadDetail) string // "" → "operator"
   ```
   ~~`ListEscalationsHeldBy` calls `GET /v1/beads?type=escalation&held_by=…`~~ *Superseded by Tasks 1–6 notes 3 and 27: it calls `GET /v1/escalations?held_by=…` and lands in Task 5.* `EscalationHeldBy` now returns `""` when unstamped (Global Constraint 7: read `audience`), not `"operator"`.
2. **Task 5 must say how the filter treats an empty value.** `held_by=operator` has to match escalations whose `held_by` is empty or missing, per Global Constraint 7. Without that, the server filter hides every legacy escalation from the operator, which breaks Review Focus 4.
3. ~~**Task ordering for `gb`.**~~ *Resolved: both client functions are Task 5's.* Task 5's `gb escalation list --held-by` needs the client filter, but the contract puts `ListEscalationsHeldBy` in Task 7, which comes later. Task 7 below adds `ListBeadsQuery.HeldBy` and `ListEscalationsHeldBy`. If Task 5 lands first, it should add `ListBeadsQuery.HeldBy` itself, and Task 7 then skips that one step.
4. **The viewer filters on the client and never on the server.** *Revised:* Task 7 splits `ListEscalations` output with #756's `ownerEscalations`, extended by `escalationOwnerRow` (no `partitionEscalations`). That keeps the viewer correct against a daemon from before Task 5, and against the open question in issue 2. Nothing in Tasks 7–9 calls `ListEscalationsHeldBy`; its consumers are `gb` and the laptop.
5. **New shared helpers in `internal/beadsapi`** (Task 7, file `escalation_lanes.go`). They are used by the viewer, gardenhub and the bridge:
   ```go
   const (LaneOperator = "operator"; LaneRequester = "requester"; LaneServer = "server")
   func HeldByFromFields(f map[string]string) string // "" when unstamped (revised; was "" → LaneOperator); the bridge's BeadEvent has only a fields map
   func EscalationExecuteBy(b *BeadDetail) string    // "" = legacy execution_target path
   // ~~func LaneIsHuman(ref string) bool~~ — dropped (R13); EscalationForOwner answers it
   func LandOwnerLane(land string) string            // "land-owner:" + land
   func LaneLabel(ref string) string                 // "the owner", "coordinator laptop", "the acme owner", …
   ```
   Also `type CanApproveResult struct{ Allowed bool; Method, As, Reason string }`, with json tags `allowed`, `method`, `as` and `reason`.
6. **The `approve:escalation` capability no longer gates any viewer button.** The daemon's `can-approve` is the only gate (D9). The capability check survives in exactly one place: as a fallback when the daemon answers `can-approve` with a 404, meaning it was rolled before Task 3 (Task 8). Task 14 should remove that fallback. The bridge SPA (`internal/bridge/api_escalations.go`) still enforces the capability; that SPA is out of scope here.
7. **Coordinator `can-approve` decision (3).** `conversation.EscalationAddressees(heldBy, land string) []EscalationAddressee` is **defined in Task 9** (`internal/conversation/escalation_addressees.go`). Task 12 reuses it and does not redefine it. Task 9 is therefore not blocked by Task 12.
8. **Tenant land** means a project bead with `land_kind == "tenant"`. In the controller that is `beadsapi.ProjectInfo.LandKind` (`internal/beadsapi/projects.go:139`) and `IsOperatorLand()` (`projects.go:590`). It does **not** mean "the project bead has `land_default_channel`", as D7 currently says. `EscalationAddressees` takes the land name only to check that a `land-owner:<land>` lane matches the escalation's own land. It never reads `land_kind`.
9. **D6(d) has no live caller.** `BEADS_ESCALATION_RESULT_MACHINES` and the EscalationReaper (in the disabled gitlab-bridge) are not part of Task 9. The bridge's executors never write a result as a machine principal in this plan.
10. **Claim must not move `status` to `in_progress`**, or else gardenhub's `waitOf` must stop reading `in_progress` as "approved". Today `waitOf` treats `approved_at != "" || status == in_progress` as approved (`internal/gardenhub/farm.go:1199`). ~~Task 9 narrows it to `approved_at != "" || (in_progress && claimed_by == "")`.~~ *That task is dropped (R11); claim never changes `status` (Tasks 1–6 note 28), so `waitOf` needs no change.* Task 5 should still say what claim does to `status`.
11. **Timestamp format.** Task 9 dedups the hand-up DM by comparing the RFC3339 UTC strings of `handed_up_at` and `operator_dm_at` (written at `escalation_operator_dm.go`, `time.Now().UTC().Format(time.RFC3339)`). Task 5 must stamp `handed_up_at` in that same format.
12. ~~**A gap, not designed here.**~~ *Closed by the idle hand-up (Tasks 1–6 note 26).* A coordinator lane that has no live member, such as the laptop being offline, never times out to the operator. The only way up is a manual `hand-up` (Task 5), and Task 9 stops the auto-pingback ladder for coordinator-held escalations. This needs either an operator decision or a later sweep in Task 2.
13. ~~**Accountability change, flagged for the operator.**~~ *Dropped with that step (R11).* Task 9 Step 7 stops the cross-operator "someone else approved your escalation" DM when `approved_as` is a coordinator or steward lane. That DM was the accountability control from ka-0000004303. Under D4 the approval is the coordinator's own lane-authorized act, recorded as `approved_by` plus `approved_as`, not an agent answering in the operator's name. The PR must say this in its description and get an operator ack before it merges.

---

### Contract notes (Tasks 10–14)

Each issue lists the problem, the evidence, and the fix that the tasks below already use. The parent should fold the fixes into the header before minting.

1. **Tenant-land test.**
   - *Problem:* D7 says "a land is a `type:project` bead carrying `land_default_channel`". That's the wrong test. `land_default_channel` is a general project field (`kbeads/internal/server/config.go`, the `"type:project"` block). The operator's own projects can carry it too.
   - *Evidence:* the real marker is `land_kind`, an enum `["operator","tenant"]`. `lands_ledger.go` already defines `const landKindTenant = "tenant"` and uses it in `tenantLandMirrorError`.
   - *Fix:* `escalationFacts.Land` is set only when the project bead's `land_kind == landKindTenant`. Tasks 11 and 12 use that test.
2. **No `beadsapi.Bead` type.**
   - *Problem:* the controller contract has `ListEscalationsHeldBy(...) ([]*Bead, error)` and `EscalationHeldBy(b *Bead)`, but `internal/beadsapi` has no `Bead` type.
   - *Evidence:* `client.go` defines `type BeadDetail struct` (re-locate by that text).
   - *Fix:* both use `*beadsapi.BeadDetail`. Task 12 calls `beadsapi.EscalationHeldBy(*beadsapi.BeadDetail)`.
3. **Tenant agents may not file escalations at all.**
   - *Problem:* D7 rule 2 needs a `lands-gardener` to file `github_pr_merge`/`gitlab_mr_merge` escalations about its own land. Track B §5.1 lists "File escalations" under the tier's "May not", and step 10.1 (gd-g77WcV22SK, still open) says "escalations from tenant agents refused with a structured denial pointing at `#support`".
   - *Fix:* Task 10 amends §5.1 and the 10.1 row: a tenant agent may file a merge escalation (`github_pr_merge`, `gitlab_mr_merge`) about a PR in its own land's repo, and nothing else. Task 11 tells the 10.1 implementer about the carve-out in a comment on gd-g77WcV22SK.
4. **D6(d) has no live caller.**
   - *Problem:* D6(d) assumes the bridge's `EscalationReaper` closes merged escalations as a machine principal. On main it doesn't run anywhere live.
   - *Evidence:*
     - `bridge.NewEscalationReaper` is wired only in `cmd/gitlab-bridge/main.go:368` (and `cmd/escalation-reap-probe`).
     - It polls GitLab only (`GitLab: gitlabClient`) and authenticates with `BEADS_DAEMON_TOKEN`, the fleet token.
     - The chart has `gitlabBridge.enabled: false` (`values.yaml:15160`).
     - GitHub merges are closed today by the merge desk (role `reviewer`, per-agent token), which D6(a) covers once it approves first.
   - *Fix:*
     - Task 13 sets `BEADS_ESCALATION_RESULT_MACHINES` to empty, and says why in deploy-values.
     - (d) stays in the code for a future GitHub reaper on a machine token.
     - Task 13 changes the merge desk prompt to **approve before it merges or closes**, so the desk's result passes D6(a).
5. **Land owners cannot write their project bead.**
   - *Problem:* D8 says the land owner writes `land_steward_approves` through the action `steward_delegate`. But `"type:project"` has `"write_policy": {"min_role": "gardenio-admin"}`, so no PATCH from a land owner can succeed.
   - *Fix:*
     - Task 11 adds a dedicated endpoint, `POST /v1/lands/{land}/steward-delegation`, body `{"operation_types":[...]}`.
     - It is allowed to a gardenio-admin, or to a scoped principal whose tier in that land passes `tierAllows(tierOwner, model.TypeProject, actionStewardDelegate)`. That needs a new `ownerActions` row (memberActions plus `model.TypeProject: {actionStewardDelegate: true}`).
     - Writing the field by generic PATCH or create returns 400 for everyone, including admins, so the validation (subset of known op types, never a floor type) has only one way in.
     - New contract names: endpoint `POST /v1/lands/{land}/steward-delegation`; Go `actionStewardDelegate = "steward_delegate"`, `validateLandStewardApproves` (at assembly, `landStewardApproves` was dropped in favour of Task 2's `landDelegatesToSteward`); controller `beadsapi.(*Client).SetStewardDelegation(ctx, land string, ops []string) error`.
6. **Chart values for the new env vars don't exist.**
   - *Evidence:* `templates/beads/deployment.yaml` (re-locate by `BEADS_ESCALATION_APPROVER_CLIENTS`) renders `BEADS_ESCALATION_APPROVER_CLIENTS` from `.Values.beads.escalationApproverClients` and nothing else for escalations.
   - *Fix:* Task 13 adds `beads.escalationCoordinatorClients`, `beads.escalationResultMachines` and `beads.escalationResultRequiresApproval` (default `""`, `""`, `false`) and renders them only when non-empty or true.
     - This is a `gardenio/helm/**` + `deploy-values/**` PR, so the merge desk takes it as a high-risk path.
     - Because it changes who may approve, it is also **operator-merge** (Global Constraint 2). Add Task 13 to the operator-merge list.
7. **Tasks 9 and 12 both compute the escalation's addressee.**
   - *Problem:* Task 9 (bridge DM follows `held_by`) and Commons DM-first placement (§2.5, Task 12) both need "which person or people is `held_by` right now".
   - *Superseded at assembly:* Task 9 defines `conversation.EscalationAddressees(heldBy, land string) []EscalationAddressee` and Task 12 reuses it. The original proposal, kept for the record: Task 12 defines `conversation.EscalationAddressees(heldBy string, land string) []Addressee` in `internal/conversation/placement.go`, a pure mapping from a lane ref to an addressee kind:
     - `operator` → the operator seat;
     - `land-owner:<land>` → the land's owners;
     - `coordinator:*` / `steward:*` / `requester` / `server` → no human DM.
   - **The parent should tell the Task 9 writer to call this instead of defining a second one.** Whichever task lands first adds it; the other reuses it.
8. **`machine_gates_test.go` pins code lines.**
   - *Evidence:* it pins code lines, including `escalation_approval_authz.go`'s `case authMethodOIDC:`.
   - *Fix:* Task 4 (coordinator approve) must update that inventory. This is only a note for Task 4's writer; Tasks 10–14 don't touch that file.
9. **Who merges a tenant PR.**
   - *Problem:* Track B §4.2 says the steward "does not merge PRs: the tenant merges on GitHub". D7 rule 2 has `execute_by: requester`, so the gardener merges after approval.
   - *Evidence:* the Lands installation token can merge: `githubapp.LandsTokenPermissions()` grants `contents: write`, `pull_requests: write`.
   - *Fix:*
     - Task 10 rewrites the sentence: the steward never merges. A `lands-gardener` merges its own PR only after its merge escalation is approved by the steward (when delegated) or a land owner.
     - A land owner who merges on GitHub themselves closes the escalation by **rejecting** it with reason `merged on GitHub`. That's a land-owner action Commons already offers.
     - There is no GitHub reaper for lands (issue 4). An unattended merged-elsewhere escalation stays `held_by=land-owner:<land>`, never the operator.
10. **The merge desk never approves today.**
    - *Evidence:* the `gd-WksO1FSmvV` prompt closes with `gb escalation close --status success` and has no approve step.
    - *Problem:* with the D6 gate on, its result is refused unless it approved first.
    - *Fix:* Task 13's prompt edit, which must be live before Task 14 flips the gate. The prompt's own 401/403 rule (`merge-desk:close-denied`) is the visible tripwire if the order slips.

---
