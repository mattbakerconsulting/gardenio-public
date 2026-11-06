# Project-scope hardening: findings and design

Status: findings verified 2026-09-21 against `main` (df8e22c0) and the live
`gardenio` k3s deployment (release `gardenio` rev 31, chart 2026.263.2).
Author: laptop-claude for the operator. Companion plan:
`docs/superpowers/plans/2026-09-21-project-scope-hardening.md`.

This document is the evidence and the decisions. The plan is the work. A crew
executing a plan task reads the task, then the sections of this document the
task cites. **§0 is binding.**

## 0. Decisions (binding)

| # | Decision | Why |
|---|---|---|
| D1 | **Keep `project:` and `BOAT_PROJECT`.** Retiring the env var is a non-goal of this plan. | `project` is the code/deploy scope, not the tenant. The org/tenant research (`gd-UaaOcw9xbl` under epic `gd-8bn6fDcGkv`) proposes `type:org` **beside** project, not replacing it; operator decisions `gd-WAMcx4aJtw`, `gd-nuAZpSdlxg`, `gd-rvU5DwseTp` ratify that model. The live daemon already holds `project:gascity` beads beside `project:gardenio`. Nothing anywhere proposes retiring project. |
| D2 | **`/claude-config` resolves per (project, role), never per a single viewer-wide project.** `viewerProject` (`BOAT_PROJECT` on the viewer) becomes the *default filter* for the page, not the input to resolution. An unset filter shows every project with a project column and a banner that says so. | The daemon is multi-project. A single scope silently hides every other project's beads; an *unset* scope today hides every project-scoped bead (§2). Either way the page lies while claiming it "can never drift from what actually runs" (`cmd/beads-viewer/claude_config.go:8-10`). |
| D3 | **Interim truth fix ships first:** when `viewerProject == ""` and any open `claude-settings` bead carries a `project:` label, the matrix and detail pages render a warning banner with the count of unreflected beads, and the viewer logs at Warn on startup. | Painted-door: one afternoon of work that stops the lie today, independent of the (project, role) rework in D2. |
| D4 | **`kd claim` cross-project guard:** empty agent project → skip the check but print a stderr warning; `GetBead` failure → return the error (fail closed). `--force` unchanged. | Laptop operators legitimately run `kd` with no project (skipping is correct for them); a daemon error is not "no conflict" (`kbeads/cmd/kd/claim.go:252-260` fails open on both today). |
| D5 | **Pool pods re-materialize Claude config after project hydration.** `runSetupClaude` is re-run after `hydrateAssignmentEnv` sets `BOAT_PROJECT`; the ordering is pinned by a test. | `runSetup` (`cmd/gb/init_agent.go:79`) writes `settings.json`/`.mcp.json`/`settings.local.json`/`CLAUDE.md` before `standbyWait` → `hydrateAssignmentEnv` (`init_agent_loop.go:651`, `init_agent.go:588`, `:642-645`) exports the project. Nothing re-runs setup, so a pooled pod's files are permanently project-less while `gb prime` (env read at call time) sees the project. **Latent on this cluster** (no pools deployed; 0 of 575 agent beads carry a pool field) — fix it before pools are, not after. |
| D6 | **Trigger engine refuses to spawn on an empty `trig.Project`** with a logged error naming the trigger bead, *before* writing an agent bead. Belt-and-braces to the daemon's authoring validation (`kbeads/internal/server/runaway_validation.go:275-306`). | Today `bridge/triggers.go` (`:1833-1844`, `:3578`, `:3892`, `:4476`) tolerates an empty project and writes an orphan agent bead; the controller's credential guard (`spawn_credential_guard.go:70-77`) then refuses the pod permanently — loud, but late, and it leaves the orphan. |
| D7 | **One Go resolver in `cmd/gb`:** `resolveProject(flag string) string` = `flag` > `KD_PROJECT` > `BOAT_PROJECT`. `buildSubscriptions` uses it and warns on empty. `BOAT_RIG` is removed. `kd`'s `agentProject` keeps its precedence (`KD_PROJECT` > `BOAT_PROJECT` > `BEADS_AGENT_NAME` prefix) and is documented; no shared cross-module package (YAGNI). | Four resolvers disagree today (§3). `buildSubscriptions` is the one that decides which config beads a pod gets and it ignores `KD_PROJECT` and honours a dead alias. |
| D8 | **`projectCompatible` mirrors `MatchesSubscriptions`:** every `project:` label on a bead must match (AND), not any (OR). | `internal/claudeconfig/resolve.go:247-262` is OR; `internal/advice/matching.go:216-220` is AND. The doc comment on `projectCompatible` claims it "mirrors" the other. |
| D9 | **Out of scope, to be filed as follow-up beads by the plan gardener, not fixed here:** kbeads hooks `Enrich` drift (`kbeads/internal/hooks/handler.go:144-180` vs `internal/advice/matching.go:98-178`); `slack-bridge` `bridge-notifications` first-bead-wins (`cmd/slack-bridge/main.go:1179-1218`); the 9 Rust / 2 shell / 2 Python `BOAT_PROJECT` reads; jira-bridge `BOAT_PROJECTS`; `images/agent/resolve-auth.sh`; the `type:org` primitive. | Adjacent, real, and each a separate change with its own reviewer. |

## 1. What `BOAT_PROJECT` is

A pod-side **cache of the agent bead's `project` field**.

- **Written once.** `internal/podmanager/manager.go:1930-1933` (`IdentityEnvVars`,
  "the single source of truth for these keys") stamps `BOAT_PROJECT: spec.Project`
  into every agent pod for both the k8s and microvm backends. `spec.Project` is
  `event.Project` (`cmd/controller/podspec.go:442-443`), which the subscriber
  reads from the agent bead: `project := bead.Fields["project"]`
  (`internal/subscriber/sse.go:485`). There is no label fallback and no default.
- **Nothing upstream of the pod reads the env var.** The trigger engine
  (`internal/bridge/triggers.go:4476` → `trig.Project` from the trigger bead's
  `project` field), the Slack bridge (`bridge/bot_mentions.go:145,203,278`),
  viewer quick-spawn (`cmd/beads-viewer/quick_spawn.go:168-220`), and account
  routing all read the **bead field**. The one controller-side read
  (`cmd/controller/account_rotation_restart.go:961-982`) reads it off a running
  pod's `PodSpec`, not its own process env.
- **Who writes the bead field:** `gb spawn` (task bead's `project:` label →
  `defaultProject()` env → error, `cmd/gb/spawn.go:184-197,229-260`); the trigger
  engine (trigger bead `Fields["project"]`); `gb gardener start` (`--project` →
  parent epic label/field → env → error, `cmd/gb/gardener.go:350-364`);
  viewer quick-spawn (form → operator pref → 400); Slack bridge. All 14 live
  `type=trigger` beads carry `project: gardenio`.
- **Read back through four resolvers with four precedence rules**, plus raw
  `os.Getenv` sites:

| Resolver | Precedence | Callers |
|---|---|---|
| `cmd/gb/main.go:146-153` `defaultProject()` | `KD_PROJECT` > `BOAT_PROJECT` | 17 |
| `cmd/gb/project.go:25-33` `defaultGBProject()` | `--project` > `KD_PROJECT` > `BOAT_PROJECT` | 9 |
| `kbeads/cmd/kd/project.go:9-14,71-82` `agentProject()` | `KD_PROJECT` > `BOAT_PROJECT` > prefix of `BEADS_AGENT_NAME` | 6 |
| `cmd/gb/setup_config.go:803-809` `buildSubscriptions()` | `BOAT_PROJECT` > `BOAT_RIG` (ignores `KD_PROJECT`) | `setup.go:311`, `prime.go:612`, `prerun.go:225`, `init_agent_idle_ttl.go:100`, `yield_freeze.go:81`, `prime_size_by_source.go:262` |

  Plus `cmd/gb/setup_repos.go:128-131` inline with inverted precedence
  (`BOAT_PROJECT` > `KD_PROJECT`), 15 other raw Go reads, 9 Rust
  (`coop/crates/cli/src/`), 2 shell, 2 Python. Counts are from a full-tree grep
  on 2026-09-21.

- **What it drives**, by category: (a) config/advice-bead scope — which
  `project:`-labelled `claude-settings`/`claude-mcp`/`claude-hooks`/
  `claude-instructions`/`nudge-prompts`/advice beads apply; (b) cross-project
  gates — `kd claim`, `kd create`, `gb prime` auto-assign; (c) stamping —
  escalations, decisions, sessions, account usage; (d) workspace/repo routing —
  `setup_repos.go`, `resolveCoopWorkdir`, `.claude/{skills,agents,commands}`
  symlinks; (e) coopmux registration and account scoping; (f) metrics/log labels;
  (g) CLI defaults.
- **When empty**, the hard-break set is small (`gb spawn` without a labelled
  task, all `gb pool` verbs, `gb gardener start` on an unlabelled epic, the microvm
  entrypoint with a git URL). **Everything else silently degrades**: config-bead
  resolution, repo cloning, cwd selection, extension symlinks, account
  resolution, usage accounting, and `kd claim`'s guard, which fails open.

## 2. The `/claude-config` defect, precisely

1. **The chart reaches for a key that does not exist.**
   `gardenio/helm/gardenio/templates/beads-viewer/deployment.yaml:85-90` sets
   `BOAT_PROJECT` from `.Values.beadsViewer.project | default .Values.global.project`.
   `global.project` is not declared in `values.yaml` (the `global:` block at
   `values.yaml:2039+` mentions "project" only in comments), is absent from
   `values.schema.json`, and appears in no README. `beadsViewer.project`
   defaults to `""` (`values.yaml:10829-10832`). A default `helm template`
   emits **zero** `BOAT_PROJECT`. Nothing in the applied release values
   (`helm get values gardenio`, 245 lines) or `deploy-values/gardenio-k3s.yaml`
   sets a project. No chart test asserts the env in either direction.
2. **It was wired for a metrics label, not for config resolution.** The
   template comment and `values.yaml:10829-10832` cite `ka-0000006752`
   ("labels the `gardenio_operator_activity_total` view counter"). The
   `/claude-config` dependency (`cmd/beads-viewer/server.go:489-495`,
   `claude_config.go:154-161`) was layered on later with no chart change.
3. **Two contradictory meanings of an empty project meet on this page.**
   `DiscoverRoles(ctx, lister, "")` lists roles from every project
   (`internal/claudeconfig/resolve.go:214-215,247-250`: `project == ""` →
   compatible with everything). Then each role is resolved with
   `BuildSubscriptions(role, "", "")` = `[global, role:R]`
   (`settings.go:240-252`), and `MatchesSubscriptions` **rejects any bead whose
   `project:` label is not in that set** (`internal/advice/matching.go:216-220`).
   Every `project:X, role:Y` bead — the documented fleet shape
   (`gardenio/docs/config-inventory.md:211`) — is dropped. Rows render from
   defaults with `LayerCount 0`; the effort-trap detector cannot fire because
   it needs a matched pin (`effective.go:289`).
4. **Tests pin each half in isolation and never the combination.** The only
   viewer test with project-labelled beads calls `SetViewerProject("gardenio")`
   (`cmd/beads-viewer/claude_config_test.go:198-209`). `projectCompatible` has
   no direct test.
5. **Observed live**, 2026-09-21: the beads-viewer pod has no `BOAT_PROJECT`;
   `/claude-config/gardener` renders defaults and ignores `gd-1voySavBGZ`
   (`claude-settings`, `project:gardenio, role:gardener`, `{"ultracode": true,
   "env": {"CLAUDE_CODE_WORKFLOWS": "1"}}`). The **pod write path is correct**:
   `gb setup claude` run with `BOAT_PROJECT=gardenio BOAT_ROLE=gardener` against
   the live daemon produced `settings.json` with `"ultracode": true`,
   `env.CLAUDE_CODE_WORKFLOWS: "1"`, and no `CLAUDE_CODE_EFFORT_LEVEL` key
   (`clearEffortForUltracode` applied).

## 3. The project-scope matching contract (as the code implements it)

From `internal/advice/matching.go:201-255` and `internal/claudeconfig`:

1. Subscriptions are a flat string set; bead labels are normalised with
   `StripGroupPrefix` (`g12:project:x` → `project:x`) for the gate pass.
2. **`project:` is a hard AND requirement.** Every `project:X` label on the bead
   must be in the subscription set (`matching.go:218-220`).
3. **`agent:` is a hard requirement** the same way (`:221-223`).
4. **`role:` is a hard OR gate.** If the bead carries any `role:` label, at
   least one must be subscribed (`:224-233`).
5. Then the group pass: `gN:` labels form AND-groups; every unprefixed label is
   a singleton group; OR across groups (`:239-254`).
6. `global` is not special in code; it matches because every builder puts
   `"global"` in the set (`settings.go:241`, `matching.go:23`, `audit.go:34`,
   `cmd/gb/nudge_prompt.go:410`).
7. **A subscription set with no `project:` entry rejects every project-labelled
   bead.** In `MatchesSubscriptions`, `""` means *in no project*. In
   `projectCompatible`/`DiscoverRoles`, `""` means *all projects*.
8. Layer precedence (`resolve.go:117-130`, `matching.go:277-295,336-349`; ASCII
   `:` < `^` < `~`): `0:global` < `1:project` < `2:role` (multi-role
   `2:NN:role`) < `2^:role-inline` < `2~:project-inline` < `3:agent`; ties
   older-first then bead id. `CategorizeScope` classifies `[project:x, role:y]`
   as **role** scope.

Worked examples, S1 = `[global, role:gardener, project:gardenio]`,
S2 = `[global, role:gardener]`:

| bead labels | S1 | S2 |
|---|---|---|
| `[project:gardenio, role:gardener]` | match | **reject** |
| `[role:gardener]` | match | match |
| `[global]` | match | match |
| `[project:gardenio]` | match | **reject** |

**Every consumer that resolves with an empty project silently drops project
beads**: the six `buildSubscriptions` callers (§1), the viewer, `nudge_prompt.go:400-418`,
`kd advice_diff.go:89`, `wrapup.go:764-766` (uses `BuildAgentSubscriptions`
without `Enrich`). Only the advice path warns (`matching.go:119,173`).

## 4. Vocabulary (as the project has actually defined it)

| Term | Definition | Where | Authz boundary? |
|---|---|---|---|
| **project** | "a `type:project` config bead describing how agents run for a body of work (git repo, image, account, secrets)" (`gardenio/docs/onboarding-new-project-and-role.md:8`); "projects (code scope via project: labels)" (epic `gd-8bn6fDcGkv`) | `type:project` bead; `project:<name>` label; agent-bead `fields.project`; env `BOAT_PROJECT`/`KD_PROJECT`; no DB column | The only read-scope gate, **fail-open**: `scopeListFilter` (`kbeads/internal/server/read_authz_scope.go:44-52`) narrows project-bound tokens and OIDC `allowed_projects`; static/primary tokens and admins are unrestricted. Write side: `allowed_projects` gates OIDC spawns only (`spawn_authz.go:110-115`). |
| **fleet** | `type:fleet` = k8s namespace + RBAC + token. "`Fleet` is an audit-attribution field only … The old per-token read-isolation gate was a YAGNI multi-tenant escape hatch; removed under ka-0000000871 / ka-0000003285" (`kbeads/internal/server/fleet.go:26-32`). Exactly one fleet, `default`, exists live. | `beads.fleet` column; `BEADS_FLEET`; `BEADS_CROSS_FLEET_READ_FLEETS` "deprecated and ignored" (`serve.go:168`) | No (deliberately removed). |
| **bridge** | External-integration token identity (jira/gitlab/slack), formerly "fleet token" (`gd-DVWzbiCl9j`) | `bridgeTokenInfo`, `kd bridge` | Carries a project scope field; not a tenant. |
| **rig** | Dead alias for project. `BOAT_RIG` fallback survives only at `cmd/gb/setup_config.go:804-806` and in `tests/e2e/scripts/test-config-subscriptions.sh:338-355,410-423`; `docs/agent-operations.md:27` still documents a `rig:` subscription that `BuildSubscriptions` never emits. | env only | No. |
| **team** | `type:team`, "the principal kind that taints are owned by, that grants are given to" (taints ADR §8.1); cross-fleet | `type:team` bead | Grant/approver principal, not a tenant. |
| **organization / tenant** | Does not exist in code. Proposed by `gd-UaaOcw9xbl` as `type:org` + `org:` label + a third read-scope gate, seeded as one `gardenio` org, fail-closed once a second exists. | — | Proposed future hard boundary. |

**Live counts (2026-09-21, `kd list --all-types --all-projects --all-fleets`):**
~3057 beads; `project:gardenio` 1722; `project:gascity` 14 (the `k8s-live` role
bead `gd-gi5v71wMMo`, 6 agent beads, 7 session beads); two `type:project` beads
(`gd-Ui9jl00KfA` gardenio, `gd-os5iOF4ovs` gascity). Note `kd list` without
those three flags applies a default project filter and lists only issue-kind
beads — an earlier "261 beads, all gardenio" count was that trap.

**One live inconsistency to carry forward (not fixed here):** role resolution is
not project-scoped while account resolution is (`gd-GDPF1nH95g`:
`ListRoleBeads()` keys by title only; `accounts.go:82 ListAccountBeadsByProject()`
requires the label), so the `project:gascity` label on `k8s-live` is inert for
role lookup.

## 5. Other findings, verified

- **`kd claim` fails open** on empty agent project *and* on a `GetBead` error
  (`kbeads/cmd/kd/claim.go:252-260`). With `gascity` beads in the same daemon
  this is live, not theoretical.
- **Pool ordering** (D5): confirmed by reading `cmd/gb/init_agent.go:63-79`,
  `init_agent_loop.go:651`, `init_agent.go:544-588,611-645`; no caller re-runs
  `runSetupClaude` (`init_agent_setup.go:301`, `agent_start.go:277`,
  `agent_start_k8s.go:98` are all first-run sites).
- **Trigger engine tolerates empty project** (D6) — static; all 14 live
  triggers have a project.
- **Chart**: `global.project` undeclared; `beadsViewer.project` and
  `reproCockpit.project` are the only consumers; `tests/` has no test naming
  `BOAT_PROJECT`, `global.project`, or `beadsViewer.project`.
- **Non-issues checked:** the 500-row listing cap in `resolve.go:90` (1 open
  `type=config` bead live); `DiscoverRoles` alone behaves as documented.
- **Docs that are wrong today:** `docs/agent-operations.md:27` (`rig:`
  subscription); `gardenio/docs/compaction-settings.md:159-161` ("the viewer
  cannot disagree with the merge" — true only with identical subscriptions,
  including project); `gardenio/docs/config-inventory.md:227-233` (precedence
  string omits `2^:` role-inline; points at `cmd/gb/config_resolve.go`, now a
  wrapper); `gardenio/docs/research/spawn-escalated-agent/config-bead-surface.md:67,183,401`
  describes `kd`'s superset `labelsMatchConfig` as the claudeconfig mechanism
  (they are different matchers).

## 6. Non-goals

- Retiring `BOAT_PROJECT` from the pod contract (D1).
- Any change to the daemon's read-scope authz, `allowed_projects`, or fleets.
- The `type:org` primitive.
- Rust/shell/Python `BOAT_PROJECT` readers (D9).
- Changing what a pod *without* `BOAT_PROJECT` resolves to (still global+role);
  the plan makes it **loud**, not different.
