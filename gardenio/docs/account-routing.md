# Account routing — how an agent's LLM account is chosen

> **Scope.** This document describes, layer by layer, how the gardenio controller
> decides which **account** (and therefore which OAuth token + LiteLLM virtual
> key + base URL) a new agent pod is credentialed against at spawn time. It is
> the authoritative reference for the account-selection pipeline in
> `gardenio/controller/cmd/controller/podspec.go` (`applyAccountConfig`) and
> `account_spread.go` (`selectAccount`).
>
> **Status (2026-08-12).** The redundant `DefaultAccount` /
> `resolveNonExhaustedAccount` fallback chain this doc set out to deprecate has
> now been **retired** (epic ka-0000007694). The pipeline collapsed to *one
> selection function over one health predicate* in Phase 2 (ka-0000004094); the
> functions were renamed for intent in Phase 4 (ka-0000001577):
> `selectSpreadAccount` → `selectAccount` (no more spread-vs-fallback duality)
> and `accountNeedsRedirect` → `isHealthyAuthorized` (inverted — *the* health
> predicate). §3 below describes the collapsed pipeline as it runs today; §6–§7
> preserve the original proposal and the phased history that got us here.
> Origin: Matt Baker, follow-up to the idle-agent p0 ka-0000003769 / decision
> ka-0000003293 — *"we should also plan to deprecate the fallback chain because
> what even is that?"*, task ka-0000007193.

---

## 1. TL;DR

- Account selection is now **one selection function over one health predicate**,
  with role-pins and the exclusive-account (Bedrock) partition as the only
  carve-outs. `applyAccountConfig` resolves an account through at most three
  paths — role-pin, `selectAccount` (least-loaded spread over the healthy
  authorized pool), and the opus model-group downgrade — and otherwise blocks the
  spawn with one explicit, observable "no healthy account" reason. This is the
  collapsed shape the original §6 proposal called for; §7 records the phased
  path that got here.
- The **health predicate** is the load-bearing part and is well-factored:
  `isHealthyAuthorized` (formerly `accountNeedsRedirect`, inverted) is the single
  "is this account fit to bake into a pod?" test, and `selectAccount` filters its
  candidate pool through it — so a spread pick is already known-healthy.
- The **fallback chain** (`resolveNonExhaustedAccount` + `DefaultAccount`) has
  been **removed** (Phase 2, ka-0000004094). It was a *second* mechanism that
  re-implemented health gating and redirection **after** the spread selector
  already picked a healthy account — a no-op on the common path and redundant
  with the spread pool + pool-exhaustion escape hatch on the edge path. The
  per-account `fallback` field survives **only** for the *reactive* mid-session
  failover path (`auth_failover.go`), which is out of scope.
- **`DEFAULT_ACCOUNT` was unset in production** for the life of this code — every
  branch that read `cfg.DefaultAccount` was effectively dead on prod. Phase 3
  (ka-0000007959, §7) removed the config, env, helm value, and metric enum
  member outright, so it is now gone entirely.

---

## 2. The account bead (data model)

An account is a `type=account` config bead. The controller caches each one as a
`config.AccountCacheEntry` (`gardenio/controller/internal/config/config.go`). The
fields that participate in **selection** are:

| Field (bead / cache) | Meaning for selection |
|---|---|
| `labels` → `project:<name>` | Membership in a project's account pool. `selectAccount` enumerates candidates by this label. |
| `status` (`Status`) | Rotation lifecycle: `active`/`""` = usable, `exhausted` / `cooldown` = excluded. Set by the rate-limit probe. |
| `throttle_status` (`ThrottleStatus`) | Live rate-limit signal: `""` healthy, `allowed_warning` de-prioritized, `rejected` excluded. |
| `base_url` (`BaseURL`) + `litellm_key` (`LiteLLMKey`) | A `base_url`-set account with **no** `litellm_key` 401s every agent → not healthy-authorized (excluded). |
| `overage_disabled_reason` (`OverageDisabledReason`) | A *genuinely blocking* billing reason excludes the account. `out_of_credits` / `org_spend_cap_reached` are **benign** for our subscription accounts (see `overageReasonBlocks`). |
| `max_budget` + `total_cost_usd` − `spend_at_last_reset` | Soft dollar cap. `accountOverBudget` trips *before* the provider's hard 429. |
| `probe_auth_status` (`ProbeAuthStatus`) | `rejected` = terminally auth-dead (401/403 from the read-only probe) → excluded even when pool-healthy. |
| `exclusive_to_role` (`ExclusiveToRole`) | Reserves the account to one role (data-driven Bedrock partition). Never spilled onto or offered to a non-owner role. |
| `fallback` (`Fallback`) | Per-account pointer to the next account to try when this one is unfit. **No longer consulted at spawn time** (the spread pool subsumes it); survives only on the reactive mid-session failover path (`auth_failover.go`, walked transitively + cycle-guarded). |
| `spawn_weight`, `model_group_limits`, utilization windows | Bias / gate the spread *ranking* (not membership). |

---

## 3. How an account is chosen today (`applyAccountConfig`)

The entry point is `applyAccountConfig(cfg, spec)` in `podspec.go`. Since the
Phase 2/4 collapse (ka-0000004094, ka-0000001577) it resolves an account through
at most **three selection paths**, and if none yields one, takes a single
explicit terminal outcome. Precedence, top to bottom:

```
┌─ PATH 1 ── role pin ───────────────────────────────────────────────┐
│  resolveRoleAccount(cfg, spec.Role)                                 │
│    RoleCache[<role>].Account  (e.g. fsm-triage → litellm-bedrock)   │
│    First non-empty match across the comma-split role wins.          │
│    → if set, this account is used verbatim; NO spread.              │
│  (also: roleOwnsExclusiveAccount → its reserved account, unchanged) │
└────────────────────────────────────────────────────────────────────┘
        │ (role has no pin)
        ▼
┌─ PATH 2 ── the ONE selection: least-loaded over the healthy pool ──┐
│  selectAccount(cfg, project, spec, liveCounts)                      │
│    Candidate pool = accounts labeled project:<name> that pass       │
│    isHealthyAuthorized (§4) AND the role whitelist AND the          │
│    served-model / model-group gates.                                │
│    Pick lowest EFFECTIVE load (live pods + penalties − bonuses).    │
│    → returns "" only if the healthy pool is empty.                  │
└────────────────────────────────────────────────────────────────────┘
        │ (empty healthy pool)
        ▼
┌─ PATH 3 ── opus model-group downgrade ─────────────────────────────┐
│  fallbackModelGroupAccount(cfg, spec, liveCounts)                   │
│    The requested model group is fully gated, but an opus-capable    │
│    account has capacity → land there + downgrade the pod's model.   │
└────────────────────────────────────────────────────────────────────┘
        │ (no opus fallback → account name is "")
        ▼
   ── explicit terminal outcome (no silent bake) ──
   NO healthy authorized account resolved. Rather than fall through to a
   possibly-unhealthy project-cache / DefaultAccount (both retired), take
   one of two exits:
     • credentialed accounts exist but all gated → spec.SpawnBlocked =
       true + explicit SpawnBlockReason (account count + earliest pool
       reset_at) + loud WARN (ka-0000007343). TRANSIENT: reconciler
       retries next sync once a window resets.
     • no account beads at all → benign Debug (direct-provider auth).
        │  (a name DID resolve, via a role pin)
        ▼
   ── pool-exhaustion escape hatch (ka-0000002560) ──
   A role-pinned account that is capacity-exhausted and not exclusive →
   spec.SpawnBlocked = true (spread already excludes exhausted accounts,
   so only a role pin now reaches here with one). An exhausted EXCLUSIVE
   account is exempt and surfaces failure on its reserved role.
        │
        ▼
   ── role account_whitelist final gate (fail-closed, ka-0000003061) ──
   If the resolved account is not on the role's whitelist → bake NO
   credential (BOAT_ACCOUNT stays unset) rather than credential a
   forbidden pool.
        │
        ▼
   spec.ResolvedAccount = accountName   (drives env baking + live-count bump)
```

### The paths in prose

1. **Role pin (`resolveRoleAccount`).** `RoleCache[<role>].Account`. A role can
   be pinned to a specific account regardless of project — `fsm-triage →
   litellm-bedrock`. Wins over everything; never spread. `role.Account` comes
   from the `type=role` config bead's `account` field. A role that *owns* an
   exclusive account (`roleOwnsExclusiveAccount`) resolves to its reserved
   account by the same precedence.

2. **Least-loaded selection (`selectAccount`).** The primary — and now *only* —
   selection mechanism for ordinary project agents. Enumerates
   `project:<name>`-labeled accounts, filters to the healthy authorized pool
   (§4), and picks the lowest **effective load** (`live_pods + warning_penalty −
   capacity_reset_bonus − spawn_weight`, with an extra absolute-headroom bias for
   long-lived gardener/plan-gardener roles). Returns `""` only when the healthy
   pool is empty. This is the controller-side analogue of coopmux's
   `pick_least_loaded` (coopmux auto-assign is disabled,
   `COOPMUX_DISABLE_AUTO_ASSIGN=1`). It was `selectSpreadAccount` before Phase 4
   — renamed because there is no longer a "spread vs. fallback" duality to
   distinguish it from.

3. **Opus model-group downgrade (`fallbackModelGroupAccount`).** A narrow,
   well-scoped last selection path: when the requested model group is fully gated
   but an opus-capable account has capacity, land there and downgrade the pod's
   model. Unchanged by the collapse.

4. **No healthy account → one explicit outcome.** When all three paths decline,
   the account name is `""` and `applyAccountConfig` does **not** fall through to
   a maybe-unhealthy account. If the project has credentialed accounts that are
   all currently gated, it sets `SpawnBlocked` + an explicit `SpawnBlockReason`
   (naming the account count and earliest pool `throttle_reset_at`) and emits the
   loud no-credential WARN (ka-0000007343) — a *transient* refusal the reconciler
   retries once a window resets. If the project has no account beads at all, it
   emits a benign Debug (the project uses direct-provider auth). This replaces
   the old project-cache / `DefaultAccount` / `resolveNonExhaustedAccount`
   fall-through entirely (Phase 2, ka-0000004094).

**Two safety gates run after selection regardless of which path won:** the
pool-exhaustion escape hatch (blocks the spawn transiently if a role-pinned final
account is capacity-exhausted) and the role account-whitelist fail-closed gate.

---

## 4. The health predicates

There are **two** predicates, deliberately different in width:

### `isHealthyAuthorized(acct)` — "fit to bake into a pod"

The broad gate (formerly `accountNeedsRedirect`; **inverted** in Phase 4 so the
name reads as the health assertion it makes — `selectAccount` includes an account
iff `isHealthyAuthorized`). An account is **unfit** (`isHealthyAuthorized` returns
false) if **any** of:

- `Status == "exhausted"` — probe flipped the budget/rate-limit lifecycle.
- `BaseURL != "" && LiteLLMKey == ""` — keyless LiteLLM proxy → 401 every agent
  (the ka-0000003769 idle-agent buildup).
- `overageReasonBlocks(OverageDisabledReason)` — a genuinely-blocking billing
  reason (**not** the benign `out_of_credits` / `org_spend_cap_reached`).
- `accountOverBudget(acct)` — controller-synced spend since reset ≥ `max_budget`.
- `ProbeAuthStatus == "rejected"` — 401/403 from the read-only probe; terminally
  auth-dead, never heals on a rate-limit window (ka-0000002085).

`selectAccount` applies exactly this gate to its candidate pool, so **a spread
pick is already known-healthy**. There is no second post-hoc application anymore:
the `resolveNonExhaustedAccount` redirect that used to re-run the gate on a final
account was removed in Phase 2.

### `accountCapacityExhausted(acct)` — "capacity spent (transient)"

A strict **subset** used only by the pool-exhaustion escape hatch: `exhausted`
status, blocking overage, or over-budget. It **excludes** the keyless case
(that's a misconfiguration the WARN path surfaces, not a capacity stall that
heals on window reset). When *every* account a project could resolve to is
capacity-exhausted, the escape hatch blocks the spawn until the earliest reset.

---

## 5. What the "fallback chain" actually was (historical — removed in Phase 2)

> **This section is retained as the analysis that justified the deprecation. The
> `resolveNonExhaustedAccount` fallback chain it describes was removed in Phase 2
> (ka-0000004094); the present tense below is as-of the pre-collapse code.**

The task's original framing (2026-06-09) was:

> The fallback chain (`resolveNonExhaustedAccount`) only ever considered
> `DefaultAccount` (currently empty) as a candidate, so in practice it returned
> the original unchanged, i.e. did almost nothing.

**This is now out of date.** Since ka-0000005505 the redirect also walks each
account's own transitive `fallback` pointer (`accountFallbackChain`), matching
the reactive failover path (`auth_failover.findHealthyFallback`). And those
pointers **are** populated in production — e.g. `fable → gardenio3 →
matthewbaker-acme`, `gardenio2 → gardenio3`. So the chain is no longer inert:
on prod it can redirect a spawn away from an exhausted primary onto a curated
successor account.

But that only sharpens the deprecation question, because the redirect is now
**redundant with the spread selector** rather than merely empty:

- On the **common path**, `applyAccountConfig` reaches
  `resolveNonExhaustedAccount` with an account the spread selector *already*
  filtered through `accountNeedsRedirect`. The redirect's first check
  (`!accountNeedsRedirect` → return original) passes immediately. **No-op.**
- The redirect only does work when the *incoming* account is unhealthy, which
  today means it came from **layer 3/4** (project-cache or `DefaultAccount`) —
  the empty-healthy-pool fall-through. In other words, the fallback chain is a
  second, differently-shaped health-gate-and-redirect that exists to patch the
  one path where the spread selector returned nothing.
- When the whole pool is dead, the **escape hatch** — not the fallback chain —
  is what produces the correct observable outcome (`SpawnBlocked`, retry on
  reset). The fallback chain's "return the original unchanged (with a WARN)"
  terminal case is precisely the silent-bake-a-dead-account behavior the escape
  hatch was added to override.

So we have **three** mechanisms converging on one decision — spread pool health
gating, the fallback-chain redirect, and the escape hatch — with `DefaultAccount`
threaded through two of them while being unset. This is the "what even is that?"
tangle.

---

## 6. Proposed simplified model

**One selection function over one predicate**, with role-pins and the Bedrock
partition as the only special cases:

```text
chooseAccount(cfg, spec):
    # 1. role pin — unchanged, wins outright
    if a := resolveRoleAccount(spec.Role): return healthyOrBlock(a)

    # 2. exclusive-owner role — unchanged
    if roleOwnsExclusiveAccount(spec.Role): return healthyOrBlock(<its account>)

    # 3. the ONE selection: least-loaded over the healthy authorized pool
    pool := accounts labeled project:<name>
              filtered by isHealthyAuthorized(acct, spec)   # == !accountNeedsRedirect
                        + role whitelist + served-model + model-group gates
    if pool non-empty: return leastLoaded(pool)              # selectSpreadAccount

    # 4. opus model-group downgrade — unchanged (narrow, well-scoped)
    if a, m := fallbackModelGroupAccount(...): return a, downgrade m

    # 5. NO healthy account → single explicit, observable failure
    spec.SpawnBlocked = true
    spec.SpawnBlockReason = "no healthy authorized account for project <p> ..."
    slog.Warn(...); return ""     # surface — never silently bake a dead account
```

Key properties:

- **One predicate.** `isHealthyAuthorized` == the inverse of the old
  `accountNeedsRedirect` (renamed + inverted for intent in Phase 4). It is already
  the union of every real health signal and is already the spread pool's filter.
  There is nothing the fallback chain gated on that this predicate doesn't.
- **The spread pool IS the fallback chain.** "Try account A, else its fallback,
  else …" is subsumed by "pick the least-loaded *healthy* account in the pool."
  A curated `fallback` pointer becomes unnecessary for spawn-time selection: if
  `fable` is exhausted, the pool simply doesn't include it and the selector picks
  the next healthy account by load. (The `fallback` field stays meaningful for
  the *reactive* mid-session failover path, `auth_failover.go`, which is a
  separate concern and out of scope for this deprecation.)
- **Retire `DefaultAccount` and `resolveNonExhaustedAccount`.** Replace layers
  3–5 with a single terminal outcome: either the healthy pool produced an
  account, or there is **no healthy account** and we block the spawn with an
  explicit, observable `SpawnBlockReason` (the escape hatch's behavior, promoted
  to the one and only "empty pool" answer). No silent bake of a project-cache or
  default account that may itself be unhealthy.
- **Keep** the two safety gates that are genuinely orthogonal: the role
  account-whitelist fail-closed gate and the served-model / model-group gates.
  Keep the no-credential-spawn *observability* (the WARN when a project has
  credentialed accounts but resolution picked none) — but as the block reason,
  not as a reason to fall through to a maybe-dead account.

The net effect: **spread over the healthy authorized pool, else one explicit
"no healthy account" failure.** Role-pins and the exclusive-account (Bedrock)
partition are the only carve-outs.

### What we lose, and why it's fine

- **`ProjectCache[project].Account` as a spawn source.** Its only job was the
  empty-pool fall-through; under the new model an empty *healthy* pool blocks the
  spawn instead of baking a possibly-unhealthy alphabetically-first account. The
  cache can still exist for display/other consumers; it just stops being a
  credential source. (Verify no other consumer depends on it before removing —
  see Phase 1.)
- **`DefaultAccount`.** Unset in prod; removing the env + config field is pure
  simplification. (Grep for any staging/e2e reliance first.)
- **Per-account `fallback` at spawn time.** The pool selector replaces it. The
  field itself stays for reactive failover; only `resolveNonExhaustedAccount`'s
  use of it goes away.

---

## 7. Deprecation plan (follow-up MRs)

Staged so each step is independently shippable and observably safe. Each phase is
a follow-up task/MR under this epic.

**Phase 0 — instrument (no behavior change).**
Add a counter/log at each `applyAccountConfig` exit recording *which layer*
produced the account (role-pin / spread / opus-fallback / project-cache /
DefaultAccount / redirect-changed-it / escape-hatch-blocked). Ship, let it run a
few days, and confirm empirically that on prod the project-cache and
DefaultAccount/redirect layers essentially never change the outcome. This is the
evidence that de-risks Phases 2–3 (measure-two-ways, advice ka-0000005924).

**Phase 1 — prove `ProjectCache[].Account` has no other credential consumer.**
Audit every reader of `cfg.ProjectCache[...].Account` and
`buildProjectAccountOverride`. Confirm the *only* selection use is the layer-3
fall-through in `applyAccountConfig`. Document findings on the bead.

**Phase 1 result (ka-0000005006, DONE — audited 2026-08-12, HEAD `2026.224.1`).**
Grepping every read of a `ProjectCacheEntry.Account` field across
`cmd/controller/` + `internal/` (non-test) yields exactly **two** runtime readers,
and only one is credential selection:

1. **`applyAccountConfig` (`podspec.go`, the layer-3 fall-through)** — the single
   *credential-selection* consumer. It reads `cfg.ProjectCache[spec.Project].Account`
   only in the `else if` tail after `resolveRoleAccount`, `selectSpreadAccount`, and
   `fallbackModelGroupAccount` all decline — i.e. exactly the empty-pool
   no-credential-spawn guard (ka-0000007343) this doc describes. This is the reader
   Phase 2 removes.
2. **`resolveSpendAccount` (`podspec.go`)** — a **non-credential** consumer. It reads
   the same field only to label the billing `account` on
   `gardenio_agent_spend_usd_total` and to pick the per-account spend-read base URL
   (via `resolveSpendReadBaseURLForAccount` / the `spendReconciler.resolveLabels`
   closure in `main.go`). It selects *no pod credentials* — it mirrors
   `applyAccountConfig`'s precedence purely so the metric label matches the account a
   pod actually spawned on. Phase 2 must therefore leave `resolveSpendAccount`
   intact, or reconcile it against the collapsed selector so labels stay identical.

The **writer** is `refreshProjectCache` (`main.go`), which sets `entry.Account` from
the project bead / `buildProjectAccountOverride` override under `ProjectCacheMu`.
`buildProjectAccountOverride` itself only *produces* the override map; it is never a
reader/consumer.

Every other `cfg.ProjectCache[...]` access in the tree reads a **different** field —
`Image`, `Repos`, `Secrets`, `EnvVars`/`EnvOverrides`, `Ziti*`, `ExecutionTarget`,
`Model`, `Ultracode`, `PrewarmedPool`, `Prefix`/`GitURL` — never `.Account`
(`applyProjectDefaults`, `resolveModelOverride`, `resolveExecutionTarget`,
`resolveZitiAccess`, `resolveUltracode`, the secret/BOAT_PROJECTS wiring,
`poolmanager`, `account_pool_validator`, `secretreconciler`, `served_model_sync`).
So removing `ProjectCache[].Account` as a *spawn credential source* (Phase 2) is
safe: no consumer other than the layer-3 fall-through depends on it for credentials,
and the one non-credential reader (`resolveSpendAccount`) must be preserved.

**Phase 2 — collapse the empty-pool tail into an explicit block.**
Replace layers 3 (project-cache), 4 (DefaultAccount), and 5
(`resolveNonExhaustedAccount`) with: healthy-pool empty ⇒ set
`SpawnBlocked` + explicit `SpawnBlockReason` (reusing the escape-hatch machinery)
and return `""`. Keep the "project has credentialed accounts but resolution
picked none" WARN as the block reason. Delete `resolveNonExhaustedAccount`,
`accountFallbackChain`'s spawn-path use, and the `DefaultAccount` candidate
append. Update `podspec_test.go` / `account_pool_exhaust_test.go`.

**Phase 2 result (ka-0000004094, DONE — 2026-08-12).**
`applyAccountConfig` now resolves an account through exactly three paths —
role-pin (`resolveRoleAccount`), spread (`selectSpreadAccount`), and the opus
model-group fallback (`fallbackModelGroupAccount`). When all three decline, the
account name is `""` and the function takes one of two explicit exits:

- **Credentialed accounts present but all gated** (`countCredentialedProjectAccounts > 0`):
  set `SpawnBlocked = true` with a `SpawnBlockReason` naming the account count and
  the earliest pool `throttle_reset_at` (via `earliestPoolResetAt`, defaulting to
  `unknown`), stamp `AccountLayerNoCredentialWarn`, and emit the loud WARN
  (ka-0000007343). The reconciler re-attempts on the next sync cycle once a window
  resets — a TRANSIENT refusal; the bead is not failed and no escalation is filed.
- **No account beads at all**: benign `AccountLayerNoCredentialBenign` Debug — the
  project uses direct-provider auth (the common case).

The escape hatch (ka-0000002560) still guards a *resolved* account that is
capacity-exhausted, but the only path that now reaches it with an exhausted
account is a **role pin** (spread already excludes exhausted/keyless/overage
accounts). A role-pinned exhausted non-exclusive account is `SpawnBlocked`
rather than chain-failing-over; an exhausted **exclusive** account
(`exclusive_to_role`) is exempt and surfaces the failure on its reserved role,
exactly as before.

Deleted: `resolveNonExhaustedAccount`, `accountFallbackChain`, and the
now-orphaned `accountIsExclusive` (its sole caller was the fallback walk). The
per-account `fallback` field and its transitive walk survive **only** on the
reactive mid-session failover path (`auth_failover.go findHealthyFallback`),
which is explicitly out of scope. `resolveSpendAccount` (the non-credential
billing-label reader identified in the Phase 1 audit) is untouched and still
mirrors the old precedence for the `gardenio_agent_spend_usd_total` label. The
`AccountLayerProjectCache` / `AccountLayerDefaultAccount` /
`AccountLayerRedirectChanged` metric enum members remain valid (historical
series) but are no longer stamped; the `DefaultAccount` config + enum removal
landed in Phase 3 (ka-0000007959), which dropped `AccountLayerDefaultAccount`.
Tests updated: `podspec_test.go`, `account_pool_exhaust_test.go`,
`podspec_account_layer_test.go`, `account_spread_test.go`,
`podspec_account_model_pin_test.go`, `podspec_model_override_test.go` (accounts
that relied on the deleted project-cache fall-through now carry a
`project:<name>` label so the spread pool selects them, matching production).

**Phase 3 — remove `DefaultAccount` config + env + helm.**
Delete `cfg.DefaultAccount`, the `DEFAULT_ACCOUNT` env read, the
`agents.defaultAccount` value, and the deployment.yaml block. Guard with the
Phase 0 telemetry showing it was never used.

**Phase 3 result (ka-0000007959, DONE — 2026-08-12).**
The terminal cross-project default is gone end to end:

- **Config/env.** Deleted `config.Config.DefaultAccount` and the
  `os.Getenv("DEFAULT_ACCOUNT")` read (`internal/config/config.go`).
- **Helm.** Deleted the `agents.defaultAccount` value (`helm/gardenio/values.yaml`)
  and the `{{- if .Values.agents.defaultAccount }}` DEFAULT_ACCOUNT env block
  (`helm/gardenio/templates/controller/deployment.yaml`). The POC terraform overlay
  (`terraform/modules/gardenio-chart/values-aws-poc.yaml.tftpl`) no longer pins
  `matthewbaker1110`; gardenio agents route via `project:gardenio`-labeled accounts
  (the spread pool) instead.
- **Metric enum.** Removed the `AccountLayerDefaultAccount` ("default-account")
  member from the bounded account-layer enum (`internal/metrics/events.go`); it
  was never stamped after Phase 2. `AccountLayerProjectCache` /
  `AccountLayerRedirectChanged` stay as accepted historical series.
- **Validator.** The account-cache Criterion 4 audit
  (`findProjectsWithoutEmptyPoolHome`) existed only to check whether a healthy
  `DEFAULT_ACCOUNT` net caught a stranded pool's empty-pool fall-through. With no
  fall-through left, a stranded pool has no safety net at all, so the audit
  collapses into Criterion 3 (`findStrandedProjectPools`), whose WARN now carries
  the no-credential-spawn (ka-0000007343) severity framing. `validateAccountPools`
  dropped its `defaultAccount` parameter.
- **Spend.** `resolveSpendAccount` (the non-credential billing-label reader) no
  longer falls through to `cfg.DefaultAccount`; it returns `""` when neither a
  role pin nor a project-cache account resolves. The spend-proxy transparency
  E2E now routes via `ProjectCache` instead of the removed field.
- **E2E.** `test-airgap-image-distribution.sh` legs 8+9 flipped from asserting the
  helm→env seam is intact (ka-0000001890) to asserting the removal is complete —
  the chart must never emit a `DEFAULT_ACCOUNT` env, even for a stray override.

**Phase 4 — rename for intent.**
`accountNeedsRedirect` → `isHealthyAuthorized` (inverted) or keep as-is but
document it as *the* health predicate. `selectSpreadAccount` becomes simply
`selectAccount` — there is no longer a "spread vs. fallback" duality to
distinguish. Update this doc's §3 to the collapsed pipeline.

**Phase 4 result (ka-0000001577, DONE — 2026-08-12).**
`selectSpreadAccount` / `selectSpreadAccountAt` → `selectAccount` /
`selectAccountAt` (pure rename). `accountNeedsRedirect` → `isHealthyAuthorized`,
**inverted** so the name states the health assertion it makes: `selectAccount`
includes an account in the spread pool iff `isHealthyAuthorized(acct)`. The two
call sites (the spread-pool filter and the tests) flip sense accordingly; the
five triggers (exhausted / keyless-proxy / blocking-overage / over-budget /
probe-auth-rejected) are unchanged. Comments across `podspec.go`, `main.go`,
`account_spread.go`, `account_rotation_restart.go`, `config.go`, and the tests
were updated to the new names, and §§1–4 + §8 of this doc were rewritten to the
collapsed three-path pipeline. No behavior change — a rename + doc pass only.

**Phase 5 — role-pin exhaustion spill (ka-0000001307, DONE — 2026-08-19).**
Closes a fragility Phases 2–3 left in place. A role that pins a single account
(`RoleCacheEntry.Account`, e.g. `gardener-release-owner → gardenio3`) bypasses the
spread pool entirely (`resolveRoleAccount` wins in `applyAccountConfig`). So when
that pinned account is capacity-exhausted at spawn, the pool-exhaustion escape
hatch set `SpawnBlocked` and the reconciler parked the agent `queued`
**indefinitely** — until the pinned account's rate window reset (45+ min to
days) — *even though the project had other healthy accounts the pin could never
reach*. This stalled the release lane twice (ka-0000006248 2026-07-03,
ka-0000007145 2026-07-13), each needing a manual repoint/unpin.

The fix (Option 1 of the task): in the escape hatch, before blocking, when the
account was resolved **by the role pin** (`layer == AccountLayerRolePin`) and the
role permits spill, call `selectSpreadAccountAt` — the spread ranking with the
role-pin short-circuit removed but every other guard (whitelist / exclusive /
served-model / model-group / long-lived headroom) intact — and land on a healthy
account instead of blocking. Stamped `AccountLayerRolePinSpill` ("role-pin-spill")
so the spill is observable in `gardenio_agent_account_layer_total`.

Guards keep it narrow and reversible:
- Only fires for a **role-pin**-resolved account. A spread/opus pick that lands
  exhausted is the whole-pool-exhausted case the block already handles correctly.
- Only when the role permits it: a lane that MUST stay pinned sets
  `no_account_spill: true` on its role bead (`RoleCacheEntry.NoAccountSpill`) and
  keeps the old block-and-wait behavior. Default is spill-allowed (the safer
  default — a running agent on a healthy sibling beats an invisibly-parked one).
- **Exclusive** accounts (`exclusive_to_role`) never spill: `selectSpreadAccountAt`
  returns `""` for an exclusive-owning role, so a reserved lane still surfaces
  failure on its own account. A genuinely all-exhausted pool still blocks (the
  spread pool is empty → `""` → block).

Source: `applyAccountConfig` (`podspec.go`), the extracted `selectSpreadAccountAt`
(`account_spread.go`), `roleAllowsAccountSpill` + `NoAccountSpill` plumbing
(`podspec.go`, `config.go`, `beadsapi/roles.go`, `main.go`), and
`account_role_pin_spill_test.go`.

**Non-goals / explicitly out of scope.**
- The **reactive** mid-session failover (`auth_failover.go`) and the account
  `fallback` field it consumes — separate mechanism, keep.
- The rate-limit **probe** and how it writes `status` / `throttle_status` /
  `probe_auth_status` — the health *signals* are correct; only the *selection*
  plumbing on top is being simplified.
- The **Bedrock / exclusive-account** partition (`exclusive_to_role`) — keep as
  the one special case.

---

## 8. Source map

| Concern | Function | File |
|---|---|---|
| Top-level orchestration | `applyAccountConfig` | `cmd/controller/podspec.go` |
| Role pin | `resolveRoleAccount` | `cmd/controller/podspec.go` |
| The ONE selection (least-loaded) | `selectAccount` / `selectAccountAt` | `cmd/controller/account_spread.go` |
| Opus model-group downgrade | `fallbackModelGroupAccount` | `cmd/controller/account_spread_model_group.go` |
| Health predicate (broad) | `isHealthyAuthorized` | `cmd/controller/podspec.go` |
| Capacity predicate (narrow) | `accountCapacityExhausted` | `cmd/controller/podspec.go` |
| Overage / budget helpers | `overageReasonBlocks`, `accountOverBudget` | `cmd/controller/podspec.go` |
| Exclusivity | `roleOwnsExclusiveAccount`, `roleMatchesExclusive` | `cmd/controller/podspec.go`, `account_spread.go` |
| Reactive failover (out of scope) | `findHealthyFallback` | `cmd/controller/auth_failover.go` |
| Billing-label reader (non-credential) | `resolveSpendAccount` | `cmd/controller/podspec.go` |
| Account cache model | `AccountCacheEntry` | `internal/config/config.go` |

Removed in the deprecation (Phases 2–3): `resolveNonExhaustedAccount`,
`accountFallbackChain`, `accountIsExclusive` (spawn-path fallback chain, Phase 2
ka-0000004094); the `ProjectCache[].Account` spawn-credential source (Phase 2);
and the `DefaultAccount` config + `DEFAULT_ACCOUNT` env + `agents.defaultAccount`
helm value (Phase 3 ka-0000007959). `buildProjectAccountOverride` /
`ProjectCache[].Account` remain only as the non-credential billing-label source
read by `resolveSpendAccount`.

Related history: ka-0000003769 (idle-agent p0), ka-0000003293 (its decision),
ka-0000002660 (Bedrock partition), ka-0000005505 (per-account fallback walk +
budget gate), ka-0000002560 (pool-exhaustion escape hatch), ka-0000007343
(no-credential-spawn guard), ka-0000002085 (probe_auth_status gate),
ka-0000003061 (role account whitelist), ka-0000004861 (spread selection).
