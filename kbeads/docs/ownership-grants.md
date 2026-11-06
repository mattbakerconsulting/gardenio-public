# Ownership grants: an inventory of every exception to the ownership gate

**Status**: Inventory (step 1 of plan gd-fRmqrpHIwk), design (step 2, approved by the owner on gd-BAXfAAEbBS), the step 4 coverage map, step 5: the table decides, and step 6: the old role fields are gone
**Bead**: gd-UONsy7MiEv (inventory), gd-LJzuIpRwoS (design), gd-0tOwIcwaK8 (coverage), gd-8IcxHwgGF5 (step 5), gd-OsLTduO6u9 (step 6)
**Checked against**: `main` at e9de84b57 (2026-10-31)
**Code**: `internal/server/write_authz.go` `authorizeOwnership`, and the grants it calls

> **Since step 5** the branches below are gone: `authorizeOwnership` admits a write
> only when a row of the grant table does. The inventory and the step 4 map are kept as
> the record of what each row replaced. For the code as it is, read
> [Step 5: the table decides](#step-5-the-table-decides); to give a role write reach,
> read [Give a role write reach](#give-a-role-write-reach). Since step 6 a role's rows
> come only from `ownership_grants`: `bypass_ownership` and `labeled_ownership`
> (branches 14 and 15) are no longer read, gated or declared
> ([Step 6](#step-6-the-old-role-fields-are-gone)).

kbeads lets a caller update, close or delete an existing bead only when the caller
created it, holds it (assignee) or owns it (owner). On top of that rule,
`authorizeOwnership` has a series of exceptions, each added for one case. This doc
lists every branch of that function and every grant it calls into. For each one
it records:

- **Principal**: who the branch admits. The kinds are the `AuthIdentity.Type`
  values: `oidc` (a human's bearer), `edge` (a verified edge envelope), `static`
  (the shared primary token, retired in gd-ke6PC7bRto), `fleet` (a bridge token or a
  per-agent `agent-<bead id>` token), `machine` (a registered Keycloak service
  account with `gardenio-machine-*` roles), `land`, `window`, `runner`, and none.
- **Actions** and **type / label**: what it admits.
- **State**: whether it depends on the bead's state or on a relationship to
  another bead, so that a static table could not express it.
- **Origin**: the bead that added it.
- **Tests** and **metric**.
- **Class**: either **tabular** (expressible as principal × action × type ×
  label) or **dynamic** (needs a named condition evaluated against the store).

Step 2 of the plan designs a single table of grants from this inventory: see
[Design: one grant table](#design-one-grant-table).

## Contents

- [Where the gate runs](#where-the-gate-runs)
- [The branches of authorizeOwnership, in order](#the-branches-of-authorizeownership-in-order)
- [Branch details](#branch-details)
- [Machine capability grants (machine_grants.go)](#machine-capability-grants-machine_grantsgo)
- [Role grants and roleIsPrivileged](#role-grants-and-roleisprivileged)
- [Exceptions that skip the gate on the update path](#exceptions-that-skip-the-gate-on-the-update-path)
- [Findings for the design step](#findings-for-the-design-step)
- [Design: one grant table](#design-one-grant-table)
- [Step 4: every exception and the row that covers it](#step-4-every-exception-and-the-row-that-covers-it)
- [Step 5: the table decides](#step-5-the-table-decides)
- [Step 6: the old role fields are gone](#step-6-the-old-role-fields-are-gone)
- [Gardener self-idle](#gardener-self-idle)
- [Steward re-arm](#steward-re-arm)
- [Steward advice](#steward-advice)
- [Give a role write reach](#give-a-role-write-reach)

## Where the gate runs

`authorizeOwnership(ctx, action, bead)` is called from:

- `authorizeWriteForBead` (write_authz.go), after `authorizeWriteTitled` (type
  write_policy, token scopes, scoped-principal and runner label checks, frozen
  land) and `runnerProjectDenial` on the bead's `fields.project`. It is followed by the kill-switch lifecycle check, the taint gate and
  `recordEscalationCloseGrant`. Callers include close and delete (HTTP and gRPC),
  `POST /v1/beads/{id}/close`, hook emit, labels, dependencies, followers, comment
  delete and trim, the single-field endpoints and the ledger field endpoints.
- `updateBead` (beads.go) directly, as the `default` arm of a switch. The other
  arms skip the gate for narrow patches; see
  [Exceptions that skip the gate on the update path](#exceptions-that-skip-the-gate-on-the-update-path).
- `authorizeServiceFieldBypass` (service_field_policy.go), as the fallback when a
  service-field patch is refused the bypass.

The `action` values that reach it are `update`, `close`, `delete` and
`hook-emit`. Comment create never reaches it: `loadBeadForComment` and the gRPC
`AddComment` run write_policy without ownership (ka-0000005884). `hook-emit` is
only called on agent beads, which are config-kind and leave at branch 1.

A PATCH that sets `status=closed` is checked as `update`, except for a machine
principal, where `machinePatchOwnershipAction` checks it as `close`
(gd-RZJk5gspFv). So for every non-machine principal, a grant of `update` is
also a grant of close through PATCH.

## The branches of authorizeOwnership, in order

The first branch that returns nil admits the write. The numbers are used in the
rest of this doc.

| # | Branch | Principal | Actions | Type / label | State or relationship | Class |
|---|---|---|---|---|---|---|
| 0 | nil bead | any | any | — | — | guard |
| 1 | Config-kind skip | any | any | `kind=config` (agent, role, account, schedule, team, config, …) | no | tabular |
| 2 | Admin role | caller with `gardenio-admin` in its verified realm roles (an OIDC human, or a verified-v2 edge caller) | any | any | no | tabular |
| 3 | Static token (retired, gd-ke6PC7bRto) | — | — | — | — | — |
| 4a | Machine typed `Ownership` | `machine` with a role listing the type | any (update, close, delete) | per role, see [machine table](#machine-capability-grants-machine_grantsgo) | no | tabular |
| 4b | Machine `LabeledOwnership` | `machine` with a role listing type + label + action | per entry (all today: `update`) | per role: `doc` + one label | no | tabular |
| 5 | Machine `ReclaimOwnership` | `machine` with a role listing the type | `update` only | `bug`, `feature`, `chore`, `epic` (controller) | **yes**: assignee is a gone agent | dynamic |
| 6 | Privileged fleet | `fleet` with `Privileged` set in the fleet map | any | any | no | tabular |
| 7 | DM carve-out | any `fleet` | any (meant for update and close) | `dm` | no | tabular |
| 8 | Shared queue: escalations and decisions | escalation: any non-role-bearing caller (fleet incl. per-agent, machine, verified-v1 edge, runner, no-auth) or a role-bearing operator+; decision: the same, narrowed to `decisionResolverAllowed` (oidc, edge, machine, a bridge fleet, none/anonymous/empty type) | any | `escalation`, `decision` | no (land principal excluded) | tabular |
| 9 | No identity | caller with empty `Subject` | any | any | no | tabular |
| 10 | Legacy unowned bead | any | any | any | **yes**: `created_by`, `assignee`, `owner` all empty | dynamic |
| 11 | **The rule** | caller whose `Subject` equals `created_by`, `assignee` or `owner` | any | any | **yes**: the bead's own fields | dynamic (the base rule) |
| 12 | Bridge rename | `fleet` with subject `bridge:<name>` | any | any | **yes**: `created_by == "<name>-bridge"` | dynamic |
| 13 | Operator of the bead (`operated_by`) | caller whose `Subject` equals `operated_by`, or an `oidc`/`edge` caller linked to the same operator row | any | any | **yes**: `operated_by` + operators alias table | dynamic |
| 14 | Role `bypass_ownership` | per-agent fleet token whose role bead sets `bypass_ownership=true` | update, close (not delete) | any type except `decision` | no (reads the caller's own role bead) | tabular |
| 15 | Role `labeled_ownership` | per-agent fleet token whose role bead lists type + label + action | per entry: `update`, `close` | per entry; not `decision` | no (reads the caller's own role bead) | tabular |
| 16 | Steward notes | per-agent token of a lands-tenant agent holding the steward role | `update` (and `comment`, which never arrives) | `doc` titled `steward-notes:<land>` with `project:<land>`, land = caller's project | **yes**: title and project match the caller | dynamic |
| 17 | Plan scope | per-agent token bound to the plan (assignee, thread agent, plan gardener); or `machine` with `PlanScope` | update, close | `epic`, `task`, `bug`, `feature`, `chore`, `bundle`, `doc` (reactor also: `update` on the plan's gardener trigger) | **yes**: parent chain to the nearest plan, and the binding | dynamic |
| 18 | Escalation close grant | per-agent token named `escalation-<id>` | `close` only | any type except `decision`, named by the escalation | **yes**: open, approved bead-close escalation naming the bead | dynamic |
| 19 | Denial | — | — | — | — | — |
| 21 | Dispatched task | per-agent token whose open agent bead's `task_id` is the bead or its work-item parent | update, close | `task`, `bug`, `feature`, `chore` | **yes**: `task_id`, fixed after spawn, the token's project, and the bead unassigned, the caller's or a gone agent's | dynamic |

Leaving out the guard (0), the base rule (11) and the denial (19), 18 branches
admit a write the base rule would refuse. The plan epic lists 16: its list has
no entry for the no-identity skip (9), and the role `labeled_ownership` grant
(15) shipped after it was written.

Row 21 (`dispatched-task`, gd-GrJRP7kLBY) was added to the table after the
inventory, so it is not in that count.

## Branch details

### 0. nil bead

`if bead == nil { return nil }`. A guard, not a grant: every caller has already
loaded the bead and 404s on a miss.

### 1. Config-kind skip

- **Code**: `bead.Kind == model.KindConfig`.
- **Covers**: every config-kind type, whatever the caller. These are governed by
  the type's `WritePolicy` (`min_role`, `project_scoped`) in `authorizeWrite`,
  plus field-level gates (agent identity fields, role fields, team membership).
- **Origin**: B6 (the original gate; cited in the doc comment, no bead id).
- **Tests**: `TestAuthorizeOwnership_ConfigKindSkipped`,
  `TestGRPCCloseBead_ConfigKindSkipsOwnership`.
- **Metric**: none.

### 2. Admin role

- **Code**: `hasMinimumRole(RolesFromContext(ctx), RoleAdmin)`.
- **Principal**: realm roles are set only for role-bearing callers. A land
  principal never holds admin (`landPrincipalRoles`); a machine principal's
  `gardenio-admin` grants nothing (`TestAuthorizeWrite_Machine_AdminRoleGrantsNothing`).
  In practice, an OIDC human with `gardenio-admin`, or a verified-v2 edge caller
  with it (edge_auth.go stamps v2 as `authMethodOIDC` with its real roles).
- **Origin**: the original gate.
- **Tests**: `TestAuthorizeOwnership_AdminBypass`, `TestGRPCDeleteBead_AdminAllowed`.
- **Metric**: none.

### 3. Static token (retired)

- **Retired** in gd-ke6PC7bRto: the daemon no longer accepts the shared static
  token, so no request carries `Type == "static"` and the bypass
  (`id.Type == "static" && bead.Type != model.TypeDecision`) is gone, along with
  its grant rows `static-token` and `queue-escalation-static`. The row number is
  kept so the numbering below does not move.
- **Origin**: the original gate; the decision exception was gd-Du6wHclx4g.
- **Tests**: `TestOwnershipGrants_InventoryRows` ("3 retired static token").
- **Metric**: none.

### 4a. Machine typed Ownership

- **Code**: `id.Type == "machine" && machineGrantsOwnership(ctx, bead.Type)`.
- **Covers**: any action on a bead whose type is in the `Ownership` list of one
  of the machine's `gardenio-machine-*` roles. See the
  [machine table](#machine-capability-grants-machine_grantsgo).
- **Origin**: gd-Hc34EENbO9 (machine principals replace the static bypass),
  design gd-gQ4iLIpWCm, decision gd-ZsfxvXRiyv ("tight").
- **Tests**: `TestAuthorizeOwnership_Machine_TypedGrant`,
  `TestMachineGrantsOwnership_OnlyForMachineMethod`,
  `TestAuthorizeOwnership_Machine_TriggerOps`,
  `TestAuthorizeOwnership_Machine_SlackDMDelivery`,
  `TestAuthorizeOwnership_Machine_ControllerOwnership`,
  `TestMachineGrants_PlanReactorOwnership_HTTP_DeniedForWrongPrincipal`,
  `TestMachineCapabilityGrants_Golden` (pins the whole table to
  `testdata/machine_grants.golden.json`).
- **Metric**: none for the admit. A refused machine shows as
  `gardenio_authz_denials_total{principal_kind="machine:<clientId>"}`.

### 4b. Machine LabeledOwnership

- **Code**: `id.Type == "machine" && machineGrantsLabeledOwnership(ctx, action, bead)`.
- **Covers**: beads of one type carrying one label, for the listed actions.
  Today: `update` on `doc` labelled `grafana:dashboard-model` (dashboard-sync) and
  on `doc` labelled `uncovered-open-lane-rollup` (trigger-ops).
- **Origin**: gd-Hc34EENbO9 (dashboard-sync, commit bd558cdf5), decision
  gd-ZsfxvXRiyv.
- **Tests**: `TestAuthorizeOwnership_Machine_DashboardSync`,
  `TestAuthorizeOwnership_Machine_TriggerOps`.
- **Metric**: none.

### 5. Machine ReclaimOwnership

- **Code**: `id.Type == "machine" && s.machineMayReclaim(ctx, action, bead)`.
- **Covers**: `update` only, on `bug`, `feature`, `chore`, `epic` for
  `gardenio-machine-controller-ownership`.
- **State**: the bead's assignee must be a gone agent (`assigneeIsGoneAgent`): no
  `@` in the name, no open agent bead of that name, and a closed one. A lookup
  error fails closed. A PATCH to closed counts as `close` for a machine, so it
  never passes.
- **Kept across the reap** (row `machine:<role>/reclaim-reaped`, condition
  `assignee_reaped_agent`): when a reopen, park or half-reap clear removes a gone
  agent's assignee, it stamps `fields.reaped_assignee=<that agent>`. The same
  update grant then holds while the assignee is empty and `reaped_assignee`
  names an agent with a closed bead (`assigneeWasReapedAgent`). A live bead of
  the same name does not refuse it, since a resume crew minted under the dead
  crew's name is live when the controller claims its task (`ClaimSpawnTask`),
  and that claim deletes the field. An email, a name with no closed agent bead,
  a non-empty assignee or a lookup error is refused. It is the state the row
  above admitted a moment before the reap, not a wider one.
- **Origin**: gd-RZJk5gspFv, decision gd-mM3IFDGoTq ("narrow"); kept across the
  reap by gd-8Irb2vDFf7.
- **Tests**: `TestAuthorizeOwnership_Machine_ControllerReclaim`,
  `TestMachineGrantsReclaimOwnership_OnlyForMachineMethod`,
  `TestMachineGrants_ControllerReclaim_HTTP`,
  `TestAuthorizeOwnership_Machine_ControllerReclaimReaped`,
  `TestMachineGrants_ControllerClaimsReapedBug_ResumeCrewCloses`
  (machine_reclaim_test.go).
- **Metric**: none.

### 6. Privileged fleet

- **Code**: `id.Type == "fleet" && id.Privileged`.
- **Covers**: every bead, every action, delete included. `Privileged` comes from
  the operator's fleet map, never from a request. Meant for the gitlab-bridge's
  produces / triggered-by deps and `mr_url` back-fill.
- **Origin**: ka-0000001946.
- **Tests**: `TestAuthorizeOwnership_PrivilegedFleetBypass`,
  `TestAuthorizeOwnership_PrivilegedFleetAddDependency`.
- **Metric**: none.

### 7. DM carve-out

- **Code**: `id.Type == "fleet" && bead.Type == "dm"`.
- **Covers**: any fleet token, including every per-agent `agent-*` token, on any
  `dm` bead, for any action (the comment says update and close; the code does not
  restrict the action, so delete passes too).
- **Origin**: ka-0000005087. The machine form is `Ownership: dm` on
  `gardenio-machine-slack-dm-delivery`.
- **Tests**: `TestAuthorizeOwnership_FleetDMCarveout`,
  `TestAuthorizeOwnership_FleetDMCarveoutScoped`.
- **Metric**: none.

### 8. Shared queue: escalations and decisions

- **Code**: `bead.Type == escalation || (bead.Type == decision && decisionCallerMayAnswer(ctx))`,
  then `method != authMethodLand && (!roleBearing(method) || hasMinimumRole(roles, RoleOperator))`.
- **Covers**:
  - escalation: every non-role-bearing caller (static, every fleet token
    including per-agent ones, machine, a verified-v1 edge envelope, runner,
    no-auth) and a role-bearing caller (OIDC, verified-v2 or burn-in edge) with
    operator-or-above. Land principals and window principals (role-bearing,
    no operator role) are refused here and fall through.
  - decision: the same audience, narrowed by `decisionResolverAllowed` to oidc,
    edge, machine, a bridge fleet (`bridge:<name>` subject or `<name>-bridge`
    fleet, never `agent-*`) and identity types `none`, `anonymous` and empty.
    Static and per-agent tokens are out.
- **Actions**: every action, delete included.
- **Origin**: ka-0000002032 (escalations), ka-0000000798 (decisions), narrowed by
  gd-Du6wHclx4g (decisions) and commit 8ce5de1aa (land principal excluded).
- **Tests**: `TestAuthorizeOwnership_EscalationCrossOperatorClose`,
  `…_EscalationCrossOperatorUpdate`, `…_EscalationViewerDenied`,
  `…_EscalationFleetAllowed`, `…_EscalationCarveoutScoped`,
  `…_DecisionCrossOperatorAllowed`, `…_DecisionCrossOperatorUpdate`,
  `…_DecisionViewerDenied`, `…_DecisionFleetAgentDenied`,
  `TestAuthorizeOwnership_DecisionCarveOutExcludesAgentFleet`.
- **Metric**: none.

### 9. No identity

- **Code**: `id.Subject == ""`.
- **Covers**: no-auth dev mode and any caller whose identity carries no subject.
  The outer middleware decides.
- **Origin**: the original gate.
- **Tests**: `TestAuthorizeOwnership_AnonymousSkipped`.
- **Metric**: none.

### 10. Legacy unowned bead

- **Code**: `bead.CreatedBy == "" && bead.Assignee == "" && bead.Owner == ""`.
- **State**: depends on the bead having no ownership fields at all.
- **Origin**: the original gate; instrumented by ka-0000000671.
- **Tests**: `TestAuthorizeOwnership_LegacyBeadSkipped`,
  `TestRecordAuthzLegacyFallthroughEmitsCounter`.
- **Metric**: `gardenio_authz_legacy_fallthrough_total{action,bead_type}`. The
  only admit in this function with its own counter.

### 11. The base rule

- **Code**: `bead.CreatedBy == actor || bead.Assignee == actor || bead.Owner == actor`.
- **Principal**: any caller; `actor` is `id.Subject` (an email for OIDC, the
  agent name for a per-agent token, `bridge:<name>`, `machine:<clientId>`, the
  `X-Actor` claim for static).
- **Tests**: `TestAuthorizeOwnership_CreatorAllowed`, `…_AssigneeAllowed`,
  `…_OwnerAllowed`, `…_CrossOwnerDenied`,
  `…_FleetTokenMatchingSubjectAllowed`, `…_FleetTokenNotBypassed`.

### 12. Bridge rename

- **Code**: `id.Type == "fleet" && bridgeSubjectMatchesLegacyCreatedBy(actor, bead.CreatedBy)`.
- **State**: `created_by` must be `<name>-bridge` for a caller `bridge:<name>`.
  It is the base rule across a rename, not a widening.
- **Origin**: ka-0000007614.
- **Tests**: `TestAuthorizeOwnership_BridgeLegacyCreatedBy`,
  `…_BridgeLegacyCreatedByScoped`, `…_NonBridgeFleetNoLegacyMatch`,
  `TestBridgeSubjectMatchesLegacyCreatedBy`.
- **Metric**: none.

### 13. Operator of the bead (callerIsBeadOperator)

- **Code**: `bead.OperatedBy != "" && s.callerIsBeadOperator(ctx, id, bead)`.
- **Principal**: step 1 is a byte-equal match of `id.Subject` and `operated_by`
  for any identity type. Step 2 resolves an `oidc` or `edge` caller and the bead's
  operator through the operators alias table and compares the operator ids.
  Agent subjects cannot contain `:` or `@` (`validateAgentFleetRegistration`) and
  a static `X-Actor` containing `:` or `@` is ignored (`actorNamesPrincipal`,
  xactor_identity.go), so in practice only humans match, plus a runner
  principal, whose `Subject` is its human owner's identity (runner_auth.go): it
  matches `operated_by` (and the base rule) on that owner's beads.
- **State**: depends on `operated_by` (or `operated_by_id`) and on the alias table.
- **Actions**: any, delete included, on any type.
- **Origin**: ka-0000007637 (decisions), generalised to every type; alias table
  ka-0000001692.
- **Tests**: `TestAuthorizeOwnership_DecisionOperatedByAllowed`,
  `…_OperatedByAllowedForNonDecision`, `…_OperatorBypassCanonicalMatch`,
  `…_OperatorBypassDeniedDifferentOperator`, `…_OperatorBypassEdgeBareEmail`,
  `…_OperatorBypassDeniedUnlinked`, `TestOperatorAliasForIdentity`.
- **Metric**: none.

### 14. Role bypass_ownership (agentRoleBypassesOwnership)

> **Retired in step 6** (gd-OsLTduO6u9). Its reach is now the all-types row in
> `ownership_grants`; the tests below were moved to that row.

- **Code**: `action != "delete" && bead.Type != decision && s.agentRoleBypassesOwnership(ctx)`.
- **Principal**: a per-agent fleet token. The agent bead is found from the bead id
  in the fleet name; any element of its `role` list naming an open `type:role`
  bead with `bypass_ownership=true` grants it. Role beads are one global
  namespace by title.
- **Covers**: update and close on every non-decision type in the caller's project
  (project scope is enforced upstream). Holders today: the merge desk
  (reviewer), gardeners, captains.
- **Who can set it**: an OIDC admin, an auth-disabled daemon, or an approved
  escalation replay (`denyAgentIdentityFields`, agent_identity_authz.go). It makes
  the role privileged at spawn (`roleIsPrivileged`).
- **Origin**: gd-kLRIbndGNp (per-agent tokens get the static agent's bypass;
  bypass roles are privileged, PR #363); decisions excluded by gd-Du6wHclx4g.
- **Tests**: `TestAuthorizeOwnership_AgentFleetBypassIsRoleBeadField`,
  `TestAgentToken_ReviewerWritesOtherCrewsBeads`
  (agent_identity_equivalence_test.go), `TestAuthorizeOwnership_RoleBypassExcludesDelete`,
  `TestRoleBead_OwnershipGrants_AgentTokenDenied` (privileged_role_grants_test.go),
  `TestRoleBead_OwnershipGrants_MachineDenied`.
- **Metric**: none.

### 15. Role labeled_ownership (agentRoleGrantsLabeledOwnership)

> **Retired in step 6** (gd-OsLTduO6u9). Its reach is now a labelled row in
> `ownership_grants`; labeled_ownership_role_test.go was deleted with it.

- **Code**: `s.agentRoleGrantsLabeledOwnership(ctx, action, bead)`.
- **Principal**: a per-agent fleet token whose role bead holds a
  `labeled_ownership` list, e.g. `[{"type":"task","label":"thread","actions":["update","close"]}]`.
- **Covers**: the listed actions on beads of that type carrying that label.
  Delete and `decision` are refused both at write time
  (`parseRoleLabeledOwnership`) and at check time. A stored invalid value grants
  nothing.
- **Holder**: none yet. The herder is meant to hold update and close on `task`
  labelled `thread` and then drop its `bypass_ownership`; on 2026-10-31 its role
  bead still has only the bypass (gd-Wvdb3Gs2Hs applies the grant).
- **Who can set it**: as for `bypass_ownership`. It does **not** make the role
  privileged at spawn.
- **Origin**: gd-Hhp3DsjOgL (PR #1630), decision gd-MW75fl51aS.
- **Tests**: `TestAuthorizeOwnership_RoleLabeledGrant`,
  `TestRoleIsPrivileged_LabeledOwnershipDoesNotCount`,
  `TestRoleBead_LabeledOwnership_AgentTokenDenied`,
  `TestValidateRoleFields_LabeledOwnership` (labeled_ownership_role_test.go).
- **Metric**: none.

### 16. Steward notes (stewardOwnsNotes)

- **Code**: `stewardOwnsNotes(ctx, s.store, action, bead)`.
- **Principal**: a per-agent token whose agent bead's role holds the lands-tenant
  steward role (`landsTenantCallerFrom`, `isSteward`).
- **Covers**: `update` on the one `doc` titled `steward-notes:<land>` that carries
  `project:<land>`, where `<land>` is the caller's project. The code also accepts
  `comment`, but no caller passes `comment` to the gate (comment create skips
  it), so that arm is dead.
- **State**: depends on the bead's title and label matching the caller's land.
- **Origin**: gd-mN1TLhGpGe (step 9.2a), commit 652cc0e5a.
- **Tests**: `TestStewardOwnsNotes`, `TestStewardNotes_OnlyTheLandsNotesBead`.
- **Metric**: none.

### 17. Plan scope (authorizePlanScope)

- **Code**: `s.authorizePlanScope(ctx, action, bead)`, then `recordPlanScopeAdmission`.
- **Principal and binding** (plan_scope_authz.go), all on a per-agent token in
  the token's project, with the bead, the plan and the agent all in that project:
  - `plan_assignee`: the agent's name is the plan epic's assignee;
  - `plan_thread_agent`: the agent has role `thread` or `dm` and its
    `thread_id` (or `slack_thread_bead_id`) is the plan's `thread_id`;
  - `plan_gardener`: the agent has role `plan-gardener` and its `trigger_id`
    names an open gardener trigger labelled `gardener-epic:<plan>` in the plan's
    project;
  - `plan_reactor`: a machine principal with `PlanScope`
    (`gardenio-machine-plan-reactor`), any project, plus `update` on the plan's
    gardener trigger while the plan is open (`authorizePlanReactor`). The same
    role also holds typed `Ownership: epic`, and branch 4a runs first, so every
    reactor write to an epic (any epic, any project, delete included) is
    admitted at 4a and never reaches this rule or its audit row. Rule 17 is
    what admits the reactor's writes to the plan's non-epic beads and its
    gardener.
- **Covers**: update and close (never delete) on `epic`, `task`, `bug`,
  `feature`, `chore`, `bundle`, `doc` that are the plan or under it through
  parent-child edges, the nearest plan on the path winning (`planScopePlans`,
  at most 32 nodes).
- **State**: depends on the parent chain, on the plan's assignee and
  `thread_id`, on the gardener trigger's status and label, and on the agent
  bead's link fields. Those fields are locked against per-agent tokens
  (`planScopeBindingPatchDenial`).
- **Origin**: gd-Mk64Bko3XO, decision gd-7h2bWUl0Uv; reactor gd-Dl88Htipuq.
- **Tests**: `TestPlanScope_Authorize`, `TestPlanScope_OwnershipGateAndAudit`,
  `TestPlanScope_UpdatePlanFields`, `TestPlanScope_SpawnFieldsStayRefused`,
  `TestPlanScope_BindingKeysLocked`, `TestPlanScope_NestedPlanBelongsToItself`,
  `TestPlanReactor_PlanScope`, `TestPlanReactor_EnableSealedFromApproval`.
- **Metric**: no counter. Each admit writes a `mutation_history` row, field
  `authz.plan_scope`, value `<rule> plan=<id> action=<action>`, and an
  `slog.Info("ownership: plan-scoped write admitted")`. The row is written at
  admission, so it appears even when a later gate (the taint gate, say) refuses
  the write, and `recordMutationNoDibs` skips it when no operator can be
  attributed.

### 18. Escalation close grant (escalationCloseGrant)

- **Code**: `s.escalationCloseGrant(ctx, action, bead) != ""`.
- **Principal**: a per-agent token whose subject is exactly
  `agentname.EscalationExecutor("escalation-", <id>)`, and whose agent bead's
  `escalation_id` names the escalation.
- **Covers**: `close` only, on non-decision beads listed in the escalation's
  `target_bead_id` or `operation_spec.bead_ids`.
- **State**: the escalation must be open, carry a server-stamped approval
  (`approved_at`), and have `operation_type` `kbeads_bead_close` or `bead_close`.
- **Origin**: gd-Ti5JGqdhpd, decision gd-7h2bWUl0Uv.
- **Tests**: `TestEscalationCloseGrant_ExecutorClosesNamedBeads`,
  `…_TargetBeadIDAndBeadCloseSpelling`, `…_ExecutorFieldIsNotTheBinding`,
  `…_Refusals`, `…_HTTPCloseRecordsAuthority`.
- **Metric**: no counter. `authorizeWriteForBead` calls
  `recordEscalationCloseGrant` after every gate passes, which writes a
  `mutation_history` row, field `close_authority`, value `escalation:<id>`, and an
  `slog.Info`. This happens even when another branch admitted the close, but only
  on the `authorizeWriteForBead` path, not for a bead already closed, and not
  when no operator can be attributed.

### 19. Denial

An `AuthzDenial` with `Escalatable: true` that names the caller and the bead's
`created_by`, `assignee`, `owner` and `operated_by` (ka-0000005214). It counts as
`gardenio_authz_denials_total{reason="escalation_required"}` with
`principal_kind` (gd-Hk70sS1XuS). Tests: `TestOwnershipDenial_NamesResourceOwnerAndCaller`,
`TestOwnershipDenial_DistinguishesUnsetFromMismatched`, `TestDescribeBeadOwnership`,
`TestAuthorizeOwnership_ReturnsAuthzDenial`.

### 21. Dispatched task (callerDispatchedOnto)

- **Code**: row `dispatched-task`, condition `caller_dispatched_onto_bead`
  (`dispatched_task_authz.go`).
- **Principal**: a per-agent token whose agent bead is open and in the token's
  project.
- **Covers**: `update` (so `kd claim` and `kd dep add`) and `close` on the
  `task`, `bug`, `feature` or `chore` the agent bead names in `task_id`, and on
  that bead's direct children when it is itself one of those types, in the
  token's project. An epic is left to plan scope (17).
- **State**: the bead written is unassigned, assigned to the caller, or
  assigned to a gone agent (`assigneeIsGoneAgent`). Any agent may spawn a crew
  onto any task in its project, so `task_id` alone is not proof of a
  dispatch: the assignee test keeps a crew off a task a live agent or a human
  holds. `task_id` is in `planScopeBindingKeys`, so a per-agent token may not
  change it on an agent bead once spawned, on PATCH or on a field endpoint.
- **Origin**: gd-GrJRP7kLBY (programme step P-G1), escalation gd-UgoSte3qGx.
- **Tests**: `TestDispatchedTaskRow`, `TestDispatchedTaskRow_TaskIDFixedAtSpawn`,
  `TestDispatchedTaskRow_HTTP`.
- **Metric**: `gardenio_authz_ownership_admits_total{grant="dispatched-task"}`.

## Machine capability grants (machine_grants.go)

`machineCapabilityGrants` maps each `gardenio-machine-*` realm role to a
`machineCapability`. Only `Ownership`, `LabeledOwnership`, `ReclaimOwnership`,
`FieldOwnership`, `DispatchClaim` and `PlanScope` relax ownership. `Writes`, `ConfigTitles`,
`TokenMint`, `TriggerLineage` and `LandPurge` are write-policy or route
capabilities, listed here only for completeness.

| Role | Ownership (4a) | LabeledOwnership (4b) | ReclaimOwnership (5) | FieldOwnership (update path) | PlanScope (17) |
|---|---|---|---|---|---|
| `gardenio-machine-accounts` | — | — | — | — | — |
| `gardenio-machine-token-mint` | — | — | — | — | — |
| `gardenio-machine-plan-reactor` | `epic` | — | — | — | yes |
| `gardenio-machine-lands-gate` | — | — | — | — | — |
| `gardenio-machine-lands-provisioner` | `plot_application`, `land_invite`, `invite` | — | — | — | — |
| `gardenio-machine-config-seed` | — | — | — | — | — |
| `gardenio-machine-dashboard-sync` | `dashboard` | `update` on `doc` @ `grafana:dashboard-model` | — | — | — |
| `gardenio-machine-trigger-ops` | `trigger`, `task`, `bug`, `feature`, `chore` | `update` on `doc` @ `uncovered-open-lane-rollup` | — | — | — |
| `gardenio-machine-slack-config-seed` | — | — | — | — | — |
| `gardenio-machine-slack-dm-delivery` | `dm`, `permission` | — | — | — | — |
| `gardenio-machine-controller-ownership` | `task`, `advice`, `session`, `trigger` | — | `bug`, `feature`, `chore`, `epic` | `epic`: `plan_spend_usd`, `plan_spend_synced_at`, `plan_spend_crew`, `plan_spend_live`; `DispatchClaim` on `bug`, `feature`, `chore` | — |
| `gardenio-machine-controller-config-seed` | — | — | — | — | — |
| `gardenio-machine-release-recorder` | `release` | — | — | — | — |
| `gardenio-machine-github-bridge` | `merge-request` | — | — | `task`, `bug`, `feature`, `chore`, `epic`: the 17 `machineGitHubBridgeWorkFields` (`mr_url`, `forge`, harvest, review flag, `resolver_*`, merge state, conflict and overlap fields) | — |

Typed `Ownership` admits every action, delete included. Every machine grant is
tabular except `ReclaimOwnership` (gone-agent assignee) and `PlanScope` (parent
chain to a plan). `FieldOwnership` is tabular in principal × type, plus a field
list, which the planned table would need as an extra column.

## Role grants and roleIsPrivileged

A role relaxes ownership for every per-agent-token agent holding it through rows in
one field on its `type:role` bead, `ownership_grants` (step 5; the two fields it
replaced, `bypass_ownership` and `labeled_ownership`, went in step 6). It is
settable only by an OIDC admin, an auth-disabled daemon or an approved escalation
replay (`denyAgentIdentityFields`, agent_identity_authz.go), and read through
`callerOpenRoleBeads`, which fails closed on every error.

`roleIsPrivileged` (spawn_authz.go) is the spawn-side consequence. A non-human
caller spawning into, or re-roling an agent bead into, a role that is privileged
must escalate (`authorizeNonHumanSpawn`, `authorizeAgentRoleUpdate`). A role is
privileged when any element of its role list names a role bead that:

- holds the `privileged` capability tier (`roleHoldsTier`, gd-Pcyz0ftcTg; the
  legacy `privileged` flag counts, and an unparseable tier set counts), or
- holds a privileged row (`roleGrantsPrivileged`): one raised with
  `privileged`, one that reaches every type, deletes, or has no label, fields or
  condition. A value that does not parse counts as privileged.

A narrow labelled row does not count (gd-Hhp3DsjOgL): the reach is small and a
close of a thread head is undone by the next reply. A store error is treated as
privileged (gd-jTIz9PSHlK). Elements are matched one by one so `crew,infra` and
`" infra"` cannot slip past (gd-Yhbc5wdjwj).

Tests: `TestRoleIsPrivileged_AllTypesRowCounts`,
`TestAuthorizeAgentSpawn_NonHumanIntoBypassRoleEscalates`,
`TestAuthorizeAgentRoleUpdate_ReRoleIntoBypassRoleEscalates`,
`TestRoleBeadLookup_ByExactTitle_NotAListCap` (privileged_role_grants_test.go),
`TestRoleIsPrivileged_OwnershipGrantsRows`, and spawn_authz_test.go.
Metric: none for the classification; a refused spawn is an escalatable denial.

## Exceptions that skip the gate on the update path

`updateBead` (beads.go) runs a `switch` before the ownership gate. Every arm
except `default` admits a narrow patch without calling `authorizeOwnership`. The
short-title fill runs inside `default`, after the gate has refused. These are
ownership exceptions too, and the design step has to decide whether they join
the table.

| Arm | Principal | Patch admitted | Type | State or relationship | Origin | Tests | Metric | Class |
|---|---|---|---|---|---|---|---|---|
| `scopedActionReact` | scoped principal (land principal, project-member OIDC) | `fields.reactions` only, own reactions | `message` | own entries only (`scopedReactionDenial`) | scoped principals | scoped_writes_test.go | none | dynamic |
| `scopedActionJoin` | scoped principal | `fields.members` only | `channel` | `scopedMembershipDenial` decides whose entries | scoped principals | scoped_writes_test.go | none | dynamic |
| `scopedActionEditHolds` | scoped principal (the land's people) | `fields.content`, only the `## Holds` section | `doc` `steward-notes:<land>` | content diff | gd-mN1TLhGpGe | `TestStewardNotes_PeopleEditHoldsOnly`, `TestStewardNotes_DeniedEditIsAudited` | none | dynamic |
| Service-managed fields | every non-fleet, non-machine caller and a privileged fleet: always; an ordinary fleet: always, except on `trigger`/`schedule`, where it must be in `BEADS_SERVICE_FIELD_FLEETS` (and project-scoped when that is required) or fall back to the gate; a machine: always falls back to the gate | only `serviceManagedFieldsByType[type]` keys (trigger fire state, …) | per map in beads.go | no | ka-0000003039, ka-0000006237 | service_field_policy_test.go | `gardenio_service_field_crossfleet_write_total` | tabular (type × field list × fleet) |
| SCIF approval stamp | fleet subject `bridge:acme-scif` | `scifPortalStampFields()`: the Gate-1 approval keys and the Gate-2 release-stamp keys, never `requested_by` | `investigation-request` | no | ka-0000001628 | scif_approval_authz_test.go | none | tabular |
| Bridge message stamp | Slack bridge fleet token | `handled_at`, `slack_channel`, `slack_ts`, each set once | `message` | **yes**: each field empty before | gd-2IEp8qx7bq | bridge_message_stamp_test.go | none | dynamic |
| Decision artifact by seat | per-agent token whose open agent bead's `seat_key` equals `owner_seat` and whose subject occupies the seat | `artifact_status` → `submitted` only | `decision` with `required_artifact` | **yes**: seat match | gd-JPfZCDbKFp | decision_artifact_seat_test.go | none | dynamic |
| Reflag by seat | same seat binding, or the bound gardener of the open plan in the bead's `requesting_plan` (gd-GrJRP7kLBY, `TestReflagByPlanGardener`) | priority down, `reflag_*` fields (plus `pingback_*` on an escalation only), or `+escalated` label, one per patch | `decision`, `escalation`, not closed | **yes**: seat match | gd-Nu4lxqkWwL | reflag_seat_test.go | none | dynamic |
| Machine `FieldOwnership` | machine with the grant | only the listed fields | `epic` (`plan_spend_*`); github-bridge's work-bead fields on `task`, `bug`, `feature`, `chore`, `epic` (gd-tCMXlS9fXW) | no | gd-Hk70sS1XuS | `TestMachineMayStampFields_TypedToEpic`, `TestMachineGrants_ControllerPlanSpendStamp_HTTP`, `TestMachineGrants_GitHubBridge_HTTP` | none (it removed ~12.6k/day of denials) | tabular (+ field list) |
| Gardener self-idle (row `gardener-self-idle`) | per-agent token of a `plan-gardener` iteration whose `trigger_id` is this trigger | `enabled` (off only), `cron`, `wake_on`, `awaiting`; `awaiting` must name an open bead | `trigger`, `schedule` pinned `gardener-epic:<plan>` to an open plan | **yes**: trigger binding, awaited bead open | gd-V68T7v5JPP | gardener_self_idle_test.go | `gardenio_authz_ownership_admits_total{grant="gardener-self-idle"}` | dynamic (+ field list) |
| Steward re-arm (row `steward-rearm`) | per-agent token of the home garden's steward, in its project | `status` → `open` and `enabled` → `true`, nothing else | closed `trigger`, `schedule` spawning `plan-gardener`, pinned `gardener-epic:<plan>` to a building plan, with a terminate marker and a valid, fresh seal | **yes**: home steward of the land, plan building, no other sealed lane | gd-miuZqFACwL | spawn_lineage_rearm_test.go | `gardenio_authz_ownership_admits_total{grant="steward-rearm"}` | dynamic (+ field list) |
| Machine `DispatchClaim` (row `machine:controller-ownership/dispatch-claim`) | machine with the grant (the controller's claim at pod create) | `status` → `in_progress` and an agent `assignee`, optionally clearing `reaped_assignee`; nothing else | `bug`, `feature`, `chore`, open and unassigned | **yes**: the assignee's live agent bead names the bead in `task_id`, set `claim_task_at_spawn`, same project | gd-GrJRP7kLBY, decision gd-pjrfJOLdiY | `TestDispatchClaim`, `TestDispatchClaim_UpdatePath` | none | dynamic (+ field list) |
| Short-title fill (inside `default`) | per-agent token, in its project | `short_title` only, set once | any except `decision` | **yes**: stored `short_title` empty, re-checked in the write | gd-JOOXA7UqLh | short_title_fill_authz_test.go | none | dynamic |

Also outside the gate: comment create skips ownership for everyone
(ka-0000005884), and `type:team` membership edits by a team owner are a
config-kind field gate (`authorizeTeamMembershipBypass`, team_membership.go),
not an ownership exception.

## Findings for the design step

1. **Tabular today**: 1, 2, 3, 4a, 4b, 6, 7, 8, 9, 14, 15, and on the update path
   the service-field, SCIF and FieldOwnership arms. Each is a (principal kind or
   role) × action × type (× label, × field list) row. Branch 8 also needs a
   role floor (operator+ for role-bearing callers) and a principal sub-kind
   (bridge fleet), and branch 3 needs a type exclusion.
2. **Dynamic**: 5 (gone-agent assignee), 10 (no owner fields), 11 (the rule
   itself), 12 (bridge rename), 13 (operated_by + alias table), 16 (notes title =
   caller's land), 17 (plan membership and binding), 18 (approved escalation
   names the bead), and the react, join, holds, message-stamp, seat and
   short-title arms. These need named conditions in the table.
3. **Metrics are almost absent.** Only branch 10 counts its admits. Plan scope
   and escalation close leave `mutation_history` rows; every other grant admits
   silently. A shadow comparison (step 4) will need a per-branch admit counter,
   e.g. `gardenio_authz_ownership_admits_total{branch,principal_kind,action,bead_type}`.
4. **Actions are uneven.** Some admit every action including delete (1, 2, 3,
   4a, 6, 7, 8, 9, 10, 12, 13), some exclude delete (14, 15, 17), some are
   close-only (18) or update-only (4b, 5, 16). For non-machine callers a PATCH to closed is checked as
   `update`, so an update grant is a close grant. A table needs an explicit
   action set per row, and the PATCH-close rule needs deciding once for all
   principals.
5. **The DM carve-out (7) admits delete** for any fleet token, including every
   per-agent token, though its comment says update and close.
6. **The escalation arm of 8 admits every per-agent token** on every escalation,
   for every action including delete. That matches what `requireRole(RoleOperator)`
   lets through on the dedicated endpoints, which is the stated reason, but it is
   wider than any coordinating role needs.
7. **`stewardOwnsNotes` accepts `comment`**, which never reaches the gate. Harmless,
   but a table row should list `update` only.
8. **Static token on decisions**: the base rule compares against the
   caller-claimed `X-Actor`. This is documented as a residual. It goes away with
   the static token itself (P7 per-agent tokens), not with this plan.
9. **The plan reactor's epic reach is wider than rule 17 says.**
   `gardenio-machine-plan-reactor` holds typed `Ownership: epic`, which branch 4a
   applies before plan scope: any epic, any project, any action including
   delete, with no `authz.plan_scope` row. The comment "Bounded to plans by
   authorizePlanReactor" (machine_grants.go) holds for its other beads, not for
   epics. The table should give the reactor plan scope alone, or a 4a row
   limited to update and close.
10. **`roleIsPrivileged` couples spawn privilege to `bypass_ownership` only.** A
   table of role grants needs its own "makes the role privileged" flag per row,
   or a rule (for example: any row whose actions include close on a type whose
   close cannot be undone).

## Design: one grant table

**Status**: Design (step 2 of plan gd-fRmqrpHIwk, bead gd-LJzuIpRwoS). Four
choices are open and go to the owner as one decision; the
[owner's answers](#owner-decisions) section records them once answered.
Nothing here changes behaviour. Steps 3 to 5 build it.

The inventory above found 18 admitting branches, three sources of grants
(code, the machine table, role bead fields) and no shared shape. The design
replaces them with one evaluator over one list of **grant rows**. A row only
ever admits, so the order of rows never changes a verdict. A new role grant
becomes a row on that role's bead, not new code in `authorizeOwnership`.

The shape follows `action_table.go`, the scoped write table: a closed Go type,
one lookup, and a test that pins every row.

### What stays outside the table

- **The nil guard (0)** and the **config-kind skip (1)**. Config beads are
  governed by their type's write policy and field gates, not by ownership. The
  evaluator never sees them.
- **The update-path field arms**: react, join, Holds, service-managed fields,
  SCIF stamp, bridge message stamp, decision artifact and reflag by seat, and the
  short-title fill. Each one admits a patch by what the patch changes, before the
  gate runs, and has its own tests. Machine `FieldOwnership` is the exception: it
  moves in, as a row with a field list, because it already lives in the machine
  table. The rest could become rows with a field list later; this plan does not
  move them (owner choice 4).
- **Recording**: the `authz.plan_scope` and `close_authority` history rows, and
  the legacy fall-through counter, stay where they are. Conditions are pure
  reads, so the shadow evaluator in step 3 writes nothing.

### The row

```go
// ownershipGrant is one row of the grant table: it admits Actions on beads
// of Types (carrying Label, changing only Fields) for Principal, when
// Condition holds. Rows only admit; no row denies.
type ownershipGrant struct {
	ID        string          // stable, unique: "static-token", "machine:trigger-ops/ownership", "role:herder/0"
	Principal grantPrincipal  // who
	Actions   []string        // "update", "close", "delete", "hook-emit"; never empty
	Types     grantTypes      // explicit list, or All with an Except list
	Label     string          // optional: the bead must carry this label
	Fields    []string        // optional, update only: the patch changes nothing else
	Condition string          // optional: a name in ownershipConditions
	Privileged bool           // optional raise; see "Privileged at spawn"
	Origin    string          // the bead that added it
}

type grantPrincipal struct {
	Kind    string // see the table below
	MinRole string // oidc, edge: the realm role floor ("gardenio-admin", "gardenio-operator")
	Name    string // machine: the gardenio-machine-* role; agent: the role bead title
}

type grantTypes struct {
	All    bool
	Only   []model.BeadType
	Except []model.BeadType // only with All
}
```

Each request resolves the caller to the **set** of kinds whose test it passes,
from `AuthIdentity` and the auth method, never from a header. A per-agent token
is `fleet`, `agent` and `any`; a bridge is `fleet`, `fleet-bridge` and `any`; a
privileged fleet is `fleet`, `fleet-privileged` and `any`. A row matches when
its kind is in the set:

| Kind | Matches | Used by |
|---|---|---|
| `any` | every caller | the base rule, legacy unowned, bridge rename, operated_by |
| `none` | `Subject == ""` (no-auth, anonymous) | 9 |
| `oidc` | an OIDC bearer, with `MinRole` from its verified realm roles | 2, 8 |
| `edge-v2` | a verified v2 or burn-in edge envelope (role-bearing), with `MinRole` | 2, 8 |
| `edge-v1` | a verified v1 edge envelope (not role-bearing) | 8 |
| `fleet` | any fleet token, per-agent ones included | 7, 8 |
| `fleet-bridge` | a bridge fleet (`bridge:<name>` or `<name>-bridge`, never `agent-*`) | 8, 12 |
| `fleet-privileged` | a fleet in `BEADS_PRIVILEGED_FLEETS` | 6 |
| `agent` | a per-agent token; `Name` set means "whose agent bead's role list names this open role bead" | 14, 15, 16, 17, 18 |
| `machine` | a machine principal; `Name` is the `gardenio-machine-*` role | 4a, 4b, 5, FieldOwnership, 17 |
| `runner` | a runner principal | 8 |

`land` and `window` principals have no rows of their own. They reach only the
`any` rows (the base rule, legacy unowned, operated_by), which is what they
reach today.

**Actions.** Every row lists its actions. The inventory found that a PATCH to
`status=closed` is checked as `close` for a machine and as `update` for
everyone else (finding 4). The table normalises this once, for every principal:
the gate is asked about `close` whenever a patch closes an open bead
(`machinePatchOwnershipAction` loses its machine-only test). After that, a row
that lists `update` and not `close` really does not admit a close. Today only
the steward notes rule (16) grants update without close to a non-machine
caller, so the only write this refuses is a steward closing its own land's
notes doc by PATCH. Rule 15 could do the same (an entry may list `update`
without `close`), but no role bead holds such an entry today. It also
**widens** one rule: the executor of an approved close escalation (18) that
closes by PATCH is refused today, since it is checked as `update`, and would be
admitted as `close`. That path must then write the `close_authority` row too,
because `recordEscalationCloseGrant` runs only in `authorizeWriteForBead`. Both
are part of owner choice 3, with the known widenings.

**The base rule is a row**, `owner`, with condition `caller_owns_bead`
(`created_by`, `assignee` or `owner` equals the subject). This puts every
admission in one evaluator and on one metric, and lets the denial reason about
it like any other row.

### Conditions: a closed registry

A row that depends on bead state or on a relationship names a condition. The
names live in one Go map; nothing else may be named:

```go
// ownershipCondition reports whether the dynamic part of a row holds. It reads
// the store, never writes, and fails closed: an error is "does not hold" and
// is counted (gardenio_authz_ownership_condition_errors_total{condition}).
type ownershipCondition func(ctx context.Context, s *BeadsServer, c grantCaller, action string, b *model.Bead) (bool, error)

var ownershipConditions = map[string]ownershipCondition{
	"caller_owns_bead":            ..., // 11: created_by / assignee / owner == subject
	"bead_unowned":                ..., // 10: all three empty (keeps its counter)
	"bridge_created_legacy":       ..., // 12: created_by == "<name>-bridge" for bridge:<name>
	"caller_operates_bead":        ..., // 13: operated_by, through the operators alias table
	"assignee_gone_agent":         ..., // 5: assigneeIsGoneAgent
	"assignee_reaped_agent":       ..., // 5: assigneeWasReapedAgent (unassigned, reaped_assignee died)
	"steward_notes_of_callers_land": ..., // 16: doc steward-notes:<land>, project:<land>, caller is the land's steward
	"in_callers_plan":             ..., // 17: assignee, thread agent or gardener binding (plan_scope_authz.go)
	"plan_reactor_scope":          ..., // 17: the reactor: beads in or under any plan
	"plan_reactor_gardener":       ..., // 17: the reactor: the plan's gardener trigger, plan open, same project
	"named_by_approved_close_escalation": ..., // 18: escalationCloseGrant
	"listed_by_claimed_approved_escalation": ..., // 19: escalationExecutorGrant
	"decision_caller_may_answer":  ..., // 8: decisionResolverAllowed
}
```

A condition only narrows its row (it is ANDed with the row's other columns), so
adding one to a row can never widen it. Conditions are of two kinds, marked in
the registry:

- **caller-bound**: `caller_owns_bead`, `caller_operates_bead`,
  `in_callers_plan`, `steward_notes_of_callers_land`,
  `named_by_approved_close_escalation`, `listed_by_claimed_approved_escalation`,
  `bridge_created_legacy`. Each one ties
  the bead to the caller's own identity, plan, land or escalation.
- **bead-only**: `bead_unowned`, `assignee_gone_agent`, `assignee_reaped_agent`, `plan_reactor_scope`,
  `plan_reactor_gardener`, `decision_caller_may_answer`. These hold for a bead
  whoever asks, so a row narrowed only by one still reaches many callers' beads.

Role rows may name only caller-bound conditions; a bead-only condition on a role
row is refused when written. A row naming an unknown condition is refused when it is
written, and grants nothing if one is ever stored (fail closed, as
`labeled_ownership` does today).

The evaluator checks the cheap columns first (principal, action, type, label,
fields) and calls a condition only for rows that pass them, so the store reads
are the ones today's branches already make.

### Where rows live

One source per principal kind, so no row is written twice:

| Rows for | Live in | Changed by |
|---|---|---|
| `any`, `none`, `oidc`, `edge-*`, `static`, `fleet*`, `runner`, and the code-backed `agent` rows (steward notes, plan scope, escalation close) | a Go slice, `ownershipGrantRows` in a new `ownership_grants.go` | a reviewed PR; the golden file shows the diff |
| `machine` | `machineCapabilityGrants` (machine_grants.go), unchanged; the evaluator derives rows from its `Ownership`, `LabeledOwnership`, `ReclaimOwnership`, `FieldOwnership` and `PlanScope` columns | a reviewed PR, as today; the chart's tests still cross-check it |
| `agent` with a role `Name` | the role bead, in one new field `ownership_grants` (a JSON list of rows without the principal, which is the role) | OIDC admin, an auth-disabled daemon, or an approved escalation replay: the gate `bypass_ownership` and `labeled_ownership` have today (`configK8sIdentityFields`, `podIdentityKeys`, `agentIdentityFieldGrants`) |

The machine table stays the source for machines because each of its roles is a
Keycloak client role the chart provisions, and its golden is already pinned.
Re-keying it would only add a second place to keep in step.

**Role rows** (owner choice 1). The field takes the same JSON shape as a code row
minus `Principal` and `Origin`:

```json
[{"types": ["task"], "label": "thread", "actions": ["update", "close"]}]
```

Write-time validation (`validateRoleFieldsImpl`) refuses: no actions, `delete`,
type `decision` (also inside `All`, which forces `Except` to hold it), an
unknown condition, a bead-only condition, `Fields` with any action but
`update`, and unknown keys. A
stored invalid value grants nothing. Row ids are derived: `role:<title>/<index>`.

During the move the evaluator also reads the two old fields as rows:

- `labeled_ownership` entry → `{types:[t], label:l, actions:a}`;
- `bypass_ownership=true` → `{types: All except [decision], actions: [update, close], privileged: true}`.

Live state on 2026-10-31: `bypass_ownership=true` on reviewer, gardener,
plan-gardener, plan-captain, captain, release-captain and herder; no role bead
holds `labeled_ownership` yet (the herder's is gd-Wvdb3Gs2Hs, which then drops
its bypass).

Step 5 rewrites each role bead to `ownership_grants` through one owner
escalation and then stops reading the old fields. A role that today holds the
full bypass keeps it as that same row until someone narrows it, so nothing about
the merge desk, gardeners or captains changes by accident.

### Privileged at spawn

`roleIsPrivileged` today is "holds the privileged tier, or sets
`bypass_ownership`". With rows it becomes "holds the privileged tier, or holds a
privileged row". A row is privileged when **any** of these is true (owner choice 2):

- its writer set `privileged: true`;
- `Types.All` is set;
- its actions include `delete`;
- it has no `Label`, no `Fields` and no `Condition`, so it reaches every bead of
  its types.

A writer can raise a row to privileged but cannot lower it: the floor is
computed, never stored. So the herder's row (`task` with label `thread`) is not
privileged, a type-wide "update every task" row is, and the old bypass row is.
A role row narrowed only by a condition is not privileged, because role rows may
name only caller-bound conditions, which tie it to the caller's own plan, land
or escalation.

This replaces both special cases:

- `roleIsPrivileged` (spawn_authz.go) asks the role's rows instead of reading
  `bypass_ownership`. Every caller (spawn, re-role, escalation approval, scope,
  audience, lane members) picks it up unchanged.
- `landsTenantSpawnTargetDenial` (lands_tenant_authz.go) refuses a
  `lands-gardener` role bead that holds **any** row, privileged or not, or an
  `ownership_grants` value that does not parse. That is what it does today by
  refusing `bypass_ownership` and any `labeled_ownership`, an unparseable one
  included.
- `roleIsPrivileged` keeps reading closed role beads too (`roleBeadsByTitle`
  has no status filter), while the grants read only open ones
  (`callerOpenRoleBeads`). That asymmetry is the conservative one, a closed
  role bead's rows still make a spawn into it escalate, and it stays.

A row whose close cannot be undone should be raised by its writer. The
evaluator cannot know that, so it stays a review judgement, recorded on the row.

### The denial names the rows that would have allowed it

When no row admits, the evaluator still knows which rows matched the bead side
(action, type, label, fields) and failed on the principal or the condition. The
denial lists them:

- a new field on `AuthzDenial`, `allowing_grants`: up to five entries, sorted by
  id, each `{"grant": "<id>", "principal": "<kind>[:<name>]", "condition": "<name>"}`;
- one sentence appended to the message: `would be allowed for: machine
  gardenio-machine-trigger-ops, agent role herder (row role:herder/0)`.

Only rows that name a role (agent or machine) or a type list narrower than All
are listed. The rest (admin, static, privileged fleet, `none`, the base rule,
legacy, bridge rename, operated_by, escalation close) match every bead and say
nothing about this one; the message already names the bead's owners and admin. The
caller's owner fields stay in the message as ka-0000005214 requires. What is
disclosed is the shape of the table, which the golden publishes anyway, and the
names of conditions, never their inputs or results.

### The golden of the effective table

`testdata/ownership_grants.golden.json` holds every code row and every
machine-derived row, sorted by id, each with its kind, name, actions, types,
label, fields, condition, origin and **computed** privilege.
`TestOwnershipGrants_Golden` fails on any drift and `-update` rewrites it,
exactly as `TestMachineCapabilityGrants_Golden` does. A second test checks
that every machine-derived row traces to a `machineCapabilityGrants` entry, so
the two goldens cannot disagree.

Role rows are data, so they are not in the golden. Step 5 adds a read for the
whole effective table, code rows plus every open role bead's rows:
`GET /v1/ownership-grants` (operator+) and `kd admin ownership-grants`.

### Metrics

- `gardenio_authz_ownership_admits_total{grant, principal_kind, action, bead_type}`:
  every admit, by row id. Row ids are bounded (about 45 code and machine rows,
  plus `role:<title>/<n>`).
- `gardenio_authz_ownership_shadow_disagreements_total{legacy_branch, grant, action}`
  (step 3 only): a verdict the table and the old code disagree on, with
  `grant="none"` when the table denies. Each one is also logged with the bead id
  and the principal.
- `gardenio_authz_ownership_condition_errors_total{condition}`: a condition that
  failed closed on a store error.
- `gardenio_authz_legacy_fallthrough_total` is kept, emitted by `bead_unowned`.

### Every branch, and the row that covers it

| # | Branch | Row id(s) | Principal | Actions | Types / label / fields | Condition |
|---|---|---|---|---|---|---|
| 0, 1 | nil, config kind | outside the table | — | — | — | — |
| 2 | admin | `admin-oidc`, `admin-edge` | `oidc`, `edge-v2` @ `gardenio-admin` | all | All | — |
| 3 | static token (retired) | none | — | — | — | — |
| 4a | machine typed | `machine:<role>/ownership` | `machine` | all | the role's `Ownership` list | — |
| 4b | machine labelled | `machine:<role>/labeled/<n>` | `machine` | the entry's | type + label | — |
| 5 | machine reclaim | `machine:<role>/reclaim` | `machine` | update | the role's `ReclaimOwnership` list | `assignee_gone_agent` |
| 5 | machine reclaim, reaped | `machine:<role>/reclaim-reaped` | `machine` | update | the role's `ReclaimOwnership` list | `assignee_reaped_agent` |
| — | machine fields | `machine:<role>/fields/<n>` | `machine` | update | type + field list | — |
| 6 | privileged fleet | `fleet-privileged` | `fleet-privileged` | all | All | — |
| 7 | DM carve-out | `fleet-dm` | `fleet` | all (today) | `dm` | — |
| 8 | escalation queue | `queue-escalation-<kind>` for `fleet`, `machine`, `edge-v1`, `runner`, and `oidc`/`edge-v2` @ operator | each | all (today) | `escalation` | — |
| 8 | decision queue | `queue-decision-<kind>` for `oidc`/`edge-v2` @ operator, `edge-v1`, `machine`, `fleet-bridge` | each | all | `decision` | `decision_caller_may_answer` |
| 9 | no identity | `anonymous` | `none` | all | All | — |
| 10 | legacy unowned | `legacy-unowned` | `any` | all | All | `bead_unowned` |
| 11 | the base rule | `owner` | `any` | all | All | `caller_owns_bead` |
| 12 | bridge rename | `bridge-legacy-creator` | `fleet` | all | All | `bridge_created_legacy` (subject `bridge:<name>`, created_by `<name>-bridge`) |
| 13 | operated_by | `operator-of-bead` | `any` | all | All | `caller_operates_bead` |
| 14 | role bypass | `role:<title>/bypass` (read from `bypass_ownership` until step 5) | `agent` + role | update, close | All except `decision` | — |
| 15 | role labelled | `role:<title>/<n>` | `agent` + role | the entry's | type + label | — |
| 16 | steward notes | `steward-notes` | `agent` | update | `doc` | `steward_notes_of_callers_land` |
| 17 | plan scope | `plan-scope`; `machine:<role>/plan-scope` | `agent`; `machine` with `PlanScope` | update, close | `epic`, `task`, `bug`, `feature`, `chore`, `bundle`, `doc` | `in_callers_plan`; `plan_reactor_scope` |
| 17 | reactor's gardener | `machine:<role>/plan-gardener` | `machine` with `PlanScope` | update | `trigger` | `plan_reactor_gardener` |
| 18 | escalation close | `escalation-close` | `agent` | close | All except `decision` | `named_by_approved_close_escalation` |
| 19 | escalation executor | `machine:<role>/approved-targets` | `machine` with `EscalationExecutor` | update, close | `epic`, `task`, `bug`, `feature`, `chore` | `listed_by_claimed_approved_escalation` (the caller holds the claim on an open, approved, bead-only escalation whose `execute_by` is a coordinator lane it is in and whose `approved_targets` lists the bead; escalation routing Task 15, gd-sZNUPjzygT) |
| 19 | escalation executor, bundle close | `machine:<role>/approved-close` | `machine` with `EscalationExecutor` | close | `bundle` | `listed_by_claimed_approved_escalation`, and the escalation is a bead-close type (`kbeads_bead_close`, `bead_close`): only a bead-close approval stamps a bundle into `approved_targets` (gd-dNYrSP9KkW) |
| 23 | steward advice | `steward-advice` | `agent` | update, close | `advice` | `home_steward_executes_approved_advice` (the home steward holds the claim on an open, approved advice escalation of its project whose `execute_by` is `steward:<project>` and whose `approved_targets` lists the bead, and the bead is that project's advice; gd-kEis8bURm5, see [Steward advice](#steward-advice)) |

Branch 8 becomes one row per principal kind because today's test
(`!roleBearing(method) || operator+`, land excluded) is a union of kinds. The
`edge-v1` rows reproduce today's admission of a verified v1 envelope with no
role check. Branch 8 also admits identity types `none` and `anonymous`; those
carry no subject, so the `anonymous` row (9) already admits them on every type
and branch 8 needs no row for them. Window and burn-in edge callers hold no
operator role, so no branch 8 row admits them, as today.

### Known widenings: reproduce, then tighten

Step 4 has to drive the shadow diff to zero, so its rows reproduce today's
verdicts exactly, the widenings included. Four rows are wider than any caller
needs (findings 5, 6, 9 and the PATCH rule):

| Row | Today | Tightened |
|---|---|---|
| `fleet-dm` | any fleet token, per-agent ones included, may **delete** any dm | update, close |
| `queue-escalation-fleet` | any agent token may **delete** any escalation | update, close |
| `machine:plan-reactor/ownership` | the reactor may update, close and **delete** any epic in any project | dropped; `plan_reactor_scope` already covers its plans' epics |
| PATCH close as `update` | an update grant is a close grant for non-machines; an escalation executor's PATCH close is refused | close is checked as `close` for everyone: steward PATCH close refused, executor PATCH close admitted and recorded as `close_authority` |

Owner choice 3 settled when these tighten: after the table is authoritative,
each as a one-row change with its own PR and a day's admit metric showing nobody
uses it. Step 4 reproduces all four exactly, so the shadow comparison can reach
zero.

### Steps 3 to 5 against this design

- **Step 3** (gd-Ccp01OYo3N): `ownership_grants.go` with the row type, the
  registry, the code rows, the machine derivation, the golden and the metrics.
  `authorizeOwnership` returns which branch admitted; the evaluator runs after it
  and only counts and logs a disagreement.
  **As built**: `authorizeOwnershipBranch` is today's code, unchanged except
  that it names the admitting branch; `authorizeOwnership` hands that to
  `shadowOwnershipGrants` (`ownership_grants_shadow.go`) and returns today's
  verdict. Step 3 already reads `bypass_ownership` and `labeled_ownership` as
  role rows (`roleOwnershipGrants`), so the counter is meaningful from the first
  deploy. The evaluator tries the rows that read no store first, then the
  caller's role rows, then the rest, so a write the base rule admits costs no
  extra read; a denial repeats today's reads once. `BEADS_OWNERSHIP_SHADOW=off`
  turns the evaluator off without a release. `TestOwnershipGrants_InventoryRows`
  pins one case per branch to its legacy branch and its row, and
  `TestOwnershipGrants_AgreeAcrossCallerMatrix` checks about 8,000 caller × type
  × owner × action cells agree. The suite's own fixtures disagree only where a
  test sets an `AuthIdentity` without an auth method, which no request path does.
- **Step 4** (gd-0tOwIcwaK8): fill the rows until the disagreement counter reads
  zero for two days (step 3 already reads the two old fields as rows). The
  `ownership_grants` field moved to step 5; see
  [What step 4 defers to step 5](#what-step-4-defers-to-step-5).
- **Step 5** (gd-8IcxHwgGF5): add `ownership_grants` to the role bead schema,
  enforce the table, delete branches 2 to 18, switch
  `roleIsPrivileged` and the lands-gardener check to rows, add
  `allowing_grants`, migrate the role beads in one owner escalation, stop
  reading `bypass_ownership` and `labeled_ownership`, and write the runbook
  "Give a role write reach".

### Owner decisions

The owner approved all four recommendations on decision gd-BAXfAAEbBS
(option `recommended`, resolved 2026-10-31 09:28Z). Steps 3 to 5 build to these:

1. **Where agent role rows live: a field on the role bead.** `ownership_grants`
   on the role bead, gated like `bypass_ownership` (OIDC admin or an approved
   escalation), one place per role. Not chosen: one `type:grant` config bead per
   row, which would have given each grant its own history and a per-project
   scope, at the cost of a second lookup per write and a new type to gate.
2. **How a row becomes privileged at spawn: computed.** The floor above (All,
   delete, or no label, fields or condition) decides; a writer may raise a narrow
   row to privileged but may not lower a broad one. Under it the herder's
   `update`/`close` on task beads labelled `thread` is not privileged, and
   today's full bypass is. Not chosen: an explicit per-row flag with no rule, or
   every role row counting as privileged.
3. **The known widenings: reproduce first, tighten later.** Step 4 reproduces
   the dm delete, escalation delete, plan-reactor epic reach and PATCH close as
   `update` exactly, so the disagreement counter can reach zero. Each tightens
   after step 5, as its own small PR, once its admit metric shows nobody uses
   it. Not chosen: tightening them inside step 4 as declared disagreements.
4. **The update-path field arms: stay outside.** React, join, Holds,
   service-managed fields, SCIF stamp, bridge message stamp, decision artifact
   and reflag by seat, and the short-title fill keep their own arms; only machine
   `FieldOwnership` (the `plan_spend` stamps) moves in as a row with a field
   list. Not chosen: folding the service-field and SCIF arms into rows too.

## Step 4: every exception and the row that covers it

**Bead**: gd-0tOwIcwaK8. **Checked against**: `main` at 757a487a0 (the step 3
table, PR #1651), plus this step's test changes.

Each of the 18 admitting branches of the inventory now has a row in the
as-built table (`ownership_grants.go`, pinned by
`testdata/ownership_grants.golden.json`). The tabular ones are rows with no
condition, and each dynamic one is a row narrowed by a named condition from
`ownershipConditions`. **Legacy branch** is the `legacy_branch` label the
disagreement counter carries for it. **Pinned by** is the case in
`TestOwnershipGrants_InventoryRows` that asserts both the branch and the row.

| # | Exception | Class | Legacy branch | Row id(s), as built | Condition | Pinned by |
|---|---|---|---|---|---|---|
| 1 | Config-kind skip | outside the table | `config-kind` | none: `evaluateOwnershipGrants` and the shadow return before the rows for `kind=config` | — | `0 and 1 stay outside the table` |
| 2 | Admin role | tabular | `admin` | `admin-oidc` (`oidc` @ `gardenio-admin`), `admin-edge` (`edge-v2` @ `gardenio-admin`) | — | `2 admin oidc`, `2 admin edge v2` |
| 3 | Static token | tabular | `static-token` | `static-token` (all except `decision`) | — | `3 static token`, `3 static token not on a decision` |
| 4a | Machine typed `Ownership` | tabular | `machine-ownership` | `machine:<role>/ownership`, one per role with an `Ownership` list: controller-ownership, dashboard-sync, github-bridge, lands-provisioner, plan-reactor, release-recorder, slack-dm-delivery, trigger-ops | — | `4a machine typed`; `17 plan scope` (reactor delete on an epic) |
| 4b | Machine `LabeledOwnership` | tabular | `machine-ownership` | `machine:dashboard-sync/labeled/0` (`doc` @ `grafana:dashboard-model`), `machine:trigger-ops/labeled/0` (`doc` @ `uncovered-open-lane-rollup`) | — | `4b machine labelled`, `4b machine labelled, not close` |
| 5 | Machine `ReclaimOwnership` | dynamic | `machine-reclaim` | `machine:controller-ownership/reclaim` and `/reclaim-reaped` (update on `bug`, `chore`, `epic`, `feature`) | `assignee_gone_agent`, `assignee_reaped_agent` | `5 machine reclaim` |
| 6 | Privileged fleet | tabular | `fleet-privileged` | `fleet-privileged` | — | `6 privileged fleet` |
| 7 | DM carve-out | tabular | `fleet-dm` | `fleet-dm` (every action, delete included: known widening) | — | `7 dm carve-out` |
| 8 | Shared queue, escalations | tabular | `queue` | `queue-escalation-{static,fleet,machine,edge-v1,runner}`, and `queue-escalation-{oidc,edge-v2}` @ `gardenio-operator` | — | `8 escalation, *` (seven cases, land and viewer refused) |
| 8 | Shared queue, decisions | tabular + a bead-only condition | `queue` | `queue-decision-{edge-v1,machine,fleet-bridge}`, and `queue-decision-{oidc,edge-v2}` @ `gardenio-operator`; `none`/`anonymous` callers are admitted by `anonymous` (9) | `decision_caller_may_answer` | `8 decision, *` (five cases, agent token refused) |
| 9 | No identity | tabular | `anonymous` | `anonymous` (kind `none`) | — | `9 no identity` |
| 10 | Legacy unowned bead | dynamic | `legacy-unowned` | `legacy-unowned` | `bead_unowned` | `10 legacy unowned` |
| 12 | Bridge rename | dynamic | `bridge-legacy-creator` | `bridge-legacy-creator` | `bridge_created_legacy` | `12 bridge rename` |
| 13 | Operator of the bead | dynamic | `operator-of-bead` | `operator-of-bead` | `caller_operates_bead` | `13 operated_by` |
| 14 | Role `bypass_ownership` | tabular (role row) | `role-bypass` | `role:<title>/bypass`, read from the role bead by `roleOwnershipGrants` (update, close; all except `decision`; raised to privileged) | — | `14 role bypass` |
| 15 | Role `labeled_ownership` | tabular (role row) | `role-labeled` | `role:<title>/<n>`, one per entry, read by `roleOwnershipGrants` | — | `15 role labelled` |
| 16 | Steward notes | dynamic | `steward-notes` | `steward-notes` (update only, on `doc`) | `steward_notes_of_callers_land` | `16 steward notes` |
| 17 | Plan scope | dynamic | `plan-scope` | agents: `plan-scope`; the reactor: `machine:plan-reactor/plan-scope` and `machine:plan-reactor/plan-gardener` (update on `schedule` or `trigger`, the two types `isGardenerBead` accepts) | `in_callers_plan`; `plan_reactor_scope`; `plan_reactor_gardener` | `17 plan scope` |
| 18 | Escalation close grant | dynamic | `escalation-close` | `escalation-close` (close only; all except `decision`) | `named_by_approved_close_escalation` | `18 escalation close` |
| 19 | Escalation executor grant | dynamic | `escalation-executor` | `machine:escalation-executor/approved-targets` (update, close; executor types only), `machine:escalation-executor/approved-close` (close; `bundle`, bead-close escalations only) | `listed_by_claimed_approved_escalation` | `TestEscalationExecutorGrant_*` (escalation_executor_grant_test.go) |
| 23 | Steward advice | dynamic | `steward-advice` | `steward-advice` (update, close; `advice` only) | `home_steward_executes_approved_advice` | `TestHomeStewardAdvice_*` (escalation_steward_advice_test.go) |

The base rule (11) is the row `owner` with condition `caller_owns_bead`. The
machine `FieldOwnership` arm of the update path is the row
`machine:controller-ownership/fields/0`; it never matches inside
`authorizeOwnership`, which does not see the patch, so it is in the table for
the golden and is still decided by `machineMayStampFields`. The other
update-path arms stay outside (owner choice 4).

One row never admits today: `queue-escalation-static`, because
`static-token` already admits every non-decision bead. It keeps the static
token on the escalation queue once `static-token` goes with P7.

The four known widenings (`fleet-dm` delete, `queue-escalation-fleet` and
`-static` delete, `machine:plan-reactor/ownership`, PATCH close checked as
`update`) are reproduced exactly, as owner choice 3 requires. Each tightens
after step 5.

### Which callers a request path builds

The legacy branches and the table read the caller differently. The legacy code
reads the realm roles on their own (admin, 2), `id.Type` alone (static 3, the
fleet arms 6, 7 and 12), and the auth method alone in the queue (8:
`!roleBearing(method)` admits any caller that is not role-bearing). The table
resolves the caller once, from `id.Type` and the auth method together
(`resolveGrantCaller`). The two agree whenever the identity type and the method
are the pair a request path stamps. These are every such pair in
`internal/server` today:

| Identity type | Auth method | Stamped by |
|---|---|---|
| `oidc` | `authMethodOIDC` | the gRPC and HTTP OIDC paths (interceptors.go) |
| `edge` | `authMethodOIDC` (verified v2, burn-in) or `authMethodEdge` (verified v1) | `EdgeAuthMiddleware` (edge_auth.go) |
| `static`, `fleet` | `authMethodStatic` over HTTP; none over gRPC | interceptors.go; `escalation_spawn_task_claim.go` (fleet, static) |
| `machine` | `authMethodMachine` | `machineContext` (machine_identity.go), HTTP only |
| `runner` | `authMethodRunner` | runner_auth.go; `runnerOwnerWriteContext` (no method) writes only config-kind beads |
| `land`, `window` | `authMethodLand`, `authMethodWindow` | land_principal.go, window_principal.go |
| `none` | `authMethodNone` | an auth-disabled daemon (interceptors.go) |
| `server` | `authMethodStatic` | `auditRecorderContext`, which only creates audit beads |

`TestOwnershipGrants_AgreeAcrossCallerMatrix` holds a caller for each pair,
the gRPC static, fleet and bridge tokens without a method, a privileged fleet,
an HTTP per-agent token, a roleless OIDC caller and the auth-disabled `none`
included, and the two verdicts agree on every cell.

### The 10 disagreements in the step 3 suite: fixture artefacts

Step 3 reported 10 disagreements across the `internal/server` suite, from two
tests. Each came from an `identityMutator` fixture that set an `AuthIdentity`
with no auth method, a pair no request path builds:

| Test | Caller fixture | Count | Legacy | Table | Why they split |
|---|---|---|---|---|---|
| `TestDecisionQuestion_ImmutableOnGenericWrites` | `oidc admin`: type `oidc`, no method, no roles | 3 (PATCH prompt, PATCH options, close body) | `queue` admits | `none` | No method is not role-bearing, so the queue admitted it as a service token. The table's `oidc` rows need the operator role. |
| same | `machine`: type `machine`, no method | 3 | `queue` admits | `none` | The queue admitted it as a service token. The table counts a caller as `machine` only under `authMethodMachine`, as `machineGrantsOwnership` does. |
| `TestDecisionAnswerWrites_RestampResolver` | `admin`: type `edge`, roles `gardenio-admin`, no method | 2 (two closes) | `admin` admits | `none` | Branch 2 reads roles whatever the method. The table counts an edge caller as `edge-v2` only under a role-bearing method, which is what `EdgeAuthMiddleware` stamps on a v2 envelope that carries roles. |
| same | `machine`: type `machine`, no method | 2 (close, PATCH) | `queue` admits | `none` | as above |

None of them is a gap in the rows. On a pair no request path builds, the table
is the stricter of the two and fails closed, which is the side to be on when
step 5 makes it authoritative. The fix is in the fixtures. Both tests now stamp
the method the real path stamps (`withMethod` in decision_resolver_stamp_test.go),
and `oidc admin` holds the admin role its name says. They pass unchanged and
log no disagreement. No row or condition changed.

### What step 4 defers to step 5

The `ownership_grants` field on the role bead (owner choice 1) is not added in
step 4. In shadow, a row that only the table can read is a disagreement by
construction: the table admits and the legacy code refuses. It would also need
`roleIsPrivileged` and the lands-gardener refusal to read it on the day it
could first be set. It lands with step 5 (gd-8IcxHwgGF5), in the same change
that makes the table authoritative and rewrites the role beads.

### Watching production

The doc's map is complete offline. The rest of step 4 is the live counter:
`gardenio_authz_ownership_shadow_disagreements_total` must read zero across two
days of normal traffic (herder, gardener, steward, bridge and merge-desk
writes) after the step 3 build is deployed (helm_deploy gd-x9e7P1ZY9o). Each
disagreement seen there is recorded below with its root cause and fix.

| Window | Disagreements | Root cause and fix |
|---|---|---|
| 2026-10-31 16:50Z to 2026-11-02 05:11Z (36h, six beads pods) | 0 | none needed |

**Watch log** (gd-qC3QJ4B1Nb, read 2026-11-02 05:11Z from VictoriaMetrics
through the Grafana datasource proxy, and from the live pod's `/metrics`).
The step 3 build went live at 16:50Z on 10-31, the first sample of
`gardenio_authz_ownership_admits_total`. The bead set a 48h clock. The owner
rule of 2026-11-02 (CLAUDE.md "No bake-in periods") replaces it with
evidence, which is below.

- **Disagreements: none.** `count_over_time(gardenio_authz_ownership_shadow_disagreements_total[4d])`
  returns no series, across all six beads pods that served since the roll
  (`gardenio-beads-65d465cf9f-zrp4c`, `-68588495c6-scww9`, `-68588495c6-wjf8h`,
  `-78f5b4d5df-984md`, `-8b9f8b5bd-6l2fg`, `-69568c68f5-bbms5`). A counter
  vector with no observation exports no series, and the live pod's
  `/metrics` has 25 admits series and none for disagreements. The shadow ran
  the whole time: it is what increments the admits counter, and
  `BEADS_OWNERSHIP_SHADOW` is unset. `gardenio_authz_ownership_condition_errors_total`
  has no series either, so no condition failed closed on a store error.
- **Traffic covered: about 64,000 admitted checks plus 238 denials.** The
  admits by row (`increase(...[3d])`) cover every writer the step named:

  | Writer | principal_kind | Row | Admits |
  |---|---|---|---|
  | controller | `machine:gardenio-controller` | `machine:controller-ownership/ownership`, `/reclaim`, `machine:lands-provisioner/ownership` | 30,593, 3, 1 |
  | trigger engine | `machine:gardenio-trigger-engine` | `machine:trigger-ops/ownership`, `owner`, `/labeled/0` | 12,398, 736, 25 |
  | crews and other agents | `agent` | `owner` | 11,040 |
  | any agent on the escalation queue | `agent`, `fleet` | `queue-escalation-fleet` | 5,280, 715 |
  | gardener | `agent` | `role:plan-gardener/bypass` | 1,847 |
  | merge desk (role `reviewer`) | `agent` | `role:reviewer/bypass` | 959 |
  | operator | `human` | `admin-oidc`, `queue-decision-oidc` | 349, 1 |
  | plan reactor (beads-viewer) | `machine:gardenio-beads-viewer` | `machine:plan-reactor/ownership` | 139 |
  | bridge (fleet token) | `fleet` | `owner`, `queue-escalation-fleet` | 95, 715 (above) |
  | herder | `agent` | `role:herder/0` | 75 |
  | static token | `static` | `static-token` | 61 |
  | grafana bridge | `machine:gardenio-grafana-bridge` | `machine:dashboard-sync/ownership`, `/labeled/0` | 2, 2 |
  | steward | `agent` | `steward-notes` | 2 |
  | release captain | `agent` | `role:release-captain/bypass` | 1 |

- **Enforcement unchanged.** `gardenio_authz_denials_total` over the 36h
  after the roll is 238, against 13,108 over the 36h before. The drop is
  the `escalation_required` refusals of `update` on `epic` (12,889 before,
  8 after): the `plan_spend_*` loop that gd-Hk70sS1XuS (4cab6beda, controller
  `FieldOwnership`) fixed in the same roll. The shadow cannot cause it,
  because it never changes the verdict. Leave out `update` on `epic` and the
  rest is 219 before and 230 after, the same rate.

Rows not exercised in the window: the edge, runner and land rows, and the
dynamic `legacy-unowned`, `bridge-legacy-creator`, `operator-of-bead`,
`plan-scope` and `escalation-close` rows. None of them was a legacy admit the
table refused, because that would have counted as a disagreement. Each is
still pinned offline by `TestOwnershipGrants_InventoryRows`. Step 5
(gd-8IcxHwgGF5) is unblocked on this evidence.

## Step 5: the table decides

**Bead**: gd-8IcxHwgGF5. Unblocked by the step 4 watch above (zero disagreements in
36h and about 64,000 checks).

### What changed

- **`authorizeOwnership` enforces the table.** It returns nil for a nil or
  config-kind bead, and otherwise admits only when `evaluateOwnershipGrants` finds a
  row. Branches 2 to 18 and `authorizeOwnershipBranch` are deleted, with the helpers
  only they called (`agentRoleBypassesOwnership`, `agentRoleGrantsLabeledOwnership`,
  `machineGrantsOwnership`, `machineGrantsLabeledOwnership`, `machineMayReclaim`,
  `machineGrantsReclaimOwnership`). The shadow evaluator, its disagreement counter and
  `BEADS_OWNERSHIP_SHADOW` are deleted too; `gardenio_authz_ownership_admits_total`
  and `gardenio_authz_ownership_condition_errors_total` stay
  (`ownership_grants_metrics.go`).
- **Records stay with the rows.** A condition whose admission leaves a record
  returns it, and `authorizeOwnership` runs it once that row has admitted:
  `bead_unowned` counts `gardenio_authz_legacy_fallthrough_total`, and
  `in_callers_plan`, `plan_reactor_scope` and `plan_reactor_gardener` write the
  `authz.plan_scope` history row. `close_authority` is still written by
  `authorizeWriteForBead` after every gate.
- **Row order.** The evaluator tries rows with no condition, then rows whose
  condition reads no store, then the caller's role rows, then the rows whose
  condition reads the store. Order changes only which row is credited in the admit
  metric, never the verdict.
- **The denial.** The message still names the caller and the bead's `created_by`,
  `assignee`, `owner` and `operated_by` (ka-0000005214). It gains
  `allowing_grants` on the 403 body (up to five rows, sorted by id, each
  `{grant, principal, condition}`) and one clause at the end of the message,
  `; would be allowed for: agent role herder (row role:herder/0), ...`. Only rows
  that name a role (a machine role or a role bead) or reach fewer than all types are
  listed; the rest match every bead. Role rows are read from every open role bead,
  on a denial only.
- **`ownership_grants` on the role bead** (owner choice 1). A JSON list; each entry
  is a row without its principal (the role) or id (`role:<title>/<index>`):
  `types` (a list, or `["*"]` for every type, which then needs `except` to name
  `decision`), `except`, `label`, `actions` (`update`, `close`), `condition` (a
  caller-bound one only) and `privileged` (a raise). Unknown keys, delete,
  hook-emit, a decision type, an unknown or bead-only condition and more than 32
  rows are refused at write time (`parseRoleOwnershipGrants`, run by
  `validateRoleFieldsImpl`); a stored invalid value grants nothing. It is gated
  like the other authority grants: an OIDC admin, an auth-disabled daemon or an
  approved escalation replay (`configK8sIdentityFields`, `podIdentityKeys`,
  `agentIdentityFieldGrants`).
- **Privilege is computed from rows** (owner choice 2). `roleIsPrivileged` asks
  `roleGrantsPrivileged`: a row is privileged when raised, when it reaches every
  type, when it deletes, or when it has no label, fields or condition. A value that
  does not parse counts as privileged. `landsGardenerRoleDenial` refuses a
  `lands-gardener` role bead holding any row or a value that does not parse.
- **The old fields are a fallback, retired on an event.** A role bead that sets
  `ownership_grants` (an empty list included) is read from it alone. One that does
  not is still read from `bypass_ownership` (one privileged row,
  `role:<title>/bypass`) and `labeled_ownership` (one row per entry), so the merge
  desk, gardeners, captains and herder keep their reach across the deploy. A new
  write may not turn `bypass_ownership` on or set a new `labeled_ownership`
  (`legacyRoleGrantError`); a stored value is kept on the bead's later writes. The
  fallback went in step 6 ([below](#step-6-the-old-role-fields-are-gone)).
- **Unchanged.** The PATCH-close rule (a PATCH to closed is checked as `update`
  except for a machine) and the four known widenings are reproduced as they were;
  each tightens as its own PR (owner choice 3). The update-path field arms stay
  outside the table (owner choice 4).

### Two test fixtures that relied on dead arms

Both legacy arms admitted callers that no request path builds, so no live write
changes:

- `action == "comment"`. Only `update`, `close`, `delete` and `hook-emit` reach the
  gate (every caller passes a literal, and comment create never calls it). The old
  steward and machine typed-ownership code accepted any action string; the rows list
  their actions. `TestStewardOwnsNotes` and
  `TestAuthorizeOwnership_Machine_SlackDMDelivery` no longer assert a `comment`.
- An identity of type `static`. The static token is retired (gd-ke6PC7bRto) and no
  request path stamps that type; the old queue branch still admitted it on an
  escalation through `!roleBearing(method)`. `TestMachineGates_Ownership_SameAsFleet`
  no longer lists it.

### Moving the role beads to rows

Live on 2026-11-02: `bypass_ownership=true` on reviewer (gd-MYJTpBdVnw), gardener
(gd-2Rff4PT2ju), plan-gardener (gd-4zxECOafGY), plan-captain (gd-pvQYXrNN4X),
captain (gd-NrE7xvHacR) and release-captain (gd-qcChUb3bSt); `labeled_ownership` on
herder (gd-aCkGoblwMT). One owner escalation (gd-tgCGgmf0rQ) rewrites them once the
release carrying step 5 is live, each to the row that reproduces its reach today:

| Role | `ownership_grants` |
|---|---|
| reviewer, gardener, plan-gardener, plan-captain, captain, release-captain | `[{"types":["*"],"except":["decision"],"actions":["update","close"]}]` |
| herder | `[{"types":["task"],"label":"thread","actions":["update","close"]}]` |

and unsets `bypass_ownership` and `labeled_ownership`. The admit metric's `grant`
label then reads `role:<title>/0` where it read `role:<title>/bypass`. Narrowing a
role (the merge desk, say, to the types it closes) is a later one-row change on its
bead, made the way the next section describes.

## Step 6: the old role fields are gone

**Bead**: gd-OsLTduO6u9. Gated on an event: escalation gd-tgCGgmf0rQ rewrote the
role beads in the table above (2026-11-03), and `kd list --type=role` then showed no
role bead, open or closed, with `bypass_ownership=true` or a non-empty
`labeled_ownership`.

- **`roleOwnershipGrants` reads `ownership_grants` only.** Unset, `null` and `[]`
  are no row; there is no `role:<title>/bypass` row any more. A role bead that
  still stores either old key gets nothing from it.
- **Deleted**: `roleBypassOwnershipField`, `roleLabeledOwnershipField`,
  `parseRoleLabeledOwnership`, `legacyRoleGrantError`, the two keys in
  `configK8sIdentityFields`, `podIdentityKeys` and `agentIdentityFieldGrants`, and
  the two fields in the built-in `type:role` schema. A new write of either key is
  an unknown field and refused (400); a key already stored on a bead is
  grandfathered by the schema's update check, so the bead's other writes pass.
- **Tests.** labeled_ownership_role_test.go is deleted;
  bypass_ownership_privileged_test.go is privileged_role_grants_test.go and seeds
  the all-types row (`bypassRoleFields`). `TestRoleOwnershipGrants_Rows` checks the
  old keys give no row, and `TestRoleBead_RetiredGrantFieldsUngated` that neither is
  gated and the schema refuses a new one.
- **Left on the herder** (gd-aCkGoblwMT): a stored `bypass_ownership=false`, which
  granted nothing before and is not read now. An admin may drop it with
  `kd update gd-aCkGoblwMT --unset-field bypass_ownership`.

## Gardener self-idle

**Bead**: gd-V68T7v5JPP (step P-G2 of plan gd-fRmqrpHIwk). A plan's gardener may idle
its own trigger until one named bead closes, and the close turns it back on. Before
this, only the operator could disable a gardener trigger, so a gardener waiting on a
merge or a decision kept firing for nothing.

- **The row.** `gardener-self-idle`: agent, `update`, `trigger` and `schedule`, fields
  `enabled`, `cron`, `wake_on`, `awaiting`, condition `callers_own_gardener_trigger`.
  It names fields, so `authorizeOwnership` never matches it; the update path checks
  the patch (`gardenerMayIdleOwnTrigger`, `gardener_self_idle.go`) and counts each
  admit in `gardenio_authz_ownership_admits_total{grant="gardener-self-idle"}`.
- **Who.** The caller writes on its own per-agent token, in the token's project. Its
  agent bead is open, has role `plan-gardener`, and its `trigger_id` is the trigger
  being written (an agent token may not change `trigger_id`,
  `planScopeBindingPatchDenial`). The trigger is an open gardener labelled
  `gardener-epic:<plan>`, and the plan is an open epic labelled `plan` in the same
  project (`callerRunsGardenerTrigger`, `agentIsPlanGardener`). Another plan's
  trigger, a crew holding the trigger link, and a closed iteration are refused.
- **What.** The patch changes only those four fields and sets `awaiting` to the id of
  a bead that exists and is open. It may turn the trigger off (`enabled=false`) but
  never on. A patch with no `awaiting`, an empty one, or one naming a closed or
  missing bead is refused: nothing would ever wake the trigger. An event name that is
  not a bead id is refused for the same reason, until something fires on it.
  `wake_on` is admitted and stored; nothing reads it yet.
- **The wake.** Closing the awaited bead turns every open trigger or schedule whose
  `awaiting` names it back on (`enabled=true`) and clears `awaiting`
  (`wakeGardenersAwaiting`, run from `afterBeadClosed` on every close). It writes as
  `system:gardener-wake`, leaves a comment on the trigger, and records the change in
  mutation history. It is an event gate, never a timer. A `cron` the gardener set
  while idle stays as it set it.
- **The seal.** Turning a trigger off leaves its spawn seal alone, and neither
  `enabled` nor `awaiting` is spawn content, so an idle and its wake keep the seal. A
  `cron` change is spawn content and voids it, so the self-idle write carries the
  seal the trigger held before the patch onto the new content, with the same author
  and the same `iat` (`carrySpawnSealAcrossSelfIdle`, run in `resealSpawnLineage`
  beside `stampSpawnLineageFromPlanApproval`). The seal still ages out on its TTL. A
  trigger that held no valid seal gains none.
- **From the gardener.** `kd update <trigger-id> --field enabled=false --field
  awaiting=<bead-id>`, in one call. The exit guidance every plan gardener reads says
  so (`AppendExitGuidance`, `gardenio/controller/internal/plangardener`).
- **Tests**: `TestGardenerSelfIdle_OwnTriggerIdlesAndWakesOnClose`,
  `TestGardenerSelfIdle_OtherCloseDoesNotWake`, `TestGardenerSelfIdle_Denied`,
  `TestGardenerSelfIdle_CarriesSeal`.

## Steward re-arm

**Bead**: gd-miuZqFACwL (owner, 11-04). The home garden's steward re-arms a plan's
gardener that the engine closed by reopening the same trigger, and the trigger keeps
the seal the owner gave it. Before this, the steward does not own the owner's trigger
and was refused, so it minted a new one with `gb plan gardener`. Any reopen by anyone
but an OIDC admin also strips the seal (`voidSpawnLineageOnReactivation`).

- **The row.** `steward-rearm`: agent, `update`, `trigger` and `schedule`, field
  `enabled`, condition `home_steward_rearms_plan_gardener`. It names fields, so the
  update path checks the patch (`homeStewardMayRearm`, `spawn_lineage_rearm.go`) and
  counts each admit in `gardenio_authz_ownership_admits_total{grant="steward-rearm"}`.
- **Who.** The home garden's steward on its own per-agent token
  (`landsTenantCallerFrom`: Home), in the trigger's project. A tenant steward, a crew
  and a machine are refused. The owner's own OIDC session needs no row: its reopen
  re-stamps the seal.
- **What.** The patch sets `status=open` and `enabled=true` and nothing else. The
  trigger is closed, spawns `plan-gardener`, is pinned by `gardener-epic:<plan>` to an
  epic labelled `plan` in the same project with `plan_state=building`, and no other
  open trigger for that plan is sealed.
- **Only an engine close.** The trigger must still carry a terminate marker
  (`gardener_terminated_kind`, `gardener_terminated_slot` or
  `gardener_stuck_escalated`), which the patch cannot clear. The engine's
  operator-reopen normalization (`gardenerRearmFromOperatorReopen`) keys on the marker:
  on its next cycle it refills the run budget, zeroes the resume counters and starts a
  fresh wall-clock window. A trigger someone closed by hand has no marker and is not
  the steward's to reopen.
- **The seal.** The seal must be valid and within its TTL. The admitted write skips
  the reactivation void, and since neither `status` nor `enabled` is spawn content the
  seal still covers the trigger, with its original author and `iat`. The row never
  creates a seal or changes who it names.

## Steward advice

**Bead**: gd-kEis8bURm5 (owner pick on decision gd-c0b7m4dErB, 11-05: "let's let the
steward write advice for the project and escalate to him"). An advice ask for a project
goes to that project's steward lane, and the home garden's steward approves and executes
it (`escalation_steward_advice.go`).

- **The lane.** The home garden's steward (`landsTenantCallerFrom`: Home, role exactly
  `steward`, its project the home garden by the project bead) is a member of
  `steward:<its project>` for an advice-class escalation (`kbeads_advice_write` and its
  aliases), and the lane is staffed while the project is a home garden. A tenant steward
  keeps `land_steward_approves` for every class. D4 still holds: the steward may not
  approve its own ask.
- **The row.** `steward-advice`: agent, `update` and `close`, `advice`, condition
  `home_steward_executes_approved_advice`. It admits the write while the steward holds the
  claim on an open, server-approved, off-floor, non-operator-only advice escalation
  labelled with its project, whose `execute_by` is `steward:<project>` and whose
  `approved_targets` lists the bead. The same check stands in for advice's `min_role` on
  update (`stewardAdviceWriteHeld`, `authorizeWriteFrozen`); close and create were
  already open to every agent token (`machineAllowedWrites`).
- **Only its project's advice.** The bead must carry exactly one project label,
  `project:<project>`, and no `global` label; an approval stamps an advice bead into
  `approved_targets` only then (`isStewardAdviceTarget`). The steward may not relabel
  advice out of its project (`stewardAdviceLabelsDenial` on update,
  `stewardAdviceLabelChangeDenial` on the single-label routes), nor set any `hook_*` field
  on it (`stewardAdviceHookDenial`): an advice hook runs a command in every session the
  advice matches, which an advice ask does not buy.
- **Metric.** `gardenio_authz_ownership_admits_total{grant="steward-advice"}`.

## Give a role write reach

A role's agents write beads they did not file through rows on the role bead. No
kbeads code changes; the change is config, made by an OIDC admin or through an
approved escalation.

1. **Name the writes.** Which bead types, which actions (`update`, `close`; never
   `delete`, never a `decision`), and what narrows them: a label the beads carry, or
   a caller-bound condition (`caller_operates_bead`, `in_callers_plan`,
   `steward_notes_of_callers_land`, `named_by_approved_close_escalation`,
   `caller_owns_bead`, `bridge_created_legacy`). A 403 the role hit already lists the
   rows that would have allowed it (`allowing_grants`).
2. **Write the smallest row that covers them.** Prefer a label to a whole type, and
   a whole type to `["*"]`:

   ```json
   [{"types":["task"],"label":"thread","actions":["update","close"]}]
   ```

   Add `"privileged": true` when a close the row admits cannot be undone. A row is
   privileged anyway when it reaches every type, has no label and no condition, or
   deletes; a privileged row makes a non-human spawn into the role, or a re-role into
   it, escalate (`roleIsPrivileged`).
3. **Apply it.** The field replaces the whole list, so write every row the role keeps:

   ```sh
   kd show <role-bead> --json | jq .fields.ownership_grants   # what it holds now
   kd update <role-bead> -f ownership_grants='[{"types":["task"],"label":"thread","actions":["update","close"]}]'
   ```

   From an agent, file `gb escalation create --operation-type kbeads_admin_write`
   naming the role bead and the exact value. The daemon refuses an invalid value
   with a 400 that names the entry and what is wrong, and refuses the write from any
   caller but an OIDC admin, an auth-disabled daemon or an approved escalation.
4. **Check it.** The next write admitted for the role counts in
   `gardenio_authz_ownership_admits_total{grant="role:<title>/<index>"}`. To take
   reach away, write the shorter list, or `[]`.

A machine principal's reach is not a role bead: it is its `gardenio-machine-*` entry
in `machineCapabilityGrants` (machine_grants.go), changed by a reviewed PR with both
goldens. A new kind of condition is code too: add it to `ownershipConditions` with
its test, mark it caller-bound only if it ties the bead to the caller's own identity,
plan, land or escalation.
