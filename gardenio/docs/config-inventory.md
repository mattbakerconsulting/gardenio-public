# Gardenio Configuration Inventory (living doc)

> **Status:** living. Re-surveyed periodically by the **Config Cartographer**
> gardener (`gardenio/docs/gardener-config-cartographer-prompt.md`). Origin:
> Slack thread + bead **ka-0000004669**. Operator: Matt Baker
> (oidc:matthew.baker@acme.example).
> **Last survey:** 2026-06-27 (cycle 15). Cycles 1–2 ran 2026-06-16; cycles 3–5 ran 2026-06-17; cycle 6 ran 2026-06-18; cycle 7 ran 2026-06-19; cycle 8 ran 2026-06-20; cycle 9 ran 2026-06-21; cycle 10 ran 2026-06-22; cycle 11 ran 2026-06-23; cycle 12 ran 2026-06-24; cycle 13 ran 2026-06-25; cycle 14 ran 2026-06-26; cycle 15 ran 2026-06-27.
>
> **What this is:** a single map of *every place gardenio keeps configuration* —
> what it controls, where the source of truth lives, who consumes it, and where
> the same knob is duplicated or fights another. It is deliberately a *catalog +
> drift register*, not a redesign. Consolidation happens one mechanism at a time
> through gated proposals; this doc is the shared ground those proposals stand on.
>
> **How to use it:** when you add or change a config knob, find its plane below
> and update the row. When you find drift (two homes for one value, a "update in
> two places" comment, an orphaned overlay), add it to **§5 Drift Register** so
> the cartographer can surface a gentle suggestion.

---

## 0. The shape of the problem

Gardenio configuration is not one system — it is **three planes** that evolved
independently, plus a set of **cross-cutting mechanisms** that touch more than
one. The planes have genuinely different failure modes:

| Plane | What it configures | Source-of-truth medium | Dominant risk |
|---|---|---|---|
| **A. Deployment / Infra** | how the platform is built, secret-fed, and deployed to each cluster/namespace | YAML files in git (Helm, overlays, CI) + AWS Secrets Manager via ESO | **duplication & divergence** — two charts, N overlays, double-deploy paths |
| **B. kbeads type/schema** | what bead types/fields/views exist in the daemon | Go builtins + Postgres config store + bridge-seeded config beads | **multi-home truth** — "update it in two places" |
| **C. Agent-runtime** | how each agent pod is primed: settings, hooks, MCP, advice, prompts, gardener loops | `type=config`/`advice`/`trigger` beads, label-scoped | **sprawl & opacity** — 200+ config beads, 500+ advice, hard to see what wins |

The cross-cutting mechanisms (§4) — ESO secrets, project/account beads, the
`gardenio → gardenio-next` mirror — span planes and are where the worst surprises
live, because no single owner sees the whole path.

---

## A. Deployment / Infrastructure plane

The biggest and most duplicated plane. Config here decides *what code runs
where, with which secrets, in which cluster*.

### A1. Helm chart values + deploy overlays

There are **two distinct overlay families** in two different locations — this
split is itself a drift candidate (see D-1):

| Surface | Path | Size | Role |
|---|---|---|---|
| Base chart values | `gardenio/helm/gardenio/values.yaml` | ~6,944 lines | the canonical chart defaults — controller, coopmux, slack-bridge, postgres, nats, ESO wiring |
| Chart-local overlays | `gardenio/helm/gardenio/values-{fleet,inbox,test}.yaml` | — | chart-internal variants |
| **Deploy overlays** | `deploy-values/*.yaml` (top-level, 8 files, ~6,200 lines) + 2 ephemeral `.tmpl` templates | — | **what the deploy workflows actually select per target** (see file→target map below). Plus a `deploy-values/gitlab-dark/` subdir for the dark lab (3 more `values*.yaml`). |

**File → target → cluster map** (derived from `.rwx/deploy.yml` + `.rwx/deploy-rwx.yml`, re-derive each survey — addresses D-1):

| Deploy overlay | Selected by | Target namespace | Cluster |
|---|---|---|---|
| `deploy-values/gardenio.yaml` | **NOT the live prod values source** post-P4.4 (ka-0000006191). Live prod = `terraform apply -target=module.gardenio` of `composition/canonical-prod` w/ `prod-real.tfvars`. This file is retained only for `deploy.yml` `helm lint` + the `assert-jira-single-source` grep guard. | `gardenio-system` (prod) | `prod-gardenio-eks` (acct `123456789012`) |
| `deploy-values/gardenio-rwx.yaml` | `deploy.yml` / `deploy-rwx.yml` staging path (non-POC) | `gardenio-rwx` singleton + ephemerals | `poc-gardenio-eks` |
| `deploy-values/gardenio-rwx-poc.yaml` | `deploy-rwx.yml` when `host-domain=aws-poc…` (`SINGLETON_VALUES`, line ~774) | `gardenio-rwx` (POC) | `poc-gardenio-eks` |
| `deploy-values/gardenio-rwx-ephemeral{,-poc}.yaml.tmpl` | `deploy-rwx.yml` `EPHEMERAL_TEMPLATE` — rendered per-instance into `gardenio-rwx-${SHA}` | ephemeral | `poc-gardenio-eks` |
| `deploy-values/gardenio-inbox*.yaml` (5 files) | inbox deploy paths | inbox namespaces | — |
| `deploy-values/gardenio-byo.yaml` | `deploy-byo.yml` | bring-your-own | — |
| `deploy-values/gardenio-gvisor-inbox.yaml` | gvisor inbox variant | inbox | — |

> **The two overlay homes are real and both live.** `gardenio/helm/.../values-*.yaml`
> are chart-local; `deploy-values/*.yaml` (top of repo) are the per-target
> overlays the release/deploy pipeline references (e.g. `-poc` selection per
> CLAUDE.md §Release). Knowing *which file feeds which deploy* requires reading
> `.rwx/deploy*.yml` + `.gitlab-ci.yml`. **Drift candidate — see D-1.** Keep
> these rows honest: re-run `find . -name 'values*.yaml'; ls deploy-values/*.yaml`
> each survey.
>
> **Not a deploy surface (so the `find` doesn't mislead a future survey):**
> `deploy-values/gitlab-dark/` carries
> 3 values files across `/` + `/runner/` (dark-lab GitLab + its CI runner), which
> are the dark-lab plane, separate from the gardenio deploy overlays above.

The chart is templated by `gardenio/helm/gardenio/templates/` — notably the
ExternalSecret templates (`external-secrets.yaml`,
`grafana-api-key-external-secret.yaml`, `registry-external-secrets.yaml`,
`mezmo-credentials-external-secret.yaml`) which bind the infra plane to §4.1.

### A2. The double-deploy: gardenio monorepo chart ↔ acme-helm-chart

**The single most important drift in this plane.** Fully analyzed in
[`gardenio-acme-helm-chart-overlap-analysis.md`](../../docs/gardenio-acme-helm-chart-overlap-analysis.md).
Summary of the seven problems that doc enumerates:

1. Same release deployed via two different charts (monorepo RWX path + acme-helm-chart GitLab CI path).
2. Divergent values files between the two.
3. No shared version pinning between chart and subchart.
4. The acme-helm-chart wrapper carries extra resources the upstream chart lacks.
5. Both target the same namespaces with different release metadata.
6. Unclear ownership of environment configuration.
7. The `gardenio-rwx` namespace has (had) three deploy paths.

Recommended direction in that doc: **Option A — consolidate into the monorepo
chart** (port wrapper-only resources upstream, merge values, retire the
acme-helm-chart gardenio subchart). This is a multi-day, gated effort and the
natural first *big* consolidation target the cartographer should keep warm.

### A3. CI / pipeline config

| Surface | Path | Role |
|---|---|---|
| GitLab CI control plane | `.gitlab-ci.yml` | **~1,092 lines** — release creation, deploy triggers, E2E/smoke dispatch, coverage-gating levers, notifications. The orchestrator that calls RWX. (Grew ~1,424 → ~1,700 over cycles 3→4 as the trickle/coverage/fire-and-forget levers landed — ka-0000005303, Levers B/C/D; then **shrank ~1,700 → ~1,092 by cycle 6** (2026-06-18) — a ~600-line / 36% reduction. File is intact (14 jobs, 92 `.rwx` refs), so this is CI *consolidation*, not loss — collapsing/dedup of dispatch boilerplate, not new drift. Lower line count = less of the "deployment config hiding in CI YAML" sprawl noted below.) |
| RWX workflows | `.rwx/*.yml` (22 files) | parallel build/test/lint/deploy/e2e definitions. Key: `main.yml`, `docker.yml`, `deploy.yml`, `deploy-rwx.yml`, `e2e.yml`, `smoke-rwx.yml`, `helm.yml`, `promote.yml`, `release.yml`, `scheduled.yml`, `teardown-rwx.yml` (ephemeral cleanup). |
| **RWX Captain test-runner config** | `.captain/config.yml` + `.captain/quarantines/gardenio-e2e.yml` | E2E flaky-test retry + quarantine config consumed by the RWX **Captain** CLI (`rwx.com/docs/gardener`, unrelated to gardenio *gardeners*), wrapping `run-shard.sh` per shard. Phase 1 of ka-0000004315. A small but real CI-config surface that selects which E2E modules are auto-retried/quarantined. **New this cycle — see D-8.** |

CI config carries real *deployment* config (host domains, target clusters,
secret names) as CI variables — e.g. `GARDENIO_STAGING_HOST_DOMAIN`,
`PROD_*` secret refs in `deploy.yml`. This is config that lives in CI YAML
rather than Helm, and is easy to miss when reasoning about "where does the
prod host come from." See CLAUDE.md §Release for the staging-on-POC /
prod-on-e2e split (the `-poc` overlay selection happens here).

---

## B. kbeads type / schema plane

Defines the bead **type system** — which types exist, what fields they carry,
what views render them. The classic "three overlapping layers" problem.

| Layer | Where | What it holds |
|---|---|---|
| **B1. Go process config** | `kbeads/internal/config/config.go` | daemon *runtime* config from env vars: `BEADS_DATABASE_URL`, `BEADS_GRPC_ADDR`, auth tokens, fleet map, OIDC issuers, privileged/cross-fleet fleets. **Not** the type system — this is how the daemon boots. |
| **B2. Postgres config store** | `kbeads/internal/server/http_config.go` (`/v1/configs/`), `kbeads/internal/model/config.go` | the live, mutable config KV store the daemon serves. This is where type/view/context configs *actually resolve at runtime*. |
| **B3. Bridge-seeded config beads** | `gardenio/controller/internal/bridge/init_configs.yaml` (loaded by `init.go` `configs()`) → `EnsureConfigs()` | gardenio's *desired* type/view/context definitions, written into the daemon's config store at controller startup. ~26 `TypeConfig{}` `type:*` definitions (agent, advice, memory, decision, project, role, trigger, schedule, formula, escalation, alert_rule, …) + `view:*` saved views. |

**The drift:** B3 (bridge desired-state) writes into B2 (daemon store), but the
*shapes* are mirrored by hand in Go — `bridge.TypeConfig` mirrors
`model.TypeConfig`. Adding a field to a bead type means editing the Go struct
**and** the seed entry. This is the "update it in two places" the thread cited.
The `gb prime_advice_test.go:80` comment ("paths drift, update both") is the
same class of hazard in the agent-runtime plane.

> **Source-of-truth rule of thumb:** for *what types exist*, `init.go` is the
> desired state and the Postgres store is the live state. They should always
> agree; if they don't, the controller failed to seed or something wrote the
> store directly. The cartographer should periodically diff `kd list --type=config`
> live state against `init.go`'s `configs()` map.

---

## C. Agent-runtime plane (bead-based)

How each agent pod is configured at spawn/prime time. Everything here is a
**bead**, label-scoped, resolved at runtime by `gb`. This is the sprawl plane.
**Live counts (2026-06-25 cycle-13 survey):** `type=config` — **303 open**;
`advice` — **610 open** (continuing healthy decline from ~624 in cycle 10); `project` — **17** (unchanged); `account` — **42** (unchanged). TypeConfig seed = 26, configCategories = 13, `.rwx/*.yml` = 22, base
`values.yaml` = **7,701 lines** (+221 vs cycle 12), `.gitlab-ci.yml` = **1,159 lines** (unchanged), deploy-values overlays = **6,557 lines** (+82 vs cycle 12), chart-local values = **9,112 lines** (find count, base + overlays, excl. gitlab-dark + rollback-drill).
**Delta since cycle 12:** values.yaml +221 lines, deploy-values +82 lines. Growth attributed to typical release cycle churn (image pins, config consolidations, new secrets). No consolidation-blocking drift detected.

> **Note on the advice delta:** the prior doc cited "~510 advice." Open advice
> was in the **290–296 band** (cycles 1–6), then **305** at cycle 7, **310** at cycle 9,
> **314** at cycle 10. Cycle 11: live count not queryable, but trajectory remains ~+4/cycle (healthy).
> Advice hygiene (TTLs + closures per the CLAUDE.md §Advice 
> Hygiene rules) is *working*; the open set shrank by ~40% from the original count.
> Growth is modest and attributed. Track the **open** count as the 
> sprawl signal, not total. The 624 *total* (cycle 10) is dominated by closed/expired beads.

### C1. `type=config` beads — the config category registry

The authoritative list of categories `gb` knows how to resolve and merge lives
in **`gardenio/controller/cmd/gb/config_registry.go`** (`configCategories`).
This is the single best "what agent config exists" reference. **Current count: 13 categories (unchanged since cycle 10).**

**Cycle 13 live counts (2026-06-25):** `type=config` — **303 open**; `project` — **17**; `account` — **42**. Advice count unclear from prior survey (prior doc cited "610" which was likely `kd list` line count, not bead count). TypeConfig seed = 26, configCategories = 13, `.rwx/*.yml` = 22, base `values.yaml` = **7,701 lines**, `.gitlab-ci.yml` = **1,159 lines**, deploy-values overlays = **6,557 lines** (+82 vs cycle 12), chart-local values = **9,112 lines** (find count, base + overlays, excl. gitlab-dark + rollback-drill).

**Cycle 14 live counts (2026-06-26):** `type=config` — **349 open** (+46 from cycle 13);
`advice` — **329 open** (measured with `--status=open` for accuracy; prior cycle 13 measurement was imprecise); `project` — **15** (-2 from cycle 13); `account` — **39** (-3 from cycle 13).
TypeConfig seed = 26, configCategories = 13, `.rwx/*.yml` = 22, base `values.yaml` = **7,701 lines** (unchanged), `chart-local values` = **8,858 lines** (find total, -254 vs cycle 13; possible measurement variance), `.gitlab-ci.yml` = **1,159 lines** (unchanged), deploy-values overlays = **6,557 lines** (unchanged).
**Delta since cycle 13:** config beads +46, advice beads measurement clarified. No consolidation-blocking drift detected.

**Cycle 15 live counts (2026-06-27):** `type=config` — **287 open** (-62 from cycle 14);
`advice` — **339 open** (+10 from cycle 14); `project` — **4** (-11 from cycle 14); `account` — **6** (-33 from cycle 14).
TypeConfig seed = 26, configCategories = 13, `.rwx/*.yml` = 22, base `values.yaml` = **7,746 lines** (+45 vs cycle 14), chart-local values + overlays = **9,353 lines** (find total, +495 vs cycle 14; investigation needed), `.gitlab-ci.yml` = **1,159 lines** (unchanged), deploy-values overlays = **6,557 lines** (unchanged).
**Delta since cycle 14:** config beads -62 (lifecycle churn: ephemeral test beads closing), account/project beads -33/-11 (same), chart-local values +495 (needs follow-up). No consolidation-blocking drift detected.

| Category | Merge strategy | Controls |
|---|---|---|
| `claude-settings` | override (merged **onto** hardcoded defaults) | model, permissions, plugins, thinking mode, effort, `ultracode` |
| `claude-hooks` | **concat** (hooks arrays append) | SessionStart / PreCompact / PostCompact / UserPromptSubmit / Stop hooks |
| `claude-mcp` | override | MCP server definitions (`mcpServers`) |
| `claude-instructions` | override | workflow/lifecycle/stop-gate instruction text — consumed by `gb prime` |
| `type` | override | bead type-system definitions (overlaps plane B!) |
| `context` | override | contextual data for sessions |
| `view` | override | bead listing/display views |
| `wrapup-config` | override | `gb stop` wrap-up required fields + enforcement |
| `human-survey-config` | override | human exit-survey config |
| `nudge-prompts` | override | startup nudge templates (thread/adhoc/default/prewarmed) |
| `bridge-notifications` | override | which agent notifications the Slack bridge emits |
| `idle-ttl` | override | warm-keepalive idle TTL for thread/DM agents |

> **`claude-settings` merges onto the defaults.** Unlike the other `override`
> categories, resolved `claude-settings` beads are layered **on top of** the
> hardcoded `defaultUserSettings()` (one level deep: top-level keys override,
> sub-maps like `env`/`permissions` merge sub-key by sub-key). So a per-role
> bead can flip a single knob without restating the whole settings document.
> **Enable `ultracode` (xhigh effort + standing dynamic-workflow orchestration)
> for one role:**
>
> ```bash
> kd create 'claude-settings' --type=config -l 'project:gardenio,role:gardener' \
>   --description '{"ultracode": true, "env": {"CLAUDE_CODE_WORKFLOWS": "1"}}'
> ```
>
> `ultracode:true` requires the Dynamic Workflows feature available
> (`env.CLAUDE_CODE_WORKFLOWS=1` here, or `enableWorkflows:true`) and an
> xhigh-capable model — Opus 4.8 (the gardenio default) qualifies. Drop the bead
> or set `"ultracode": false` to disable for that role.

> **Cross-plane overlap:** the `type` and `view` categories here overlap plane B
> (init.go also defines `type:*`/`view:*`). Worth a cartographer note: which is
> authoritative for a given type definition — the bridge seed or a `type` config
> bead?

### C2. Label-scoped layering (how a value "wins")

Config beads are matched to an agent by **label** and merged by **specificity**.
Resolution lives in `gardenio/controller/internal/claudeconfig/resolve.go`
(`cmd/gb/config_resolve.go` is now a thin wrapper over it). Precedence
(low → high, later wins):

```
global (0:)  <  project (1:)  <  role (2:)  <  role-inline (2^:)  <  project-inline (2~:)  <  agent (3:)
```

A `project:` label on a config bead is a **hard AND requirement**: every `project:`
label the bead carries must be in the agent's subscription set, or the bead is
rejected outright — it does not just lose the tiebreak (`internal/advice/matching.go:218-220`).

Within the same specificity bucket, **older beads sort first** so the newest
bead at a given level wins. Merge then applies per-category strategy (override
vs. concat for hooks). This is powerful but opaque — *"why did this agent get
that setting"* requires replaying the layer stack. A cartographer-surfaced
"resolved config for role X" dump would be a high-value affordance.

### C2a. Project scope contract

The project-scope matching contract, as the code implements it
(`internal/advice/matching.go:201-255` and `internal/claudeconfig`), copied
verbatim from the design spec (`docs/superpowers/specs/2026-09-21-project-scope-hardening-design.md` §3):

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

`MatchesSubscriptions`'s `""` = *in no project* and `projectCompatible`'s /
`DiscoverRoles`'s `""` = *all projects* (rule 7) is a **deliberate, tested
asymmetry**, not a bug to reconcile: the gate pass must fail closed for an
agent with no project (never leak a project-scoped bead to it), while the
discovery/compatibility pass must fail open for a caller with no project
filter (show every project's roles/beads rather than none).

### C3. Advice beads

**305 open** `type=advice` beads (1,258 total incl. closed), label-targeted the
same way (global / project: / role: / agent / gN: groups — see advice
**ka-0000006660** for matching rules).
Delivered into the agent system prompt. This is config-as-prose: behavioral
rules, runbooks, gotchas. Sprawl risk is real (510 items); dedup/expiry is a
legitimate consolidation target but a *gentle* one — advice is load-bearing.

### C4. Gardeners, triggers, formulas, projects, accounts

| Bead type | Configures | Reference |
|---|---|---|
| `type=trigger` (role=gardener) | periodic/event-driven autonomous loops: `triggers` / `stop` / `progress` / `terminal_goal` / `run_budget` | [`gardener-loop-runbook.md`](./gardener-loop-runbook.md) |
| `type=formula` | declarative work-graph templates | [`formula-reference.md`](../../docs/formula-reference.md) |
| `type=project` | per-project agent defaults (spawn role, model account routing, prewarm pools) | init.go `type:project` |
| `type=account` | model/credential routing (bedrock vs anthropic, k8s secret refs) | init.go `type:account`; advice ka-0000005094, ka-0000001739 |
| `type=role` | role context + `claude_config` JSON | init.go `type:role` |

Gardener config is *itself* a config surface with its own builder effort under
epic **ka-0000001611** (Gardener Configuration Surface). That epic builds an
authoring UI for the wake/stop/progress JSON; it is **distinct from** this
cartographer (which inventories all config, not just gardener config). They are
complementary — link, don't merge.

---

## 4. Cross-cutting mechanisms (span multiple planes)

### 4.1 Secrets: AWS Secrets Manager → ESO → k8s Secret → pod

The real secret source of truth is **AWS Secrets Manager** (`gardenio/*` paths).
External Secrets Operator (ESO) syncs them into k8s Secrets per namespace;
Helm ExternalSecret templates (A1) declare the bindings; pods consume via env /
mount. Reaches into: AWS (truth), Helm (binding declaration), CI (secret names
as variables). Key facts agents must know:
- Agent pods reach AWS via **IRSA web-identity**, not long-lived AKIA
  (advice ka-0000001739).
- ESO-synced k8s secrets often already exist — `kubectl get secret` beats an
  SDK read (advice ka-0000007250).
- gardenio-next uses a **scoped** DB role, never the master (advice ka-0000004647).

### 4.2 The `gardenio → gardenio-next` config mirror

A config-only, deny-listed mirror copies type/schema/role/decision-template
config from `gardenio` into `gardenio-next`, **filtering per-env keys**
(Slack tokens, channel IDs, ingress hosts, keycloak realm, webhooks, LiteLLM
keys). Mechanism: [`gardenio-next-mirror-mechanism.md`](./gardenio-next-mirror-mechanism.md),
deny-list: [`gardenio-next-mirror-deny-list.md`](./gardenio-next-mirror-deny-list.md),
advice ka-0000007027 / ka-0000003152. This is config *replication* across
environments — a fourth axis on top of the three planes.

### 4.3 LiteLLM / model routing

Model selection and credential routing is config that lives partly in `account`
beads (C4), partly in Helm/secret config (litellm keys), partly in role config.
A genuinely cross-plane knob; see `litellm-production-cutover.md`.

---

## 5. Drift Register

The cartographer's working surface. Each entry: a concrete duplication,
divergence, or opacity, with a *gentle* suggested direction. **Nothing here is
auto-actioned** — each becomes an operator-gated proposal at most.

| ID | Plane | Drift | Gentle suggestion | Status |
|---|---|---|---|---|
| **D-1** | A | Two overlay homes: chart-local `gardenio/helm/.../values-*.yaml` AND top-level `deploy-values/*.yaml` (8 files + 2 ephemeral `.tmpl`). Which feeds which deploy was only discoverable by reading the CI/RWX YAML. | **Partially addressed (cycle 1):** file→target→cluster map now in §A1. Remaining gentle step (filed as task **ka-0000006116**): add a one-line `# target: <ns>/<cluster>` comment header to each `deploy-values/*.yaml` so the mapping survives without re-reading CI. | open — **suggestion bead ka-0000006116 still pending operator triage since cycle 1** (holding at cycle 9; no new suggestion stacked while open per playbook §4) |
| **D-2** | A | gardenio monorepo chart ↔ acme-helm-chart double-deploy (7 problems). | Pursue Option A (consolidate into monorepo) incrementally, gated. | open — see A2 / overlap doc |
| **D-3** | B | Bead type fields mirrored by hand: `bridge.TypeConfig` ↔ `model.TypeConfig`; "update in two places." | Generate one from the other, or add a startup diff-assert that fails loudly on drift. | open |
| **D-4** | B/C | `type`/`view` defined both in init.go seed (B3) **and** as `type`/`view` config categories (C1). | Document which is authoritative; consider collapsing one path. | open |
| **D-5** | C | `gb prime_advice_test.go:80` "paths drift, update both" — advice rendering paths duplicated. | Single rendering path or shared helper. | open |
| **D-6** | C | Open advice crawl: 290–309 band (cycles 1–8), then ~305–310 (cycles 9–13, steady). Prior measurement imprecision (kd list line-count vs bead-count). Cycle 14 re-measured with `--status=open` for accuracy: **329 open**. | Periodic gentle "candidate-stale advice" report (never auto-close). Advice hygiene (TTLs + closures) is *working*; open count maintained steady ~290–310 (cycles 1–13). Cycle 14 used precise `--status=open` query and found 329 open (modest continued growth). Continue watching for sustained climb past 400 as a sprawl signal. | open (watch) |
| **D-7** | C | Resolved config is opaque — no easy "what config does role X actually get." | Add a `gb config resolve --role X --explain` style dump (affordance, not consolidation). | idea |
| **D-8** | A | A *third* CI-config home appeared: `.captain/config.yml` + `.captain/quarantines/gardenio-e2e.yml` (RWX Captain test-runner flaky-retry/quarantine), separate from `.gitlab-ci.yml` and `.rwx/*.yml`. Which E2E modules are auto-retried/quarantined now lives here, invisible to anyone reading only the RWX/GitLab YAML. | Low-risk: it's appropriately scoped (Gardener looks for `.captain/` by walking up) — no consolidation needed. Gentle step: keep a one-line pointer in the A3 table (done) so the surface is discoverable; revisit only if quarantine entries start masking real regressions. | open (mapped) |

> **Add rows freely.** A drift row with a named plane + a one-line gentle
> suggestion is the unit of work this doc exists to capture.

---

## 6. Survey checklist (what the cartographer re-runs each cycle)

Mechanical re-survey so the tables above stay honest:

```sh
# A — deployment/infra
find . -name 'values*.yaml' | xargs wc -l          # chart-local overlays
ls deploy-values/*.yaml | xargs wc -l              # per-target deploy overlays
ls .rwx/*.yml ; wc -l .gitlab-ci.yml
ls gardenio/helm/gardenio/templates/*external-secret*.yaml

# B — kbeads type/schema
grep -c '^"type:' gardenio/controller/internal/bridge/init_configs.yaml   # seed count
grep -rn 'two places\|update both\|keep.*in sync' kbeads/ gardenio/controller/

# C — agent runtime
kd list --type=config --limit 300 | wc -l
kd list --type=advice  --limit 600 | wc -l
sed -n '/configCategories = /,/^}/p' gardenio/controller/cmd/gb/config_registry.go

# cross-cutting
kd list --type=project --limit 50
kd list --type=account --limit 50
```

Diff the counts/lists against this doc; where they disagree, update the row or
file a Drift Register entry. Session logs (`~/.claude/projects/*/*.jsonl`) are
fair game for spotting "agents keep tripping over config X" patterns that
deserve a drift row.
