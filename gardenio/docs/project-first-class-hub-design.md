# First-class, validated projects + a rich beads-viewer project hub — design doc

**Operator:** Matt Baker (oidc:matthew.baker@acme.example)
**Tracking bead (research):** ka-0000000828 · **Epic:** see §10
**Source ask (verbatim):** *"new big feature i think: we want a 'project' beads-viewer page. go through the whole concept of a project (and perhaps some of its dependent objects) and figure out what that's gonna have to look like."*
**Operator follow-ups (same thread):**
- *"if we make projects better validated and more first class I would not mind at all."*
- *"like adding a secret to a project should be easier… also we should think about how projects and roles relate, that could be important. And maybe roles get the same treatment?"*

**Precedent skeleton:** Gardener Page v2 (`gardenio/docs/gardener-page-v2-design.md`) — eager-head + lazy-tail HTMX split, operator-gated in-page config editor, golden/accuracy invariants.
**Frozen sibling:** Project-view epic sequencer **ka-0000003558** (ON-ICE) — `gardenio/plans/project-view-epic-sequencer.md`. This doc explicitly resolves the relationship (§7).

This doc closes three questions: (1) what a rich, first-class **project hub page** must contain and how it reuses existing surfaces; (2) how to make project beads **better-validated** with a single schema source of truth; and (3) how **projects relate to roles**, and what "roles get the same treatment" would entail. The most important section is **§5 (validation)** — it is cheap, unblocks everything else, and fixes a *live, verified* schema-drift bug.

---

## 0. TL;DR — decisions

| # | Question | Decision |
|---|----------|----------|
| **D1** | Route: new `/project/{name}` or upgrade `/p/{project}`? | **Upgrade `/p/{project}` into a rich hub; keep the existing list as the `Work` tab (`?view=list`).** `/p/{project}` is already linked from `/u/{slug}`, the index, every project-list row, and Slack. A new route orphans those links. The list view is the work-breakdown section of the hub, not a competitor. |
| **D2** | What is a "project", concretely? | A **`type:project` config-kind bead** (`kbeads/internal/server/config.go:420`) carrying ~28 K8s/agent config fields, PLUS its **dependent objects discovered by label/reference**: issue beads (`project:<name>`), `type:account`, `type:trigger` gardeners, `type:schedule`, role-scoped agents, per-project K8s Secrets, coverage rows, cost rollups. The hub renders the config bead AND aggregates the dependents. |
| **D3** | Hub sections | 9 sections: **Config summary, Health, Work breakdown, Agents, Gardeners/Schedules, Accounts, Costs/Coverage, Recent activity, Decisions/Escalations.** Eager: config summary + health + work-breakdown digest (reuse `projectSummary`). Lazy (HTMX-on-reveal): the other six. |
| **D4** | Validation: where is the source of truth? | **The daemon `type:project` TypeConfig is the single source of truth** (`kbeads/internal/server/config.go:420`). It is already enforced on create (`beads.go:194`) and update (`beads.go:971`) via `model.ValidateTypeFields`. Tighten it (enums, the missing fields), fix the drift, and have the viewer editor + controller parser both *consume* it — never re-declare a parallel schema. |
| **D5** | **Live schema-drift bug (VERIFIED)** | The controller's `ProjectInfo` parser reads **7 fields the `type:project` schema does not declare**: `account`, `model`, `runtime_class`, `triggers_enabled`, `dm_default`, `resource_mapping`, `claude_config` (`beadsapi/projects.go:291–380` vs `config.go:423–449`). Because `type:project` does NOT set `allow_additional_fields`, the daemon's unknown-field check (`validate_fields.go:90`) **rejects these on `create` and on any `update` that introduces them** (existing keys are grandfathered on update — `validate_fields.go:79–86`). **P1 adds them to the schema.** |
| **D6** | Sequencer epic ka-0000003558 | **Leave frozen; do NOT supersede.** The hub's tab bar reserves a `Sequencer→` slot that lights up (`SequencerEnabled`) when the operator unfreezes it — zero rework. The hub builds the *list*; the sequencer builds the *map*. |
| **D7** | Config editor in UI? | **Read-only in P1–P3; admin-gated editor in P4.** Today projects are edited CLI-only. `type:project` WritePolicy is `min_role: gardenio-admin` (lvl 3) and is **not** `project_scoped`, so the editor must gate on **admin** (not operator), and the daemon enforces it regardless. "Adding a secret should be easier" (operator ask) is the editor's flagship affordance: a structured *add-secret* form beats hand-editing the `secrets` JSON. |
| **D8** | Index page | Upgrade `/` project cards to surface health + config-validity badges (the index already loads full `ProjectInfo` via `ListProjectBeads`). |
| **D9** | **Projects ↔ roles + "roles get the same treatment"** | Roles are **already partway first-class**: a `type:role` config bead + `RoleInfo` model + `applyRoleDefaults` pod overrides exist. Precedence is **global < project < role < agent**, implemented in `podspec.go`. Roles have the **same drift bug** (`RoleInfo` reads `model` — `roles.go:153` — which `type:role` schema omits — `config.go:574`). Recommendation: **P1 fixes both project AND role schema drift together** (cheap, same mechanism), and a **separate sibling epic** delivers a `/role/{name}` hub + role registry/validation so it can be green-lit independently without bloating the project epic. See §8. |

---

## 1. The concept of a "project"

A project is a **`type:project` config bead** (`kind: config`, `write_policy.min_role: gardenio-admin`). Its title is the project name (legacy beads carry a `"Project: "` prefix the controller strips — `projects.go:281`). Its `fields` JSON declares everything about how agents run for that project. The schema (`config.go:420`) declares 28 fields:

```
prefix, git_url, default_branch, image, storage_class, service_account,
docker, postgres, cpu_request, cpu_limit, memory_request, memory_limit,
secrets, env, env_json, repos, slack_channel, channel_roles, channel_modes,
channel_runtime_class, auto_assign, prewarmed_pool, nudge_prompts,
default_timezone, schedules_enabled, needs_k8s_api, session_resume
```

The controller (`beadsapi/projects.go`, `ProjectInfo`) is the **authoritative consumer** — it parses these into pod specs, credential routing, schedule/trigger gating, Slack-channel→project resolution, prewarmed pools, and FSM resource mapping. `ProjectInfo` is a deep struct (~40 fields incl. the 7 undeclared ones).

### 1.1 Dependent objects ("perhaps some of its dependent objects")

These are NOT fields on the project bead — they are discovered by label/reference and are what make the hub *rich*:

| Dependent | How discovered | Existing surface to reuse |
|---|---|---|
| **Issue beads** (epics/tasks/bugs) | label `project:<name>` | `handleProject` list + `buildProjectSummary` (`project_summary.go`) |
| **Accounts** | `ProjectInfo.Account` → `type:account`; also `ListAccountBeadsByProject` (`accounts.go:82`) | `/costs` account cards (`costs.go:128 costAccountView`) |
| **Gardeners** | `type:trigger` beads, project field/label | `/gardeners` index, `/gardener/{id}` |
| **Schedules** | `type:schedule` beads; gated by `schedules_enabled` | — |
| **Agents** | `type:agent`/`session` beads scoped to project | `/agents`, `agents.go` (incl. existing per-project role breakdown `agents.go:289`) |
| **Per-project Secrets** | `ProjectInfo.Secrets` (`SecretEntry[]`) | render names/keys only — **never values** |
| **Coverage** | `coverage_per_project.go` | reuse rollup view-model |
| **Costs** | account rollup filtered to project's account | `/costs` summary |
| **Decisions / escalations** | `BlockedOnDecision` beads; escalations `?project=` filter (`escalations.go:66`) | `projectSummary.BlockedBeads`, `escalations.go` |

---

## 2. Route decision (D1): upgrade `/p/{project}`

The existing `/p/{project}` (`server.go:2005 handleProject`) is a pure bead list with a right-rail facet panel (`projectSummary`). It is referenced everywhere: `/u/{slug}` rows, index project cards (`buildProjectChipHref`, `server.go:1520`), per-row links, Slack. **A new `/project/{name}` route would orphan all of those.**

**Decision:** `/p/{project}` becomes the hub. The current list becomes the **Work breakdown** section, also reachable as `/p/{project}?view=list` so the optimized filter-and-bulk-act list (`project_bulk.go`) is preserved verbatim. A **tab/section bar** sits under the project header:

```
[ Overview ]  [ Work ]  [ Agents ]  [ Gardeners ]  [ Costs ]  [ Sequencer→ ]  [ Config ]
```

`Overview` (default) renders config summary + health + a compact work-breakdown digest. This mirrors how the gardener page kept `/gardener/{id}` stable while adding `/gardener/{id}/runs` etc.

---

## 3. Hub section design

Reuses the **gardener-page eager-head + lazy-tail skeleton** (`gardener_page.go`, `handleGardenerRunsSection` at `:162`): cheap above-the-fold content renders eagerly; expensive aggregations are HTMX fragments revealed on scroll under the `/b/...` fragment prefix (the viewer already reserves `/b/` to disambiguate from `/p/{project}` — `server.go:1172`).

### 3.1 Eager (rendered inline on first paint)
1. **Config summary** — labeled render of `ProjectInfo`: git_url/branch, image, resources, storage class, service account, account, docker/postgres/k8s-api flags, schedules/triggers enabled, slack channels, repos count, **secrets count + names only**. Drives the config-validity badge (§5.3). One `ListProjectBeads` call (already used by the index).
2. **Health** — derived from `projectSummary` (`project_summary.go`, one query): Open/InProgress/Blocked/StaleInProgress/Created24h, P0–P4 distribution, top assignees, Blocked + Stale attention lists. Reuse `buildProjectSummary` unchanged.
3. **Work breakdown digest** — the type-chip strip with counts + a "view full list" link to the `Work` tab. Reuse `projectTypeChips` + `summary.TypeCounts`.

### 3.2 Lazy (HTMX fragment, revealed on scroll / tab open)
4. **Agents** — active project-scoped agents, reuse `agents.go` row rendering. `GET /b/project/{name}/agents`.
5. **Gardeners / Schedules** — `type:trigger` + `type:schedule` for the project, with a paused banner when `schedules_enabled`/`triggers_enabled` is false. Links to `/gardener/{id}`. `GET /b/project/{name}/gardeners`.
6. **Accounts** — resolved `type:account` bead(s) via `ProjectInfo.Account` / `ListAccountBeadsByProject`: provider, model, status, budget. Reuse `costAccountView`. `GET /b/project/{name}/accounts`.
7. **Costs / Coverage** — cost rollup for the project's account(s) + coverage view-model (`coverage_per_project.go`) when enabled. `GET /b/project/{name}/costs`.
8. **Recent activity** — most-recently-updated project beads (mirror `computeIndexRecent`, scoped). `GET /b/project/{name}/activity`.
9. **Decisions / escalations** — open decision/escalation beads (`projectSummary.BlockedBeads` + `escalations.go`). `GET /b/project/{name}/decisions`.

Every fragment is **best-effort**: a daemon error renders an empty section with retry, never a 500 — matching `handleProject`/gardener-page degradation rules.

---

## 4. View-model + backend

A single `projectHubView` assembles eager data:

```go
type projectHubView struct {
    Name             string
    Config           ProjectInfo            // from ListProjectBeads (reuse)
    ConfigBeadID     string                 // for link + editor PATCH target
    ConfigBeadStatus string
    Validity         projectValidityReport  // §5.3 — health signal, not a schema
    Summary          *projectSummary        // from buildProjectSummary (reuse)
    Chips            []typeFilterChip
    IsAdmin          bool                   // gates the Config-edit affordance
    SequencerEnabled bool                   // tab visible iff ka-0000003558 unfrozen
}
```

`ListProjectBeads` currently returns `map[name]ProjectInfo` and **discards the bead ID** (`projects.go:283`). Add `GetProjectBead(ctx, name)` (or a `BeadID` field on `ProjectInfo`) so the hub links to the config bead and the editor can PATCH it. This is the one small backend change the eager path needs; all dependent aggregations reuse existing functions.

---

## 5. Validation plan (D4, D5) — the cheap, high-value core

### 5.1 Single source of truth: the daemon TypeConfig
Project-field validation already exists and is enforced: `model.ValidateTypeFields` at create (`beads.go:194`) and `ValidateTypeFieldsForUpdate` at update (`beads.go:971`), driven by the `type:project` TypeConfig (`config.go:420`). `FieldDef` (`model/type_config.go`) already supports `Required`, `enum`/`Values`, integer `Min`/`Max`. **We do not build a new schema — we tighten the existing one and make every other layer consume it.**

### 5.2 Concrete field tightening (what to validate, where)
All in `kbeads/internal/server/config.go` `type:project` block:
- **Fix the drift (P1, blocking):** add the 7 controller-read fields so writes don't get rejected — `account` (string), `model` (string), `runtime_class` (string), `triggers_enabled` (boolean), `dm_default` (boolean), `resource_mapping` (json), `claude_config` (json). Do a full field-by-field diff of the `ProjectInfo` parser vs the schema; this is a mechanical, test-backed change.
- **Promote enums:** `session_resume` → enum `["true","false","auto"]` (matches controller `parseTriBool`). Do **not** add `required` to fields legacy beads omit (would wedge existing project beads).
- **Layered-validation fields (documented):** K8s quantity strings (`cpu_request/limit`, `memory_request/limit`) and structured-JSON fields (`secrets`, `repos`, `env`, `prewarmed_pool`, `resource_mapping`, `channel_*`). The daemon `FieldDef` can only express "valid JSON"/"string"; their *shape* (`SecretEntry{env,secret,key}`, `RepoEntry{url,branch,role}`) is defined by Go structs in `projects.go`. Validate shape in the **editor** (pre-POST) + a controller parse-time warning — document this as the one field class where validation is layered.

### 5.3 Where validation surfaces
- **Daemon (authoritative):** rejects unknown keys, wrong types, bad enums, out-of-range ints on create/update. Already wired; tightening the schema is the whole change.
- **Controller parse path:** make every JSON-field unmarshal failure log a uniform `slog.Warn(project, field, error)`. Today some are silently swallowed (`if json.Unmarshal(...) == nil`), and `env_json` builds an error then discards it (`projects.go:~391`). Keep skip-don't-fail semantics, but fail *loud*.
- **Viewer read path:** `projectValidityReport` — a **health signal, NOT a parallel schema** — flags (a) JSON fields that don't parse into their expected struct, (b) resource strings that aren't valid K8s quantities, (c) `account`/`image` references that don't resolve to a live bead. Drives a config-validity badge on the hub header + index cards.
- **Viewer editor (P4, admin-gated):** pre-validates the form against the live TypeConfig fetched from the daemon, shows inline field errors, then the daemon re-validates on PATCH (defense in depth, mirroring `gardener_ops.go` config save).

**Drift guard:** add a kbeads (or cross-module) test asserting every field `ProjectInfo` reads is declared in the `type:project` schema — so D5-style drift can never silently recur.

---

## 6. Config editor (D7) — "adding a secret should be easier"

Today projects are edited **CLI-only** (`kd create`/`kd update` against the `type:project` bead). The gardener page added an in-UI editor (`gardener_ops.go`: `GET /gardener/{id}/edit`, `POST /gardener/{id}/config`) gated by `viewerCanOperate` (`gardener_ops.go:664`, `RoleOperator` lvl 2).

**Auth is the load-bearing difference:** `type:project` WritePolicy is `min_role: gardenio-admin` (lvl 3) and is **not** `project_scoped`. So:
- The hub's Config-edit affordance gates on **admin** (`viewerCanAdmin`, mirroring `viewerCanOperate` but on `oidcauth.RoleAdmin`). The daemon enforces `min_role: gardenio-admin` regardless of UI.
- POST routes register behind admin middleware (mirror `operatorOnly` at `server.go:1024`).
- Because `type:project` is **not** project-scoped, a "project admin" is a *global* admin — no per-project delegation today (open question §9).

**Flagship affordance:** a structured **add-secret form** (`env` / `secret` / `key` → appends a `SecretEntry` to the `secrets` JSON) so the operator never hand-edits JSON. Same pattern for repos and env vars.

**Phasing:** P1–P3 ship the hub **read-only** with an admin-gated "Edit config (CLI: `kd …`)" hint; P4 ships the in-UI editor with §5.3 pre-validation. Land the rich page and prove value before taking on the write surface.

---

## 7. Relationship to the frozen sequencer epic ka-0000003558 (D6)

The sequencer plan (`gardenio/plans/project-view-epic-sequencer.md`) is **ON-ICE** by operator instruction. It already proposes `GET /p/{project}/sequencer` as a **new view adjacent to** `/p/{project}` (its AC-9: "Sequencer tab alongside the existing chip strip").

**Decision: leave frozen, do NOT supersede.** This hub epic is complementary:
- The hub's **tab bar (§2) is the home the sequencer's AC-9 tab was always meant to dock into.** Unfreezing ka-0000003558 lights up the `Sequencer→` tab (`SequencerEnabled`) with zero rework.
- The hub builds the *list* (Work breakdown); the sequencer builds the *map* (spatial single-primary-select fan-out). They are the two tabs the sequencer plan itself anticipated.
- This hub epic must **NOT** touch the sequencer's frozen child tasks (AC-1..AC-10). It only ensures the tab bar can host a future tab.

---

## 8. Projects ↔ roles, and "roles get the same treatment" (D9)

### 8.1 What a role is today
A "role" (gardener/crew/thread/triage/jira-mention) is the union of scattered parts:
- A **free-form `role` string** on the agent bead → `BOAT_ROLE` (`subscriber/sse.go:475`, `podmanager/manager.go:1243`); unvalidated, may be comma-separated multi-role.
- A **`role:<name>` label** on the agent bead (what advice/config targeting matches — `advice/matching.go`).
- A **`type:role` config bead** (`RoleInfo`, `beadsapi/roles.go:63`), keyed by bead title, carrying pod overrides (image, runtime_class, account, model, secrets, env, claude_config, …).
- Role-scoped **config beads** (claude-settings/hooks/instructions/nudge-prompts) and **advice beads** labeled `role:<name>`.

So roles are *more* first-class than the surface ask assumed — `type:role` + `RoleInfo` + `applyRoleDefaults` already exist. The gaps are **validation, enumeration, and a viewer hub**.

### 8.2 Precedence (verified)
Documented chain **global < project < role < agent**, implemented in `gardenio/controller/cmd/controller/podspec.go`:
- Pod overrides: `ApplyDefaults` → `applyProjectDefaults` → `applyRoleDefaults` (role runs after project, so role wins) → agent metadata last.
- Account: role-pinned account > project account > default (`resolveRoleAccount`); role `account_whitelist` narrows the project's account pool (fail-closed on empty intersection).
- Model: role model supersedes project model, agent supersedes role (`resolveModelOverride` + `applyAgentModelOverride`).
- Config-bead categories: specificity `global=0 < project=1 < role=2 < agent=3`, with inline `claude_config` layers ordered role-inline < project-inline < agent (`gb setup claude`).

### 8.3 Where projects and roles join
`PrewarmedPoolConfig.Roles` (per-project per-role pools), `ChannelRoles`/`ChannelForRole` (project bead maps Slack channel → role), `role:`+`project:` labels co-applied at spawn (`create_gardener.go:228`), advice/config AND-group targeting `(project AND role)`, and the existing **per-project role breakdown** on `/p/{project}`'s agent panel (`agents.go:289`).

### 8.4 The same drift bug exists for roles (verified)
`RoleInfo` reads `fields["model"]` (`roles.go:153`) but the `type:role` schema (`config.go:574`) **omits `model`** — identical class to the project D5 bug. There is also a divergent second `type:role` schema in the controller (`bridge/init.go:316`) that has drifted from the kbeads builtin.

### 8.5 Recommendation
- **Fold role schema-drift into the project P1** — same mechanism, trivial marginal cost: add `model` (and reconcile the divergent controller copy) to `type:role`, and add the same drift-guard test for `RoleInfo`.
- **Make a full role hub a SEPARATE sibling epic** (`/role/{name}`: agents-with-this-role across projects, resolved `type:role` overrides, role-scoped config/advice, role registry + name validation). It mirrors the project hub and reuses the advice-scope `Role:` rendering (`advice/matching.go`). Kept separate so Matt can green-light "roles get the same treatment" independently without blocking the project work.

| Capability | Project (today) | Role (today) | First-class role adds |
|---|---|---|---|
| Typed bead | `type:project` ✓ | `type:role` ✓ (schema incomplete) | complete + unified schema |
| Go model + cache | `ProjectInfo` ✓ | `RoleInfo` ✓ | — |
| Pod overrides | `applyProjectDefaults` ✓ | `applyRoleDefaults` ✓ | — |
| Name dedup guard | ✓ (`projects.go`) | ✗ (last-writer-wins) | add guard |
| Field validation | partial (D5) | **broken for `model`** | declare all fields |
| Registry/enum | implicit (project beads) | none | role registry + ref check |
| Viewer hub | `/p/{project}` (becoming a hub) | **none** | `/role/{name}` hub |

---

## 9. Open questions for the operator

1. **Config editor in v1, or CLI-only?** Hub can ship read-only (P1–P3) + add the in-UI editor (incl. the easy add-secret form) in P4. Want the in-UI editor, or keep project config CLI-only?
2. **Route:** confirm upgrading `/p/{project}` into the hub (list → Work tab) vs minting a new `/project/{name}`. Upgrading preserves every existing link.
3. **Sequencer ka-0000003558:** confirm "leave frozen, dock later" (hub reserves the tab) vs "unfreeze and fold in now."
4. **Per-project config delegation:** `type:project` writes are global-admin-gated and NOT `project_scoped` — no "project admin who edits only their project." Want per-project delegation (a daemon WritePolicy change), or is global-admin-only correct?
5. **Validity badge loudness:** treat K8s-quantity / JSON-shape errors (which the daemon can't enforce) as hard "invalid" (red) or soft "warning" (amber) on the index + hub header?
6. **Roles "same treatment":** green-light the sibling role-hub epic now, or just take the role schema-drift fix (folded into project P1) and defer the role hub?

---

## 10. Phased implementation plan

Each phase is gardener-drivable; each task is crew-sized. P1 is the cheap unblocking core (validation), then backend, then frontend, then editor + index.

### P1 — Validation + schema source-of-truth (no UI)
1. Audit `ProjectInfo` vs `type:project` schema; add the 7 missing fields. (D5)
2. Fold in the role drift fix: add `model` to `type:role`, reconcile the divergent `bridge/init.go` copy. (D9)
3. Promote `session_resume` enum; document the K8s-quantity/JSON-shape fields as editor-validated.
4. Add drift-guard tests: every `ProjectInfo`-read field declared in `type:project`; every `RoleInfo`-read field declared in `type:role`.
5. Make all project JSON-field parse failures log loudly and uniformly.

### P2 — Hub backend / view-model + dependent aggregations
6. `GetProjectBead(name)` (or retain bead ID in `ListProjectBeads`).
7. `projectHubView` + `buildProjectHub` eager assembler (reuse `buildProjectSummary`).
8. `projectValidityReport` health validator (consumes the live TypeConfig).
9. Dependent-object aggregator fragments under `/b/project/{name}/{agents,gardeners,accounts,costs,activity,decisions}`.

### P3 — Hub frontend / template
10. `project_hub.html` with header + tab bar + eager Overview.
11. Wire the 6 lazy sections with HTMX reveal-on-scroll.
12. Preserve the existing list as the `Work` tab (`?view=list`); keep all query params + bulk routes.
13. Golden/render tests: empty project, fully-populated, degraded-daemon.

### P4 — Config editor + index upgrade
14. `viewerCanAdmin` + admin-gated `GET /p/{project}/edit` + `POST /p/{project}/config` (incl. the add-secret form), §5.3 pre-validation, daemon re-validation.
15. Index project cards gain health + config-validity badges; reserve/wire the dark `Sequencer→` tab slot.

---

## 11. Coordinates / do-not-duplicate

- **Single source of truth:** `type:project` / `type:role` in `kbeads/internal/server/config.go:420,574`; validation in `kbeads/internal/model/{type_config,validate_fields}.go`; enforcement `kbeads/internal/server/beads.go:194,971`.
- **Authoritative consumers:** `gardenio/controller/internal/beadsapi/projects.go` (`ProjectInfo`), `roles.go` (`RoleInfo`).
- **Reuse, don't rebuild:** `project_summary.go` (health), `costs.go` (accounts/costs), `coverage_per_project.go` (coverage), `agents.go` (agents + role breakdown), `escalations.go` (decisions), gardener-page eager/lazy skeleton (`gardener_page.go`), gardener editor pattern (`gardener_ops.go`).
- **Frozen, don't touch:** sequencer epic **ka-0000003558** and `gardenio/plans/project-view-epic-sequencer.md`.
- **Auth:** `oidcauth.RoleAdmin` (lvl 3) for project config writes; `viewerCanOperate` precedent at `gardener_ops.go:664`.
