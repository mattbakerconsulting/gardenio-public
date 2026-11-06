# Machine principals: inventory of every non-OIDC daemon call site (gd-gUOMZdeUHg)

Status: authoritative inventory. Replaces design §5 (`machine-principal-design.md`), which was
explicitly preliminary. Plan gd-UdM4LnnaGE. Tasks 5 (gd-Hc34EENbO9) and 6 (gd-whES7ZdkAy) build
their grants from this document and nothing else.

Evidence commit: **`67a7ad34`** (`origin/main`). Every `file:line` below is at that commit.
Paths without a prefix are relative to `gardenio/controller/`; `kbeads/...` and `coop/...` are
repo-root relative; `IB/` = `internal/bridge/`.

## 0. Findings that change the plan

Read these before the tables. Items marked **(new)** were not in the task brief or design §5.

1. **Only 3 of the 15 `machineAllowedWrites` entries are deletable, not the design's 4 families.**
   `update/config`, `comment/project` and `create/jira-poller` can go once their service callers
   have grants. `project/create`, `project/update`, `user/create`, `user/update` and `config/create`
   all keep a `gb` caller that runs on the static token in agent pods until P7 (section 6).
2. **A `machine` principal has no operator resolution (new).** `resolveOperatedByPure`
   (`kbeads/internal/server/audit.go:1020`) has cases for `oidc`, `static`, `fleet`, `edge` and
   `none`, and none for `machine`. An empty resolved operator is a 400 for every create, comment,
   dependency, formula pour and mutation-history write (`audit.go:898`, `beads.go:152`,
   `http_relations.go:196,531`, `server.go:273,448,886`, `mutation_history.go:56,382`), whatever
   `BEADS_AUTHORING_HUMAN_GATE` says. Design §4.1 says "operated_by is the principal" but nothing
   implements it. Task 3 must add the case; without it every machine write is refused.
3. **`update/account` writers: 8, not 6 (new).** Seven in the controller (the known six plus
   `cmd/controller/key_rotation.go:202`) and one in **coopmux** (`coop/crates/mux/src/credential/failover.rs:742`),
   a Rust deployed component that holds the primary static token (section 7).
4. **The ownership bypass is the dominant dependency, and it is bigger than the brief says.**
   `write_authz.go:413` carries ~10 controller sites, the stale reaper, most of trigger-engine's trigger/task/label/close writes,
   the viewer's edits of other people's beads, and 3 grafana-bridge sites. A machine principal
   gets none of it. `created_by` is the auth subject (`beads.go:142`, `audit.go:200`), so beads
   those components created carry `static` or `bridge:slack` and a new `machine:<client>`
   subject matches neither (section 4).
5. **Two bypasses the brief omits (new):**
   - The **field primitives** (`increment`, `fields/{f}/append|reorder|edit`) call only
     `tokenAuthorizedForBead` (`kbeads/internal/server/http_beads.go:905`). They skip
     `authorizeWrite`, ownership and `checkTokenScopes`. `gb session` writes account usage through
     `increment` (`cmd/gb/session.go:586`), so that path needs no grant and cannot be revoked by
     removing an allow-list entry. Whether to close that is a Task 3 question, not an inventory one.
   - The **service-managed-field bypass** (`beads.go:973,1040,1217`) is applied to every caller
     type, not only static. `authorizeServiceFieldBypass` (`service_field_policy.go:122`) returns
     `nil` for anything that is not an ordinary fleet token, so a `machine` principal gets the
     `trigger`/`schedule`/`agent` field stamps unconditionally and is not subject to
     `BEADS_SERVICE_FIELD_FLEETS`. Task 3 must decide whether `machine` goes through that policy.
6. **Moving slack-bridge off a fleet token loses fleet-only rules (new).** The `type=dm` carve-out
   (`write_authz.go:439`) and the legacy `created_by` match (`:527`, `bridgeSubjectMatchesLegacyCreatedBy` `:645`)
   are `id.Type == "fleet"` only. Five dm sites and the thread/gate/message self-ownership rows
   depend on them (section 3.5).
7. **`X-System-Operator` is a no-op on fleet tokens (new).** The fleet branch does not read it
   (`interceptors.go:906-922`). slack-bridge's four `WithSystemOperator` sites
   (`IB/bot_mentions.go:1934`, `thread_task_autoclose.go:257,1066`, `decisions.go:506`) already do
   nothing. The comments at `cmd/slack-bridge/main.go:94-104` and `thread_task_autoclose.go:5-12`
   assume the opposite.
8. **Writes that already 403 on the static path today (new):** every `update/account` (the
   `/costs` bug), `update/team` (`bv/team_page.go:270`), `DELETE /v1/beads/{id}` for the viewer's
   saved views (`bv/views.go:450`), `update/advice` and every delete in roles-server, and
   `update/role` in `gb config load`. Granting a machine role does not fix these unless the role
   names them (section 6.2).
9. **Stale comments in the code** are listed in section 9.

## 1. Where a non-OIDC caller is let through today

All lines are at commit `67a7ad34` (main when this was written) unless a row names another path.

| # | Mechanism | Where | Who it admits | Machine principal (design §4.1) |
|---|---|---|---|---|
| M1 | `machineAllowedWrites` (15 pairs, section 6 of this doc) | `kbeads/internal/server/write_authz.go:194-210`, consulted `:293` | any non-OIDC caller, on admin-gated types, after `checkTokenScopes` | not consulted; replaced by role grants |
| M2 | `requireRole` below-admin bypass | `interceptors.go:136` (bypass branch `:143-149`) | every non-OIDC caller, every route at operator tier or lower | needs `gardenio-operator` realm role or it gets 403 on all of them |
| M3 | `requireRoleMachineAllowed` | `interceptors.go:212`, doc `:160-211`; routes below | every non-OIDC caller, admin routes | not admitted unless the route is granted |
| M4 | `isAdminRequest` literal-token check | `http_fleet_tokens.go:332`; used `:76,:261,:299`; `http_embeddings.go:34,71,104,198,390` | only the literal primary static token | **a machine principal can never satisfy it**; token-mint needs a new gate (Task 3) |
| M5 | Static-token ownership bypass | `write_authz.go:413` | primary static token | none by default |
| M6 | Privileged-fleet ownership bypass | `write_authz.go:425`; live `BEADS_PRIVILEGED_FLEETS=gitlab-bridge` (chart `values.yaml:6546`) | fleet identity with `Privileged` | none by default |
| M7 | `type=dm` carve-out | `write_authz.go:439` | any fleet token (`id.Type == "fleet"` only; not static, not machine) | none by default |
| M8 | Escalation / decision shared-queue carve-out | `write_authz.go:476-481` (comment `:443-475`) | every non-OIDC caller passes on `type=escalation|decision`; OIDC needs operator+ | a machine principal is non-OIDC, so it passes here too |
| M9 | Service-managed-field ownership bypass | `beads.go:973` (`serviceManagedFieldsByType`), `:1040` (`patchOnlyChangesServiceManagedFields`), applied `beads.go:1217`, gate `service_field_policy.go:122` | **every caller type**: the `case` at `beads.go:1217` skips `authorizeOwnership` for any caller whose patch touches only those fields; `authorizeServiceFieldBypass` then narrows only ordinary fleet tokens to `BEADS_SERVICE_FIELD_FLEETS` (live chart default `slack-bridge,jira-bridge,gitlab-bridge`, `values.yaml:6559`) and returns nil for everything else, including a future `machine` | **missing from the task's list.** `trigger` and `schedule` beads, `agent` beads (`context_tokens`, `live_cost_usd`, `live_turns`) |
| M10 | Config-kind beads skip ownership | `write_authz.go:401` | everyone | n/a; account, project, user, role, fleet, team, `config`, `agent`, `schedule` beads are config-kind |
| M11 | Field primitives skip write policy | `http_beads.go:905` `tokenAuthorizedForBead`, called `:966` (increment), `:1030` (append), `:1100` (reorder), `:1170` (edit) | any caller with operator tier | **missing from the task's list.** Only token-project scope and the `type:user` identity-field gate run (`authorizeUserBeadFieldWrite`, `user_identity_authz.go:140`). No `authorizeWrite`, no ownership, no `checkTokenScopes`. So `IncrementBeadFields` on an **account bead is not write-gated at all** |
| M12 | Header trust, static branch | `interceptors.go:840-890` | `X-Actor`, `X-Operated-By`, `X-User-Bead-Id`, `X-No-Auto-Parent`, `X-Trigger-Bead`, `X-System-Operator`, `X-No-Owner-Default`, `X-Owner-Override` | none honored |
| M13 | Header trust, fleet branch | `interceptors.go:907-922` | `X-Operated-By`, `X-User-Bead-Id`, `X-No-Owner-Default`, `X-Owner-Override` | none honored |
| M14 | Operated-by resolver | `audit.go:1020` `resolveOperatedByPure`, required by `resolveAuditOperatorRequired` `audit.go:898` | cases `oidc`, `static`, `fleet`, `edge`, `none/anonymous`; **no case for `machine`** | section 0 item 2: without a `machine` case every machine write is refused |
| M15 | Edge-auth v2 HMAC envelope | `client.go:3286-3315` (sender), `edge_auth.go` (verifier) | a caller holding `DAEMON_HMAC_KEY` asserts a human operator and roles | **not in the task's list**; it is how beads-viewer and slack-bridge act as a human today (section 5) |

Admin routes that still admit the static token (`requireRoleMachineAllowed`, `http.go`):

| Route | `http.go` line | Real caller found in this inventory |
|---|---|---|
| `PUT /v1/configs/{key...}` (SetConfig) | `:260` | `kd config create` only (`kbeads/cmd/kd/config.go:33`). The Go `beadsapi.Client` has **no** `SetConfig` method |
| `POST /v1/bridge-tokens`, `DELETE /v1/bridge-tokens/{bridge}` | `:267,:269` | controller |
| `POST /v1/fleet-tokens`, `DELETE /v1/fleet-tokens/{fleet}` | `:270,:272` | controller |
| `POST /v1/admin/operators/{id}/identity` | `:300` | slack-bridge |
| `POST /v1/admin/operators/link-aliases`, `/prune-aliases` | `:306,:307` | slack-bridge, inviteservice |
| `POST /v1/embeddings/sync-batch` | `:310` | `kd embed sync` (CronJob, not deployed on k3s) |

## 2. How a client call becomes `authorizeWrite(action, type)`

`authorizeWrite` / `authorizeWriteForBead` call sites in kbeads, non-test: the Step 1 grep
(`git grep -n -E 'authorizeWrite(ForBead)?\(' -- kbeads/internal/server ':!*_test.go'`)
returns **13 lines: 2 definitions (`write_authz.go:250,374`), 1 internal call (`:375`, the
wrapper) and 10 external call sites.** Of the 10, `http_relations.go:67,69` is the shared
`loadBeadForWriteCore`, reached from 9 handlers.

| Daemon route | Handler | Gate | Action passed | Ownership gate? |
|---|---|---|---|---|
| `POST /v1/beads` | `http_beads.go:49` → `beads.go:432` | `authorizeWrite` | `create` on the new bead's type | no (creator is the caller) |
| `PATCH /v1/beads/{id}` | `http_beads.go:821` → `beads.go:1206` | `authorizeWrite`, then `:1234` `authorizeOwnership` unless M9 or the scif stamp applies; then `authorizeUserIdentityFields` `:1211` | `update` | yes (config-kind skips) |
| `POST /v1/beads/{id}/close` | `http_beads.go:1228` → `:1276` | `authorizeWriteForBead` | `close` | yes |
| `DELETE /v1/beads/{id}` | `http_beads.go:763` | `authorizeWriteForBead`, route is `requireRole(RoleAdmin)` | `delete` | yes; **admin route, static token denied** (`http.go:250`) |
| `POST /v1/beads/{id}/increment`, `/fields/{f}/append`, `/reorder`, `/edit` | `http_beads.go:941,1006,1080,1152` | `tokenAuthorizedForBead` (M11) | none | **no** |
| `POST /v1/beads/{id}/dependencies`, `DELETE` | `http_relations.go:165` → `:186`; remove `:302` | `loadBeadForWrite` → `authorizeWriteForBead` on the **dependent** bead (`{id}`); the `depends_on` target is not checked | `update` | yes, on `{id}` |
| `POST /v1/beads/{id}/labels`, `DELETE .../{label}` | `http_relations.go:381,429` | `loadBeadForWrite` | `update` | yes |
| `POST /v1/beads/{id}/comments` | `http_relations.go:521` | `loadBeadForComment` → `authorizeWrite` only (ka-0000005884) | `comment` | **no** |
| `POST .../comments/trim`, `DELETE .../comments/{id}` | `http_relations.go:636,581` | `loadBeadForWrite` | `update` | yes |
| `POST/DELETE /v1/beads/{id}/followers` | `http_followers.go:59,108` | `loadBeadForWrite` | `update` | yes |
| `POST /v1/hooks/emit` | `http_hooks.go:94` | `authorizeWriteForBead` on the agent bead | `hook-emit` | yes |
| `POST /v1/formulas/{id}/pour` | `http_formula_pour.go:400,579` | `createBead` | `create` per step type | no |
| `POST /v1/decisions/{id}/resolve`, `/cancel`; `POST /v1/escalations/{id}/approve`, `/reject`, `/result` | `http_decisions.go:133,239`, `http_escalations.go` | `authorizeResolutionTarget`, operator tier | own gate (M8) | shared-queue carve-out |
| gRPC `CloseBead`, `DeleteBead` | `beads.go:1975,2039` | `authorizeWriteForBead` | `close`, `delete` | yes |

The `beadsapi.Client` method for each route (`gardenio/controller/internal/beadsapi/`), so
every later row can cite a method and get a route:

| Client method | `file:line` | Route | Action |
|---|---|---|---|
| `CreateBead`, `CreateBeadDetail` | `client.go:1114,1129` | `POST /v1/beads` | `create` |
| `UpdateBeadFields`, `UpdateBeadFieldsTyped`, `UpdateBeadNotes`, `UpdateBeadDescription`, `UpdateAgentState`, `SetLabels`, `UpdateBead` | `client.go:2577,2663,2788,2799,2864`, `client_labels.go:49`, `client_query.go:424` | `PATCH /v1/beads/{id}` (the field methods first `GET` the bead) | `update` |
| `CloseBead`, `CloseBeadAs` | `client.go:2903,2909` | `POST /v1/beads/{id}/close` | `close` |
| `DeleteBead` | `client_query.go:472` | `DELETE /v1/beads/{id}` | `delete` (admin route) |
| `IncrementBeadFields`, `IncrementBeadFieldsReturning`, `UpdateAgentUsage`, `UpdateAccountUsage` | `client.go:2717,2732`, `client_session.go:309,321` | `POST .../increment` | none (M11) |
| `AppendBeadArrayField`, `ReorderBeadArrayField` | `client.go:2747,2764` | `.../fields/{f}/append`, `/reorder` | none (M11) |
| `AddDependency`, `RemoveDependency` | `client_query.go:480,498` | `POST/DELETE .../dependencies` | `update` on the dependent |
| `AddLabel`, `RemoveLabel` | `client_labels.go:12,22` | `POST/DELETE .../labels` | `update` |
| `AddComment` | `client_comments.go:31` | `POST .../comments` | `comment` |
| `AddBeadFollower`, `RemoveBeadFollower` | `client_followers.go:64,81` | `POST/DELETE .../followers` | `update` |
| `EmitHook`, `PublishHookEvent` | `client_hooks.go:28`, `client_hooks_publish.go:22` | `POST /v1/hooks/emit`, `/publish` | `hook-emit` / none |
| `PourFormula` | `client_formulas.go:77` | `POST /v1/formulas/{id}/pour` | `create` |
| `ResolveDecision`, `CancelDecision` | `client_decisions.go:18,27` | `POST /v1/decisions/{id}/resolve`, `/cancel` | operator tier |
| `ApproveEscalation`, `RejectEscalation`, `CaptureEscalationResult` | `client_escalations.go:107,129,158` | `POST /v1/escalations/{id}/...` | operator tier; approve and reject can ride a user bearer |
| `SatisfyGate`, `ClearGate` | `client_gates.go:30,39` | `POST/DELETE /v1/agents/{id}/gates/...` | operator tier |
| `RegisterBridgeToken` (+`WithTTL`, `WithRefresh`), `RevokeBridgeToken` | `client.go:3505,3512,3550,3640` | `POST /v1/fleet-tokens`, `DELETE /v1/fleet-tokens/{fleet}` | admin route, M3/M4 |
| `LinkOperatorAliases`, `ResyncUserBeadAliases`, `PruneUserBeadAliases`, `UpdateOperatorIdentity` | `client_admin_operator_aliases_link.go:75,54,111`, `client_admin_operators_slack.go:53` | `/v1/admin/operators/...` | admin route, M3 |
| `LinkOperatorAlias` | `client_operator_link.go:38` | `POST /v1/operators/me/aliases` | operator tier |
| `RevokeUser` | `client_admin.go:30` | `POST /v1/admin/revoke-user` | admin, **static token denied** |
| `RecordSlowToolCall`, `RecordUITelemetry` | `client_slow_tool_calls.go:52`, `client_ui_telemetry.go:40` | `POST /v1/metrics/slow-tool-calls`, `/v1/ui/telemetry` | operator tier, no bead write |
| `CreateTaint`, `UpdateTaintPolicy`, `ApproveScrubRequest`, `RejectScrubRequest` | `client.go:1645,1784,2076,2097` | `/v1/taints`, `/v1/scrub-requests/...` | **always on a user bearer** (`doJSONAsUser`), never the process token |
| `GrantTaint`, `RevokeTaint`, `ApplyTaint`, `CreateScrubRequest` | `client.go:1837,1849,1865,1690` | taint routes | operator tier on the process token |

## 3. Per component

Deployed on k3s per `deploy-values/gardenio-k3s.yaml`: `slackBridge` `:822-823`, `grafanaBridge`
`:1029-1030`, `agents` (the controller) `:1041-1042`, `beadsViewer` `:1158-1159`, `triggerEngine`
`:1282-1283`, and also `coopmux` `:761-762`, which the brief omits. alertmanager-, gitlab-, jira-,
github-, wl-, push-bridge, roles-server, inviteservice and the repro cockpit are chart-only
(`values.yaml` default `enabled: false`; none appears in the k3s file).

Full per-call-site tables (every row `file:line`, method, route, action, bead type, gate,
ownership) are Appendices A-C. This section is the roll-up that Tasks 5 and 6 read.

| Component | On k3s | Daemon client | Credential today | Write call sites (control count) |
|---|---|---|---|---|
| controller | yes | `cmd/controller/main.go:345-348` (`daemon`), SSE watcher `:221-228` | primary static `BEADS_DAEMON_TOKEN`, `templates/controller/deployment.yaml:488-494` | 72 bead writes (28 in `cmd/controller`, 44 in `internal/**`) + 1 non-bead + 8 fleet-token calls. Step-2 grep matches 18 of 46 non-test files in `cmd/controller` (19 with `main.go`, see 3.1) |
| coopmux (Rust) | yes | `coop/crates/mux/src/beads.rs:126-130`, `:312-318` | primary static as `BEADS_TOKEN`, `templates/coopmux/deployment.yaml:194-198`, secret `coopmux.authTokenSecretName` (`_helpers.tpl:418-424`, defaults to `beads.tokenSecret`) | 2 call sites (`failover.rs:710,742`) |
| trigger-engine | yes | `cmd/trigger-engine/main.go:270-272` | primary static, `templates/trigger-engine/deployment.yaml:85-88` | ~101 in `internal/bridge` (20 files) + `cmd/trigger-engine/main.go:1185,1216,1227,1453` + `internal/scheduler/per_run_task_close.go:67`. Step-2 grep: 1 file, 5 lines in `cmd/` |
| grafana-bridge | yes | `cmd/grafana-bridge/main.go:71,119,185` | primary static, `templates/grafana-bridge/deployment.yaml:56-59` | 15 in 5 `internal/bridge` files. Step-2 grep: 1 file, 1 line in `cmd/` |
| beads-viewer | yes | `cmd/beads-viewer/main.go:75-83` | primary static, `templates/beads-viewer/deployment.yaml:94-97` | 116 `s.daemon.<write>(` sites in 46 files + ~17 in internal packages it drives. Step-2 grep: 58 files, 176 lines |
| slack-bridge | yes | `cmd/slack-bridge/main.go:71-73`; proxies `helpers.go:145,158` | **fleet token** `bridge:slack` from Secret `bridge-tokens` key `slack-bridge`, `templates/slack-bridge/deployment.yaml:81-92`, helper `_helpers.tpl:709`; `Privileged=false` | ~121 in 40 `internal/bridge` files + 4 admin-route calls. Step-2 grep: 1 file, 2 lines in `cmd/` (method names, not calls) |
| agent pods (`gb`, `kd`) | yes | `cmd/gb/main.go:259-311`; `kbeads/internal/client` | the shared primary static token (`defaultToken()`); per-agent tokens are dormant on k3s (PR #273 open) | `gb`: 266 matching write lines in `cmd/gb` (238 non-test files); `kd`: 145 non-test files in `kbeads/cmd/kd` |

Every non-zero. A zero in any deployed row would mean the grep was wrong; the `cmd/` greps for
the bridges are tiny by design because their `cmd/<name>` directories are thin and the daemon
traffic is in `internal/bridge`.

### 3.1 controller

Summary of what it writes (Appendix A has all 72 rows):

| Write class | Sites | Evidence (first / all in App. A) | Admin-gated? | Needs |
|---|---|---|---|---|
| `update/account` | 7 | `spend_sync.go:224` (section 7) | yes, config-kind | `gardenio-machine-accounts` |
| `update/config`, `create/config` | 2 + 1 | `IB/init_ensure.go:97,107` (via `EnsureConfigs`, `main.go:466`); `auth_config_legacy.go:227` | yes | `gardenio-machine-config-seed` |
| `close/advice` | 1 | `advice_sweeper.go:118` | yes | `gardenio-machine-advice-close` + ownership bypass (advice is data kind, creator is an agent) |
| `close`/`update` on `task` | 7 | `statusreporter/stalereaper.go:614,725,905,957,1096,1249`, `reconciler/reconciler.go:1432` | no | ownership bypass |
| `update` on `session`, `trigger`, `schedule` | 1 + 1 | `reconcile_session_spend.go:546`; `captain_spend_sync.go:127` | no | ownership bypass (`captain_spend_*` is not a service-managed field) |
| `agent` create/update/close/label, `message` and `escalation` create, task `AddComment` | the remaining ~45 | `poolmanager.go:540,1268`, `statusreporter/reporter.go:473,939`, `microvm/lifecycle.go:326`, `assistantturnminter.go:994`, `github_app_credentials.go:598` | no (`agent` is config-kind) | `gardenio-operator` only |
| fleet-token routes `POST/GET/DELETE /v1/fleet-tokens` | 8 | `agenttoken.go:102,138,172`, `microvm_credentials.go:67,92`, `bridgetokenrotator/rotator.go:258,348` | admin route, plus `isAdminRequest` | `gardenio-machine-token-mint`. **Dormant on k3s** (`PER_AGENT_TOKENS`, `BRIDGE_TOKEN_ROTATION_ENABLED` unset) |
| `RecordSlowToolCall` | 1 | `internal/toollatency/consumer.go:381` | operator route, not a bead write | `gardenio-operator` |

No controller call to `SetConfig`, operator-alias routes, embeddings, `RevokeUser`, `DeleteBead`,
taints, hooks, decisions or `PourFormula`. No controller write rides a user bearer.

The Step-2 grep misses `cmd/controller/main.go` (it calls `UpdateAgentState`/`MarkAgentFailed`,
`main.go:2274,2280,2614,2983`) and false-positives on `reconcile_session_spend.go:294,502`
(metrics). Tasks 4-5 should use the 68-method write closure (Appendix A, method) rather than the
Step-2 pattern.

**Flags for Task 5:** (a) `bridgetokenrotator/rotator.go:348` passes `privileged = c.Fleet.Privileged`,
so `token-mint` implicitly includes minting `Privileged` fleet tokens, i.e. the ownership-bypass
identity (`write_authz.go:425`). Does a machine principal get to set `privileged`? (b) MicroVM
shared-token mode hands the controller's own `BEADS_DAEMON_TOKEN` to the satellite
(`cmd/controller/microvm_credentials.go:62-79`) and agent pods get the shared token
(`deployment.yaml:289`). A machine client secret must never take either path.

**Since the evidence commit (gd-ovzZSuuHny, PR #1525; recorded by gd-wahYHJndnb).** Line numbers
in this note are at `origin/main` after that PR, not at `67a7ad34`.

- *Daemon writes:* no new call site. The stale reaper's stranded-task reopen
  (`stalereaper.go:905` in Appendix A, now `:1061`) also removes the `task:orphaned-with-pr`
  label in the same `UpdateBead` when every linked PR is closed or merged, so that row writes
  `labels` as well as status, assignee and fields. Same action (`update/task`), same ownership
  bypass. The flag write (`:1249` in Appendix A, `flagOrphanedWithPR`, now `:1529`) is unchanged,
  but now fires only for an open or unreadable PR.
- *GitHub credential (not a daemon call):* to read a linked PR's state the controller mints a
  fleet GitHub App installation token per lookup, scoped to the one repository and to
  `pull_requests: read` only, and revokes it after the read (`internal/githubapp/githubapp.go:462`
  `PullRequestState`, wired by `cmd/controller/github_pr_state.go:24` and `main.go:1131`). It
  rides the controller's existing fleet App key, needs no new daemon grant, and stays off when
  the fleet App cannot mint. A machine principal changes nothing here, but the App's installation
  must keep the `pull_requests: read` permission or every lookup fails and the reaper falls back
  to flagging.

### 3.2 coopmux (Rust, deployed, not in the brief)

- Reads accounts, configs and roles (`beads.rs:140,183,232`) and writes two things through the
  primary static token, no operator header (`beads.rs:312-318` sets only `Authorization`):
  - `failover.rs:710` `update_bead_description` → `PATCH /v1/beads/{id}` on the legacy `config:auth`
    bead (type `config`): `update/config`.
  - `failover.rs:742` `update_bead_fields` → `PATCH` on an **account** bead (`status=exhausted`,
    `exhausted_at`, `cooldown_until`): `update/account`. Denied today, same as the controller's.
- Both targets are config-kind, so there is no ownership dependency.
- It needs `gardenio-machine-accounts` and `gardenio-machine-config-seed` (update half), plus an
  OAuth client-credentials token source in Rust (the Go `TokenSource` plan in design §4.2 does not
  cover it). Task 4's list of account writers must include `failover.rs:742`.
- Whether a header-less PATCH succeeds on the static path today is unverified (section 10).
- **Done (gd-Wcu0DOkHfN):** the `failover.rs:742` account write now runs as the
  `gardenio-coopmux` machine principal (`gardenio-machine-accounts` only) through
  `coop/crates/mux/src/machine_token.rs`, the Rust client-credentials token source mirroring
  `beadsapi/machine_token.go`; chart switch `coopmux.machinePrincipal.enabled`. The
  `failover.rs:710` `config:auth` write (`update/config`) was removed from coopmux
  (gd-in7EZvvd3T, decision gd-HYilfX1Bk8 "controller-owns"): coopmux's in-memory cache still
  flips, and the controller records the failover on the `config:auth` bead
  (`auth_failover.go` -> `updateAuthConfigForProject`). Its reads now use the machine principal.

### 3.3 trigger-engine

(Appendix B §1.) Static token. **No `machineAllowedWrites` entry and no admin route.** Everything it
writes is on non-admin types: `trigger` (data kind), `task`/`bug`/`molecule`/`doc`/`message`/`decision`
creates and updates, `agent` (config-kind), `schedule` (config-kind).

- Ownership: it updates, closes and labels trigger beads authored by humans or agents
  (`IB/captain_evaluator.go:564,602,1362`, `captain_hooks.go:131`, `captain_second_life.go:181`,
  `trigger_source_schedule.go:282`), labels arbitrary task and agent beads
  (`cmd/trigger-engine/main.go:1185,1216,1453`) and closes per-run tasks
  (`internal/scheduler/per_run_task_close.go:67`). All ride `write_authz.go:413`.
- Only the stamps `last_checked`, `last_outcome(_at)`, `last_run`, `last_agent_id`, `last_match`,
  `last_fire_error(_at)` are service-managed fields (`IB/triggers.go:2340,2367,2437,2454`).
  `captain`, `enabled` and `consecutive_failures` writes are not
  (`IB/triggers.go:2418`, `captain_evaluator.go:1362,1440`), so they need the ownership bypass.
- Roles: `gardenio-operator` + the system-graph ownership capability (section 8). Nothing else.

### 3.4 grafana-bridge

(Appendix B §2.) Static token plus a client-wide `SetSystemOperator("grafana-bridge")`
(`cmd/grafana-bridge/main.go:126`).

- Admin-gated writes: only `create/config` and `update/config` via `EnsureConfigs`
  (`cmd/grafana-bridge/main.go:79`, `IB/init_ensure.go:97,107`).
- Ownership: `grafana_readback.go:284,353` and `grafana_sync.go:207` mutate dashboard beads that
  operators or agents authored; `grafana_dashboard_store.go:97` updates a doc it created earlier
  under `created_by=static`.
- Roles: `gardenio-operator`, `gardenio-machine-config-seed`, ownership capability or a one-time
  re-owner migration (4 sites).

### 3.5 slack-bridge

(Appendix B §3.) Fleet token (`bridge:slack`, not privileged), so it is governed by fleet-only rules
a `machine` principal would lose.

- Admin-gated: `create/config`, `update/config` (`EnsureConfigs`, `cmd/slack-bridge/main.go:119`);
  `update/project` and `comment/project` (`IB/bot_schedule_cmd.go:182,192,216,226`,
  `IB/bot_trigger_cmd.go:131,141,165,175`). `IB/user_directory.go:545` writes `slack_id` on a user
  bead and is already refused by the identity-field gate, so it needs nothing.
- Admin routes (`requireRoleMachineAllowed`): `GET /v1/admin/operators/slack-aliases`
  (`IB/slack_operator_resync.go:93`, `cross_operator_notify.go:134`), `POST .../{id}/identity`
  (`slack_operator_resync.go:135`), `POST .../link-aliases` (`slack_oidc_operator_alias_resync.go:157`),
  `POST .../prune-aliases` (`:242`). No `SetConfig`, token or embeddings call.
- Fleet-only ownership rules it relies on: `type=dm` carve-out (`write_authz.go:439`) at
  `IB/dm_delivery.go:204,222,281,292,404`; the legacy `created_by` match (`:527`) for thread-task,
  head, gate, message, rating and bug beads it created under `bridge:slack`/`slack-bridge`.
- Rules every non-OIDC caller gets: escalation/decision shared-queue carve-out
  (`write_authz.go:476-481`), ~35 sites. A `machine` principal keeps these.
- Already failing today: `IB/decisions.go:511` (label on a captain trigger bead) and `:609`
  (close of a remediation task): fleet token, not creator, no bypass.
- Roles: `gardenio-operator`, `config-seed`, `project-ops`, `operator-sync`, an operated-by relay
  (14 sites set `X-Operated-By`), and either keep the token a fleet token or give `machine` the dm
  carve-out and a created_by migration.

### 3.6 beads-viewer

(Appendix C, 23 feature groups W1-W23.) One process-wide static token for every write.
`BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES` is off, OIDC and edge-auth are off and
`devAuthBypass: true` on k3s (`deploy-values/gardenio-k3s.yaml:1157-1218`), so there is no user
bearer to forward. Only 7 call sites ever use a human bearer (`CreateTaint`, `UpdateTaintPolicy`,
`Approve/RejectScrubRequest`, `Approve/RejectEscalation`); a viewer machine principal never covers them.

- Admin-gated writes (all 4 groups also appear in section 6): `create/advice` (`bv/create_advice.go:142`),
  `update/user` (`bv/settings.go:454`, `user_status_message.go:236`, `user_inbox.go:439`,
  `inbox_flag.go:94`; the pin write at `inbox_flag.go:94` is not named in the allow-list comment),
  `update/project` + `comment/project` (`bv/project_config.go:314,320`).
- Already failing today: `update/team` (`bv/team_page.go:270`) and `DELETE /v1/beads/{id}`
  (`bv/views.go:450`, admin tier).
- Admin route: `GET /v1/admin/operators/slack-aliases` (`bv/people_mentions.go:84`).
- Ownership: W1-W4, W6, W8, W9, W13, W17, W18 edit and close beads the viewer did not create
  (Appendix C section O, O1). The viewer's beads carry `created_by=static`.
- Headers: `X-Operated-By` on all human-driven writes (`bv/middleware.go:58`, 16 more `WithOperator`
  sites), `X-Actor` on comments only (`bv/server.go:3940`), `X-System-Operator` for the plan-label
  reactor only (`bv/plan_label_reactor.go:389`).
- Roles: `gardenio-operator`, `advice-create`, `project-ops`, `user-self`, `operator-sync`, an
  operated-by relay, the ownership capability.

### 3.7 agent pods (`gb`, `kd`): out of scope, record only

Out of scope for grants (P7, per-agent tokens). They matter because they are the reason most
allow-list entries cannot be deleted. `gb` reads its token from `defaultToken()`
(`cmd/gb/main.go:259-267`: `GARDENIO_TOKEN`, then `KBEADS_TOKEN`, `BEADS_AUTH_TOKEN`,
`BEADS_DAEMON_TOKEN`), which in an agent pod is the shared static token. Callers per allow-list
entry are in section 6. `gb` also calls `UpdateAccountUsage` (`cmd/gb/session.go:586`) through
the ungated `increment` route (section 0 item 5). `kd config create` is the only caller of
`PUT /v1/configs` (`kbeads/cmd/kd/config.go:33`); `kd embed sync` is the only caller of
`POST /v1/embeddings/sync-batch` (`kbeads/cmd/kd/embed.go:177`). Headers set by the agent clients:
`SetActor` (`cmd/gb/main.go:330`), `X-Operated-By` from `BEADS_OPERATED_BY`, and for `kd`
`X-Fleet`, `X-No-Auto-Parent`, `X-Trigger-Bead` (`kbeads/internal/client/http.go:1660-1680`,
`kbeads/cmd/kd/claim.go:48`).

### 3.8 Not deployed on k3s (chart-only)

| Component | Credential (template) | Admin-gated writes / notable | Appendix |
|---|---|---|---|
| alertmanager-bridge (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | primary static, `templates/alertmanager-bridge/deployment.yaml:56-60` | `create/config`, `update/config` (`EnsureConfigs`, `cmd/alertmanager-bridge/main.go:91`) | B §4 |
| gitlab-bridge | fleet `gitlab-bridge` (**privileged**, `values.yaml:4680`), `templates/gitlab-bridge/deployment.yaml:71-79` | `EnsureConfigs`; cross-owner produces/triggered-by deps and task closes need the privileged-fleet bypass | B §4 |
| jira-bridge | fleet `jira-bridge`, `templates/jira-bridge/deployment.yaml:71-80` | `create/jira-poller` (`IB/jira_config.go:191`), `EnsureConfigs` | B §4 |
| github-bridge | own key `github-bridge`, `templates/github-bridge/deployment.yaml:74-82` | task/MR updates need the same bypass; `BEADS_PRIVILEGED_FLEETS` lists only `gitlab-bridge` | B §4 |
| wl-bridge | primary static | `EnsureConfigs` (`cmd/wl-bridge/main.go:87`). Not inventoried further | none |
| roles-server | primary static, `templates/roles-server/deployment.yaml:50-54` | `create/config`, `update/config`, `create/advice`; **already denied:** `update/advice` (`cmd/roles-server/web_advice.go:150`) and every `DeleteBead` (`web.go:256`, `web_advice.go:185`) | B §4 |
| inviteservice | primary static | operator-alias admin routes (`internal/inviteservice/operator.go`, per the `requireRoleMachineAllowed` doc) | none |
| push-bridge, repro-cockpit | primary static | not inventoried | none |

`EnsureConfigs` is run by seven binaries: controller `main.go:466`, grafana `:79`, slack `:119`
(deployed) and alertmanager `:91`, gitlab `:107`, jira `:103`, wl `:87` (chart-only). **`gb` does
not run it** and `roles-server` does not either, contrary to the `machineAllowedWrites` comment.

## 4. Ownership dependencies, across components

`authorizeOwnership` (`write_authz.go:396`) passes when the bead is config-kind (`:401`), the
caller is admin, `static` (`:413`), a privileged fleet (`:425`), a fleet on a `dm` (`:439`), any
non-OIDC on an escalation/decision (`:476-481`), the bead has no ownership metadata, or
`CreatedBy|Assignee|Owner == id.Subject` (`:507`), or the legacy bridge match (`:527`). A `machine` principal
gets only the escalation/decision carve-out, the no-metadata skip and self-ownership.

| Component | Non-config beads it mutates without being creator/assignee/owner | Bypass relied on | Where |
|---|---|---|---|
| controller | task close/update/label/reopen (7), advice close, session settle, trigger spend stamp | `:413` | App. A (O) |
| trigger-engine | trigger update/close/label, task labels, per-run task close, decision updates | `:413` (+ service-managed-field for the stamps) | App. B §1 (O) |
| grafana-bridge | dashboard and doc updates, `blocks` dependency on a dashboard | `:413` | App. B §2 (O) |
| beads-viewer | edits, closes, labels, followers, reactors on beads created by anyone | `:413` | App. C (O1) |
| slack-bridge | dm, escalation, decision beads | `:439` (fleet-only), `:476-481` | App. B §3 (O) |
| gitlab-bridge, github-bridge (chart-only) | task/MR graph writes | `:425` privileged fleet | App. B §4 |

Comments never reach the ownership gate (`http_relations.go:30-39`), and `agent`, `account`,
`project`, `user`, `team`, `role`, `schedule`, `config` writes are config-kind, so neither needs
a bypass. **Dependencies and bypass count are the same list: the ownership capability in section 8
exists for exactly the rows above.**

`created_by` consequence: the daemon sets `created_by` from the auth subject, never the request
(`beads.go:142`, `audit.go:200`). Existing rows carry `static` (controller, trigger-engine,
grafana-bridge, viewer), `bridge:slack`/`slack-bridge` (slack-bridge) and `jira-bridge`. After a
principal change they stop matching, so even beads a component made itself become ownership 403s
unless it keeps the bypass or the rows are re-owned. Task 5 must pick.

## 5. Header dependencies, across components

Daemon trust: static branch `interceptors.go:840-890`, fleet branch `:907-922`. A `machine`
principal honors none (design §4.1). Client side: only `beadsapi/client.go:3279-3281` (`X-Actor`),
`:3318-3320` (`X-Operated-By`), `:3323-3325` (`X-System-Operator`), `:3328` (`X-Escalate`) and the
edge-auth envelope `:3286-3315`.

| Component | `X-Operated-By` | `X-Actor` | `X-System-Operator` | Others | If ignored |
|---|---|---|---|---|---|
| controller | `assistantturnminter.go:989,1027` | none | `poolmanager.go:240,1026`; `assistantturnminter.go:991,1029` | none | plan-transcript turns lose human attribution and are silently dropped (best-effort, `:996-1000`); pool create/assign refused |
| coopmux | none | none | none | none | unverified (section 10) |
| trigger-engine | `IB/triggers.go:166,169,200,3324`, `formula_step_dispatch.go:859,863` | none | 9 files: `captain_epic_reaper.go:183`, `captain_liveness.go:231,406`, `captain_pouring_stall.go:185`, `closed_enabled_drain.go:277`, `stale_revival_review_escalate.go:206`, `trigger_check_liveness.go:111`, `trigger_credential_wedge.go:100`, `trigger_dark_lane.go:141`, `uncovered_open_lane_rollup.go:166` | none | autonomous fires lose human lineage; trigger creates (`captain_chain_spawn.go:403`) are refused under `enforce` (`beads.go:178`); sweep writes have no operator (400) |
| grafana-bridge | none | none | client-wide `cmd/grafana-bridge/main.go:126` | none | every write has no operator (400) |
| slack-bridge | 14 sites (list in App. B §3 H) | none | 4 sites, all no-ops on a fleet token | edge-auth envelope `IB/bot_person_identity.go:115,124`, dormant (`edgeAuth.enabled: false`) | human attribution lost; under `enforce` fleet creates with no human are refused (`audit.go:1077`) |
| beads-viewer | `bv/middleware.go:58` + 16 sites | `bv/server.go:3940` (comments) | `bv/plan_label_reactor.go:389` | edge-auth, dormant | human attribution lost on every human-driven write; `Bind` create 400s |
| `gb`/`kd` | from `BEADS_OPERATED_BY` | `SetActor` `cmd/gb/main.go:330` | none | `kd`: `X-Fleet`, `X-No-Auto-Parent`, `X-Trigger-Bead` | out of scope |

**`X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`: no code in this repo sets them**
(full-repo grep, excluding the daemon's own readers and docs/comments). Their trust branches
(`interceptors.go:848-890`, `:907-922`) have no in-tree caller. They are deletion candidates, but
an out-of-tree caller cannot be ruled out from the repo.

Net requirement for machine principals: not a header relay per se but **operator resolution for the
`machine` type** (section 0 item 2). A relay of `X-Operated-By` is additionally needed only by
components that attribute work to a human: beads-viewer, slack-bridge, trigger-engine, and the
controller's assistant-turn minter. Flagged in section 8.


## 6. `machineAllowedWrites` reconciliation

`kbeads/internal/server/write_authz.go:194-210`, consulted at `:293`. Rule applied: an entry is
**deletable only if every remaining caller has a machine principal with a grant for it.** An
agent-pod `gb`/`kd` caller keeps the entry until P7, because those still run on the shared static
token. Design §5 named `project/*`, `user/*`, `config/*` and `jira-poller/create` as candidates;
the check below disagrees for all but two of them.

### 6.1 The 15 entries

`gb` callers are agent-pod callers (section 3.7). "k3s" marks callers that run in this cluster.

| # | Entry | Callers (component, `file:line`) | Deletable after Tasks 4-5? |
|---|---|---|---|
| 1 | `create/report` | `gb decision report` `cmd/gb/decision.go:1326` | **No.** `gb` agent pods |
| 2 | `update/report` | `gb decision report` `AddDependency` on the new report: `decision.go:1349,1367,1382,1497` | **No.** `gb` |
| 3 | `close/report` | `gb decision report` `decision.go:1426` | **No.** `gb` |
| 4 | `create/advice` | `gb advice add` `cmd/gb/advice.go:268`; `gb config load` `config.go:670`; `kd advice add` `kbeads/cmd/kd/advice_add.go:104`; beads-viewer (k3s) `bv/create_advice.go:142`; roles-server (chart-only) `cmd/roles-server/web_advice.go:92` | **No.** `gb`, `kd` |
| 5 | `close/advice` | controller (k3s) `cmd/controller/advice_sweeper.go:118`; `gb advice remove` `advice.go:519`; `kd advice remove` `kbeads/cmd/kd/advice_remove.go:25` | **No.** `gb`, `kd` |
| 6 | `create/role` | `gb role scaffold` `cmd/gb/role.go:440`; `gb config load` `config.go:227` | **No.** `gb` |
| 7 | `create/config` | `gb config load` `config.go:452`; `gb config migrate` `config.go:813`; `gb role scaffold` `role.go:464`; `EnsureConfigs` `IB/init_ensure.go:107` run by controller `main.go:466` (k3s), grafana-bridge `main.go:79` (k3s), slack-bridge `main.go:119` (k3s), alertmanager `:91`, gitlab `:107`, jira `:103`, wl `:87` (chart-only); roles-server `web.go:139` | **No.** `gb` |
| 8 | `update/config` | `EnsureConfigs` `IB/init_ensure.go:97` (same 7 binaries); controller `cmd/controller/auth_config_legacy.go:227`; coopmux `coop/crates/mux/src/credential/failover.rs:710`; roles-server `web.go:214,222,301,308`, `web_instructions.go:143`. **No `gb`/`kd` caller** (`gb config load` skips existing config beads, `config.go:417`) | **Yes, candidate.** Needs `config-seed` on controller, grafana-bridge, slack-bridge and coopmux (k3s); chart-only callers (4 bridges, roles-server) lose the write until they get a principal |
| 9 | `create/account` | `gb account create` `cmd/gb/account_create.go:133`; `gb config load` `config.go:411` | **No.** `gb` |
| 10 | `create/project` | `gb project create` `cmd/gb/project.go:200`; `gb config load` `config.go:408` | **Removed** (gd-WCM9rfOL8L): a project bead with no `land_kind` is an operator project with fleet-App GitHub tokens, so these now need an OIDC `gardenio-admin`. The Lands provisioner keeps its machine grant for `land_kind=tenant` only (`authorizeProjectCreate`) |
| 11 | `update/project` | `gb pool enable/disable` `cmd/gb/pool.go:458`; `gb config load` `config.go:408`; slack-bridge (k3s) `IB/bot_schedule_cmd.go:182,216`, `bot_trigger_cmd.go:131,165`; beads-viewer (k3s) `bv/project_config.go:314` | **No.** `gb pool` and `gb config load`. The two service callers can move, the entry cannot go |
| 12 | `comment/project` | slack-bridge (k3s) `IB/bot_schedule_cmd.go:192,226`, `bot_trigger_cmd.go:141,175`; beads-viewer (k3s) `bv/project_config.go:320`. No `gb`/`kd` flow comments on a project bead | **Yes, candidate**, once slack-bridge and beads-viewer hold `project-ops` |
| 13 | `create/user` | `gb identity add` `cmd/gb/identity.go:101`; `gb identity seed` `identity_seed.go:140`; `gb user add` `user.go:101` | **No.** `gb` |
| 14 | `update/user` | beads-viewer (k3s) `bv/settings.go:454`, `user_status_message.go:236`, `user_inbox.go:439`, `inbox_flag.go:94`; `gb user update` `cmd/gb/user.go:291,300`; slack-bridge `IB/user_directory.go:545` (refused by the identity-field gate regardless) | **No.** `gb user update`. The viewer can move |
| 15 | `create/jira-poller` | jira-bridge (chart-only) `IB/jira_config.go:191`. No other caller | **Yes, candidate**, once jira-bridge has a principal with `jira-poller`. Nothing on k3s calls it, so it can be deleted from the Go list only together with the chart giving jira-bridge a client |

Result: **3 candidates** (`update/config`, `comment/project`, `create/jira-poller`), **12 stay
until P7**. `project/update`, `user/update` and `config/create` are the ones design §5 got wrong:
each has a `gb` caller. Each candidate is conditional on the grants in section 8 reaching every
caller listed, including chart-only ones (a deployment that runs them on the static token would
break).

### 6.2 Writes that are NOT in the list and already 403 on a non-OIDC caller today

Granting a machine role fixes these only if the role names the pair. Listed so Task 5 does not
assume otherwise.

| Pair | Callers | Note |
|---|---|---|
| `update/account` | controller x7 (section 7), coopmux `failover.rs:742`, `gb account update` `cmd/gb/account.go:175,228,243`, `gb config load` `config.go:411` | the `/costs` bug. Fixed by `gardenio-machine-accounts` for services; `gb` stays denied |
| `update/team` | viewer `bv/team_page.go:270` | `authorizeTeamMembershipBypass` needs an `oidc`/`edge` caller (`write_authz.go:703-718`). Comment at `bv/team_page.go:195-203` is stale |
| `delete/*` (`DELETE /v1/beads/{id}`) | viewer saved-view delete `bv/views.go:450`; roles-server `web.go:256`, `web_advice.go:185`; `gb advice remove --hard` `advice.go:514`; `kd` delete | `http.go:250` is `requireRole(RoleAdmin)`, not machine-allowed |
| `update/advice` | roles-server `web_advice.go:150` | chart-only |
| `update/role` | `gb config load` `config.go:215` | operator tool; works only with an OIDC admin |

The field primitives (`increment`, `append`, `reorder`, `edit`) never consult this list at all
(section 0 item 5): `gb session`'s account usage, the viewer's plan-step reorders and
`AppendBeadArrayField` calls are unaffected by any change here.

### 6.3 Admin routes (`requireRoleMachineAllowed`) reconciliation

| Route | Callers found | Deletable? |
|---|---|---|
| `PUT /v1/configs/{key...}` | `kd config create` only (`kbeads/cmd/kd/config.go:33`); the e2e seed `gardenio/tests/beads-viewer-e2e/seed/seed.mjs` per the route's doc comment. **No Go platform component** | No: `kd` in agent pods |
| `/v1/bridge-tokens`, `/v1/fleet-tokens` | controller only (8 sites, dormant on k3s) | Yes in the `requireRoleMachineAllowed` sense, if `token-mint` replaces `isAdminRequest` |
| `/v1/admin/operators/{id}/identity`, `link-aliases`, `prune-aliases` | slack-bridge; inviteservice (chart-only) | Yes, if `operator-sync` exists |
| `GET /v1/admin/operators/slack-aliases` | slack-bridge, beads-viewer `bv/people_mentions.go:84` | Yes, if `operator-sync` exists |
| `POST /v1/embeddings/sync-batch` | `kd embed sync` (CronJob, not deployed on k3s: `beads.embedding.enabled: false`) | No: uses the literal primary token |

## 7. Controller account writers (Step 7)

All are `PATCH /v1/beads/{id}` on a `type:account` bead: action `update`, admin-gated,
config-kind (no ownership gate). `update/account` is absent from `machineAllowedWrites`, so all
are denied today.

| # | `file:line` | What | Loop (started at) |
|---|---|---|---|
| 1 | `cmd/controller/spend_sync.go:224` | total/daily/weekly cost, 5h/7d windows, spend limit | `newSpendSync` `main.go:1159` (LiteLLM on k3s) |
| 2 | `cmd/controller/ratelimit_probe.go:699` | `probe_auth_status=rejected` | `newRateLimitProbe` `main.go:1138` |
| 3 | `cmd/controller/ratelimit_probe.go:767` | utilization, throttle, reset, model_group_limits | same |
| 4 | `cmd/controller/ratelimit_probe.go:835` | `soft_failover_state` | same |
| 5 | `cmd/controller/auth_failover.go:238` | `status=exhausted`, `deprioritized_until`, `rotation_count` | `newAuthFailover` `main.go:1004` |
| 6 | `cmd/controller/auth_failover.go:456` | `status=active`, `cooldown_until=""` (sweep recovery) | same |
| 7 | **`cmd/controller/key_rotation.go:202`** (new) | `litellm_key`, `key_rotated_at`, `key_rotation_count`; fires only when `LITELLM_KEY_ROTATION_ENABLED` and the key is past max age | `newKeyRotator` `main.go:1255` |
| 8 | **`coop/crates/mux/src/credential/failover.rs:742`** (new, not the controller) | `status=exhausted`, `exhausted_at`, `cooldown_until` | coopmux failover |

Checked and **not** account writers: `key_acl_widen.go`, `litellm_aliases.go`,
`account_exhaustion.go` (NATS listener, no daemon call), `account_spread*.go`,
`account_pool_validator.go`, `served_model_sync.go`, `tag_spend_sync.go`, `credsync_notifier.go`,
`rotation_notifier.go`, `probe_event_listener.go`, `failover_runtime.go`. No create, close or
delete on accounts anywhere in the controller. `auth_config_legacy.go:227` writes the legacy
`config:auth` bead (`update/config`), not an account.

Task 4 moves **7 controller sites plus 1 coopmux site**, not 6. Per the brief, comment on
gd-s7aPIf0Bgp with this list.

## 8. Capability vocabulary (Step 6)

Every machine client also holds `gardenio-operator` (design §4.1), which covers all routine
create/update/close/comment traffic (the "agent, message, escalation, task comment" rows above).
Roles below are *additional*. Names marked **fixed** are the ones Task 3 creates.

### 8.1 Write capabilities (each a set of `(action, type)` pairs)

| Role | Pairs | Held by (evidence) |
|---|---|---|
| `gardenio-machine-accounts` (**fixed**) | `update/account` | controller (section 7, 7 sites), coopmux (`failover.rs:742`) |
| `gardenio-machine-config-seed` | `create/config`, `update/config` | controller (`init_ensure.go:97,107`, `auth_config_legacy.go:227`), grafana-bridge, slack-bridge (`EnsureConfigs`), coopmux (`failover.rs:710`, update only). Chart-only: alertmanager-, gitlab-, jira-, wl-bridge, roles-server. If split by half, `update/config` alone suffices for coopmux |
| `gardenio-machine-project-ops` | `update/project`, `comment/project` | slack-bridge (`bot_schedule_cmd.go`, `bot_trigger_cmd.go`), beads-viewer (`project_config.go:314,320`) |
| `gardenio-machine-user-self` | `update/user` (non-identity fields only; the `authorizeUserIdentityFields` gate still applies) | beads-viewer (4 sites) |
| `gardenio-machine-advice-create` | `create/advice` | beads-viewer (`create_advice.go:142`); chart-only roles-server |
| `gardenio-machine-advice-close` | `close/advice` | controller (`advice_sweeper.go:118`) |
| `gardenio-machine-jira-poller` | `create/jira-poller` | jira-bridge (chart-only) |

Not proposed: `report/*`, `role/create`, `account/create`, `project/create`, `user/create` and
the `gb`-shared halves of `advice/*` and `config/*` stay on the static token and in
`machineAllowedWrites` until P7. `account/update` is never added to the allow-list.

### 8.2 Non-write capabilities

| Role | Grants | Held by (evidence) | Reopens a trust branch? |
|---|---|---|---|
| `gardenio-machine-token-mint` (**fixed**) | `POST/GET/DELETE /v1/fleet-tokens`, `/v1/bridge-tokens`; requires replacing the `isAdminRequest` literal-token gate (`http_fleet_tokens.go:76,261,299,332`) | controller (8 sites; dormant on k3s) | **Yes.** `isAdminRequest` currently makes a JWT unable to call these. Also decide whether the principal may set `privileged` (`rotator.go:348`) |
| `gardenio-machine-operator-sync` | `GET /v1/admin/operators/slack-aliases`, `POST .../{id}/identity`, `.../link-aliases`, `.../prune-aliases` | slack-bridge (4 routes); beads-viewer needs the `GET` only (`people_mentions.go:84`); chart-only inviteservice | No new branch, but replaces the `requireRoleMachineAllowed` bypass for 4 routes. Consider moving the viewer's read to operator tier instead |
| `gardenio-machine-system-graph` (ownership) | skip `authorizeOwnership` on non-config beads | controller (10 sites, section 3.1), trigger-engine (most writes), beads-viewer (O1), grafana-bridge (4 sites); chart-only gitlab-bridge/github-bridge (privileged-fleet bypass today) | **Yes, reopens the `write_authz.go:413` / `:425` bypass for a new identity type.** Take to a decision. Alternative: a one-time re-owner migration plus per-bead ownership for the self-made rows |
| `gardenio-machine-operated-by-relay` | honor `X-Operated-By` (and `X-Actor` for comments) from a machine principal | beads-viewer (16+ sites, plus `X-Actor` `bv/server.go:3940`), slack-bridge (14 sites), trigger-engine (`triggers.go:166,169,200,3324`), controller assistant-turn minter (`assistantturnminter.go:989,1027`) | **Yes, reopens the header-trust branch** (`interceptors.go:846-848`, `:907-910`). Needed only to attribute work to a human. Without it, every machine write is attributed to `machine:<client>` |

Not proposed: an `X-System-Operator` relay. Once the daemon gives `machine` its own operator
(section 0 item 2), the header is redundant for every caller that uses it (trigger-engine,
grafana-bridge, controller pool manager, viewer label reactor). That is a Task 3 requirement, not
a grant. Under `BEADS_AUTHORING_HUMAN_GATE=enforce`, autonomous types (`trigger`, `schedule`)
additionally need a **human** operator at create (`beads.go:178`): trigger-engine's chain-heir
create (`captain_chain_spawn.go:403`) and the viewer's plan-label bind
(`plan_label_reactor.go:391`) are the two writers affected, and they are the case for the relay
or for a designated human fallback.

### 8.3 Role per k3s component (replaces design §5)

| Principal | Needs beyond `gardenio-operator` | Evidence |
|---|---|---|
| controller | `accounts`, `config-seed`, `advice-close`, `system-graph`, `operated-by-relay` (assistant-turn minter only), `token-mint` (dormant on k3s) | 3.1, section 7 |
| coopmux | `accounts`, `config-seed` (update) | 3.2 |
| trigger-engine | `system-graph`, `operated-by-relay`. **No allow-list entry**. As built (gd-Hc34EENbO9, decisions gd-ZsfxvXRiyv "tight" + gd-dI7PfsFlvr "lineage"): one role `gardenio-machine-trigger-ops` = ownership typed to `trigger`, `task`, `bug`, `feature`, `chore` + update of the `uncovered-open-lane-rollup` doc, and `trigger_lineage` (a create carrying `fields.trigger_id` is attributed to that trigger's human author, server-derived) instead of the relay. Epic ownership deliberately left out. **Spawn gate (gd-is1H5NmI05, decision gd-wNOUd0MCJo "lineage"):** a create by `machine:gardenio-trigger-engine` (with `trigger_lineage`) whose `fields.trigger_id` names an open/enabled trigger or schedule in the same project, whose configured `role` equals the requested role, and which carries a valid **spawn-lineage seal**, passes `authorizeNonHumanSpawn` with no escalation, recorded as `operated_by = the sealed admin` (`kbeads/internal/server/spawn_lineage_authz.go`). The seal (`fields.spawn_lineage`, `spawn_lineage_seal.go`) is stamped by the server only when an **OIDC `gardenio-admin` session** writes the trigger/schedule, holds that session's subject, the stamp time and a digest of the spawn-relevant fields (what runs, when, how often, who may; the `gardener` blob contributes only its configuration keys, never the engine's run counters), and is void if any of them changed since (by anyone). A reopen or re-enable by anything other than an OIDC `gardenio-admin` session strips the seal outright. It is server-owned: no create/update/field-mutation path can set it. `operated_by` / `created_by` / `X-Operated-By` are never trusted for this gate. The daemon holds **no Keycloak credential** for this gate: admin status is proven at seal time from the OIDC token, and the seal lapses after `BEADS_SPAWN_LINEAGE_SEAL_TTL` (default `720h`), so a demoted admin's triggers stop qualifying within that window; any OIDC admin write to a sealed trigger renews it. Triggers created before this change, by a non-OIDC credential, or whose seal lapsed or was stripped are unsealed until an admin runs `kd update <id> -f reseal_spawn_lineage=true` after reviewing them. Any other caller or case escalates as before | 3.3 |
| grafana-bridge | `config-seed`, `system-graph` (or re-own) | 3.4 |
| slack-bridge | `config-seed`, `project-ops`, `operator-sync`, `operated-by-relay`; decision: stay a fleet token (keeps the dm carve-out and legacy created_by match) or inherit them. As built (gd-Hc34EENbO9 component 4, decision gd-ZsfxvXRiyv "tight, amended"): the bridge KEEPS its fleet token for everything and gets a machine principal for two background writes only, per call: `gardenio-machine-slack-config-seed` (config titles typed to the dm/message/mail/decision/escalation types and the decisions/escalations/mail-inbox views, `EnsureSlackBridgeConfigs`) and `gardenio-machine-slack-dm-delivery` (ownership typed to `dm`, the machine form of the fleet dm carve-out, `dm_delivery.go:204,222,281,292,404`). **No relay, no `project-ops`, no `operator-sync`**: person-initiated writes (including `/schedule` and `/trigger`) ride the person's own token via account linking; the admin routes and the legacy created_by match stay on the fleet token | 3.5 |
| beads-viewer | `advice-create`, `project-ops`, `user-self`, `operator-sync` (read), `system-graph`, `operated-by-relay` | 3.6 |
| agent pods | out of scope | 3.7 |

Design §5's rows for `gardenio-gitlab-bridge`, `-jira-bridge`, `-alertmanager-bridge` and
`-roles-server` are chart-only and are not built here. The design's `gardenio-trigger-engine` row
("none identified: `AddComment` on non-admin types") understated the ownership dependency: the
trigger-engine needs the `system-graph` capability.

## 9. Corrections to existing comments and design §5

Fix as part of the Task 3 / Task 5 PRs, not this one (docs-only).

1. `write_authz.go:128-170` says `EnsureConfigs` is run by "every bridge (slack/gitlab/jira/grafana/alertmanager-bridge)" and by `gb`/roles-server. Actual callers: controller, alertmanager, gitlab, grafana, jira, slack, wl (section 3.8). Roles-server does not call it. `gb` does not.
2. `write_authz.go:167-177` names three `update/user` call sites; the viewer has four (`inbox_flag.go:94`).
3. The same comment names `gb pool` and slack-bridge for `project/update`; the viewer (`project_config.go:314,320`) is a third caller.
4. `write_authz.go` does not list `advice/update` or `*/delete`, which roles-server calls (`web_advice.go:150,185`) and which are denied.
5. `bv/team_page.go:195-203` claims the static token is passed through for team writes; it is denied.
6. `cmd/slack-bridge/main.go:94-104` and `IB/thread_task_autoclose.go:5-12` assume `X-System-Operator` binds on a fleet token; it does not (`interceptors.go:906`).
7. Design §5: the `gardenio-controller` row lists five `update/account` writers and `close/advice`; it omits `key_rotation.go:202` and coopmux. Its `gardenio-gitlab-bridge` row names `ownership:system-graph` correctly; the same capability is needed by trigger-engine, controller, viewer and grafana-bridge. The `machineAllowedWrites` shrink list (`project/*`, `user/*`, `config/*`, `jira-poller/create`) is wrong: see 6.1.

## 10. Controls and open verifications

Controls (this task changes no code):

- `authorizeWrite`/`authorizeWriteForBead` in kbeads, non-test: 13 grep lines = 2 definitions + 1 internal wrapper call + 10 external call sites (`git grep -n -E 'authorizeWrite(ForBead)?\(' -- kbeads/internal/server ':!*_test.go'`). `loadBeadForWriteCore` (`http_relations.go:67,69`) is reached from 9 handlers (`http_relations.go:186,302,381,429,521,581,636`, `http_followers.go:59,108`).
- Step-2 grep, non-test files matched per component: controller 18/46 (`cmd/controller`), trigger-engine 1 (`cmd/`), grafana-bridge 1, beads-viewer 58, slack-bridge 1, roles-server 3, github-bridge 3, alertmanager-/gitlab-/jira-bridge 0 (work in `internal/bridge`). All deployed components are non-zero. The `cmd/` counts for the bridges understate by design; the real counts are in section 3's table.
- `machineAllowedWrites`: 15 entries, 15 verdicts (6.1).
- Account writers: 8 (section 7).

Not answerable from the repo (Task 5 must check on the live system before relying on them):

1. Whether header-less static writes (controller `PATCH`/close, coopmux, `EnsureConfigs` on the controller) succeed today. The strict resolver returns empty for a static caller with no header, but `mutation_history.go`'s leniency was not proven. Check `gardenio_beads_operated_by_writes_by_source_total{kind="empty"}` for those subjects.
2. Whether the k3s edge supplies `X-Auth-Request-Email` to the viewer (otherwise most viewer writes 401 in-handler).
3. Whether `gb prime` (`bv/prime.go:42`) writes anything.
4. Whether a live `type:view` config row exists and what its write policy is.
5. Live `BEADS_PRIVILEGED_FLEETS` / `BEADS_SERVICE_FIELD_FLEETS` / `BEADS_AUTHORING_HUMAN_GATE`: this document uses the chart values (`values.yaml:6546,6559`, `authoringGate: "warn"`), which match the brief.
6. Whether builtin type configs (`kbeads/internal/server/config.go`) are overridden by live config rows; admin-gating above is from the builtins.
7. Appendix rows carrying a leading `~` on a line number are approximate (fork output, not re-opened); the table headers and section-1 rows were re-verified.


---

# Appendices: per-call-site tables

Generated by per-component sweeps at commit `67a7ad34`, then spot-checked against the daemon source. Heading numbers inside each appendix are the sweep's own (so "App. B §3" is the slack-bridge section of Appendix B). Within an appendix, paths are relative to `gardenio/controller/` unless prefixed.

## Appendix A. controller

Scope: `gardenio/controller/cmd/controller` + every `gardenio/controller/internal/**` package that the controller process
actually runs and that talks to the beads daemon. All `path:line` below are at 67a7ad34 and relative to
`gardenio/controller/` unless prefixed `kbeads/`.

Method (reproducible):
- Daemon client = `beadsapi.Client`, built once at `cmd/controller/main.go:345-348` with `Token: os.Getenv("BEADS_DAEMON_TOKEN")`
  (primary static). No `SetActor`/`SetOperatedBy`/`SetSystemOperator` call anywhere in `cmd/controller` (grep, no hits).
- Computed the transitive closure of `*beadsapi.Client` methods that reach a POST/PATCH/PUT/DELETE (68 methods; script
  `scratchpad/closure.py`, output `scratchpad/writemethods.txt`), then grepped `cmd/controller` and each package in
  `go list -deps ./cmd/controller` for calls to any of them (`git grep -E "\.(<68 names>)\("`).
- Packages in the dep graph but NOT run by the controller (so excluded): `internal/scheduler/**`, `internal/scif`,
  `internal/pausegate` (only reachable through `internal/bridge`'s trigger/captain code, which the controller never starts;
  the controller calls exactly `bridge.EnsureConfigs`, `bridge.NewClaudeStatusMetrics` / `bridge.ClaudeStatusMetrics` -
  `cmd/controller/main.go:466,667`). Packages with 0 write hits: advice, coopmux, coopmuxapi, deathreport, githubapp,
  healthsnapshot, metrics, nodelifecycle, perftracesrelay, podmanager, pvcgc, pvcobs, pvgc, secretreconciler, spawnguard,
  spendlogs, subscriber, thawepoch, trustmetrics, contextwindow, natsauth, oidcauth, tracing, nscount, covflush, config.
- Route/action mapping used below (kbeads side):
  - `UpdateBeadFields`/`UpdateBeadFieldsTyped`/`UpdateBead`/`UpdateBeadDescription`/`UpdateBeadNotes`/`UpdateAgentState`/`MarkAgentFailed` →
    (GET `/v1/beads/{id}` then) `PATCH /v1/beads/{id}` → `updateBead` → `authorizeWrite(ctx,"update",type,labels)`
    `kbeads/internal/server/beads.go:1206`, then ownership gate `beads.go:1234` (skipped for config-kind, `write_authz.go:401`; replaced by
    `authorizeServiceFieldBypass` when patch only touches service-managed fields, `beads.go:1218-1236`).
  - `CloseBead`/`CloseBeadAs` → `POST /v1/beads/{id}/close` → `authorizeWriteForBead("close")` `http_beads.go:1276` (write policy + ownership + taint gate).
  - `CreateBead` → `POST /v1/beads` → `authorizeWrite("create")` `beads.go:432`.
  - `AddComment` → `POST /v1/beads/{id}/comments` → `loadBeadForComment` → `authorizeWrite("comment")` only; **no ownership gate** (`http_relations.go:30-39,521`).
  - `AddLabel` → `POST /v1/beads/{id}/labels` → `loadBeadForWrite(...,"update")` = write policy + ownership (`http_relations.go:381`).
  - `Register/Revoke/ListBridgeToken*` → `/v1/fleet-tokens` (+`/v1/bridge-tokens` alias) → `requireRoleMachineAllowed(RoleAdmin)` (`http.go:267-272`) + self-gate `isAdminRequest` (literal primary token, `http_fleet_tokens.go:332`).
  - `RecordSlowToolCall` → `POST /v1/metrics/slow-tool-calls` → `requireRole(RoleOperator)` (`http.go:244`); not a bead write, no `authorizeWrite`.
- Type facts (from `kbeads/internal/server/config.go` builtins): admin-gated (`min_role: gardenio-admin`) = account (`:1046-1048`, config-kind), advice (`:470-472`, data), report, project, user, role, fleet, team, config, jira-poller, credential, audit, rig, convoy.
  `agent` (`:618-620`, **kind=config**, `project_scoped`+`escalatable`, no min_role), `schedule` (`:1970`, config-kind), `trigger` (`:1842`, **data**), `session` (`:1771`, data), `message` (`:1304`, data), `escalation` (`:1576`, data), `task` (issue) → no `min_role`.

#### (W) Write table

Legend - `min_role admin?`: type has `min_role: gardenio-admin`. `kind=config?`: ownership gate skipped (`write_authz.go:401`).
Ownership column: `n/a` = create/comment (no ownership gate on those), `skip(config)` = config-kind, `STATIC-BYPASS` = controller is not creator/assignee/owner and relies on `write_authz.go:413`.
"allow-list" = already in `machineAllowedWrites` (`write_authz.go:194-210`).

##### W1. cmd/controller

| file:line | beadsapi method | route | action | bead type | min_role admin? | kind=config? | ownership |
|---|---|---|---|---|---|---|---|
| cmd/controller/account_rotation_restart.go:152 | UpdateBeadFields (`controller_restart_reason`) | PATCH /v1/beads/{id} | update | agent | no | yes | skip(config) |
| cmd/controller/advice_sweeper.go:118 | CloseBead (`close_reason`,`closed_at`) | POST …/close | close | advice (queried at `:84`) | **yes** (allow-list `close/advice`) | no (data) | **STATIC-BYPASS** (advice created by agents/gb/viewer; sweeper never creator) |
| cmd/controller/auth_config_legacy.go:227 | UpdateBeadDescription (via `updateAuthConfigForProject`, called `auth_failover.go:246`) | PATCH | update | config (legacy `config:auth` bead, `:44-46` Types=config, label project:X) | **yes** (allow-list `update/config`) | yes | skip(config) |
| cmd/controller/auth_failover.go:238 | UpdateBeadFieldsTyped (status/exhausted_at/cooldown_until/rotation_count/…) | PATCH | update | **account** | **yes** | yes | skip(config) - **action `update/account` is NOT in allow-list ⇒ 403 today** |
| cmd/controller/auth_failover.go:456 | UpdateBeadFields (`status=active`,`cooldown_until=""`, sweep-exhausted recovery) | PATCH | update | **account** | **yes** | yes | skip(config) - not in allow-list ⇒ 403 today |
| cmd/controller/captain_spend_sync.go:127 | UpdateBeadFieldsTyped (`captain_spend_usd`,`captain_spend_synced_at`) | PATCH | update | **trigger** and **schedule** (`listCaptains` `:161`) | no | trigger: **no (data)**; schedule: yes | trigger: **STATIC-BYPASS** (`captain_spend_*` not in `serviceManagedFieldsByType[trigger]`, `beads.go:~985-1015`); schedule: skip(config) |
| cmd/controller/github_app_credentials.go:598 | CreateBead (`buildGithubAppRefusalEscalation`, Type escalation, `CreatedBy: gardenio-controller`, idempotency key) | POST /v1/beads | create | escalation | no | no (data) | n/a (create) |
| cmd/controller/key_rotation.go:202 | UpdateBeadFieldsTyped (`litellm_key`, rotated_at, count) | PATCH | update | **account** | **yes** | yes | skip(config) - not in allow-list ⇒ 403 today |
| cmd/controller/main.go:2274 | UpdateAgentState (`markAgentFailed`) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/main.go:2280 | MarkAgentFailed | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/main.go:2614 | UpdateAgentState "queued" | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/main.go:2983 | UpdateAgentState "queued" | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/microvm_git_credentials.go:355 | UpdateBeadFields (git token id/project path) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/ratelimit_probe.go:699 | UpdateBeadFields (`probe_auth_status`) | PATCH | update | **account** | **yes** | yes | skip(config) - 403 today |
| cmd/controller/ratelimit_probe.go:767 | UpdateBeadFields (utilization/throttle/model_group_limits/status) | PATCH | update | **account** | **yes** | yes | skip(config) - 403 today |
| cmd/controller/ratelimit_probe.go:835 | UpdateBeadFields (`soft_failover_state`) | PATCH | update | **account** | **yes** | yes | skip(config) - 403 today |
| cmd/controller/reconcile_session_spend.go:306 | UpdateBeadFieldsTyped (`spend_baseline`) | PATCH | update | agent (`agentBeadID`) | no | yes | skip(config) |
| cmd/controller/reconcile_session_spend.go:317 | UpdateBeadFieldsTyped (first baseline) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/reconcile_session_spend.go:456 | UpdateBeadFieldsTyped (`total_cost_usd`, close total) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/reconcile_session_spend.go:464 | UpdateBeadFieldsTyped (first-baseline back-fill) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/reconcile_session_spend.go:546 | UpdateBeadFieldsTyped (`sessionSettledCostField`) | PATCH | update | **session** (`:533`) | no | **no (data)** | **STATIC-BYPASS** (session bead created by gb, not controller) |
| cmd/controller/spawn_credential_guard.go:183 | MarkAgentFailed | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/spawn_credential_guard.go:189 | CreateBead (`buildSpawnRefusalEscalation`) | POST | create | escalation | no | no | n/a |
| cmd/controller/spend_backfill.go:170 | UpdateBeadFieldsTyped (total_cost_usd/close/baseline, one-shot, default off) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/spend_sync.go:224 | UpdateBeadFieldsTyped (total/daily/weekly cost, 5h/7d windows, spend limit) | PATCH | update | **account** | **yes** | yes | skip(config) - **403 today (the `/costs` bug)** |
| cmd/controller/spendlogs_sweep.go:262 | UpdateBeadFieldsTyped (`spend_logs_cost_usd`) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/stuck_handler.go:137 | UpdateBeadFields (`controller_restart_reason`) | PATCH | update | agent | no | yes | skip(config) |
| cmd/controller/terminal_auth_failure.go:230 | UpdateBeadFieldsTyped (error_category/detail/total_errors) | PATCH | update | agent | no | yes | skip(config) |

##### W2. internal packages run by the controller

| file:line | method | route | action | bead type | min_role admin? | kind=config? | ownership |
|---|---|---|---|---|---|---|---|
| internal/bridge/init_ensure.go:97 (`bridge.EnsureConfigs` ← cmd/controller/main.go:466) | UpdateBeadDescription | PATCH | update | config | **yes** (allow-list `update/config`) | yes | skip(config) |
| internal/bridge/init_ensure.go:107 | CreateBead (`Type:config Kind:config CreatedBy:gardenio`) | POST | create | config | **yes** (allow-list `create/config`) | yes | n/a |
| internal/assistantturnminter/assistantturnminter.go:994 | CreateBead (assistant turn, `plantranscript` `Type:message`, `plantranscript.go:145`) | POST | create | message | no | no | n/a |
| internal/assistantturnminter/assistantturnminter.go:1032 | CreateBead (tool turn) | POST | create | message | no | no | n/a |
| internal/microvm/lifecycle.go:326 (`PersistLaunch`) | UpdateBeadFields | PATCH | update | agent | no | yes | skip(config) |
| internal/microvm/lifecycle.go:407 (`Reconcile`) | UpdateBeadFields (`runtime_state`) | PATCH | update | agent | no | yes | skip(config) |
| internal/microvm/lifecycle.go:444 (`Reconcile`) | CloseBead | POST close | close | agent | no | yes | skip(config) |
| internal/microvm/lifecycle.go:477 (`Terminate`) | UpdateBeadFields | PATCH | update | agent | no | yes | skip(config) |
| internal/microvm/lifecycle.go:545 (`Resume`) | UpdateBeadFields | PATCH | update | agent | no | yes | skip(config) |
| internal/microvm/lifecycle.go:606 (`Suspend`) | UpdateBeadFields | PATCH | update | agent | no | yes | skip(config) |
| internal/microvm/lifecycle.go:824 (`reapExpiredStandby`) | CloseBead | POST close | close | agent | no | yes | skip(config) |
| internal/microvm/rehydrate.go:321 | UpdateBeadFields | PATCH | update | agent | no | yes | skip(config) |
| internal/microvm/rehydrate.go:356 | UpdateBeadFields | PATCH | update | agent | no | yes | skip(config) |
| internal/poolmanager/poolmanager.go:540 | CreateBead (`Type:agent`, prewarmed) | POST | create | agent | no | yes | n/a |
| internal/poolmanager/poolmanager.go:552 | AddLabel (`role:<r>`) | POST labels | update | agent | no | yes | skip(config) |
| internal/poolmanager/poolmanager.go:564 | UpdateBead (`Status:closed`, `closePrewarmedAgent`) | PATCH | update | agent | no | yes | skip(config) |
| internal/poolmanager/poolmanager.go:1268 | UpdateBeadFields (assign: agent_state, task_id, `operated_by` *field*, thread, model…) | PATCH | update | agent | no | yes | skip(config) |
| internal/poolmanager/poolmanager.go:1281 | UpdateBead (Description) | PATCH | update | agent | no | yes | skip(config) |
| internal/reconciler/reconciler.go:756 | CloseBead (podless leak) | POST close | close | agent | no | yes | skip(config) |
| internal/reconciler/reconciler.go:1365 | CloseBead (queue expiry) | POST close | close | agent | no | yes | skip(config) |
| internal/reconciler/reconciler.go:1432 (`closeOrphanedMirrorTask`) | CloseBead (`spawn expired before claim`) | POST close | close | **task** (mirror task of trigger/schedule run) | no | **no (issue)** | **STATIC-BYPASS** (task created by trigger-engine/scheduler) |
| internal/reconciler/reconciler.go:1449 | UpdateAgentState "queued" | PATCH | update | agent | no | yes | skip(config) |
| internal/reconciler/reconciler.go:1463 | MarkAgentFailed | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/reporter.go:473 | UpdateAgentState | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/reporter.go:545 | UpdateBeadNotes (backend metadata) | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/reporter.go:696 | UpdateAgentState (coop override) | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/reporter.go:771 | UpdateBeadFields (`pod_scheduling_blocked`) | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/reporter.go:939 | UpdateBeadFieldsTyped (`context_tokens`,`live_cost_usd`,`live_turns` - service-managed fields for agent, `beads.go:~1020`) | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/stalereaper.go:468 | CloseBead (stale agent) | POST close | close | agent | no | yes | skip(config) |
| internal/statusreporter/stalereaper.go:614 | CloseBead (stranded run-instance task) | POST close | close | **task** (`:560` Types=task) | no | no | **STATIC-BYPASS** |
| internal/statusreporter/stalereaper.go:725 | CloseBead (stranded open run-instance) | POST close | close | **task** (`:669`) | no | no | **STATIC-BYPASS** |
| internal/statusreporter/stalereaper.go:905 | UpdateBead (reopen: status=open, assignee="", reopen_attempts; since gd-ovzZSuuHny also Labels -= `task:orphaned-with-pr` when the task carried it) | PATCH | update | **task** | no | no | **STATIC-BYPASS** (not creator/assignee/owner; patch not service-managed) |
| internal/statusreporter/stalereaper.go:923 | AddComment (`gardenio`) | POST comments | comment | task | no | no | n/a (comments skip ownership) |
| internal/statusreporter/stalereaper.go:957 | UpdateBead (park: status=blocked, assignee="", reopen_capped…) | PATCH | update | **task** | no | no | **STATIC-BYPASS** |
| internal/statusreporter/stalereaper.go:979 | AddComment | POST comments | comment | task | no | no | n/a |
| internal/statusreporter/stalereaper.go:1096 | UpdateBead (clear dead assignee) | PATCH | update | **task** | no | no | **STATIC-BYPASS** |
| internal/statusreporter/stalereaper.go:1111 | AddComment | POST comments | comment | task | no | no | n/a |
| internal/statusreporter/stalereaper.go:1249 | UpdateBead (Labels += `task:orphaned-with-pr`; since gd-ovzZSuuHny only when a linked PR is open or its state unreadable) | PATCH | update | **task** | no | no | **STATIC-BYPASS** |
| internal/statusreporter/stalereaper.go:1263 | AddComment | POST comments | comment | task | no | no | n/a |
| internal/statusreporter/stalereaper.go:1677 | UpdateBead (runtime-ceiling dry-run stamp fields) | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/stalereaper.go:1721 | CreateBead (`Type:escalation`, idempotency key) | POST | create | escalation | no | no | n/a |
| internal/statusreporter/wake_scanner.go:248 | UpdateBead (reopen parked agent: status=open + wake fields) | PATCH | update | agent | no | yes | skip(config) |
| internal/statusreporter/wake_scanner.go:278 | AddComment | POST comments | comment | agent | no | yes | n/a |
| internal/statusreporter/wake_scanner.go:290 | UpdateBead (clear wake fields) | PATCH | update | agent | no | yes | skip(config) |
| internal/toollatency/consumer.go:381 | RecordSlowToolCall | POST /v1/metrics/slow-tool-calls | - (no `authorizeWrite`; route `requireRole(RoleOperator)`) | not a bead | - | - | - |

Reads on the same client (`GetBead`, `ListBeadsFiltered`, `ListAgentBeads`, `ListAccountBeads`, `ListProjectBeads`, `ListRoleBeads`, `GetBead`…)
and the SSE watcher (`internal/subscriber/sse.go:130`, GET `/v1/events/stream` with the same static token, `cmd/controller/main.go:221-228`)
are reads; a machine principal needs read parity for them (not covered here). `UpdateBeadFields*` also issues a GET `/v1/beads/{id}` before the PATCH (`client.go:2583,2668`).

Totals: **28** write call sites in `cmd/controller` (W1), **44** in `internal/**` (W2) = **72** bead-write sites; +1 non-bead write (`toollatency`);
+8 fleet-token admin call sites (section A). By type: account 7, config 3 (init_ensure x2, auth_config_legacy), advice 1, trigger/schedule 1, session 1, task 7 writes + 3 comments, message 2, escalation 3 (spawn_credential_guard:189, github_app_credentials:598, stalereaper:1721), agent comments 1, agent remaining.

#### (A) Admin-route / `requireRoleMachineAllowed` calls

| file:line | method | route | notes |
|---|---|---|---|
| cmd/controller/agenttoken.go:102 | RegisterBridgeTokenWithRefresh (privileged=false) | POST /v1/fleet-tokens | per-agent token mint, **gated by `PER_AGENT_TOKENS`** (`internal/config/config.go:2392`, default false; chart `templates/controller/deployment.yaml:291`). Not set in `deploy-values/gardenio-k3s.yaml` ⇒ **dormant on k3s** (PR #273 open changes this). |
| cmd/controller/agenttoken.go:138 | RevokeBridgeToken (rollback) | DELETE /v1/fleet-tokens/{fleet} | same gate |
| cmd/controller/agenttoken.go:172 | RevokeBridgeToken (`revokeAgentToken`; callers main.go:2856,2925,2961, tokenjanitor.go:149) | DELETE | same gate (janitor `beadsTokens`, `tokenjanitor.go:106`) |
| cmd/controller/microvm_credentials.go:67 | RegisterBridgeTokenWithRefresh (privileged=false) | POST | MicroVM backend only + PerAgentTokens |
| cmd/controller/microvm_credentials.go:92 | RevokeBridgeToken | DELETE | MicroVM backend only |
| internal/bridgetokenrotator/rotator.go:258 | ListBridgeTokensDetailed | GET /v1/fleet-tokens?detailed=true | rotator started `cmd/controller/main.go:1330`, only if `BRIDGE_TOKEN_ROTATION_ENABLED` (default false, `config.go:2635`; chart `deployment.yaml:959-961`); no `bridgeTokenRotation` key in `deploy-values/gardenio-k3s.yaml` ⇒ **dormant on k3s** |
| internal/bridgetokenrotator/rotator.go:348 | RegisterBridgeTokenWithRefresh (**`privileged = c.Fleet.Privileged`**) | POST | **mints PRIVILEGED fleet tokens** for fleets in `BridgeTokenRotationPrivilegedFleets` (`main.go:1316-1328`) |

All of these self-gate on `isAdminRequest` (`kbeads/internal/server/http_fleet_tokens.go:76,261,299,332`): a literal constant-time compare against the primary static token.
A machine principal therefore cannot call them even with a grant until kbeads changes that gate (Task 3 / `token-mint`).
No controller call to: SetConfig (`PUT /v1/configs`), operator-alias routes (`/v1/admin/operators/*`), embeddings, `RevokeUser`, `DeleteBead`, taints, `Hooks`, `ResolveDecision/CancelDecision`, `PourFormula` (grep clean).
**Flag (reopens an ownership bypass):** the rotator's `privileged` argument means `token-mint` implicitly includes minting `Privileged` fleet tokens (the ownership-bypass identity, `write_authz.go:425`). Task 5 should take "can a machine principal set `privileged`?" to a decision.
**Flag (credential egress):** MicroVM shared-token mode hands the controller's own `BEADS_DAEMON_TOKEN` to the satellite (`microvm_credentials.go:62,71,79`), and K8s pods get the shared token via `BEADS_TOKEN_SECRET` (`deployment.yaml:289`) unless PerAgentTokens. A machine-principal client secret must never take that path; the static token has to stay for agents until P7.

#### (H) Header dependencies

The controller's client sets headers only via `beadsapi` `doJSON` (`client.go:3279-3325`): `X-Actor` (ctx `WithActor` / `SetActor`), `X-Operated-By` (ctx `WithOperator` / `SetOperatedBy`), `X-System-Operator` (ctx `WithSystemOperator` / `SetSystemOperator`), `X-Escalate`, edge-auth envelope.
Repo-wide there is **no setter** of `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`, `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet` in `beadsapi` or the controller (grep of `"X-…"` literals under `gardenio/`). Inbound-only reads of `X-Actor`: `cmd/controller/main.go:3733,3756,3817`, `internal/podmanager/freezethaw_interrupt_handler.go:66` (controller HTTP endpoints, not daemon calls).

| header | set at | covers | what breaks if the daemon ignores it |
|---|---|---|---|
| X-Actor | **never** (controller has no `SetActor`/`WithActor`) | - | nothing |
| X-Operated-By | `internal/assistantturnminter/assistantturnminter.go:989` (assistant turn mint) and `:1027` (tool turn mint) via `WithOperator(plan epic's operated_by, read at :887)` | message creates `:994,:1032` | human attribution of plan-transcript turns lost; static + no header ⇒ strict resolver (`kbeads/internal/server/audit.go:898`, used by create `beads.go:152`) has nothing ⇒ create refused 400 `operator could not be resolved` unless the machine principal's own identity resolves. Turn persistence is best-effort (`:996-1000`, logs and drops) ⇒ transcript turns silently lost. |
| X-System-Operator | `internal/poolmanager/poolmanager.go:240` (`source=pool-manager`, covers prewarm create/label/close/recycle in the reconcile loop: `:540,552,564`), `:1026` (assign: `:1268,:1281`), and `assistantturnminter.go:991,1029` (`assistant-turn-minter`, fallback when epic has no operator) | agent create/label/update/close in the pool; message creates | Honored only in the static branch (`interceptors.go:~874-880`). Comment in poolmanager (`:232-239`) documents HTTP 400 "operator could not be resolved" when absent on a static token. For a machine principal it is **not honored** (design §4.1) ⇒ pool create/assign refused unless the resolver attributes `machine:<clientId>` itself. Under `BEADS_AUTHORING_HUMAN_GATE=enforce` (live: `warn`) `system:<src>` collapses to `system:machine` (`audit.go:~1100`). |
| (none) | every other write site in W1/W2 sets **no** operator/actor/system-operator ctx | all controller PATCH/close/comment/escalation-create sites | They resolve today by whatever the static branch yields for a header-less call. The strict resolver (create/comment/dependency/formula-pour/mutation history: `beads.go:152`, `server.go:273,448,886`, `http_relations.go:196,531`, `mutation_history.go:56,382`) returns empty for static-without-header ⇒ I could not prove from the repo whether live PATCH/close succeed header-less (e.g. via the `mutation_history` path being lenient) vs 400. **Open verification for Task 5:** check `gardenio_beads_operated_by_writes_by_source_total{kind="empty"}` for the controller subject. If these writes fail closed today, `escalation` creates (`github_app_credentials.go:598`, `spawn_credential_guard.go:189`, `stalereaper.go:1721`) and the stale-reaper comments are already best-effort-failing; a machine principal fixes them only if the resolver stamps `machine:<id>` as operator. |
| env into agent pods (not a daemon call) | `internal/podmanager/manager.go:1996,2018` set `BEADS_ACTOR=<agent>` and `BEADS_OPERATED_BY=<spec.OperatedBy>`; `cmd/controller/podspec.go:240,463` fill `OperatedBy` | agent-pod `gb`/`kd` | out of scope (P7); noted because the controller is what makes `X-Actor`/`X-Operated-By` exist on agent traffic |

Also note: `poolmanager.go:1224-1231` writes `operated_by` as a *bead field* in the assign PATCH (not a header); a machine principal still needs `update/agent`-equivalent operator access for it.

#### (O) Ownership dependencies (non-config beads where the controller is not creator/assignee/owner)

All rely on the **primary static token bypass** `kbeads/internal/server/write_authz.go:413`. A `machine` principal gets none of: static bypass `:413`, privileged-fleet bypass `:425`, dm carve-out `:439`. It would 403 on each row unless given an ownership capability.

| site | bead (type, kind) | action | why controller is a stranger |
|---|---|---|---|
| internal/statusreporter/stalereaper.go:614 | task (issue) | close | reaps stranded run-instance task owned by another agent/trigger |
| internal/statusreporter/stalereaper.go:725 | task | close | same, never-claimed open run-instance |
| internal/statusreporter/stalereaper.go:905 | task | update (status=open, assignee="", fields; labels when dropping `task:orphaned-with-pr`) | reopens work whose assignee died |
| internal/statusreporter/stalereaper.go:957 | task | update (status=blocked, assignee="") | parks capped task |
| internal/statusreporter/stalereaper.go:1096 | task | update (assignee="") | clears dead assignee |
| internal/statusreporter/stalereaper.go:1249 | task | update (labels) | labels `task:orphaned-with-pr` |
| internal/reconciler/reconciler.go:1432 | task | close | closes the mirror task of an expired spawn; ownership check inside is on `trigger_id/schedule_id` field match (`:1415-1424`), unrelated to bead ownership |
| cmd/controller/advice_sweeper.go:118 | advice (data) | close | sweeps advice created by agents/viewer |
| cmd/controller/reconcile_session_spend.go:546 | session (data) | update | session beads created by `gb session` |
| cmd/controller/captain_spend_sync.go:127 | trigger (data) | update (`captain_spend_*`) | `captain_spend_*` is NOT in `serviceManagedFieldsByType[trigger]` (list is last_checked/last_outcome/last_run/last_agent_id/last_match/last_fire_error*, `beads.go:~990-1012`), so the service-field bypass does not apply; the schedule half is config-kind (skip) |

Comments (`AddComment`) never hit the ownership gate (`http_relations.go:30-39`), so the 4 stalereaper/wake-scanner comment sites need only type-level `comment` rights.
Not used by the controller: service-managed-field bypass for trigger/schedule (that is trigger-engine/jira-bridge), dm carve-out (`:439`), escalation/decision shared-queue carve-out, privileged-fleet bypass, `authorizeScifApprovalStamp`.
Every `agent`/`account`/`config` write is config-kind ⇒ no ownership dependency (only the type write-policy matters; for `account`/`config` that is the admin gate).

**Capability implication:** the controller needs, beyond `gardenio-operator`, an **ownership bypass** (or a narrower "reap/close stranded tasks, close advice, settle sessions, stamp captain spend" capability) for the 10 rows above - this reopens the `:413` branch for a new principal type; take to a decision in Task 5.

#### (ACC) Complete list of controller writes to `type:account` beads

All are `PATCH /v1/beads/{id}` ⇒ action `update`, type `account` (admin-gated, config-kind). `update/account` is **absent** from `machineAllowedWrites` ⇒ all 403 today (design §1).

| # | file:line | what | loop |
|---|---|---|---|
| 1 | cmd/controller/spend_sync.go:224 | UpdateBeadFieldsTyped: total/daily/weekly cost, shadow_*, 5h/7d windows, spend limit fields | `newSpendSync` main.go:1159 (LiteLLM enabled on k3s, `deploy-values/gardenio-k3s.yaml:666-730`) |
| 2 | cmd/controller/ratelimit_probe.go:699 | UpdateBeadFields `probe_auth_status=rejected` | `newRateLimitProbe` main.go:1138 |
| 3 | cmd/controller/ratelimit_probe.go:767 | UpdateBeadFields utilization/throttle/reset/model_group_limits/legacy status | same |
| 4 | cmd/controller/ratelimit_probe.go:835 | UpdateBeadFields `soft_failover_state` | same |
| 5 | cmd/controller/auth_failover.go:238 | UpdateBeadFieldsTyped status=exhausted / deprioritized_until / rotation_count (via failover `Handle`) | `newAuthFailover` main.go:1004; fed by `account_exhaustion.go` NATS listener (that file itself has no daemon call) |
| 6 | cmd/controller/auth_failover.go:456 | UpdateBeadFields status=active, cooldown_until="" (sweep-exhausted recovery) | same |
| 7 | **cmd/controller/key_rotation.go:202** | UpdateBeadFieldsTyped `litellm_key`, `key_rotated_at`, `key_rotation_count` - **NEW, not in the known six** | `newKeyRotator` main.go:1255 (runs widen-only unless `LITELLM_KEY_ROTATION_ENABLED`; the PATCH only fires when rotation is enabled and the key is past max age) |

Checked and NOT account writers: `key_acl_widen.go` (LiteLLM only, no daemon call), `litellm_aliases.go`, `account_exhaustion.go`, `account_spread*.go`, `account_pool_validator.go`, `served_model_sync.go`, `tag_spend_sync.go` (reads + metrics only), `credsync_notifier.go`, `rotation_notifier.go`, `probe_event_listener.go` (reads), `failover_runtime.go`. No create/close/delete on accounts anywhere in the controller. `auth_config_legacy.go:227` writes the *legacy `config:auth` bead* (type `config`, `update/config`, allow-listed) - adjacent, not account.
So Task 4 moves **7** sites, not 6 → a comment on gd-s7aPIf0Bgp is owed (key_rotation.go:202).
Outside controller (FYI): `beadsapi.UpdateAccountUsage` (`client_session.go:321`) increments account bead usage via `POST /v1/beads/{id}/increment`, called only from `cmd/gb/session.go:586`; that handler (`kbeads/internal/server/http_beads.go:941-990`) calls `tokenAuthorizedForBead` and **not** `authorizeWrite`, so it is not subject to `min_role`/`machineAllowedWrites` at all.

Writes made on a user's bearer (`doJSONAsUser`, `user_bearer.go`): **none** in the controller - every call above uses the process static token.

#### (C) Counts (controls)

- `cmd/controller` non-test files matched by the task's exact Step 2 grep (`\.(CreateBead|UpdateBead\w*|CloseBead|DeleteBead|AddComment|AddDependency|RemoveDependency|AddLabel|RemoveLabel|SetConfig|Register\w*Token\w*|Revoke\w*Token\w*|\w*Operator\w*)\(`): **18 files** of 46 non-test files in `cmd/controller`
  (account_rotation_restart, advice_sweeper, agenttoken, auth_config_legacy, auth_failover, captain_spend_sync, github_app_credentials, key_rotation, microvm_credentials, microvm_git_credentials, ratelimit_probe, reconcile_session_spend, spawn_credential_guard, spend_backfill, spend_sync, spendlogs_sweep, stuck_handler, terminal_auth_failure).
  The literal grep false-positives `reconcile_session_spend.go:294,502` (`EnsureOperatorSeries`/`RecordOperatorSpend` are metrics, not daemon calls) and does NOT match `main.go` (it uses `UpdateAgentState`/`MarkAgentFailed`). The 68-method closure grep matches **19 files** (the 18 minus none, plus `main.go`; `main.go:2274,2280,2614,2983`). Use the closure list, not the Step 2 grep, for Task 5.
- Write call sites recorded: **72** bead writes (28 cmd/controller + 44 internal) + 1 non-bead write + 8 fleet-token/admin calls = **81**. Non-zero for every group.
- `authorizeWrite`/`authorizeWriteForBead` call sites in kbeads non-test code (for the parent's Step 1): 11 outside the definitions - `beads.go:432,1206,1975,2039`, `http_beads.go:763,1276`, `http_hooks.go:94`, `http_relations.go:67,69`, `user_identity_authz.go:148`; plus definitions/internal chain `write_authz.go:250,374,375`.
- Controller deployed on k3s: yes (`deploy-values/gardenio-k3s.yaml:1041-1042`). Live-on-k3s loops with account/config/advice/task writes: spend sync, rate-limit probe, auth failover, key rotator (widen-only; PATCH only on rotation), spend reconciler/sweeps (LiteLLM enabled), stale reaper, wake scanner, advice sweeper, captain spend sync, pool manager (if pool configured), EnsureConfigs at boot. Dormant on k3s by missing values: per-agent token mint/revoke, bridge-token rotator, MicroVM paths.

#### Capability summary for the controller (input to Step 6)

1. `gardenio-machine-accounts` = `update/account` - 7 sites (ACC). The only admin-gated type the controller writes beyond the existing allow-list.
2. Already allow-listed and controller-used: `close/advice` (advice_sweeper.go:118), `create/config` + `update/config` (EnsureConfigs, auth_config_legacy.go:227). Controller is a caller of `update/config` and `create/config` ⇒ these two entries are NOT deletable until the controller has grants for them (and EnsureConfigs is also run by every bridge).
3. `gardenio-machine-token-mint` = fleet-token routes (POST/GET/DELETE) - dormant on k3s, but `isAdminRequest` literal-token gate must be generalised. Includes `privileged` flag question.
4. **Ownership bypass capability** (new, reopens `write_authz.go:413` branch): 10 sites in (O).
5. **Operator-attribution**: controller's `WithOperator`/`WithSystemOperator` (assistantturnminter, poolmanager) are ignored for a machine principal; resolver must stamp `machine:<clientId>`.
6. Everything else (agent/message/escalation/task comment, agent PATCH/close) = `gardenio-operator` baseline via `requireRole`.


## Appendix B. trigger-engine, grafana-bridge, slack-bridge, chart-only bridges, roles-server

All `path:line` are at commit `67a7ad34`. Paths are relative to `gardenio/controller/` unless prefixed `kbeads/`, `gardenio/helm/` or `deploy-values/`.
`IB` = `internal/bridge/`. "static" = primary static token (`kd-beads-token`). "fleet" = bridge token (`bridge:<name>` subject).

#### 0. Method and server-side facts the rows rely on

* Step-2 regex applied to `cmd/<c>` finds almost nothing for the bridges: their `cmd/` dirs are thin and the work lives in `internal/bridge` (307 non-test files; 90 match the Step-2 regex, 346 matched lines) and, for trigger-engine, `internal/scheduler`. The Step-2 regex also misses `SpawnAgent` / `SpawnAgentResult` (`internal/beadsapi/client.go:1167,1216`; `SpawnAgentResult` ends in `c.CreateBead` of `type:agent` at `client.go:1330`) and `UpsertMergeRequestBead` (`internal/beadsapi/client_mr.go:128`, `CreateBead`/`UpdateBead` of `type:merge-request`). Both are added below as writes.
* Write-site extraction used: `git grep -E '\.(CreateBead\w*|UpdateBead\w*|CloseBead\w*|DeleteBead|AddComment|AddDependency|RemoveDependency|AddLabel|RemoveLabel|SetLabels|SetConfig|Register\w*Token\w*|Revoke\w*Token\w*|\w*Operator\w*|ResolveDecision|CancelDecision|CaptureEscalationResult|DeleteDevice|...)\('` over `internal/bridge`, `internal/scheduler`, each `cmd/<c>`, plus a distinct-method census of every `daemon.X(` call to catch methods the regex misses.
* Method → route → authz action (from `kbeads/internal/server/http.go:173-189`, `http_relations.go`, `http_beads.go`):
  `CreateBead` → `POST /v1/beads` → `create` (`beads.go:432`); `UpdateBead*`/`UpdateBeadFields*`/`UpdateBeadDescription`/`SetLabels` → `PATCH /v1/beads/{id}` → `update` (`beads.go:1206`, then ownership `beads.go:1234`); `CloseBead` → `POST /v1/beads/{id}/close` → `close` (`http_beads.go:1276`, ownership included); `AddDependency`/`RemoveDependency`/`AddLabel`/`RemoveLabel` → `update` on the **dependent/labelled** bead via `loadBeadForWrite` (`http_relations.go:186,302,381,429`, ownership enforced); `AddComment` → `comment` via `loadBeadForComment` (`http_relations.go:521`, **no ownership gate**, `authorizeWrite` only); `DeleteBead` → `DELETE /v1/beads/{id}` → `requireRole(RoleAdmin)` (`http.go:250`, **not** machine-allowed) + `delete`; `ResolveDecision`/`CancelDecision` → `POST /v1/decisions/{id}/resolve|cancel` (`requireRole(RoleOperator)`, `http.go:218-219`); `CaptureEscalationResult` → `POST /v1/escalations/{id}/result` (operator route, `http.go:222`).
* Admin-gated builtin types (`kbeads/internal/server/config.go`): `jira-poller` :454 (config), `advice` :470 (data), `report` :832 (data), `project` :938 (config), `fleet` :988 (config), `user` :1001 (config), `account` :1046 (config), `team` :1118 (config), `role` :1143 (config), `spawn_policy` :1193 (config), `rig`/`convoy`/`config` :1202-1204 (config), `credential` :1509 (data), `audit` :1529 (data). Config-kind (skips `authorizeOwnership`, `write_authz.go:401`) additionally: `agent` :618 (project_scoped, no min_role), `schedule` :1970 (project_scoped). `trigger` :1842 is **data** kind (ownership applies), project_scoped+escalatable, no min_role. `project_scoped` is enforced only for OIDC (`write_authz.go:313`), so it is inert for static/fleet/machine.
* Operator resolution (`kbeads/internal/server/audit.go`): every create/dep/comment/close/update path calls `resolveAuditOperatorRequired` (`beads.go:152`, `http_relations.go:196,531`, `server.go:273,448,886`, `mutation_history.go:56,382`, `http_formula_pour.go:657`). An **empty** resolved operator is a 400 refusal regardless of `BEADS_AUTHORING_HUMAN_GATE` (`audit.go:903-905`). `resolveOperatedByPure` (`audit.go:1020`) has cases only for `oidc`, `static`, `fleet`, `edge`, `none` - **no `machine` case**. static: `X-Operated-By` header, else agent-bead chain walk on subject `static` (no agent bead → empty), else `X-System-Operator` override. fleet: `X-Operated-By`, else spawn-chain human, else `fleet:<subject>` sentinel unless gate=enforce (then refused). `X-System-Operator` is read **only** in the static branch (`interceptors.go:871`; fleet branch comment at `:906` says it is static-only). Chart default `global.humanOperator.authoringGate: "warn"` (`gardenio/helm/gardenio/values.yaml:~2123`); warn = behaves as off except metric (`authoring_human_gate.go:72`, enforce-only `authoringHumanGateEnabled`). Autonomous types (trigger/schedule) additionally require a **human** operator at create under enforce (`beads.go:178`).
* beadsapi header surface: only `X-Actor`, `X-Operated-By`, `X-System-Operator`, `X-Escalate`, and edge-auth envelope headers (`internal/beadsapi/client.go:3279-3328`). `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default` are **set by no code in gardenio/controller** (full-repo grep). `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet` are set only by the kbeads `kd` client (`kbeads/internal/client/http.go:1660-1683`), i.e. agent pods. `SetActor` is called only by `cmd/gb/main.go:330`; `WithActor` only by beads-viewer.
* created_by is the **auth subject**, never the request field (`beads.go:142,794`, `actorFromContext` = `AuthIdentity.Subject`, `audit.go:200`). So every bead a static-token component creates carries `created_by="static"`, and a fleet token's beads carry `bridge:<name>` (`bridgetokenrotator.deriveSubject`). A new machine subject will match neither (see Cross-cutting #3).
* Ownership exceptions that matter below: static bypass `write_authz.go:413`; privileged fleet `:425` (live `gitlab-bridge` only; `values.yaml:4680`); dm carve-out `:439` (any fleet); escalation+decision shared-queue carve-out `write_authz.go:482` (**any non-OIDC caller** passes on type escalation/decision); bridge-legacy created_by match (`bridgeSubjectMatchesLegacyCreatedBy`, `:~545`, fleet only); service-managed-field bypass (`beads.go:1040`, field allowlist for `trigger` and `schedule` at `beads.go:973-1015`, gate `service_field_policy.go:122`: static and privileged fleet unconditional, ordinary fleet only if listed in `BEADS_SERVICE_FIELD_FLEETS`); `authorizeScifApprovalStamp` (not used by these components). `callerIsBeadOperator` (`write_authz.go:669`) never fires for static/fleet.
* Authentication on k3s (verified): trigger-engine `templates/trigger-engine/deployment.yaml:85-88` and grafana-bridge `templates/grafana-bridge/deployment.yaml:56-59` use the **primary static token**; slack-bridge uses the **fleet token** from Secret `bridge-tokens` key `slack-bridge` (`templates/slack-bridge/deployment.yaml:81-92`; `deploy-values/gardenio-k3s.yaml:816`). `edgeAuth.enabled: false` on k3s (`deploy-values/gardenio-k3s.yaml:1312`) so slack-bridge's edge-auth HMAC signing is **dormant live**.

---

#### 1. trigger-engine (deployed on k3s; static primary token)

Entry: `cmd/trigger-engine/main.go:270-272` builds the client with `BEADS_DAEMON_TOKEN` and no `SetActor`/`SetOperatedBy`/`SetSystemOperator`. It runs `bridge.TriggerEngine` (+ `ScheduleTriggerSource`, `FormulaStepDispatcher`, `NoticeNotifier`, `scheduler.{Orphan,AutoIntake,NeedsCaptain}Sweeper`). It does **not** call `EnsureConfigs`.

##### (W) Writes

Types: `trigger`=data, no min_role, project_scoped(OIDC only); `agent`,`schedule`=config-kind; nothing here touches an admin-gated type (no `min_role` row). Ownership column: "static" = relies on `write_authz.go:413`.

| file:line | function | method (route/action) | bead type | admin-gated? | config kind? | ownership relationship |
|---|---|---|---|---|---|---|
| `IB/triggers.go:2340,2367,2437,2454` | stampCheckedOutcome / stampLastRun / stampFireError / clearFireError | `UpdateBeadFields` (PATCH, update) | trigger | no | no | trigger bead authored by a human/agent, engine is not creator/assignee/owner. Patch limited to service-managed fields (`last_checked,last_outcome(_at),last_run,last_agent_id,last_match,last_fire_error(_at)`, `beads.go:~1000`) → `authorizeServiceFieldBypass` (`service_field_policy.go:122`) returns nil for static; **depends on the service-field bypass, which for a non-static, non-privileged caller needs the token's fleet in `BEADS_SERVICE_FIELD_FLEETS`** |
| `IB/triggers.go:2418` | stampFireError (captain arm) | `UpdateBeadFieldsTyped` (update) | trigger | no | no | patch includes `captain` and `consecutive_failures`, which are **not** in the service-field allowlist → goes to `authorizeOwnership` → **static bypass** |
| `IB/triggers.go:3291` | commentLeaseStaleOnce | `AddComment` (comment) | trigger | no | no | comment: no ownership gate |
| `IB/triggers.go:4072` | createTaskBead | `CreateBead` (create) | task/bug/feature/chore (`createBeadTypeAllowlist`, :4030) | no | no | creator = subject `static` |
| `IB/triggers.go:4005,4012,4081` | writeTriggerLineage / createTaskBead | `AddDependency` (update on dependent) | dependent = just-created task/molecule | no | no | dependent is self-created (`created_by=static`) |
| `IB/triggers.go:3644,3969` | fireAction / driver spawn | `SpawnAgent` → `CreateBead` type agent | agent | no | **yes** | privileged-role spawn gate `authorizeNonHumanSpawn` (`spawn_authz.go:141`) applies to any non-OIDC caller; not an allow-list item |
| `IB/trigger_permissions.go:243` | recordTriggerAudit | `CreateBead` | event | no | no | self-created |
| `IB/trigger_source_schedule.go:282,301` | healWedgedTriggers | `UpdateBead` (status=open, assignee="") + `AddComment` | trigger / schedule beads | no | trigger no; schedule yes | trigger half: **static bypass** (status/assignee are non-service fields) |
| `IB/trigger_source_schedule.go:537,555` | healStalledOpenCaptains | `UpdateBeadFields` (`captain`,`last_checked`) + `AddComment` | trigger | no | no | `captain` not allowlisted → **static bypass** |
| `IB/captain_evaluator.go:548,658,746,853,991,1042,1165,1308` | captain* | `AddComment` | trigger | no | no | comment |
| `IB/captain_evaluator.go:564,1168,1319` | captainResumeOrEscalate / captainEscalateCredCrashloop / captainTerminate | `CloseBead` (close) | trigger | no | no | **static bypass** (closing a captain trigger authored by someone else) |
| `IB/captain_evaluator.go:602,661,749,856,995,1046` | captain* | `UpdateBeadFields` (`captain`) | trigger | no | no | **static bypass** |
| `IB/captain_evaluator.go:1362,1440,1720` | captainDisarmOnClose / captainPostFire / captainAgentTracker | `UpdateBeadFieldsTyped` (`enabled`, counters, `captain`) | trigger | no | no | **static bypass** |
| `IB/captain_evaluator.go:1097` | captainAddLabel | `AddLabel` (update) | arbitrary bead id | no | maybe | **static bypass** (label on a bead the engine did not author) |
| `IB/captain_hooks.go:131,264,398,471` | captain progress hooks | `UpdateBeadFields` (`captain`) | trigger | no | no | **static bypass** |
| `IB/captain_second_life.go:118,238` / `181` / `205` / `230` | captainGrantSecondLife | `AddComment` / `UpdateBead` (status=open reopen) / `UpdateBeadFields` / `UpdateBeadFieldsTyped` | trigger | no | no | **static bypass** (reopens a closed trigger) |
| `IB/captain_operator_reopen.go:126,136` | captainRearmFromOperatorReopen | `UpdateBeadFields` + `AddComment` | trigger | no | no | **static bypass** |
| `IB/captain_chain_spawn.go:145,166` / `212` / `220` / `225` | maybeSpawnChainHeir / spawnChainHeir | `AddComment` / `CreateBead` type **trigger** (autonomous type, `captain_chain_spawn.go:403`) / `AddLabel` / `AddDependency`(heir→epic, parent-child) | trigger | no | no | create requires a **human operator** under authoring gate=enforce (`beads.go:178`); deps on self-created heir |
| `IB/captain_epic_reaper.go:335,340` | reapOneClosedEpicCaptain | `AddComment` + `CloseBead` | trigger (captain) | no | no | **static bypass**; ctx has `WithSystemOperator` (:183) |
| `IB/captain_liveness.go:546,631` / `561,639` | createLivenessAllowListBead / createLivenessBead | `CreateBead` type **bug** + `AddDependency`(→captain, parent-child) | bug | no | no | dependent self-created; ctx `WithSystemOperator` (:231,406) |
| `IB/captain_pouring_stall.go:499,515,521` / `581` / `593` | applyPourStallVerdict / createPourStallBead | `UpdateBeadFields` on trigger / `CreateBead` bug / `AddDependency` | trigger, bug | no | no | trigger stamp = **static bypass** (fields not in allowlist); ctx `WithSystemOperator` (:185) |
| `IB/trigger_credential_wedge.go:212` / `258` / `265` | fireCredentialWedgeAlert / createCredentialWedgeBead | `AddComment` on **agent** bead / `CreateBead` bug / `AddDependency` | agent (comment), bug | no | agent yes | comment no ownership; ctx `WithSystemOperator` (:100) |
| `IB/trigger_dark_lane.go:367,565` / `374,572` | createDarkLaneBead / createConditionDarkLaneBead | `CreateBead` bug + `AddDependency`(→trigger, parent-child) | bug | no | no | self-created; ctx `WithSystemOperator` (:141) |
| `IB/closed_enabled_drain.go:431,485,648` | autoArchive*Cohort / archiveDecisionCandidate | `UpdateBeadFieldsTyped` (`enabled=false`) | trigger/schedule (closed) | no | schedule yes | **static bypass** for trigger; ctx `WithSystemOperator` (:277) |
| `IB/closed_enabled_drain.go:446,499,656` | same | `AddComment` | trigger/schedule | no | - | comment |
| `IB/closed_enabled_drain.go:601,607` | executeDrainDecision | `UpdateBeadFields` | **decision** bead | no | no | decision shared-queue carve-out would pass any non-OIDC; also static |
| `IB/closed_enabled_drain.go:773` | fileDrainDecision | `CreateBead` | decision | no | no | self-created |
| `IB/stale_revival_review_escalate.go:277,294` | escalateStaleRevivalReview | `UpdateBead` (priority) + `AddComment` | decision | no | no | decision carve-out (any non-OIDC) and static; ctx `WithSystemOperator` (:206) |
| `IB/uncovered_open_lane_rollup.go:433,446` | upsertUncoveredOpenLaneRollupDoc | `UpdateBeadFields` / `CreateBead` | doc | no | no | self-created doc (`created_by=static`); ctx `WithSystemOperator` (:166) |
| `IB/gardener_dormant_sweep.go:225,262` | fileDormantGardenerEscalation | `AddComment` / `CreateBead` | escalation | no | no | self-created escalation (comment only on its own marker-labelled one); ctx `WithSystemOperator` (:120) |
| `IB/dark_agent_budget.go:177,189` | markAgentBudgetExceeded | `UpdateBeadFields` + `AddComment` | agent | no | **yes** | config-kind, ownership skipped |
| `IB/formula_instantiate.go:85` / `171` / `193,215,242` / `201` / `258` | instantiateFormula | `CreateBead` (molecule, then step beads: type `typ` from formula) / `AddDependency` (parent-child, blocks) / `UpdateBeadFields` / `UpdateBeadFieldsTyped` | molecule + step types (task/…) | no | no | self-created in same call; cross-pour dep :242 targets an **upstream step in another molecule** → **static bypass** if not self-created |
| `IB/formula_step_dispatch.go:730,788` / `781` | dispatch | `UpdateBeadFields` on step bead / `SpawnAgent` | step type / agent | no | agent yes | step beads created by the engine itself; ctx operator from step or default (`:854-863`) |
| `IB/notice.go:87,101` | NoticeNotifier | `CreateBead` type message + `CloseBead` | message | no | no | self-created then closed |
| `cmd/trigger-engine/main.go:1185,1216` | orphanStore.AddLabel / needsCaptainStore.AddLabel | `AddLabel` (update) | **arbitrary ready task bead / active agent bead** | no | agent yes | task beads authored by others → **static bypass** (the dominant cross-owner write) |
| `cmd/trigger-engine/main.go:1227` | orphanStore.AddComment | `AddComment` | task | no | - | comment |
| `cmd/trigger-engine/main.go:1453` | autoIntakeStore.RemoveLabel | `RemoveLabel` (update) | arbitrary intake task | no | no | **static bypass** |
| `internal/scheduler/per_run_task_close.go:67` (via `IB/captain_hooks.go:52`) | ClosePerRunTaskBead | `CloseBead` | per-run task bead | no | no | created by the engine/spawn path under subject `static`; bypass on any bead stamped `schedule_id` |

SSE/read side: `bridge.NewSSEStream` (`main.go:332`) and all `List*`/`Get*` calls are reads; out of scope.

##### (A) Admin-route calls
None. No `SetConfig`, fleet/bridge-token, operator-alias or embeddings call from this component (grep of `cmd/trigger-engine` + the bridge/scheduler files above).

##### (H) Header dependencies
* `X-Operated-By` via `beadsapi.WithOperator`: `IB/triggers.go:166` (trigger.OperatedBy), `:169` (trigger.CreatedBy), `:200` (`defaultOperator` ← `TRIGGER_ENGINE_DEFAULT_OPERATOR`, `templates/trigger-engine/deployment.yaml:162`, pinned `oidc:matthewbaker1110@gmail.com` at `deploy-values/gardenio-k3s.yaml:1296`), `:3324` (`matchOperatorResolver`), `IB/formula_step_dispatch.go:859,863`. Honored by the **static** branch (`interceptors.go:841`). If a machine principal ignores it: autonomous fires lose the human lineage; `createBead` of trigger beads (chain heir) is **refused under enforce** (`beads.go:178`); task/mol creates resolve to no operator and are refused (`audit.go:903`) unless the principal itself resolves as an operator. Needs an "operated-by relay" capability or the daemon must give `machine` its own operator.
* `X-System-Operator` via `WithSystemOperator`: `IB/captain_epic_reaper.go:183`, `captain_liveness.go:231,406`, `captain_pouring_stall.go:185`, `closed_enabled_drain.go:277`, `stale_revival_review_escalate.go:206`, `trigger_check_liveness.go:111`, `trigger_credential_wedge.go:100`, `trigger_dark_lane.go:141`, `uncovered_open_lane_rollup.go:166`. Honored only in the static branch (`interceptors.go:871`). Ignored header ⇒ these sweep writes have no operator ⇒ 400 refusal.
* `X-Actor`, `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`, `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet`: **not set** by this component. (`X-Actor` unset ⇒ subject stays `static`.)

##### (O) Ownership dependencies
Non-creator mutations of non-config beads, all riding the static bypass (`write_authz.go:413`) unless noted: every `captain_*`/`trigger_source_schedule` update/close/label on trigger beads (data kind); `AddLabel`/`RemoveLabel` on arbitrary task beads (`cmd/trigger-engine/main.go:1185,1216,1453`); `CloseBead` of per-run tasks (`per_run_task_close.go:67`); decision updates (`closed_enabled_drain.go:601,607`, `stale_revival_review_escalate.go:277`) - these would also pass the decision carve-out. Trigger stamps additionally use the service-managed-field bypass (`beads.go:1040`). Agent/schedule writes need no ownership (config-kind). A machine principal without a static-equivalent bypass gets ownership 403 on all of the above.

##### (C) Counts
Step-2 regex over `cmd/trigger-engine`: **1** non-test file, 5 matched lines (`main.go:372` option name, `:1185,1216,1227,1453` writes). Write call sites recorded in the table: **~101** (96 in `internal/bridge` incl. 2 shared notice + 7 formula_instantiate, 4 in `cmd/trigger-engine/main.go`, 1 in `internal/scheduler`). `internal/bridge` files with ≥1 write attributable to this component: 20.

---

#### 2. grafana-bridge (deployed on k3s; static primary token)

Entry: `cmd/grafana-bridge/main.go:71,118-127`: static token + `daemon.SetSystemOperator("grafana-bridge")` (client-wide `X-System-Operator`). Calls `bridge.EnsureConfigs` (`main.go:79`) with `context.Background()`.

##### (W) Writes

| file:line | function | method | bead type | admin-gated? | config kind? | ownership relationship |
|---|---|---|---|---|---|---|
| `IB/init_ensure.go:107` | ensureConfigBead | `CreateBead` type **config**, kind config | config | **yes** (`type:config` min_role admin) → `create/config` | yes | n/a (config-kind) |
| `IB/init_ensure.go:97` | ensureConfigBead | `UpdateBeadDescription` (PATCH, update) | config | **yes** → `update/config` | yes | n/a |
| `IB/grafana_backfill.go:136` | CreateDashboardBead | `CreateBead` type dashboard (issue kind) | dashboard | no | no | creator = `static` |
| `IB/grafana_dashboard_store.go:97` | PutModel | `UpdateBeadFields` on existing model doc | doc | no | no | doc created earlier by this process under `static`; operators may also have created/edited → **static bypass** |
| `IB/grafana_dashboard_store.go:111,148` | PutModel / ArchiveConflict | `CreateBead` type doc | doc | no | no | self |
| `IB/grafana_dashboard_store.go:123,160` | PutModel / ArchiveConflict | `AddDependency`(doc→dashboard, parent-child) | dependent = new doc | no | no | self-created dependent |
| `IB/grafana_dashboard_store.go:126,163` | PutModel / ArchiveConflict | `AddLabel` | doc | no | no | self |
| `IB/grafana_readback.go:284` | applyGrafanaModel | `UpdateBeadFields` on dashboard bead | dashboard | no | no | dashboard authored by an operator/agent → **static bypass** |
| `IB/grafana_readback.go:340` / `353` / `363` | openConflictDecision / comment | `CreateBead` decision (issue kind) / `AddDependency`(dashboard←decision `blocks`: **dependent = dashboard bead**) / `AddComment` | decision, dashboard | no | no | the `blocks` dep is an `update` on the dashboard bead → **static bypass** |
| `IB/grafana_sync.go:207` | SyncDashboard | `UpdateBeadFields` (dashboard) | dashboard | no | no | **static bypass** |

##### (A) Admin-route calls
None (no `SetConfig`, token, operator or embeddings routes). `EnsureConfigs` writes config **beads** via `/v1/beads`, not `PUT /v1/configs`.

##### (H) Header dependencies
* `X-System-Operator: grafana-bridge` on **every** write (`cmd/grafana-bridge/main.go:126` → `beadsapi/client.go:3325`). Honored by static branch only. Without it every grafana-bridge write has no operator (no `X-Operated-By`, no agent-bead fallback) ⇒ 400 refusal. Under gate=enforce it collapses to `system:machine`. This is the only attribution mechanism the bridge has.
* No `WithOperator`, `X-Actor`, `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`, `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet`.

##### (O) Ownership dependencies
`grafana_readback.go:284,353` and `grafana_sync.go:207` mutate dashboard beads (and the readback dep is an `update` on the dashboard) that operators or agents may have authored → static bypass. Doc beads it created itself carry `created_by=static`; a new machine subject will not match them (Cross-cutting #3).

##### (C) Counts
Step-2 regex over `cmd/grafana-bridge`: **1** file, 1 line (`main.go:126`, `SetSystemOperator`). The bridge work is in `internal/bridge` (5 files: `grafana_backfill.go`, `grafana_dashboard_store.go`, `grafana_readback.go`, `grafana_sync.go`, `init_ensure.go`). Write call sites recorded: **15**.

---

#### 3. slack-bridge (deployed on k3s; **fleet** token `bridge:slack`, `Privileged=false`)

Entry: `cmd/slack-bridge/main.go:71-73` (`BEADS_DAEMON_TOKEN` = fleet token from `bridge-tokens/slack-bridge`), no `SetSystemOperator` (removed, comment `main.go:81-104`), `SetEdgeAuthHMACKey` only if `SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY` (`:114`; chart wires it only when `edgeAuth.enabled`, which is false on k3s). `EnsureConfigs(context.Background())` at `:119`. HTTP surface it serves: `/api/decisions/*`, `/api/escalations/*`, group open/invite, agent-feedback, plus SSE proxies `bridge.NewDecisionSSEProxy` / `NewEscalationSSEProxy` (`cmd/slack-bridge/helpers.go:145,158`) which forward `BEADS_DAEMON_TOKEN` to `GET /v1/events/stream` - **read-only**, no write dependency.

##### (W) Writes

Fleet-token ownership rules apply except config-kind beads, `type=dm`, `type=escalation`/`decision` (carve-out), and beads the bridge created (`bridge:slack` / legacy `slack-bridge` created_by).

| file:line | function | method | bead type | admin-gated? | config kind? | ownership relationship |
|---|---|---|---|---|---|---|
| `IB/init_ensure.go:97,107` | ensureConfigBead | `UpdateBeadDescription` / `CreateBead` | config | **yes** → `update/config`, `create/config` | yes | n/a |
| `IB/bot_schedule_cmd.go:182,216` | disable/enableProjectSchedules | `UpdateBeadFieldsTyped` on project bead | project | **yes** → `update/project` | yes | n/a |
| `IB/bot_schedule_cmd.go:192,226` | same | `AddComment` | project | **yes** → `comment/project` | yes | comment |
| `IB/bot_trigger_cmd.go:131,165` | disable/enableProjectTriggers | `UpdateBeadFieldsTyped` | project | **yes** → `update/project` | yes | n/a |
| `IB/bot_trigger_cmd.go:141,175` | same | `AddComment` | project | **yes** → `comment/project` | yes | comment |
| `IB/bot_trigger_cmd.go:70` / `105` | pauseTriggers / unpauseTriggers | `CreateBead` type gate / `CloseBead` | gate | no | no | `gate` created by this bridge; close of a gate created earlier by the same subject; **cross-owner if the creating subject changed** |
| `IB/bot_schedule_cmd.go:135,144` | resumeSchedules | `CloseBead` | gate (schedGate/capGate) | no | no | same as above |
| `IB/dm_delivery.go:204,281,404` / `222` / `292` | handleCreated / HandleReply / scheduleReminder | `UpdateBeadFields` / `CloseBead` / `AddComment` | **dm** | no | no | **dm carve-out** (`write_authz.go:439`) - fleet only; the dm bead is created by an agent |
| `IB/decisions.go:350` | notifySpawner | `CreateBead` | mail | no | no | self |
| `IB/decisions.go:1028` | respawnAgentForDecision | `CloseBead` | agent | no | yes | n/a |
| `IB/decisions.go:1134` | respawnAgentForDecision | `SpawnAgent` | agent | no | yes | privileged-role spawn gate (non-OIDC) |
| `IB/decisions.go:511,524` | stampCaptainRetiredDeadWood | `AddLabel` / `AddComment` on the **subject captain (trigger) bead** | trigger | no | no | **not creator; fleet has no bypass for trigger ⇒ likely 403 today**; ctx `WithSystemOperator` (:506) is ignored by fleet branch |
| `IB/decisions.go:602,609` | autoCloseObsoleteRemediationTasks | `AddComment` / `CloseBead` | task (remediation) | no | no | **close is cross-owner ⇒ likely 403 today** (fleet, not creator/assignee) |
| `IB/escalations.go:331,373,456,540` | maybeNotifyPingback / QueueAlert / spawnExecutionAgent / triggerLocal | `UpdateBeadFields` | escalation | no | no | **escalation carve-out** (any non-OIDC) |
| `IB/escalations.go:462,752` | spawnExecutionAgent / respawn | `SpawnAgent` | agent | no | yes | spawn gate |
| `IB/escalations.go:477` | spawnExecutionAgent | `AddDependency`(new agent→escalation, `escalation`) | dependent = agent | no | yes | n/a |
| `IB/escalations.go:784`, `IB/escalation_reaper.go:806,848` | recordResult | `CaptureEscalationResult` (`POST /v1/escalations/{id}/result`, operator route) | escalation | no | no | carve-out |
| `IB/escalation_operator_dm.go:240`, `escalation_pingback_loop.go:235`, `escalation_staleness_digest.go:270`, `bot_escalations.go:358` | various | `UpdateBeadFields` | escalation | no | no | carve-out |
| `IB/api_decisions.go:137,176` | handleResolve / handleDismiss | `ResolveDecision` / `CancelDecision` | decision | no | no | dedicated operator routes |
| `IB/api_decisions.go:298,311,322` | PromoteDecisionToEscalation | `CreateBead` escalation / `AddDependency`(→decision `promoted`) / `CloseBead` decision | escalation, decision | no | no | self + carve-out |
| `IB/bot_decisions_modal.go:352,497,584,657,857`, `bot.go:1569`, `slack.go:413` | resolve paths | `CloseBead` | decision | no | no | carve-out |
| `IB/sla_nudge_sweep.go:373,378,383,388,401,420,440,444` | fireRung1/2/3 | `UpdateBead` / `UpdateBeadFields(Typed)` / `AddLabel` / `AddComment` | escalation, decision (`slaSweptTypes`, :82) | no | no | carve-out |
| `IB/event_wake.go:96,127` (via `decisions.go`, `escalations.go`) | eventWakeDormantAgent | `UpdateBead` + `AddComment` | agent | no | yes | n/a |
| `IB/bot_agent_kill.go:110,459` / `709` / `714,1097,1587` / `1046` / `1066,1566` | kill / respawn / restart | `CloseBead` agent / `UpdateBeadFields` / `AddDependency`(agent→thread bead, `triggered-by`) / `UpdateBead` (reopen agent) / `CreateBead` agent | agent | no | yes | n/a (config-kind) |
| `IB/bot_mentions.go:1623,1632,1786,1823,2573`, `bot_channel_invite.go:93,113`, `bot_commands.go:563`, `bot_concierge.go:177,206`, `bot_formula.go:461`, `bot_group_open.go:181`, `bot_reactions.go:252,262`, `bot_group_invite.go:250`, `bot.go:768`, `bot_agents.go:812`, `bot_human_survey_manual.go:171`, `bot_agent_death.go:192` | spawn / bind / card refs / TTFT | `SpawnAgent` (create agent) / `UpdateBeadFields` / `AddDependency` / `CloseBead` / `AddComment` | agent (bot_agent_death comment: agent) | no | yes | n/a |
| `IB/bot_commands.go:242,389` | handleSpawnCommand / handleTaskFirstSpawn | `CreateBead` | task | no | no | self-created |
| `IB/thread_bead.go:180` | getOrCreateThreadBead | `CreateBead` task (idempotency key) | task | no | no | self |
| `IB/thread_task_autoclose.go:262,939` / `443,459,673,686,935` / `467,693` / `946` | stampDurableTerminalWitness / runCheck / sweepOnce / markNeedsTriage | `UpdateBeadFields` / `AddComment` / `CloseBead` / `AddLabel` | thread task bead | no | no | thread beads created by `bridge:slack` (legacy `slack-bridge`) ⇒ self via `bridgeSubjectMatchesLegacyCreatedBy` |
| `IB/bot_agents.go:850`, `bot_reactions_wish_close.go:140` | persistHeadCardRef / handleWishCloseReaction | `UpdateBeadFields` / `CloseBead` | thread head task | no | no | self |
| `IB/bot_thread_forward.go:402` / `427` / `435` | writeThreadConversationBead | `CreateBead` message (assigned to bound agent) / `AddDependency`(msg→head `triggered-by`) / `CloseBead` | message | no | no | self-created |
| `IB/bot_formula.go:510` | instantiateFormulaSteps | `AddDependency`(molecule→trigger bead) | molecule | no | no | self (molecule made by `instantiateFormula`, `formula_instantiate.go` sites as in §1) |
| `IB/bot_human_survey_rating.go:291` / `316` / `329`, `bot_agent_feedback.go:183`, `model_fallback.go:324,334`, `refusal_log.go:248,258` | rating writers | `UpdateBeadFieldsTyped` / `CreateBead` rating / `AddDependency`(rating→agent) | rating | no | no | self |
| `IB/cred_exhaustion_alert.go:139` / `186` / `191` | emit/resolveCredExhaustionAlerts | `CreateBead` bug / `AddComment` / `CloseBead` | bug | no | no | self |
| ~~`IB/scif_release_alert.go:219` / `233`~~ | ~~closeBeadLoop~~ | removed (gd-Y4ZCGr2m2t, 2026-10-31): deleted with the scif-portal | - | - | - | needs no grant |
| `IB/notice.go:87,101` (via `cmd/slack-bridge/helpers.go:245`) | NoticeNotifier | `CreateBead` message + `CloseBead` | message | no | no | self |
| ~~`IB/user_directory.go:545`~~ | ~~ResolveBySlackEmail~~ | removed (gd-W4FkF80BlX): `SlackEmailHint` writes nothing | - | - | - | needs no grant |

##### (A) Admin-route calls (`requireRoleMachineAllowed(RoleAdmin, …)`, `http.go:299-307`)
* `GET /v1/admin/operators/slack-aliases`: `IB/slack_operator_resync.go:93` (6h ticker, `:20`), `IB/cross_operator_notify.go:134` (on decision resolution, via `bot_cross_operator_notify.go:40`, `cmd/slack-bridge/main.go:362`).
* `POST /v1/admin/operators/{id}/identity`: `IB/slack_operator_resync.go:135`.
* `POST /v1/admin/operators/link-aliases`: `IB/slack_oidc_operator_alias_resync.go:157` (also `ListSlackOperatorAliases` reuse).
* `POST /v1/admin/operators/prune-aliases`: `IB/slack_oidc_operator_alias_resync.go:242`.
* No `SetConfig`, no bridge/fleet-token route, no embeddings call. Handlers self-gate on nothing (no `isAdminRequest` in `http_admin_operators*.go`), so the only gate is `requireRoleMachineAllowed`.

##### (H) Header dependencies
* `X-Operated-By` via `WithOperator`: `IB/bot.go:1156,1501`, `bot_channel_invite.go:35`, `bot_commands.go:50`, `bot_mentions.go:39,1936,1938`, `bot_person_identity.go:112,123`, `bot_reactions.go:99`, `bot_thread_forward.go:392`, `decisions.go:1124`, `escalations.go:742`, `thread_task_autoclose.go:1064`. Honored by the **fleet** branch (`interceptors.go:907-910`). Ignored ⇒ human attribution lost; under gate=enforce, creates with no human are refused (`audit.go:1077` fleet terminal branch), and `warn`/off bind sentinel `fleet:bridge:slack`. Every Slack-originated create (task, agent, thread, message, rating) depends on it.
* `X-System-Operator` via `WithSystemOperator`: `IB/bot_mentions.go:1934`, `thread_task_autoclose.go:257,1066`, `decisions.go:506`. **Already ineffective today**: the fleet branch does not honor `X-System-Operator` (`interceptors.go:906`), so these fall to the fleet sentinel (off/warn) or are refused (enforce). Code comment at `cmd/slack-bridge/main.go:94-104` and `thread_task_autoclose.go:5-12` assume otherwise.
* `EnsureConfigs` runs on `context.Background()` with no operator/system-operator (`main.go:119`) ⇒ under gate=enforce the startup config writes would be refused for a fleet token.
* Edge-auth envelope (`X-Operator`, `X-Edge-Auth-*`): `IB/bot_person_identity.go:115,124` via `WithEdgeAuthOperator`/`WithEdgeAuthIdentity` - **dormant on k3s** (`edgeAuth.enabled:false`). If enabled it makes the daemon see `authMethodEdge`, which bypasses write policy entirely and `requireRole` (`write_authz.go:263-268`, `requireRole` `interceptors.go:~137`) - a different, stronger principal; flag for any design that touches edge handling.
* Not set: `X-Actor`, `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`, `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet`. `IB/bot_autodestruct.go:172` sets `X-Actor` but to the **controller** (`AUTODESTRUCT_TOKEN`), not the daemon.
* User-bearer (`doJSONAsUser`): **none** in slack-bridge (only beads-viewer uses `WithUserBearer`).

##### (O) Ownership dependencies
* Rely on a carve-out: dm beads (`dm_delivery.go:204,222,281,292,404` → `write_authz.go:439`, fleet only); escalation and decision beads (≈35 sites above → `write_authz.go:482`, any non-OIDC caller, so a machine principal keeps it unless the daemon narrows it).
* Rely on self-ownership (created_by match): thread task beads, head beads, messages, ratings, gates, bug alerts, molecules. These were created under fleet subject `bridge:slack` (legacy `slack-bridge`) and match through `bridgeSubjectMatchesLegacyCreatedBy` (`write_authz.go:527`). A `machine:*` subject matches **none** of the existing rows.
* Likely already failing: `decisions.go:511` (label on a captain trigger bead) and `decisions.go:609` (close of remediation task) - fleet token, not creator, no bypass.
* No dependency: agent, project, config beads are config-kind.

##### (C) Counts
Step-2 regex over `cmd/slack-bridge`: **1** non-test file matched (`main.go`, 2 lines `:362,397`; both are method names, not daemon calls). Real write sites in `internal/bridge`: **~121** across 40 files (attributable list above), plus 4 admin-route call sites. `cmd/slack-bridge` has 3 non-test files.

---

#### 4. Not deployed on k3s (chart-only; recorded briefly)

`deploy-values/gardenio-k3s.yaml` enables slackBridge, grafanaBridge, agents, beadsViewer, triggerEngine only. Tokens (templates): alertmanager-bridge = primary static (`templates/alertmanager-bridge/deployment.yaml:56-60`); gitlab-bridge = fleet `gitlab-bridge` (fallback static; `templates/gitlab-bridge/deployment.yaml:71-79`; **privileged fleet**, `values.yaml:4680`); jira-bridge = fleet `jira-bridge` (fallback static; `:71-80`); github-bridge = own key `github-bridge` (fallback static; `:74-82`); roles-server = primary static (`:50-54`).

##### alertmanager-bridge - not deployed on k3s (deleted 2026-10-31, gd-Y4ZCGr2m2t)
* (W) `IB/alert_route.go:305` `CreateBead` (type from `defaultAlertRoute`, `:55`: bug etc), `:313` `AddDependency`(→trigger parent), `:354` `AddComment`, `:359` `CloseBead` (all on beads it created); `IB/alert_rules_sync.go:297` `UpdateBeadFields`, `:313` `CreateBead` doc, `:325` `AddLabel`; `IB/init_ensure.go:97,107` (`create/config`, `update/config`, via `main.go:91`). `IB/alert_mute.go` reads only.
* (A) none. (H) no `WithOperator`/`SetSystemOperator` anywhere in the component ⇒ static-token writes resolve to no operator unless the alert payload path adds one (inferred from the resolver; not exercised live). (O) self-owned beads only; doc upsert by label/title.
* (C) Step-2 regex over `cmd/alertmanager-bridge`: 1 file, **0** matches (work is in `internal/bridge`: `alert_route.go`, `alert_rules_sync.go`, `init_ensure.go`); 9 write sites recorded.

##### gitlab-bridge - not deployed on k3s
* (W) MR/graph writes: `IB/gitlab_sync.go:263,775,1091,1182,1271` + `gitlab_upstream_poller.go:179` `UpsertMergeRequestBead` (type merge-request, issue kind); `gitlab_sync.go:272,279,781,787,1096,1101,1210,1215` `AddDependency` (task↔MR `produces`/`triggered-by`; dependent can be a task authored by an agent ⇒ **needs the privileged-fleet bypass** `write_authz.go:425`); `gitlab_sync.go:1317,1450,1470`, `gitlab_comment_mrlink.go:154`, `gitlab_mr_backlink.go:267,430`, `gitlab_mentions.go:349,498,559,592`, `gitlab_mention_catchup.go:599`, `gitlab_agent_resolver.go:305`, `gitlab_upstream_acceptance.go:176,181`, `gitlab_mr_reopen_drift.go:281,288`, `gitlab_mr_terminalize_drift.go:252,260` (task/MR updates and **closes of foreign task beads**), `escalation_reaper.go:806,848` and `escalation_review_flag.go:270,293,300` (escalation carve-out), `release_drought.go:403` (bug) via `NoticeNotifier`; `init_ensure.go:97,107`. Also the shared TriggerEngine writes in §1 (`cmd/gitlab-bridge/main.go:646`) and `UserDirectory` (`user_directory.go:545`, already refused).
* (A) none. (H) `WithOperator`: `IB/gitlab_mentions.go:229`. `createdBy` strings like `system:gitlab-sync` are dependency author labels, not headers. (O) depends on **privileged fleet** (`BEADS_PRIVILEGED_FLEETS=gitlab-bridge`) for every cross-owner produces/triggered-by dep and mr_url back-fill; design §5 already maps this to `ownership:system-graph`.
* (C) Step-2 regex over `cmd/gitlab-bridge`: 1 file, **0** matches; ~129 write sites in 33 bridge files (shared engine sites counted).

##### jira-bridge - not deployed on k3s
* (W) `IB/jira_config.go:191` `CreateBead` type **jira-poller** (config kind) → `create/jira-poller` (admin-gated allow-list entry); `jira_poller.go:714` `CloseBead`, `:943` `CreateBead` task (created_by jira-bridge), `:1131,1176` `AddDependency`, `:1197` `UpdateBeadFields`; `jira_mentions.go:665,750,909,950` (task create, `SpawnAgent` at :746, user-bead `jira_user` write :909, dep); `jira_agent_resolver.go:265,305,388,361`; `jira_status_transition.go:268,277`; `jira_webhook.go:475,524`; `jira_inbound_comment_dlq.go:247,262`; `init_ensure.go:97,107`; `user_directory.go:545` (refused).
* (A) none. (H) `WithOperator`: `IB/jira_mentions.go:601`; `JIRA_BRIDGE_DEFAULT_OPERATOR` feeds the resolver (`jira_agent_resolver.go:66`). (O) fleet: self-created task beads match via the legacy `jira-bridge` created_by rule (`write_authz.go:527` comment cites `jira_poller.go`/`jira_config.go`/`jira_mentions.go`); `service-managed-field` bypass for trigger stamping only if fleet listed.
* (C) `cmd/jira-bridge`: 1 file, **0** matches; ~21 write sites in 9 bridge files.

##### github-bridge - not deployed on k3s
* (W) in `cmd/github-bridge`: `beads.go:128` `CreateBead` message (CreatedBy `bridgeActor`), `:141` `CloseBead`, `:154` `CreateBead` mail; `pipeline.go:171` `UpdateBeadFields(mr.ID)`, `:232` `CreateBead` message, `:244` `CloseBead`; `pullrequest.go:210` `UpsertMergeRequestBead`, `:229` `UpdateBeadFields(task: mr_url, forge)`, `:235,238` `AddDependency` (task↔MR). Task/MR updates are cross-owner ⇒ would need the same privileged-fleet/system-graph bypass as gitlab-bridge; `BEADS_PRIVILEGED_FLEETS` lists only `gitlab-bridge`, so a `github-bridge` fleet token would 403 today (inference from `values.yaml:6546`).
* (A) none. (H) no operator header or `SetSystemOperator` set anywhere in `cmd/github-bridge` ⇒ header-less writes (inferred refusal under the resolver). (O) as above.
* (C) `cmd/github-bridge`: 6 non-test non-testdata files, **3** match the Step-2 regex (9 lines).

##### roles-server - not deployed on k3s
* (W) `cmd/roles-server/web.go:139` `CreateBead` type **config** (`create/config`), `:214` `UpdateBead` (title) and `:222` `UpdateBeadFields` (`update/config`), `:301,308` `RemoveLabel`/`AddLabel` (update on config bead), `:256` `DeleteBead` → **`DELETE /v1/beads/{id}` is `requireRole(RoleAdmin)` and not machine-allowed (`http.go:250`) ⇒ already denied for the static token**; `web_advice.go:92` `CreateBead` type **advice** (`create/advice`), `:150` `UpdateBead` (**`update/advice` is not in `machineAllowedWrites`** ⇒ denied today), `:185` `DeleteBead` (denied today); `web_instructions.go:143` `UpdateBeadFields` on config bead (`update/config`).
* The `machineAllowedWrites` comment (`write_authz.go:128-170`) attributes `EnsureConfigs` to "bridges" and `web.go`/`web_instructions.go` to roles-server; roles-server does **not** call `EnsureConfigs`. Actual `EnsureConfigs` callers: `cmd/alertmanager-bridge`, `controller`, `gb`, `gitlab-bridge`, `grafana-bridge`, `jira-bridge`, `slack-bridge`, `wl-bridge`.
* (A) none. (H) no operator header of any kind in `cmd/roles-server` ⇒ every static-token write resolves to no operator (refused by `audit.go:903`); inferred, not exercised (component is not deployed). (O) config-kind skipped; advice is data kind (creator `static`).
* (C) `cmd/roles-server`: 10 non-test files, **3** match (10 lines).

---

#### 5. Cross-cutting findings to feed Tasks 5 and 6 (this fork's scope)

1. **The `machine` auth method has no operator resolution.** `resolveOperatedByPure` (`audit.go:1020`) has no `machine` case and an empty operator is always a 400. Task 3/4 must make the principal itself resolve (design §4.1 says "operated_by is the principal") and must decide a human relay for trigger-engine and slack-bridge: both stamp `X-Operated-By` on nearly every create, and trigger beads need a human at create under `enforce` (`beads.go:178`). That is the "operated-by relay" capability; it **reopens a header trust branch** - Task 5 should take it to a decision.
2. **`X-System-Operator` is static-branch only** (`interceptors.go:871`). trigger-engine (10 sweep call sites) and grafana-bridge (client-wide) depend on it; slack-bridge's four `WithSystemOperator` sites already do nothing on the fleet token.
3. **created_by migration.** Static-token components' existing beads carry `created_by="static"`; slack-bridge's carry `bridge:slack`/`slack-bridge`. A `machine:<client>` subject matches neither, so every self-update/close of existing rows (grafana docs, uncovered-lane rollup doc, thread task beads, gates, messages) becomes an ownership 403 for a machine principal without an ownership grant or a one-time re-owner migration. This is the "ownership bypass" capability and **reopens an ownership bypass** - decision needed.
4. **Cross-owner writers**: trigger-engine (captain/trigger beads, orphan/auto-intake/needs-captain label writes on arbitrary tasks/agents, per-run task close) all ride `write_authz.go:413`. These are the strongest argument for a named capability (e.g. `gardenio-machine-system-graph`) rather than per-type grants. slack-bridge cross-owner writes are limited to escalation/decision/dm (existing carve-outs) plus two likely-already-failing sites (`decisions.go:511,609`).
5. **Allow-list entries these components are the callers of** (for Step 5): `create/config`, `update/config` ← EnsureConfigs in grafana-bridge, slack-bridge (deployed) and alertmanager/gitlab/jira/wl-bridge (not deployed); `update/project`, `comment/project` ← slack-bridge `/schedule`,`/trigger` (`bot_schedule_cmd.go:182-226`, `bot_trigger_cmd.go:131-175`); `create/jira-poller` ← jira-bridge only (`jira_config.go:191`, undeployed); `update/user` ← **no slack/trigger/grafana caller** (`user_directory.go:545` is refused by the field gate anyway); `create/advice` ← roles-server only here (`web_advice.go:92`), `create/report`/`update/report`/`close/report`, `close/advice`, `create/role`, `create/account`, `create/project`, `create/user` ← none of these components. trigger-engine needs **no** `machineAllowedWrites` entry at all.
6. **Admin routes**: only slack-bridge calls any (`requireRoleMachineAllowed`): slack-aliases GET, operators/{id}/identity, link-aliases, prune-aliases. trigger-engine and grafana-bridge call none.
7. **Dormant/other principals**: slack-bridge edge-auth signing (`bot_person_identity.go:115,124`) would present `authMethodEdge`, which bypasses write policy; live it is off (`edgeAuth.enabled:false`).
8. `SpawnAgent` callers (trigger-engine, slack-bridge) are subject to the non-OIDC privileged-role spawn gate (`spawn_authz.go:141`); a machine principal inherits it unless Task 3 states otherwise.
9. Stale doc/code claims found: `write_authz.go:128-170` comment (roles-server calls `EnsureConfigs`; `advice/update`,`advice/delete` missing for roles-server); `cmd/slack-bridge/main.go:94-104` and `thread_task_autoclose.go:5-12` (assume `X-System-Operator` binds on the fleet token).

#### 6. Summary (5 lines)
1. trigger-engine (static): ~101 write sites, zero allow-list entries, zero admin routes; depends on the static ownership bypass for trigger/task/label/close writes and on `X-Operated-By`/`X-System-Operator` for attribution.
2. grafana-bridge (static): 15 write sites; needs `create/config`+`update/config` (EnsureConfigs) only; every write depends on client-wide `X-System-Operator=grafana-bridge`.
3. slack-bridge (fleet `bridge:slack`): ~121 write sites; allow-list needs = `create/update config`, `update/project`, `comment/project`; four `requireRoleMachineAllowed` operator-alias routes; relies on dm/escalation/decision carve-outs and self-ownership; `X-Operated-By` pervasive; `X-System-Operator` calls are already no-ops.
4. Undeployed: gitlab/github need the privileged-fleet bypass, jira needs `create/jira-poller`, alertmanager `create/update config`, roles-server needs `create/update config` + `create/advice` and already fails on advice update and all deletes.
5. Flags for decisions: operated-by relay, ownership bypass/re-owner migration, `machine` operator resolution; also `machineAllowedWrites` comment errors listed in §5 #9.


## Appendix C. beads-viewer

All `file:line` at commit `67a7ad34`. Paths: viewer = `gardenio/controller/cmd/beads-viewer/` (abbrev `bv/`), `gardenio/controller/internal/` (abbrev `int/`), kbeads server = `kbeads/internal/server/` (abbrev `kb/`).

#### 0. Read this first: how the viewer authenticates, and what is live on k3s

| Fact | Evidence |
|---|---|
| ONE process-wide credential: `BEADS_DAEMON_TOKEN` (primary static token `kd-beads-token`) passed as `beadsapi.Config.Token`. No `TokenSource`, no `SetActor`, no `SetOperatedBy`, no `SetSystemOperator`. | `bv/main.go:75-83`; `git grep SetActor\|SetOperatedBy\|SetSystemOperator bv/` = 0 hits |
| Per-request human bearer exists (`WithUserBearer`, stamped by `operatorMiddleware`) but is **inert unless `BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1`** (chart `beadsViewer.requireUserBearerForWrites`, default `false`). When on, EVERY mutating call except `/v1/search/semantic` must carry the bearer or fails closed with `ErrUserBearerRequired`, and the bearer REPLACES the static token on the wire. | `bv/middleware.go:74`, `bv/main.go:97`, `int/beadsapi/user_bearer.go:59-110` (`applyUserBearerWriteGate`), `int/beadsapi/client.go:795-803` (`setAuth`) |
| On k3s: `requireUserBearerForWrites` is NOT set (so false); `beadsViewer.devAuthBypass: true`, `oidc.enabled: false`, `edgeAuth.enabled: false`, ingress/avaIngress/edgeService all false. So **every viewer write on k3s today is static-token + (optionally) `X-Operated-By`**; there is no user bearer and no edge-auth envelope. | `deploy-values/gardenio-k3s.yaml:1157-1218` (`beadsViewer:` block; `edgeAuth: {enabled: false}` at `:1312`; no `requireUserBearerForWrites` anywhere in the file); chart `templates/beads-viewer/deployment.yaml:106-110,116-130`; default `values.yaml:11357` |
| Consequence for the machine principal: the viewer machine principal covers **all** the static-token writes in section W. Only the 7 rows tagged `USER-BEARER-ONLY` (always forward the human bearer via `doJSONAsUser`, never use the static token) are outside its scope; they are **already non-functional on k3s** (no bearer exists). | `int/beadsapi/client.go:1651,1793,2088,2109`, `client_escalations.go:111,133` (`doJSONAsUser`) |
| Non-client uses of the token (reads only, no write): `/v1/health/snapshot` GET, charts proxy GET, `gb prime --for` subprocess env. The `gb prime` subprocess is not verified read-only here (it is `gb`, covered by the gb/agent-pod row of the master doc). | `bv/health.go:378-385`, `bv/charts_proxy.go:170-180`, `bv/prime.go:42-50` |
| Handler gate: nearly every write handler returns 401 if `oidcauth.EmailFromContext(ctx)==""`; on k3s dev-bypass the email only exists if the edge supplies `X-Auth-Request-Email` (`forwardAuthTrustedEmailToContext`). Whether live k3s traffic supplies it is NOT verifiable from the repo. | `bv/bead_actions.go:245-249`, `bv/dev_auth.go:154-178` |

The viewer is **not** the creator, assignee or owner of most beads it mutates, and `created_by` on viewer-created beads is the literal `static` (X-Actor is only set by the comment handler, section H). It relies on the static-token ownership bypass (`kb/write_authz.go:413`) everywhere in section O.

#### W. Writes by feature

Columns: **Feature group (count of call sites)** | first `file:line` (others in the same file/feature listed) | beadsapi method | HTTP route (kbeads handler) | authorizeWrite action | bead type | admin-gated (`min_role: gardenio-admin`)? | config kind? | token | ownership relationship.

Type facts from `kb/config.go`: admin-gated builtin types are jira-poller `:454`, advice `:472`, report `:834`, project `:940`, fleet `:990`, user `:1003`, account `:1048`, team `:1120`, role `:1145`, rig/convoy/config `:1202-1204`, credential `:1511`, audit `:1531`. Config-kind (ownership gate skipped, `write_authz.go:401`): agent `:619`, project, user, team, account, role, fleet, schedule `:1971`, spawn_policy `:1195`, plus a request-supplied `Kind:"config"` for `view`. Data/issue kinds (ownership gate applies): trigger `:1843` (data), decision `:762`, message, rating `:814`, gate `:1230`, epic/task/bug/feature/chore (issue), escalation `:1577`.

Route→action map for the verbs used: `UpdateBead*`/`SetLabels` = `PATCH /v1/beads/{id}` → `updateBead` → `authorizeWrite(update)` (`kb/beads.go:1206`) then `authorizeOwnership` (`:1234`); `CloseBeadAs` = `POST /v1/beads/{id}/close` → `authorizeWriteForBead(close)` (`kb/http_beads.go:1276`); `DeleteBead` = `DELETE /v1/beads/{id}` → `requireRole(RoleAdmin)` (`kb/http.go:250`) then `authorizeWriteForBead(delete)` (`kb/http_beads.go:763`); `AddLabel`/`RemoveLabel`/`AddDependency`/`RemoveDependency`/`AddBeadFollower`/`RemoveBeadFollower` → `loadBeadForWrite(update)` (full ownership gate; `kb/http_relations.go:186,302,381,429`, `kb/http_followers.go:59,108`); `AddComment` → `loadBeadForComment(comment)` (type policy only, **no ownership**; `kb/http_relations.go:521`); `AppendBeadArrayField`/`ReorderBeadArrayField` → `authorizeUserBeadFieldWrite` + ownership (`kb/http_beads.go:929`); `CreateBead*` → `authorizeWrite(create)` (`kb/beads.go:432`).

| # | Feature group | first call + all call sites | method | route | action | bead type | admin-gated? | config kind? | token | ownership relationship |
|---|---|---|---|---|---|---|---|---|---|---|
| W1 | Inline field edits on any bead (7) | `bv/title_edit.go:99`; `description_edit.go:134`; `priority_edit.go:85`; `labels_edit.go:93`; `owner_edit.go:102`; `assignee_edit.go:94`; `inbox.go:599` (set-parent rewrites description) | `UpdateBead` | `PATCH /v1/beads/{id}` | update | **any** (user picks the bead; mostly epic/task/bug) | no (varies by type) | no for issue types | static (bearer if flag) | not creator/owner → needs static bypass `write_authz.go:413` (see O1) |
| W2 | Bead status actions: claim / close / reopen / unassign / set status (5) | `bv/bead_actions.go:147` (CloseBeadAs), `:155,164,177,220` | `CloseBeadAs`, `UpdateBead` | `POST /v1/beads/{id}/close`, `PATCH` | close, update | any | no | no | static | O1 |
| W3 | Project bulk ops (4) | `bv/project_bulk.go:36,72` (UpdateBead status/assignee), `:108,117` (Add/RemoveLabel) | `UpdateBead`, `AddLabel`, `RemoveLabel` | `PATCH`, `POST/DELETE /v1/beads/{id}/labels` | update | any in project | no | no | static | O1 |
| W4 | Inbox flag toggles (2) + pin toggle (1) | labels: `bv/inbox_flag.go:35,37`; pin: `:94` | `RemoveLabel`/`AddLabel`; `UpdateBeadFieldsTyped` | labels route; `PATCH` | update | label: any bead; pin: **user** (see W18) | label: no; pin: **yes** | pin: yes | static | label: O1; pin: self user bead (config kind) |
| W5 | Generic comment post (1) + incident fast-track bypass audit (1) | `bv/server.go:3941` (sets X-Actor, `:3940`); `bv/fast_track_merge.go:131` (actor from `X-Forwarded-User`, **not** sent as X-Actor) | `AddComment` | `POST /v1/beads/{id}/comments` | comment | any / incident | by target type | by target type | static | **none** (comment skips ownership, `kb/http_relations.go:521`) |
| W6 | Captain / trigger / schedule operations (≈20) | `bv/captain_ops.go:307` (enable toggle), `:577` (captain blob), `:945` (config edit), `:534` run-now agent create, `:548,552` labels, `:559` dep, `:314,584,638,672,950` audit comments, `:630,675` close; `captain_pause.go:84,97`; `captain_successor.go:179,184`; `trigger_toggle.go:71,81`; `triggers_bulk.go:100,108`; `create_captain.go:554` (CreateBead trigger), `:560,564` deps | `UpdateBeadFieldsTyped`, `AddComment`, `CloseBeadAs`, `CreateBead`, `AddLabel`, `AddDependency` | `PATCH`, comments, close, `POST /v1/beads`, labels, deps | update/comment/close/create | **trigger** (data, `project_scoped`), **schedule** (config), agent (run-now), session/iteration | no | trigger: no (**ownership applies**); schedule: yes | static | trigger fields `enabled`,`captain`,`chain` are NOT in `serviceManagedFieldsByType["trigger"]` (`kb/beads.go:~985-1000`) so the service-field bypass does not apply → static bypass O2 |
| W7 | Agent spawn from the viewer (≈12) | `bv/quick_spawn.go:321` (+`328,336` labels, `341` dep); `captain_ops.go:534`; engine: `int/planagent/spawn.go:86,98,102,105` (create+labels+assigned dep), `:173` (wake PATCH `status/prompt/session_resume`) | `CreateBead`, `AddLabel`, `AddDependency`, `UpdateBead` | `POST /v1/beads`, labels, deps, `PATCH` | create/update | **agent** | no (`project_scoped`, `escalatable`) | yes (agent is config kind) | static | create: none. Spawn gate `authorizeAgentSpawn`→`authorizeNonHumanSpawn` applies to any non-OIDC caller (`kb/spawn_authz.go:57-78`): a machine principal keeps today's non-OIDC outcome. Wake PATCH on an agent bead is config kind → no ownership |
| W8 | Plan cockpit (≈30) | `bv/plans_create.go:161` (epic create), `plan_edit.go:121,167,206,312,322,332,572` (desc/doc/step title/phase epic create+dep+append/reorder), `plans.go:4859,4878` (step create+dep), `:4963` (last_nudge_at stamp), `:4991,5017` (close/reopen step), `:5297,5303` (plan label), `plans_lifecycle.go:230,234,518,612,658`, `plans_critique.go:215`, `plans_assistant_prompt.go:580` (operator transcript turn), `plans_master_switch.go:116` (close gate) + `int/pausegate/pausegate.go:95` (`Open`: create gate), `plans_captain_bind.go:50,133` → `int/plancaptain/plancaptain.go:885` (`Bind`: create trigger) | `CreateBead(Detail)`, `UpdateBead`, `UpdateBeadFieldsTyped`, `AppendBeadArrayField`, `ReorderBeadArrayField`, `AddDependency`, `AddLabel`/`RemoveLabel`, `CloseBeadAs`, `AddComment` | `POST /v1/beads`, `PATCH`, `/fields/{f}/append|reorder`, deps, labels, close, comments | create/update/close/comment | epic, task, message (`int/plantranscript/plantranscript.go:145`), gate, trigger, doc | no | gate/trigger/epic/task: no | static | O1 (epics/steps created by agents/other users); gate: `int/pausegate` creates it, but other components also open it → O1 |
| W9 | Plan background reactors (no human in the loop) (≈6) | `bv/plan_label_reactor.go:391` (`plancaptain.Bind` → create trigger, `WithSystemOperator("plan-label-autobind")` `:389` when epic has no human operator); `bv/plan_step_reactor.go:366` (`promptPlanAssistant`: agent create/wake via planagent, last_nudge stamp `plans.go:4963`) | `CreateBead`, `UpdateBead`, `UpdateBeadFieldsTyped` | as W7/W8 | create/update | trigger, agent, epic | no | trigger no | static | **the only viewer writes with no HTTP caller** → a machine principal cannot lean on a forwarded user; these are exactly the writes that need the viewer's own identity |
| W10 | Formula / plan pour (2) | `bv/formula_pour.go:164`, `bv/plan_pour.go:126` | `PourFormula` | `POST /v1/formulas/{id}/pour` (`requireRole(Operator)`; creates beads via `createBead` `kb/http_formula_pour.go:400,579`) | create (per bead) | molecule + steps | no | no | static | none (creates) |
| W11 | Decisions: resolve / cancel / reflag (≈6) | `bv/decisions.go:275,347`; `bv/plans.go:4458,4712`; reflag `bv/decision_reflag.go:125,131,140,153` | `ResolveDecision`, `CancelDecision`; `UpdateBead`, `UpdateBeadFieldsTyped`, `AddComment` | `POST /v1/decisions/{id}/resolve|cancel` (no `authorizeWrite`, only read-scope `kb/read_authz_scope.go:150`); `PATCH`; comments | resolve/cancel: none; reflag: update/comment | decision | no | no | static | resolve/cancel: none; reflag `PATCH`: shared-queue carve-out for decisions (`kb/write_authz.go:470-475`, non-OIDC passes) |
| W12 | Escalations approve / reject (2) | `bv/escalation_actions.go:249` (approve), `:244` (reject) | `ApproveEscalation`, `RejectEscalation` | `POST /v1/escalations/{id}/approve|reject` | n/a | escalation | no | no | **USER-BEARER-ONLY** when a bearer is on ctx (`client_escalations.go:111,133`); falls back to static + relay otherwise. Daemon only accepts approver identity of type `edge` or allow-listed OIDC client (`kb/escalation_approval_authz.go:30-43`) → the static/relay fallback is **always refused**; a machine principal must NOT become an approver | n/a |
| W13 | Followers / forward (2) | `bv/forward.go:183` (add), `:221` (remove) | `AddBeadFollower`, `RemoveBeadFollower` | `POST/DELETE /v1/beads/{id}/followers` | update | any | no | no | static | O1 (`kb/http_followers.go:59,108` → full ownership gate) |
| W14 | Taints & scrub requests (≈10) | `bv/taint_badges.go:181` (CreateScrubRequest), `:236` (ApplyTaint); `me_taints.go:136` (CreateScrubRequest); `taint_detail.go:593` (Grant), `:622` (Revoke); `taint_detail.go:457` (UpdateTaintPolicy), `:811,813` (Approve/RejectScrubRequest); `taints_registry.go:155` (CreateTaint) | `CreateScrubRequest`, `ApplyTaint`, `GrantTaint`, `RevokeTaint`, `UpdateTaintPolicy`, `Approve/RejectScrubRequest`, `CreateTaint` | `/v1/scrub-requests`, `/v1/beads/{id}/taints`, `/v1/taints/...` | taint-specific gates | taint, scrub-request | `POST /v1/taints` is `requireRole(Admin)` | n/a | CreateTaint, UpdateTaintPolicy, Approve/RejectScrubRequest = **USER-BEARER-ONLY** (`client.go:1651,1793,2088,2109`; hard error `ErrTaintCreateRequiresUserBearer` without a bearer). ApplyTaint/GrantTaint/RevokeTaint/CreateScrubRequest use static+relay (`doJSON`) | taint gates key on OIDC human (`kb/http_taints.go:270`, `http_taint_grants.go:245`, `http_scrub_requests.go:839` per design §2) → machine keeps today's non-OIDC outcome |
| W15 | Ratings (4) | `bv/ratings.go:482` (UpdateBead), `:487` (fields), `:512` (CreateBead type rating), `:518` (dep) | `UpdateBead`, `UpdateBeadFieldsTyped`, `CreateBead`, `AddDependency` | `PATCH`, `POST /v1/beads`, deps | update/create | rating (data) | no (`write_policy: escalatable` only, `kb/config.go:815`) | no | static | updates an existing rating bead: today creator is `static` (create body `CreatedBy` is ignored, `kb/beads.go:142-146,265`) and the static bypass applies. Under a machine principal creator becomes the machine subject, so same-principal re-rating still passes; ratings created by anyone else do not |
| W16 | Saved views (2) | `bv/views.go:406` (CreateBead `Type:"view"`, `Kind:"config"`), `:450` (`DeleteBead`) | `CreateBead`, `DeleteBead` | `POST /v1/beads`; `DELETE /v1/beads/{id}` | create; delete | view (no builtin `type:view` in `kb/config.go` → policy depends on a live config row) | n/a | request-supplied config | static | **`DELETE /v1/beads/{id}` is `requireRole(RoleAdmin)` (`kb/http.go:250`) and non-OIDC callers are denied at admin tier (`kb/interceptors.go:136`) → `views.go:450` already 403s on the static path today.** Needs an OIDC admin (user bearer) or a deliberate new grant |
| W17 | Threads / wish compose (3) | `bv/thread_compose.go:272` (CreateBead task head, idempotency-keyed, no `CreatedBy`), `:301` (reopen head), `:329` (CreateBead message), `:343` (`CloseBeadAs` message) | `CreateBead`, `UpdateBead`, `CloseBeadAs` | `POST /v1/beads`, `PATCH`, close | create/update/close | task (head), message | no | no | static | head reopen `:301` on a head created by someone else (Slack bridge also writes heads, idempotency key) → O1 |
| W18 | Peel request (3) | `bv/peel_request.go:146` (CreateBead task), `:153` (dep to MR), `:156` (label on the MR bead) | `CreateBead`, `AddDependency`, `AddLabel` | `POST /v1/beads`, deps, labels | create/update | task; label on `merge-request` bead | no | no | static | label write on MR bead created by the gitlab-bridge → O1 |
| W19 | Advice create (1) | `bv/create_advice.go:142` | `CreateBead` | `POST /v1/beads` | create | **advice** | **yes** | no (data) | static | none. Relies on `machineAllowedWrites` `create/advice` (`kb/write_authz.go:197`) |
| W20 | User-bead self-service (4) | `bv/settings.go:454` (prefs, display_name, pronouns, team, slack_channel_id, profile_prefs); `bv/user_status_message.go:236`; `bv/user_inbox.go:439` (`inbox_last_visit_at`, written from a page render); `bv/inbox_flag.go:94` (`profile_prefs` pin set, **not named in the `machineAllowedWrites` doc comment**) | `UpdateBeadFieldsTyped` | `PATCH /v1/beads/{id}` | update | **user** | **yes** | yes | static | none (config kind). Relies on `update/user` (`kb/write_authz.go:207`). None of the keys is in `userIdentityFields` (`kb/user_identity_authz.go:20-34`) so `authorizeUserIdentityFields` does not fire. Viewer-layer self check (`isSelf`) is the only per-user gate |
| W21 | Project config editor (2) | `bv/project_config.go:314` (fields), `:320` (audit comment) | `UpdateBeadFieldsTyped`, `AddComment` | `PATCH`, comments | update, comment | **project** | **yes** | yes | static | none. Relies on `update/project` + `comment/project` (`kb/write_authz.go:203-205`). Viewer-layer gate is its own |
| W22 | Team membership edit (1) | `bv/team_page.go:270` | `UpdateBeadFieldsTyped` | `PATCH` | update | **team** | **yes** | yes | static | **`update/team` is NOT in `machineAllowedWrites`; `authorizeTeamMembershipBypass` needs an `oidc`/`edge` caller (`kb/write_authz.go:703-718` `operatorAliasForIdentity` returns `""` for others) → the static path is denied today.** The doc comment at `bv/team_page.go:195-203` ("authorizeWrite passes an unscoped static token through unconditionally") is stale. Works only when the user bearer or edge-auth is on |
| W23 | UI telemetry (1) | `bv/incident_intel_telemetry.go:83` | `RecordUITelemetry` | `POST /v1/ui/telemetry` (`requireRole(Operator)`, no `authorizeWrite`) | n/a | n/a (side table) | no | no | static | none |

Bead-type spot checks done: no `account`, `role`, `config`, `jira-poller`, `report` or `project` **create**, and no `SetConfig`, in the viewer (`git grep 'Type:\s+"(account|role|config|project|user|team)"' bv/` finds only `view`).

Write call-site counts (see section C): `116` `s.daemon.<write>(` sites in 46 non-test files, plus `≈17` write sites in internal packages invoked by the viewer (`int/planagent/spawn.go` 5, `int/captainmode/mode.go` 12, `int/pausegate` 1, `int/plancaptain` 1; captainmode is reached via the captain-mode UI, 4 `.go` importers).

#### A. Admin-route and non-write-policy route calls

| Route | Call site | Gate today | Machine need |
|---|---|---|---|
| `GET /v1/admin/operators/slack-aliases` | `bv/people_mentions.go:84` via `ListSlackOperatorAliases` (`int/beadsapi/client_admin_operators_slack.go:41`) | `requireRoleMachineAllowed(RoleAdmin)` (`kb/http.go:299`) | needs the `requireRoleMachineAllowed` exception or an explicit grant for this read route. **Only admin-route call the viewer makes.** |
| `DELETE /v1/beads/{id}` | `bv/views.go:450` | `requireRole(RoleAdmin)` → already denied for static | see W16 |
| `POST /v1/taints` | `bv/taints_registry.go:155` | admin + own OIDC self-gate (`callerMayCreateTaint`) | USER-BEARER-ONLY, never machine |
| `PUT /v1/configs/*` (SetConfig), `/v1/bridge-tokens`, `/v1/fleet-tokens`, `/v1/embeddings/*`, `/v1/admin/operators/{id}/identity`, `link-aliases`, `prune-aliases` | **none** in `bv/` (`git grep` of `SetConfig`, `Register\w*Token`, `Revoke\w*Token`, `UpdateOperatorIdentity`, `LinkOperatorAliases`, `PruneUserBeadAliases`, `ResyncUserBeadAliases`, `RevokeUser` = 0 hits) | n/a | none |
| `POST /v1/operators/me/aliases` (`LinkOperatorAlias`) | not called from `bv/` | n/a | none |
| Reads that matter for grant planning: `GET /v1/configs` (`bv/server.go:2805`, `requireRole(Viewer)`), operator lookups (`ListOperators` 9, `LookupOperatorBySlug` 5, ...), SSE `EventStream` 9, `POST /v1/search/semantic` (read-only exempt from bearer gate) | n/a | viewer tier | machine keeps `gardenio-operator`/viewer baseline |

#### H. Header dependencies, by feature

Where headers are set: ONLY `int/beadsapi/client.go:3279-3281` (`X-Actor`), `:3318-3320` (`X-Operated-By`), `:3323-3325` (`X-System-Operator`), `:3328` (`X-Escalate`), and the edge-auth envelope `:3290-3315` (`X-Operator`, `X-Auth-Audit-Id`, `X-Edge-Auth-*`, only if `BEADS_VIEWER_EDGE_AUTH_HMAC_KEY` is set: **not set on k3s**). The viewer sets **no** `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`, `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet` anywhere (`git grep -i` over `gardenio/controller` finds none; the only `X-Trigger-Bead`/`X-No-Auto-Parent` setters are `kbeads/internal/client/http.go:1674,1680`, i.e. `kd`). No direct `req.Header.Set` for any of these in `bv/` (only `Authorization` at `health.go:384`, `charts_proxy.go:180`, `widgets_mezmo_http.go:80`, and `X-Grafana-*` auth-proxy email at `grafana_proxy.go:165`).

| Header | Feature | Set at | Value / source | Static branch honors (`kb/interceptors.go`) | What breaks if a machine principal ignores it |
|---|---|---|---|---|---|
| `X-Operated-By` | **ALL human-driven writes** (every handler behind `operatorMiddleware`) | `bv/middleware.go:58` stamps `WithOperator(canonicalOIDC(email))` for every request with a verified email; `int/beadsapi/client.go:3318` emits it. 16 further explicit `WithOperator` sites re-stamp it for multi-step plan flows: `create_captain.go:432`, `peel_request.go:70`, `plan_edit.go:92`, `plan_phases.go:438`, `plan_pour.go:120`, `plan_step_reactor.go:366`, `plans.go:4814,5053,5122,5316`, `plans_assistant_prompt.go:426`, `plans_create.go:234`, `plans_critique.go:151,194`, `quick_spawn.go:207`, `plan_label_reactor.go:386` | `oidc:<lowercased email>` | `:846-848` | loses human attribution on `operated_by` for every bead/comment (feeds "my work", plans-involving-operator filters, people pages). Without any operator **create is refused under `BEADS_AUTHORING_HUMAN_GATE=enforce`** (live value `warn`): affects every create row W6-W10, W15, W17-W19. Also drops the server-side **Owner default** that is derived from this header (`ka-0000000269`), so viewer-created beads lose their human `owner` |
| `X-Actor` | Comment post only | `bv/server.go:3940` `WithActor(email)` → `client.go:3279` | user email | `:840-844` (`id.Subject = actor`) | comment author/`created_by`/event actor becomes the machine subject instead of the human (the code comment at `bv/server.go:3930-3938` documents the earlier "Author=static" bug). Every OTHER write has no X-Actor → daemon `Subject` is the literal `static` today |
| `X-System-Operator` | Plan-label auto-bind with no human epic operator | `bv/plan_label_reactor.go:389` `WithSystemOperator("plan-label-autobind")` → `client.go:3323` | machine label | `:872-876` | without it the `Bind` CreateBead 400s ("operator could not be resolved", ka-0000004529/4835). A machine principal's own identity must resolve as a non-human operator sink or this reactor needs the header relayed |
| `X-Escalate` | none in viewer | `int/beadsapi/escalate.go:20` | n/a | n/a | n/a |
| Edge-auth envelope (`X-Operator` + sig) | all human writes **only if** HMAC key set | `bv/middleware.go:71`, `bv/main.go:120-123` | verified human | verified by edge-auth path (`AuthIdentity{Type:"edge"}`), not a header trust branch | **off on k3s**; independent of machine principal, but with it on the human write is edge-authenticated and does not use the static token at all |
| `X-User-Bead-Id`, `X-Owner-Override`, `X-No-Owner-Default`, `X-Trigger-Bead`, `X-No-Auto-Parent`, `X-Fleet` | none | not set by the viewer | n/a | `:848-890` | nothing. Machine principal needs **none** of these trust branches for the viewer |

Net header verdict: the viewer needs exactly one header-trust capability: an **operated-by relay** (`X-Operated-By` on a machine principal) for human attribution of human-initiated writes, plus (a) `X-Actor` relay for comments, (b) `X-System-Operator` relay for the label-reactor. If the design keeps these ignored (design §4.1), attribution of every viewer write degrades to `machine:<viewer>` and enforcement-mode creates must resolve the operator some other way. **Flag for Task 5:** an operated-by relay for the viewer reopens the static-branch `X-Operated-By` trust (`applyOperatedByHeader`, `kb/interceptors.go:846-848`).

#### O. Ownership dependencies (non-config beads the viewer is not creator/assignee/owner of)

Mechanism: `authorizeOwnership` passes if bead is config kind (`:401`), caller has admin (`:410`), caller is `static` (`:413`), privileged fleet (`:425`), fleet+dm (`:439`), escalation/decision for non-OIDC (`:470-475`), the bead has no ownership metadata, or `bead.CreatedBy|Assignee|Owner == id.Subject` (`:487`), or the caller is the bead's operator via alias (OIDC/edge only, `callerIsBeadOperator`). A machine principal (not `static`, not `fleet`) would hit the last two only.

| # | Bypass relied on | Viewer sites | Verdict under a machine principal |
|---|---|---|---|
| O1 | **static-token ownership bypass `write_authz.go:413`** | W1, W2, W3, W4 (label), W6 (trigger beads `captain_ops.go:307,577,945`, `captain_pause.go:84`, `captain_successor.go:179`, `trigger_toggle.go:71`, `triggers_bulk.go:100`, plus close `captain_ops.go:630,675`), W8 (epic/step/doc/gate/trigger edits and closes, `plan_edit.go:121,167,206,322,332,572`, `plans.go:4963,4991,5017,5297,5303`, `plans_lifecycle.go:230,518,612,658`, `plans_master_switch.go:116`), W9 reactors, W13 followers (`loadBeadForWrite`), W17 `thread_compose.go:301`, W18 `peel_request.go:156`, W15 re-rating | **403 ownership denial** for any bead created by a human/agent/other component. Only beads the viewer's machine subject created itself would pass (`created_by` is the machine subject from the first write onward; today it is the literal `static`). This is the dominant dependency; Task 5 must decide: (a) grant a system-graph ownership bypass to the viewer principal (reopens an ownership bypass, flag) or (b) keep these writes on the user bearer |
| O2 | service-managed-field bypass (`kb/beads.go:~985-1050`, `service_field_policy.go:122`) | **none**: the viewer's trigger writes touch `enabled`/`captain`/`chain`, not `last_checked/last_run/...`; its `last_nudge_at` stamp (`plans.go:4963`) is on an epic | n/a |
| O3 | `type=dm` fleet carve-out (`:439`) | none (viewer is static, not fleet; writes no dm) | n/a |
| O4 | escalation/decision shared-queue carve-out (`:470-475`) | W11 reflag PATCH on decision beads, and decision resolve/cancel (which skip ownership anyway) | machine passes while it is non-OIDC |
| O5 | comment skips ownership (`kb/http_relations.go:521`) | W5 and every audit comment in W6/W21 | no ownership dependency; only type policy (e.g. `comment/project`, `comment/trigger`) |
| O6 | config-kind skip (`:401`) | agent wake `planagent/spawn.go:173`, schedule toggles, project/user/team/view writes | no ownership dependency; admin-gated ones need the allow-list/grant (W19-W22) |
| O7 | privileged-fleet bypass (`:425`, live `BEADS_PRIVILEGED_FLEETS=gitlab-bridge`) | none | n/a |

`created_by` consequence: viewer-created beads have `created_by=static` today (create body `CreatedBy` ignored, `kb/beads.go:142-146,265`; X-Actor only on comments). A machine principal changes this to `machine:<viewer>`; any consumer that matches `created_by=="static"` or a human email is affected (not checked here).

#### M. `machineAllowedWrites` entries the viewer needs (feeds master Step 5)

| Entry | Viewer callers | Notes |
|---|---|---|
| `create/advice` | `bv/create_advice.go:142` | 1 call site |
| `update/user` | `bv/settings.go:454`, `user_status_message.go:236`, `user_inbox.go:439`, `inbox_flag.go:94` | 4 sites; the comment at `kb/write_authz.go:167-177` names 3 (misses the pin write) |
| `update/project` | `bv/project_config.go:314` | not named in the allow-list comment, which only names gb and slack-bridge |
| `comment/project` | `bv/project_config.go:320` | same |
| `update/team` (NOT in list) | `bv/team_page.go:270` | already denied for static; machine grant is a new capability, not a re-point |
| `delete/view`/`DELETE /v1/beads` (NOT in list) | `bv/views.go:450` | already denied; admin-tier route |
| `create/view`, `update/*`, `create/*` on non-admin types | W1-W19 | no allow-list entry needed; need `gardenio-operator` baseline plus ownership answer (O1) |
| Not used by the viewer: `report/*`, `advice/close`, `role/create`, `config/*`, `account/create`, `project/create`, `jira-poller/create` | none | viewer has no caller for any of these |

#### C. Counts (commit `67a7ad34`, `gardenio/controller/cmd/beads-viewer`)

- Non-test files in the directory (`git ls-files | grep -v _test.go`): 537 total, of which 264 are `.go` (the rest are templates/static assets).
- Files matching the master Step 2 grep verbatim (`CreateBead|UpdateBead\w*|CloseBead|DeleteBead|AddComment|AddDependency|RemoveDependency|AddLabel|RemoveLabel|SetConfig|Register\w*Token\w*|Revoke\w*Token\w*|\w*Operator\w*` followed by `(`): **58 files, 176 lines** (non-zero). The 176 includes read-only `*Operator*` helpers (`ListOperators`, `resolveOperator*`, `IsHumanOperator`, ...) and the `WithOperator` stamps.
- Strict write set (`s.daemon.` + 28 write/POST methods incl. `CreateBeadDetail`, `UpdateBeadFieldsTyped`, `CloseBeadAs`, `Append/ReorderBeadArrayField`, `Resolve/CancelDecision`, `Approve/RejectEscalation`, `PourFormula`, follower, taint/scrub, `RecordUITelemetry`): **116 call sites in 46 files**. Plus ≈17 sites in internal packages the viewer drives (`planagent` 5, `captainmode` 12, `pausegate` 1, `plancaptain` 1). `plantranscript` only builds the request (sent by `plans_assistant_prompt.go:580`).
- Files touching operated-by (`operated_?by|OperatedBy|WithOperator|X-Operated`, non-test): **80** (as hinted by the master task, grouped by feature in section H). `beadsapi.WithOperator(` call sites: **17** (list in H). Other `beadsapi.With*`: `WithActor` 1 (`server.go:3940`), `WithSystemOperator` 1 (`plan_label_reactor.go:389`), `WithEdgeAuthOperator` 1 (`middleware.go:71`), `WithUserBearer` 1 (`middleware.go:74`).
- Feature groups: **23** (W1-W23). Distinct non-OIDC dependencies: ownership bypass O1 (dominant), operated-by relay, X-Actor relay (comments), X-System-Operator relay (1 reactor), `requireRoleMachineAllowed` admin read (1 route), `machineAllowedWrites` (create/advice, update/user, update/project, comment/project).
- Needs-verification list (not answerable from the repo): (1) whether the k3s edge actually supplies `X-Auth-Request-Email` (otherwise most writes 401 in-viewer); (2) whether `gb prime` (`bv/prime.go:42`) writes; (3) existence/policy of a live `type:view` config row.

