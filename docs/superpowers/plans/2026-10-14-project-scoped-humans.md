# Project-scoped humans Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A second human (first user: a ClaimLayer teammate, Keycloak user `akashdixit@gmail.com`, Slack `U0C8VAG1V9T`) signs in to the beads-viewer at `gardenio.clonk.tv` and sees and acts on **their own project (`claimlayer`) only**: its beads, agents, plans, decisions, escalations and nothing else. Every other project, the cluster, costs, agent terminals and privileged approvals stay closed to them, and a human with a project group but no resolvable project is denied, never given everything.

**Owner direction (laptop chat, 2026-10-14 ~19:00Z, option B):** the teammate is a demo-access user with NO gardenio realm roles today. The owner wants them, in future, to use the viewer for their own project (`claimlayer`) only, scoped. Option B supersedes step 1 of escalation gd-1nDlKw5PWA, which proposed `/gardenio-admins` (owner-level). He is never added to `/gardenio-admins`.

**Architecture:** Three layers each fail closed, so any one of them alone still holds.

1. **kbeads is the authority.** A new realm role `gardenio-project-member` marks a *scoped principal*. For a scoped principal the allowed projects come from the **groups claim of the verified token on every request** (`/project-<name>` groups), not from a user-bead field that syncs late and never shrinks. Empty means deny. Reads, writes (all bead types), sub-resources, aggregates and escalation approval are all confined to that set, and a route inventory test fails the build when a new route is not classified.
2. **Keycloak carries the grants.** One group per project (`project-<name>`) maps to `gardenio-project-member`. The realm has no default gardenio role any more; viewers get `gardenio-viewers` explicitly. Removing the group removes the access within one access-token lifetime (300 s).
3. **The viewer and edge carry the human to kbeads.** The viewer sends the signed-in user's own bearer on **reads as well as writes**, so layer 1 applies. Routes that cannot be scoped (costs, taints, triggers, telepathy, agent terminals, the mux) are admin-only for scoped principals, and the shared hubs and caches that fan one daemon-wide stream to every browser are filtered or switched off for them. The viewer edge admits `/gardenio-admins` and the project groups; the mux edge stays `/gardenio-admins` only.

**Tech stack:** Go 1.25 (modules `kbeads` and `gardenio/controller`), kbeads HTTP server (`kbeads/internal/server`), beads-viewer (`gardenio/controller/cmd/beads-viewer`), daemon client (`gardenio/controller/internal/beadsapi`), Helm chart `gardenio/helm/gardenio` with Go render tests in `gardenio/helm/gardenio/tests` (separate module, `make test-helm`), `deploy-values/gardenio-k3s.yaml`, raw manifests under `gardenio/helm/gardenio-ziti`, the edge probe under `scripts/edge-probe` (from plan gd-P78ygCUgA9), `gb` and `kd`.

**Plan bead:** gd-507PTO0036 (labels `plan`, `plan:project-scoped-humans`, `project:gardenio`; `plan_state=ready`; the owner clicks Approve & build). Phases: 0 gd-30KRlwyjz8, 1 gd-RL4IS7kCZq, 2 gd-myIsTYHWX7, 3 gd-YF5fr57I4G, 4 gd-tt1p2OMFFl. Seed task: gd-oySDauyQcj. Related: escalation gd-1nDlKw5PWA (the teammate's access request), bug gd-5o6PxGfSa7 (Task 1), plan gd-P78ygCUgA9 (public edge).

**Anchors:** verified against `origin/main` at `0c2a6336` (2026-10-14). Lines drift as other PRs land. If a cited line has moved, find it by the quoted text and say so in the PR.

## Global Constraints

1. **Nobody is granted anything until Task 1 and Task 4 are merged.** Today any gardenio-operator or gardenio-admin can approve an `infra-admin` spawn (cluster-admin) because spawn_policy approvers are informational (`kbeads/internal/server/config.go:1191`, bug gd-5o6PxGfSa7). The teammate's Keycloak user must hold no gardenio group until Task 12 step 3.
2. **The teammate is never put in `/gardenio-admins` or `/gardenio-operators`.** Project humans get `gardenio-project-member` only, through `project-<name>` groups. The viewer edge today admits only `/gardenio-admins` (`gardenio/helm/gardenio-ziti/viewer-authproxy.yaml:207`), so until Task 9 lands they cannot reach the viewer at all. That is the intended interim state, not a bug.
3. **kbeads is the authority, the viewer is a courtesy.** Hiding a link in the viewer is never the control. Every denial a scoped human could hit through the viewer must also be a denial from kbeads when the same bearer calls the daemon directly.
4. **Fail closed, in this order of preference:** unknown route, empty project set, unlabelled bead, user-bead lookup error, malformed groups claim: all deny for a scoped principal. Today `oidcAllowedProjects` fails OPEN on all of these (`kbeads/internal/server/read_authz.go:36-60`); Task 2 changes that for scoped principals only. Existing global operators keep today's behaviour.
5. **Every "cannot" test has a positive control** in the same test: the same request from an allowed identity (the owner, or the scoped human on their own project) succeeds. A test that cannot fail proves nothing.
6. **No Secret value, token, cookie or password** in logs, test output, PR text, bead comments or Slack. The teammate's temporary password is handed over out of band by the operator. Test tokens in Go tests are literals marked as test fixtures.
7. **Lanes.** Crews never run kubectl, helm or Terraform against the live cluster, and never create Keycloak users or groups by hand (standing rule gd-oI2AIaSzjF). Realm objects are code (chart values, `deploy-values`). Steps marked **OPERATOR** are run by the operator session; **OWNER** steps need the owner in person.
8. **Merge windows.** Any PR that touches `deploy-values/**` or chart values that the auto-deployer rolls goes up as DRAFT with the label `merge-desk:hold`, and the crew files `gb escalation create --operation-type merge_window --requesting-task <task id>` for a watched window. PRs touching `kbeads/internal/server/*authz*`, `interceptors.go`, `gardenio/helm/**`, `deploy-values/**` or `.github/workflows/**` are merge-desk PRs (working rule (d)); say so in the description. Code-only PRs outside those paths self-merge on green. The viewer edge manifest `gardenio/helm/gardenio-ziti/viewer-authproxy.yaml` is not rolled by the auto-deployer: a merge changes nothing live until the operator applies it.
9. **Sequencing with the public edge (plan gd-P78ygCUgA9).** Tasks 1 to 8 and 10 to 11 are code and chart work that needs no public name; they are tested in-cluster and over ziti and can start now. **Task 9 is blocked by S4 (gd-UqhR5d6qPt, "Viewer and mux on public names")** because S4 puts `gardenio.clonk.tv` and `mux.clonk.tv` on the public edge and edits the same `viewer-authproxy.yaml`. **Task 12 (live check and onboarding) is blocked by Task 9.** Until S4 is live the teammate has no non-ziti viewer access; their Keycloak account still works for the ClaimLayer demo at `claimlayer.clonk.tv` (gd-1nDlKw5PWA step 1), which does not depend on this plan.
10. **Branch and review.** Branch `agent/project-scoped-humans/<task-id>`, PR against main.
11. **Go hygiene.** `gofumpt -l`, `go vet ./...`, `staticcheck ./...` clean on the touched module; `go test` on touched packages passes. Gate every commit on the test command's exit code (`go test ./... && git commit ...`), never on piped output. Agent pods have a 2Gi memory limit: run `go test` on the touched package, never whole-module `go build ./...` or `make build` locally (advice gd-QLhMjEsBsR); CI runs the rest.
12. **Non-goals for this plan.** The teammate never attaches to agent terminals through the mux (Task 9 keeps `mux.clonk.tv` admin-only; a project-limited attach is a later plan). They have no kubectl, Grafana, Headlamp or `api.clonk.tv` access. No self-service signup. No second tenant database. Slack acting as the linked person is plan work already in flight (decision gd-uPHB7YcHuI); Task 10 is routing only.

## Review Focus

1. **Scoped principal with a stale or empty user bead.** `allowed_projects` on the user bead is written by `syncGroupProjects`, which returns early when the human has no project group (`kbeads/internal/server/user_provision.go:231`), so removing a group never shrinks the bead. Pinned: Task 2 derives scope from the token claim, and `TestScopedPrincipal_GroupRemovalRevokes` (stale bead says claimlayer, token says nothing: deny).
2. **A scoped human writing a `task` or `bug`.** `project_scoped` is a per-type write policy, and `task`/`bug` do not set it, so today an OIDC operator writes any task anywhere. Pinned: Task 3 `TestScopedWrites_AllTypes` (every bead type, in and out of project, create without a label).
3. **Approving an escalation replays the write with write policy skipped** (`kbeads/internal/server/http_escalations.go:350`, `withMachineWrite`), so a scoped approver could approve their way into an admin-only write. Pinned: Task 4 `TestApprove_ScopedHumanCannotApproveOutOfProject` and `..._PrivilegedSpawn`.
4. **The viewer's daemon-wide SSE stream.** `BeadFeedHub` holds one upstream `EventStream` and fans every event to every browser subscriber (`gardenio/controller/cmd/beads-viewer/bead_feed_hub.go`, subscribed from `feed.go:1427`). Filtering list queries is not enough. Pinned: Task 8 `TestFeedSSE_ScopedSubscriberSeesOnlyOwnProject`.
5. **Handlers that call the daemon on `context.Background()`** instead of the request context (eight sites in `server.go`, for example the molecule, dashboard, rich report and bundle views at `:4143-4207`) would use the static token and skip per-user scope. Pinned: Task 7 guard test that fails on a new background-context daemon call in a request handler.
6. **A new realm user with no group.** The chart default `gardenio-viewer` `isDefault: true` (`gardenio/helm/gardenio/values.yaml:13947`) makes every realm user a global reader. Pinned: Task 6 render test that no role in the chart is `isDefault`.

## Task to step map and dependencies

| Task | Step bead | Phase | Lane |
|---|---|---|---|
| 1 spawn_policy approvers stamped and enforced; privileged spawns admin-only | gd-5o6PxGfSa7 (existing bug, adopted) | 0 | crew |
| 2 Scoped principal: claim-derived projects, fail-closed reads | gd-46J3exwQrz | 1 | crew |
| 3 Scoped principal writes: every type confined to the project | gd-ajTaGQYmRe | 1 | crew |
| 4 Scoped approvals: own project, never privileged | gd-tUi5IuttJn | 1 | crew |
| 5 Route inventory: classify every kbeads route for scoped principals | gd-IyeaKcmI82 | 1 | crew |
| 6 Realm: project-member role and per-project groups, no default role | gd-zuV3EfwrzB | 2 | crew + OPERATOR (merge window) |
| 7 Viewer reads as the signed-in user | gd-akDjTS2eh7 | 3 | crew |
| 8 Viewer route classes, hubs and caches for scoped principals | gd-PXWguyutzL | 3 | crew |
| 9 Viewer edge admits project groups; mux stays admin-only | gd-4tvNth0mfq | 3 | crew + OPERATOR |
| 10 Slack identity mapping for the teammate (routing only) | gd-9RrlPkQ8y3 | 4 | OPERATOR |
| 11 Negative-matrix acceptance suite | gd-6q0cRd9V7Q | 4 | crew |
| 12 Live check with a test user, then onboard the teammate | gd-llFKMS8Pkc | 4 | OPERATOR + OWNER |

Dependencies (each is a blocked-by edge on the dependent task): 1 → 4; 2 → 3, 4, 5, 6, 7; 5 → 8; 7 → 8; 6 → 9, 10; 8 → 9, 10, 11; 4 → 11; 3 → 11; 5 → 11; 9 → 12; 10 → 12; 11 → 12. External: 9 ← gd-UqhR5d6qPt (S4). Tasks 1 and 2 have no blockers and can start at once.

## Shared test helper (created by Task 2, used by Tasks 3, 4, 5, 11)

`kbeads/internal/server/scoped_test_server_test.go` provides:

- `newScopedTestServer(t) (*BeadsServer, *mockStore, http.Handler)`: a server with a fake OIDC verifier mapping bearer literals to identities:
  - `scoped-claimlayer-tok`: email `akash@example.test`, roles `[gardenio-project-member]`, groups `[/project-claimlayer]`
  - `scoped-nogroup-tok`: same email, roles `[gardenio-project-member]`, groups `[]` (fail-closed case)
  - `scoped-two-tok`: roles `[gardenio-project-member]`, groups `[/project-claimlayer, /project-other]`
  - `owner-admin-tok`: roles `[gardenio-admin]`, groups `[/gardenio-admins]`
  - `operator-tok`: roles `[gardenio-operator]`, groups `[/gardenio-operators]` (global operator, unchanged behaviour)
  - seeded beads: `cl-1` (task, `project:claimlayer`), `gd-1` (task, `project:gardenio`), `nl-1` (task, no project label), an escalation on each project, a `spawn` escalation for role `infra-admin` labelled `project:claimlayer`.
- `doBearerJSON(t, h, method, path, bearer string, body any, headers map[string]string) *httptest.ResponseRecorder`. If `classed_test_server_test.go` from the pre-publish-security plan already defines it, reuse that one and do not redefine it.

---

## Phase 0: Close the approval hole

### Task 1: spawn_policy approvers stamped and enforced; privileged spawns admin-only

**Step bead:** gd-5o6PxGfSa7 (existing P1 bug, adopted as this plan's phase 0). The bead body is the spec; this section is the acceptance contract. No blockers. Blocks Task 4.

**Why first:** a second human with *any* operator-level role could today approve an `infra-admin` spawn. Task 12 does not onboard them until this and Task 4 are merged.

**Files:**
- Modify: `kbeads/internal/server/http_escalations.go:198-212` (the `escalationApprovers(targetBead)` gate in `handleApproveEscalation`)
- Modify: `kbeads/internal/server/config.go:1191` area (spawn_policy human rule `approvers`), and the escalation stamping path (find with `grep -n 'spawn_policy' kbeads/internal/server/*.go`, the place that creates the spawn escalation bead)
- Test: `kbeads/internal/server/http_escalations_approvers_test.go`

- [ ] **Step 1: Failing tests first.** `TestApprove_NonListedAdminGets403` (infra-admin spawn escalation stamped with approvers `[oidc:owner@example.test]`; a different admin gets 403), `TestApprove_ListedOwnerSucceeds` (positive control), `TestApprove_PrivilegedRoleNoApproversDefaultsAdminOnly` (a privileged role whose rule lists no approvers stamps `admin-only`; a plain operator gets 403, an admin 200), `TestApprove_ApproversResolveViaLookupOperatorAlias` (entries in `oidc:` and `slack:` form resolve through `LookupOperatorAlias`, `team:` entries through `TeamsOfDirect`).
- [ ] **Step 2: Implement** per the bead: copy the matching spawn_policy human rule's approvers onto the escalation bead when it is stamped, enforce on approve, default privileged roles with no approvers to admin-only. "Privileged" is the set `bypass_ownership` roles plus `infra`, `infra-admin` and the `claimlayer-infra` family (gd-kLRIbndGNp, commit `0c2a6336`, made `bypass_ownership` roles privileged for spawn; reuse that predicate, do not re-list roles).
- [ ] **Step 3: Run** `cd kbeads && go test ./internal/server/ -run 'TestApprove_' -count=1`. Expected PASS.
- [ ] **Step 4: Commit and PR** (merge-desk PR: touches authz). Comment on gd-5o6PxGfSa7 with the PR.

---

## Phase 1: Fail-closed project scope in kbeads

### Task 2: Scoped principal: claim-derived projects, fail-closed reads

**Step bead:** gd-46J3exwQrz. Phase 1 (gd-RL4IS7kCZq). Plan gd-507PTO0036. No blockers. Blocks Tasks 3, 4, 5, 6, 7.

**Goal:** Define the *scoped principal* and make every bead read for one fail closed. A scoped principal is an OIDC caller that is not `gardenio-admin` and whose roles contain `gardenio-project-member`. Its allowed projects are `groupsToProjects(GroupsFromContext(ctx))` (`kbeads/internal/server/user_provision.go:289`, `interceptors.go:82`) read from the **verified token on this request**. The user bead's `allowed_projects` is ignored for it, so a stale bead cannot widen access, and an empty set denies instead of meaning "unrestricted".

**Who does what:** CREW. No owner step.

**Files:**
- Create: `kbeads/internal/server/scoped_principal.go`, `kbeads/internal/server/scoped_principal_test.go`, `kbeads/internal/server/scoped_test_server_test.go`
- Modify: `kbeads/internal/server/read_authz.go:23-60` (`oidcAllowedProjects`), and every caller that treats a `nil` return as unrestricted (`listBeadsFiltered`, `getBeadFiltered`, `beadMatchesOIDCProjects`: find the rest with `grep -n 'oidcAllowedProjects' kbeads/internal/server/*.go`)
- Modify: `kbeads/internal/server/interceptors.go:71-110` (add `const RoleProjectMember = "gardenio-project-member"`; leave `hasMinimumRole` ranks alone: a project member is NOT a global viewer or operator by rank)
- Modify: `kbeads/internal/server/user_provision.go:229-262` (`syncGroupProjects`: when the human has no project group, write an empty list instead of returning early, and drop the cache key, so the bead no longer keeps a revoked project)

**Interfaces:**
- Consumes: `RolesFromContext`, `GroupsFromContext`, `AuthIdentityFromContext`, `authMethodFromContext`, `authMethodOIDC`, `groupsToProjects`.
- Produces:
  - `type scopeDecision struct{ Unrestricted bool; Projects []string }`
  - `func scopeFromContext(ctx context.Context) scopeDecision`: `Unrestricted` for non-OIDC callers (static, fleet, machine: unchanged, they have their own scoping), for admins, and for OIDC callers without `gardenio-project-member` (global operators keep today's path through `oidcAllowedProjects`). For a scoped principal returns `Projects` from the token groups; empty `Projects` with `Unrestricted=false` means deny all.
  - `func (d scopeDecision) allows(project string) bool`
  - `oidcAllowedProjects` keeps its `[]string` signature for existing callers but returns `[]string{}` (non-nil, empty) for a scoped principal with no projects, and every consumer treats non-nil empty as "match nothing".

- [ ] **Step 1: Write the shared test server** `scoped_test_server_test.go` as described in "Shared test helper". Use the existing OIDC test verifier pattern in `kbeads/internal/server/oidc_e2e_test.go:167-177` (a map of bearer to email/roles) and add a `groups` field to it.
- [ ] **Step 2: Failing tests** in `scoped_principal_test.go`:
  - `TestScopedPrincipal_ListsOnlyOwnProject`: `scoped-claimlayer-tok` lists beads and sees `cl-1` only; control: `owner-admin-tok` sees all three.
  - `TestScopedPrincipal_GetOtherProject404or403`: GET `gd-1` and `nl-1` are denied (use the status `getBeadFiltered` returns today for out-of-scope, `errReadDenied`); GET `cl-1` is 200.
  - `TestScopedPrincipal_NoGroupDeniesAll`: `scoped-nogroup-tok` gets an empty list and a denial on `cl-1`. This is the fail-closed pin (empty is not unrestricted).
  - `TestScopedPrincipal_GroupRemovalRevokes`: seed the user bead with `allowed_projects:["claimlayer"]` (stale), call with `scoped-nogroup-tok`: deny. Then `TestSyncGroupProjects_EmptyGroupsClearsBead`: `syncGroupProjects` with groups `[]` writes `allowed_projects: []`.
  - `TestScopedPrincipal_TwoProjectsUnion`: `scoped-two-tok` sees `cl-1` and the `other` project's bead, not `gd-1`.
  - `TestOperatorUnchanged`: `operator-tok` (no project-member role) behaves exactly as before: with no allowed_projects it still sees everything. This pins that the change is scoped to the new role.
  - Sub-resources: for each of comments, events, mutations, labels, dependencies, followers, mentions on `gd-1`, the scoped token is denied and `cl-1` is allowed (these route through `getBeadFiltered`, `read_authz.go:111` doc comment; the test enumerates them so a new sub-resource that skips the gate fails).
- [ ] **Step 3: Run** `cd kbeads && go test ./internal/server/ -run 'TestScopedPrincipal_|TestSyncGroupProjects_|TestOperatorUnchanged' -count=1`. Expected: FAIL (compile error on `scopeFromContext`).
- [ ] **Step 4: Implement** `scoped_principal.go` and wire `oidcAllowedProjects` and `syncGroupProjects` as above. A groups claim that is present but unparseable counts as empty (deny).
- [ ] **Step 5: Run the same command.** Expected PASS. Then `go vet ./internal/server/ && staticcheck ./internal/server/`.
- [ ] **Step 6: Commit and PR** (merge-desk PR: authz). Message `feat(kbeads): fail-closed project scope for gardenio-project-member principals (<task id>)`.

### Task 3: Scoped principal writes: every type confined to the project

**Step bead:** gd-ajTaGQYmRe. Phase 1 (gd-RL4IS7kCZq). Blocked by Task 2. Blocks Task 11.

**Goal:** A scoped principal writes only beads whose `project:` label is in its project set, for **every bead type**, and only the types on an allowlist. Today `checkProjectScope` runs only when the type's `write_policy.project_scoped` is true and the caller is OIDC (`kbeads/internal/server/write_authz.go:325-337`, `:789`), and `task`/`bug` do not set it, so an OIDC operator writes any task in any project.

**Files:**
- Modify: `kbeads/internal/server/write_authz.go` (the function containing the `project_scoped` check at `:337`; add a scoped-principal branch ahead of it), `kbeads/internal/server/spawn_authz.go:110-135` (`agentFields.Project` check: a scoped principal's spawn project must be in its token projects, and `len(perms.AllowedProjects) > 0` is no longer the gate for it)
- Test: `kbeads/internal/server/scoped_writes_test.go`

**Interfaces:**
- Consumes: `scopeFromContext` (Task 2), `projectFromLabels`.
- Produces: `var scopedWritableTypes = map[model.BeadType]bool{...}` containing exactly `task`, `bug`, `feature`, `decision` (respond only), `comment` writes via the comment endpoint, and `escalation` (create only). Everything else (config, role, spawn_policy, trigger, advice, user, operator, team, taint, bundle, agent beyond the project's own spawn) is denied for scoped principals with a message naming the type.

- [ ] **Step 1: Failing tests** `TestScopedWrites_AllTypes`: a table over every `model.BeadType`; for each, create/update/close/delete with `scoped-claimlayer-tok` against (a) a bead labelled `project:claimlayer`, (b) `project:gardenio`, (c) no project label. Expect allowed only for (a) on allowlisted types; every other cell denied. Positive controls: `owner-admin-tok` succeeds on all cells; `operator-tok` is unchanged. Add `TestScopedWrites_CreateWithoutProjectLabelDenied` and `TestScopedWrites_RelabelOutOfProjectDenied` (update that changes `project:claimlayer` to `project:gardenio` is denied), `TestScopedWrites_LinkAcrossProjectsDenied` (a dependency from `cl-1` to `gd-1`).
- [ ] **Step 2: Run** `cd kbeads && go test ./internal/server/ -run 'TestScopedWrites_' -count=1`. Expected FAIL.
- [ ] **Step 3: Implement.** The type table is derived from `model.BeadType` constants so a new type is denied until added to the allowlist; `TestScopedWrites_AllTypes` iterates the same constant list, so a new type with no decision fails the test.
- [ ] **Step 4: Run** same command. Expected PASS. `go vet`, `staticcheck`.
- [ ] **Step 5: Commit and PR** (merge-desk PR: authz).

### Task 4: Scoped approvals: own project, never privileged

**Step bead:** gd-tUi5IuttJn. Phase 1 (gd-RL4IS7kCZq). Blocked by Task 1 and Task 2. Blocks Task 11.

**Goal:** A scoped principal can approve or reject an escalation only when (a) it is labelled with one of the principal's projects, (b) its `operation_type` is on a scoped allowlist, and (c) it is not a spawn or re-role of a privileged role. The approval replay does not widen this: for a scoped approver the replayed write is checked against Task 3's rules rather than skipped. Today any operator approves any escalation they can read, approvers are enforced only when the bead carries them, and the replay runs `WithMachineOperator` + `withMachineWrite` (`kbeads/internal/server/http_escalations.go:153`, `:198-212`, `:350`).

**Files:**
- Modify: `kbeads/internal/server/http_escalations.go` (`handleApproveEscalation`, `handleRejectEscalation`, and the replay block around `:340-370`)
- Create: `kbeads/internal/server/escalation_scope.go`, `kbeads/internal/server/escalation_scope_test.go`

**Interfaces:**
- Consumes: `scopeFromContext`, `authorizeResolutionTarget` (`http_escalations.go:143`), Task 1's privileged-role predicate and `escalationApprovers`.
- Produces: `var scopedApprovableOperations = map[string]bool{"decision_proxy": true, "manual_operator_action": false, ...}` (an explicit list; find the real `operation_type` values with `grep -n 'operation_type' kbeads/internal/server/http_escalations.go` and set `true` ONLY for operations that act inside the escalation's own project and need no cluster, infra or secret access; the rest are `false`), and `func (s *BeadsServer) scopedApproverAllowed(ctx, esc *model.Bead) (ok bool, reason string)`.

- [ ] **Step 1: Failing tests** in `escalation_scope_test.go`, using the Task 2 server:
  - `TestApprove_ScopedHumanApprovesOwnProjectEscalation` (positive control: claimlayer escalation, allowlisted operation: 200).
  - `TestApprove_ScopedHumanCannotApproveOutOfProject` (gardenio escalation: 403; the bead is also not readable).
  - `TestApprove_ScopedHumanCannotApprovePrivilegedSpawn` (the seeded `infra-admin` spawn escalation labelled `project:claimlayer`: 403 with the owner approving it as the positive control).
  - `TestApprove_ScopedHumanCannotApproveNonAllowlistedOperation` (a `helm_apply`-style or `manual_operator_action` escalation inside claimlayer: 403).
  - `TestApprove_ScopedReplayChecksWritePolicy` (an escalation whose replay writes an out-of-project or non-allowlisted bead: the approve call returns 403 and the write did not happen).
  - `TestReject_ScopedHumanOwnProjectOnly` (same matrix for reject).
  - `TestApprove_OperatorUnchanged` (the global operator path is byte-for-byte what Task 1 leaves).
- [ ] **Step 2: Run** `cd kbeads && go test ./internal/server/ -run 'TestApprove_|TestReject_' -count=1`. Expected FAIL on the new cases.
- [ ] **Step 3: Implement** `scopedApproverAllowed` and call it in approve and reject right after `authorizeResolutionTarget`; in the replay, when the approver is scoped, use a context that keeps Task 3's scoped check on (do not apply `withMachineWrite` for the scoped approver's replay).
- [ ] **Step 4: Run** same command. Expected PASS.
- [ ] **Step 5: Commit and PR** (merge-desk PR). In the PR description list the `scopedApprovableOperations` table so the owner can review the allowlist.

### Task 5: Route inventory: classify every kbeads route for scoped principals

**Step bead:** gd-IyeaKcmI82. Phase 1 (gd-RL4IS7kCZq). Blocked by Task 2. Blocks Tasks 8 and 11.

**Goal:** Per-bead reads are covered by Task 2, but kbeads also serves aggregates and global lists (stats and metrics, project lists, operator, team and user lists, advice, config, agent roster, gardeners, events streams, search, semantic search, graph, costs). Each must be one of: filtered to the scoped set, denied for scoped principals, or proven safe. A table plus a guard test makes a new route fail the build until classified.

**Files:**
- Create: `kbeads/internal/server/scoped_route_classes.go` (the table), `kbeads/internal/server/scoped_route_classes_test.go`
- Modify: `kbeads/internal/server/http.go` or wherever `NewHTTPHandler` registers routes (find with `grep -n 'NewHTTPHandler' kbeads/internal/server/*.go`), plus each handler that needs filtering

**Interfaces:**
- Produces: `type routeScopeClass int` with `classFiltered`, `classAdminOnly`, `classScopedSafe`; `var scopedRouteClasses = map[string]routeScopeClass{"GET /v1/beads": classFiltered, ...}` keyed by the exact mux pattern string; `func scopedRouteGate(next http.Handler) http.Handler` that, for a scoped principal, returns 403 on `classAdminOnly` and passes the others.

- [ ] **Step 1: Failing guard test** `TestEveryRouteHasScopedClass`: walk the registered patterns of `NewHTTPHandler` (the test builds the handler and reads the pattern list from the same registration helper, so it cannot drift) and fail on any pattern missing from `scopedRouteClasses`. Positive control inside the same test: delete one entry from a copy of the table and assert the walk reports it.
- [ ] **Step 2: Behavioural tests** `TestScopedRoutes_Matrix`: for each `classAdminOnly` route the scoped token gets 403 and `owner-admin-tok` gets 2xx; for each `classFiltered` list route (`/v1/beads`, search, semantic search, decisions, escalations, agents, gardeners, plans, events, dependencies graph) the scoped token's response contains no `gd-1` / `nl-1` content, with `owner-admin-tok` returning them as the positive control.
- [ ] **Step 3: Classify** (the starting table, to be completed by walking the registrations): operators, teams, users, user-provision, fleet tokens, config, roles, spawn policies, triggers, taints, cost and spend endpoints, metrics, `/v1/projects` (all-projects list), scrub requests: `classAdminOnly`. Bead lists and search, decisions, escalations, agents, gardeners, plans, event streams: `classFiltered`. Health and version: `classScopedSafe`. For streams (SSE `/v1/events`), filtering means each event's bead is looked up and dropped when out of scope; if a stream cannot be filtered cheaply, mark it `classAdminOnly` and the viewer (Task 8) serves its own filtered stream.
- [ ] **Step 4: Run** `cd kbeads && go test ./internal/server/ -run 'TestEveryRouteHasScopedClass|TestScopedRoutes_' -count=1`. Expected PASS.
- [ ] **Step 5: Commit and PR** (merge-desk PR).

---

## Phase 2: The realm carries the grants

### Task 6: Realm: project-member role and per-project groups, no default role

**Step bead:** gd-zuV3EfwrzB. Phase 2 (gd-myIsTYHWX7). Blocked by Task 2. Blocks Tasks 9 and 10.

**Goal:** Keycloak grants project access by group, and a brand-new realm user holds no gardenio role. Today `gardenio-viewer` is `isDefault: true` (`gardenio/helm/gardenio/values.yaml:13947`), which makes every realm user a global reader. The deployment keeps `defaultGroups: []` because the operator's CRD does not declare it (`deploy-values/gardenio-k3s.yaml` comments above `defaultGroups: []`), and re-declares the whole groups list because a Helm list override replaces rather than merges.

**Files:**
- Modify: `gardenio/helm/gardenio/values.yaml` (`keycloak.roles` at `:13940-13950`: add `gardenio-project-member`, drop `isDefault: true` from `gardenio-viewer`; add `keycloak.projectGroups: []`, a list of `{name: <project>}`), `gardenio/helm/gardenio/templates/keycloak/groups.yaml` (also range over `projectGroups`, rendering a `KeycloakRealmGroup` named `project-<name>` with `realmRoles: [gardenio-project-member]`), `gardenio/helm/gardenio/templates/keycloak/roles.yaml` (no change expected; confirm `isDefault` renders only when set)
- Modify: `deploy-values/gardenio-k3s.yaml` (the `groups:` list near `:327`: this is a full-list override, so add `project-claimlayer` AND keep all five existing groups verbatim; add `realmUsers` entry for `akashdixit@gmail.com` with `groups: []` for now, so the user stays role-less until Task 12)
- Test: `gardenio/helm/gardenio/tests/keycloak_project_groups_test.go`

- [ ] **Step 1: Failing render tests** (copy the helper style of an existing keycloak test in `gardenio/helm/gardenio/tests`; `ls gardenio/helm/gardenio/tests | grep -i keycloak`): `TestKeycloakProjectGroups_RendersGroupPerProject` (one `KeycloakRealmGroup` per entry, named `project-claimlayer`, with exactly `gardenio-project-member` and no other role), `TestKeycloakRoles_NoDefaultRole` (no rendered `KeycloakRealmRole` has `isDefault: true`; positive control: the same test fails if the render is given `isDefault: true`), `TestKeycloakProjectGroups_NeverGrantsGlobalRole` (no project group maps to `gardenio-viewer`, `-operator`, `-admin` or `escalation-approver`), `TestDeployValuesGroupsKeepsFiveChartGroups` (the k3s override still contains the five original groups).
- [ ] **Step 2: Run** `make test-helm` (about 5 minutes) or `cd gardenio/helm/gardenio/tests && GOWORK=off go test ./ -run 'Keycloak' -count=1`. Expected FAIL.
- [ ] **Step 3: Implement** the chart and values changes. Existing users keep their group-derived roles; the only user who relied on the default role is the owner, who is in `gardenio-admins`. Verify that claim by listing realm users (OPERATOR step in the PR description: "list realm users with no group; expect none besides the teammate").
- [ ] **Step 4: Check the revocation window.** `keycloak.realm.tokenSettings.accessTokenLifespan` is 300 s (`values.yaml:13881`). Record in the PR description: removing a user from `project-<name>` takes effect on their next access token, at most 5 minutes later, plus the oauth2-proxy session refresh interval (find it in `templates/` forwardAuth values). If the session refresh is longer than 5 minutes, note it and set the edge to re-check groups at the access-token lifetime in Task 9.
- [ ] **Step 5: Run** `helm lint gardenio/helm/gardenio/` and the test command again. Expected PASS.
- [ ] **Step 6: PR as DRAFT** with label `merge-desk:hold` (touches `deploy-values/**` and `gardenio/helm/**`). File the merge-window escalation: `gb escalation create --operation-type merge_window --requesting-task <task id> ...` listing the exact OPERATOR steps: merge, roll the release, confirm the operator reconciled the new group (`KeycloakRealmGroup project-claimlayer` Ready) and that the teammate's user still has no group.

---

## Phase 3: The viewer carries the human to kbeads

### Task 7: Viewer reads as the signed-in user

**Step bead:** gd-akDjTS2eh7. Phase 3 (gd-YF5fr57I4G). Blocked by Task 2. Blocks Task 8.

**Goal:** Reads go to the daemon with the user's own bearer, so Task 2 applies. Today the daemon client uses the user bearer only for writes (`applyUserBearerWriteGate` in `gardenio/controller/internal/beadsapi/user_bearer.go` is a no-op for reads), and the viewer's `operatorMiddleware` already attaches the bearer to the request context (`gardenio/controller/cmd/beads-viewer/middleware.go:74`). PR #343 made writes per-user (`BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1`, `deploy-values/gardenio-k3s.yaml:1484` area); this task does the same for reads, with an explicit marker for the few system reads.

**Files:**
- Modify: `gardenio/controller/internal/beadsapi/user_bearer.go` (add `SetRequireUserBearerForReads`, `WithSystemRead(ctx)`, `ErrUserBearerRequiredForRead`), and the transport (`sendOnce`; find with `grep -n 'applyUserBearerWriteGate' gardenio/controller/internal/beadsapi/*.go`) so a present bearer overrides the static token on reads when enforcement is on
- Modify: `gardenio/controller/cmd/beads-viewer/main.go` (`BEADS_VIEWER_REQUIRE_USER_BEARER_READS=1` -> `daemon.SetRequireUserBearerForReads(true)`, next to the writes flag at `:98`)
- Modify: the request-handler daemon calls that use `context.Background()`: `server.go:4143-4207` (eight sites), `main.go` (six), `project_bulk.go`, `bead_activity.go`, `bead_feed_hub.go` (the hub keeps a system context via `WithSystemRead`, see Task 8). Find all with `grep -n 'context.Background()' gardenio/controller/cmd/beads-viewer/*.go | grep -v _test`.
- Modify: `deploy-values/gardenio-k3s.yaml` (env `BEADS_VIEWER_REQUIRE_USER_BEARER_READS`, rolled in Task 12, not here: leave it OFF in this PR and say so)
- Test: `gardenio/controller/internal/beadsapi/user_bearer_reads_test.go`, `gardenio/controller/cmd/beads-viewer/no_background_ctx_test.go`

**Interfaces:**
- Produces: `func WithSystemRead(ctx context.Context) context.Context`, an explicit, greppable marker for reads that legitimately have no human (the feed hub's upstream stream, startup caches). With read enforcement on, a read with neither a user bearer nor the marker returns `ErrUserBearerRequiredForRead` and is not sent.

- [ ] **Step 1: Failing tests:** `TestReads_UseUserBearerWhenEnforced` (httptest daemon records `Authorization`; with enforcement on and a bearer in ctx, the read carries the user bearer, not the static token), `TestReads_FailClosedWithoutBearer` (no bearer, no marker: error, zero requests sent), `TestReads_SystemReadMarkerUsesStaticToken` (positive control), `TestReads_EnforcementOffUnchanged`. In `no_background_ctx_test.go`: `TestNoBackgroundCtxDaemonCallsInHandlers`, which parses the viewer package AST and fails on a `context.Background()` or `context.TODO()` argument passed to a `daemon.` or `s.daemon.` method inside a function that takes an `*http.Request` or is reached from one. Allowlist the hub and startup sites by function name in the test. Positive control: the test also runs on a tiny fixture source containing a violation and asserts it is reported.
- [ ] **Step 2: Run** `cd gardenio/controller && go test ./internal/beadsapi/ -run 'TestReads_' -count=1 && go test ./cmd/beads-viewer/ -run TestNoBackgroundCtx -count=1`. Expected FAIL.
- [ ] **Step 3: Implement.** Convert each `context.Background()` handler site to the request context (pass `r.Context()` down; for template-builder helpers add a `ctx` parameter).
- [ ] **Step 4: Run** the same commands. Expected PASS. Then `go vet ./cmd/beads-viewer/ ./internal/beadsapi/` (package-level, not whole module: 2Gi pod limit).
- [ ] **Step 5: Commit and PR** (code only; the env stays off).
- [ ] **Step 6: Second PR, the flip.** DRAFT with label `merge-desk:hold`, one line in `deploy-values/gardenio-k3s.yaml` next to the writes flag at `:1484`: `BEADS_VIEWER_REQUIRE_USER_BEARER_READS: "1"`. File the merge-window escalation. Do not ask for the window before Task 8 has merged: with reads enforced, any viewer route not yet classified would fail for the admin too. Task 12 step 1 rolls it.

### Task 8: Viewer route classes, hubs and caches for scoped principals

**Step bead:** gd-PXWguyutzL. Phase 3 (gd-YF5fr57I4G). Blocked by Task 5 and Task 7. Blocks Tasks 9, 10 and 11.

**Goal:** The viewer has about 680 source files of pages and routes. Each route a scoped human can reach is classified the way Task 5 classified kbeads: **scoped** (renders only own-project data because its reads go through the user bearer), **admin-only** (403), or **public** (health, login, static assets). The shared hubs and caches that hold data fetched with the static token are filtered per principal or switched off for scoped principals.

**Files:**
- Create: `gardenio/controller/cmd/beads-viewer/scope.go` (`type viewerScope struct{ Admin bool; Projects []string }`, `scopeFromRequest(r)` reading the verified roles and groups the OIDC/forwardauth path already puts in context; `scopeMiddleware`), `gardenio/controller/cmd/beads-viewer/route_classes.go`, tests `scope_test.go`, `route_classes_test.go`
- Modify: `bead_feed_hub.go` + `feed.go:1427` (SSE), `thinking_hub.go` + `telepathy.go:75`, `dashboard_hub.go` + `dashboardHubCache` (`server.go:655-660`), the caches `plans_enrich_list_cache.go`, `plans_project_cache.go`, `gardener_owner_cache.go`, `operator_slug_cache.go`, and the routes that proxy to Grafana (`grafana_proxy.go`), costs (`costs*.go`), taints, triggers, agent terminal and chat (`agent_input.go`, `agent_chat*.go`, `agents.go`), and `team_page.go`
- Modify: `main.go` (wire `scopeMiddleware` after `operatorMiddleware`)

**Interfaces:**
- Consumes: Task 7's per-request bearer; Task 5's kbeads route classes (a viewer page that needs an admin-only daemon route is itself admin-only).
- Produces: `var viewerRouteClasses = map[string]routeClass{...}` keyed by mux pattern. Starting classification: **admin-only** `/costs*`, `/grafana*`, `/taints*`, `/triggers*`, `/telepathy*`, `/team*`, `/settings` (global parts), `/claude-config`, `/advice*`, `/gardeners*` write actions, agent input and restart (`agent_input.go`, `agent_restart.go`), `/auth/cli-exchange`; **scoped** bead list and show, `/active`, `/decisions`, `/escalations`, `/plans*`, `/agents` (read, filtered to the project's agents), `/project/<own>`; **public** `/healthz`, `/readyz`, `/login`, static assets.

- [ ] **Step 1: Failing tests:**
  - `TestEveryViewerRouteHasClass`: walks the patterns registered on the viewer mux (`main.go:387-489` and the `Server` route table) and fails on an unclassified one; positive control in the same test as in Task 5.
  - `TestScopedViewer_AdminOnlyRoutes403`: every admin-only route returns 403 for a scoped identity and 200 for the admin identity.
  - `TestFeedSSE_ScopedSubscriberSeesOnlyOwnProject`: publish events for `cl-1` and `gd-1` into the hub; the scoped subscriber receives only `cl-1`'s, an admin subscriber both. The hub looks each event's bead labels up through the system-read client once and caches the project per bead id (bounded cache), failing closed (drop) when the lookup fails.
  - `TestHubs_ScopedCachesAreKeyedByScope`: `dashboardHubCache`, plans caches and `gardener_owner_cache` never serve a value populated for an admin to a scoped principal (key by scope, or bypass the cache for scoped principals).
  - `TestScopedViewer_BeadPageOtherProject403`: GET `/bead/gd-1` for the scoped identity is denied by the daemon's answer (not hidden by the viewer), shown as a plain not-found page.
  - `TestScopedViewer_NoOtherProjectInNavigation`: the project switcher, search box and "recent" lists for a scoped identity contain only `claimlayer`.
- [ ] **Step 2: Run** `cd gardenio/controller && go test ./cmd/beads-viewer/ -run 'TestEveryViewerRouteHasClass|TestScopedViewer_|TestFeedSSE_|TestHubs_' -count=1`. Expected FAIL.
- [ ] **Step 3: Implement** `scope.go` and the classification; filter or disable each hub and cache. The telepathy thinking feed and Grafana proxy are admin-only.
- [ ] **Step 4: Run** the same command. Expected PASS. Package-level `go vet`.
- [ ] **Step 5: Commit and PR.** Include screenshots of `/` for an admin and for a scoped identity from the in-repo test harness if the harness renders HTML; otherwise attach the route-class table.

### Task 9: Viewer edge admits project groups; mux stays admin-only

**Step bead:** gd-4tvNth0mfq. Phase 3 (gd-YF5fr57I4G). Blocked by Task 6, Task 8 and **S4 gd-UqhR5d6qPt** (public names for viewer and mux). Blocks Task 12.

**Goal:** `gardenio.clonk.tv` signs in members of `/gardenio-admins` and of each `/project-<name>` group; `mux.clonk.tv` keeps admitting only `/gardenio-admins`. Today the viewer's internal auth check passes `allowed_groups=/gardenio-admins` (`gardenio/helm/gardenio-ziti/viewer-authproxy.yaml:207`); the mux server block proxies with the injected `COOPMUX_TOKEN` and carries no per-human scope (`viewer-authproxy.yaml:309-317`), which is why a project human must never reach it.

**Files:**
- Modify: `gardenio/helm/gardenio-ziti/viewer-authproxy.yaml` (the viewer server's `location = /oauth2/auth` `allowed_groups=` list at `:207`: `/gardenio-admins,/project-claimlayer`; leave the mux server block's check untouched)
- Create: `scripts/check-viewer-edge-groups.py` and `scripts/check-viewer-edge-groups_test.py` (asserts the viewer edge's group list equals `/gardenio-admins` plus one `/project-<name>` for each entry in `keycloak.projectGroups` of `deploy-values/gardenio-k3s.yaml`, and that the mux block's list is exactly `/gardenio-admins`); add it to `.github/workflows/checks.yml` `script-selftests`
- Modify: `scripts/edge-probe/expectations.json` and `edge_probe.py` (from plan gd-P78ygCUgA9 task 1): add a check E9 "mux admits admins only" (see Task 11) once that tree exists on main

- [ ] **Step 1: Failing guard test:** `scripts/check-viewer-edge-groups_test.py` with a good fixture (passes), a fixture where the mux list gained a project group (must fail), and one where a project in values is missing from the viewer list (must fail).
- [ ] **Step 2: Implement** the edge change and the guard. oauth2-proxy accepts a comma-separated `allowed_groups` (any-of); the edge is coarse authorisation, kbeads and Task 8 are the real scope. Re-check groups at the access-token lifetime: confirm the forwardAuth `cookie_refresh` is at most the 300 s access-token lifespan; if it is longer, lower it here and record the value in the PR.
- [ ] **Step 3: Run** `python3 scripts/check-viewer-edge-groups_test.py` and `python3 scripts/check-viewer-edge-groups.py`. Expected PASS.
- [ ] **Step 4: PR** (merge-desk PR: `gardenio/helm/**`). The raw manifest is applied by the OPERATOR after merge; the crew's escalation lists the exact apply and the nginx reload, and the verification: a Keycloak user with only `project-scopetest` gets 202 on `/oauth2/auth` for the viewer host and 403 for the mux host.

---

## Phase 4: Identity routing and acceptance

### Task 10: Slack identity mapping for the teammate (routing only)

**Step bead:** gd-9RrlPkQ8y3. Phase 4 (gd-tt1p2OMFFl). Blocked by Task 6 and Task 8. Blocks Task 12.

**Goal:** Decisions and escalations in project `claimlayer` reach the teammate in Slack, so they can act on them in the viewer. This mapping is **routing, not authority**: the Slack bridge acts with its own bridge identity and the relay header is attribution only (pre-publish-security Task 3), and Slack-acts-as-the-linked-person is separate plan work (decision gd-uPHB7YcHuI, task gd-OZJ6H2S3xx). A Slack click by the teammate must therefore never be able to approve anything; approvals happen in the viewer under Task 4's rules.

**Who does what:** OPERATOR runs the commands (identity beads are `type=user`, which a scoped or agent principal cannot write). The crew prepares the exact commands and the verification.

- [ ] **Step 1: Check the current state:** `gb identity resolve U0C8VAG1V9T` and `gb identity resolve akashdixit@gmail.com`; record both outputs on the task bead.
- [ ] **Step 2 (OPERATOR): Register the mapping:** `gb identity add "Akash" --slack-id U0C8VAG1V9T --email akashdixit@gmail.com` as an OIDC gardenio-admin (the daemon answers 403 to the static and fleet tokens, see `gb identity add --help`). The later `gb identity link-operator` (run by the teammate in their own session, Task 12 step 6) succeeds only because this admin-written user bead carries their `slack_id`.
- [ ] **Step 3: Do NOT set `allowed_projects` by hand on the user bead.** It is derived from the group (Task 2) and a hand-set value is ignored for scoped principals. Verify `gb identity show` lists no project grants.
- [ ] **Step 4: Verify routing:** file a test decision labelled `project:claimlayer` and confirm it posts to the teammate's Slack DM/thread; file one labelled `project:gardenio` and confirm they do not receive it. Comment both results on the task bead. Close the test beads.

### Task 11: Negative-matrix acceptance suite

**Step bead:** gd-6q0cRd9V7Q. Phase 4 (gd-tt1p2OMFFl). Blocked by Tasks 3, 4, 5 and 8. Blocks Task 12.

**Goal:** One suite, runnable in CI without a cluster, that states the outcome the owner asked for as assertions, each paired with its positive control, plus one external check for the mux. It is the acceptance test for the plan; Tasks 2 to 8 each carry their own unit tests, and this suite is the end-to-end statement.

**Files:**
- Create: `kbeads/internal/server/scoped_acceptance_test.go` (daemon level), `gardenio/controller/cmd/beads-viewer/scoped_acceptance_test.go` (viewer level, httptest daemon backed by the kbeads handler where the module boundary allows, otherwise a recorded fake that enforces the same rules and a comment pointing at the kbeads test)
- Modify: `scripts/edge-probe/edge_probe.py` and `expectations.json` (check **E9**: with a test identity that holds only `project-scopetest`, the mux host returns 403 and the viewer host returns 202/200; positive control: the same host with the admin test identity returns 200). If the edge-probe tree is not on main yet, add E9 in the PR that follows it and say so.

- [ ] **Step 1: Write the matrix** as a table-driven test, one row per line, each row naming the identity, the request and the expected status:

| Identity | Request | Expect |
|---|---|---|
| scoped claimlayer | list beads | only `project:claimlayer` |
| scoped claimlayer | GET `gd-1` (gardenio task) and every sub-resource | denied |
| scoped claimlayer | GET an unlabelled bead | denied |
| scoped claimlayer | create a task with no project label | denied |
| scoped claimlayer | create/update/close a claimlayer task | allowed |
| scoped claimlayer | approve a gardenio escalation | 403 |
| scoped claimlayer | approve a claimlayer `infra-admin` spawn | 403 |
| scoped claimlayer | approve a claimlayer allowlisted decision escalation | 200 |
| scoped, no group | list and get | empty / denied |
| scoped claimlayer | viewer `/costs`, `/grafana`, `/taints`, `/triggers`, `/telepathy`, agent input | 403 |
| scoped claimlayer | viewer feed SSE | only claimlayer events |
| operator (global) | everything it could do before this plan | unchanged |
| owner admin | everything | allowed (positive control for each row above) |

- [ ] **Step 2: Add the group-removal row:** the same scoped bearer after its token groups lose `/project-claimlayer` gets the empty-list result on the next request (models the 300 s window).
- [ ] **Step 3: Run** `cd kbeads && go test ./internal/server/ -run TestScopedAcceptance -count=1` and `cd gardenio/controller && go test ./cmd/beads-viewer/ -run TestScopedAcceptance -count=1`. Expected PASS.
- [ ] **Step 4: Commit and PR.** Add both test names to the PR description so the reviewer can find the matrix.

### Task 12: Live check with a test user, then onboard the teammate

**Step bead:** gd-llFKMS8Pkc. Phase 4 (gd-tt1p2OMFFl). Blocked by Task 9, Task 10 and Task 11 (and therefore S4). Last task.

**Goal:** The operator proves on the live system, with a **test user**, that the matrix holds; only then does the real teammate get their group. No crew runs any of this (lanes, Global Constraint 7). The crew's job is to file one escalation listing these steps verbatim and to record the results.

**Who does what:** OPERATOR runs steps 1 to 4 and 6; OWNER is needed for step 5 (MFA enrolment on the test user's and the teammate's first sign-in) and for the approval in step 7.

- [ ] **Step 1 (OPERATOR): Roll.** After the Task 6 merge window the realm has `gardenio-project-member`, the groups and no default role. Confirm: `kubectl get keycloakrealmgroup -n gardenio` lists `project-claimlayer`; list realm users with no group and confirm only the teammate appears. Merge the Task 7 step 6 flip (`BEADS_VIEWER_REQUIRE_USER_BEARER_READS=1`) in its window and roll the viewer; confirm the owner's own viewer session still works afterwards (admin reads now carry the owner's bearer). Apply the Task 9 edge manifest and reload the edge nginx.
- [ ] **Step 2 (OPERATOR): Test user.** Create realm user `matthewbaker1110+scopetest@gmail.com` (the owner's own mailbox, so no third party is involved) in Keycloak with a temporary password handed to the owner out of band, in group `/project-scopetest` only. This needs `project-scopetest` declared in `keycloak.projectGroups` and one seed bead labelled `project:scopetest` (the crew adds the group entry in Task 6's PR as a labelled test fixture, removed in step 8; the OPERATOR creates the seed bead with `kd create "scopetest seed" --type task -l project:scopetest`).
- [ ] **Step 3 (OPERATOR): Drive the matrix** from a device with no ziti tunnel (the probe note in plan gd-P78ygCUgA9, Global Constraint 2). As the test user, in a browser, confirm and record on the bead: the viewer loads; only `scopetest` beads, agents and plans show; `/costs`, `/grafana`, `/telepathy`, `/taints` return 403; opening a `gardenio` bead URL shows not found; approving a gardenio escalation fails; `mux.clonk.tv` returns 403 at the edge. As the owner, approve and reject the test project's own escalations. Run check E9 as documented in `scripts/edge-probe/README.md` with the test identity.
- [ ] **Step 4 (OPERATOR): Group removal.** Remove the test user from `/project-scopetest`; within 5 minutes plus the session refresh the viewer must show nothing and kbeads must return empty lists. Record the observed delay.
- [ ] **Step 5 (OWNER): MFA** enrolment happens on the first sign-in of each user (TOTP, gd-RTo5vEgg9S).
  > Note (2026-10-31): TOTP is retired. Admins now sign in with a passkey and enrol it in the account console first; see `gardenio/docs/keycloak-passkeys-runbook.md` and plan gd-QXq1fjYuRD (S4c gd-GiZfavru5G).
- [ ] **Step 6 (OPERATOR): Onboard the teammate** only after steps 3 and 4 pass: add `akashdixit@gmail.com` to `project-claimlayer` through the realm CR (`realmUsers[].groups` in `deploy-values/gardenio-k3s.yaml`, a DRAFT `merge-desk:hold` PR with its merge-window escalation), confirm they are NOT in `gardenio-admins`, hand them the temporary password out of band, run `gb identity link-operator` in their session (Task 10), and have them sign in at `gardenio.clonk.tv`. Watch the first session with them: their project's beads and agents only.
- [ ] **Step 7 (OWNER): Close the loop.** Comment on gd-1nDlKw5PWA that option B replaced step 1's `/gardenio-admins` grant and step 3's hand-set project grant, and close the test fixtures.
- [ ] **Step 8: Cleanup.** Delete the test user and close this task. Keep the `project-scopetest` group, its `land-scopetest` scope and its seed bead: scopetest is now the land-door verifier's test land (gd-Hq1PnUVOUC, `gardenio/scripts/land-door-verify.sh`), whose test owner is a separate realm user.
