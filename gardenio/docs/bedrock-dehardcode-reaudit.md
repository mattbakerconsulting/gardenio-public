# Bedrock/role de-hardcode + account exhaustion & rotation re-audit

> **Bead:** ka-0000004946 · **Origin:** Matt Baker, Slack 2026-06-16 — *"we gotta
> make that less hard coded with the bedrock thing! … needs a generic config …
> need to audit the entire account and rotation mechanism again."*
> **Date:** 2026-08-09 · **Scope:** research + re-audit only (no behavior change).

## TL;DR

1. **The Bedrock/role de-hardcode the task asks for is already done and live.**
   The hardcoded constants `bedrockOnlyRole = "fsm-triage"`,
   `fallbackLastResortAccount = "litellm-bedrock"`, and the `isBedrockOnlyRole()`
   string-compare are **gone** from production Go. They were replaced by a
   data-driven `exclusive_to_role` field on the account bead (commit `cad2a72bf`,
   feature bead **ka-0000005223**, MR **!4929**, merged 2026-06-28). The
   companion design reference is [`account-routing.md`](account-routing.md)
   (landed 2026-08-04, ka-0000007193).
2. **It is provably generic.** Prod now runs **two** exclusive accounts, the
   second added with **zero controller change** — exactly the property the task
   wanted:
   - `litellm-bedrock` → `exclusive_to_role: fsm-triage` (bead ka-0000003488)
   - `glm-bedrock` → `exclusive_to_role: glm-lab` (bead ka-0000007590)
3. **The rotation/exhaustion mechanism re-audit finds it healthy and heavily
   incident-hardened.** The three follow-ups from the original nudge root-cause
   (ka-0000008022) are substantially landed. No correctness bug found.
4. **The one genuinely-remaining hard-code worth a follow-up** is the
   `model_group_alias` / model-id list, which is **quadruplicated** across Go,
   the Helm ConfigMap, the deploy-values overlays, and the Slack bridge
   allowlist, kept in sync only by drift tests. This is the recommended next
   de-hardcoding target and is filed as a follow-up (see §4).

---

## 1. Goal #1 — where the Bedrock/role binding *was* hardcoded, and its replacement

### 1.1 What the task described (as of 2026-06-16)

| Old hardcode | Location | Role |
|---|---|---|
| `const fallbackLastResortAccount = "litellm-bedrock"` | `podspec.go` | account name literal |
| `const bedrockOnlyRole = "fsm-triage"` | `podspec.go` | role name literal |
| `isBedrockOnlyRole(role)` | `podspec.go` | string-compares role to the constant |
| symmetric partition branches | `account_spread.go`, `resolveNonExhaustedAccount` | fsm-triage→Bedrock only; all others excluded from Bedrock |

The partition encoded **two** rules:
- **(a)** `fsm-triage` uses Bedrock **only** (never spills to Claude Max).
- **(b)** every **other** role **never** touches Bedrock (avoids silent Bedrock spend).

Policy of record: **ka-0000002660**.

### 1.2 What replaced it — `exclusive_to_role` (the generic config)

The controller now reads a single field off the `type=account` bead:

```
exclusive_to_role: <role>
```

Semantics (config field `AccountCacheEntry.ExclusiveToRole`,
`internal/config/config.go`; hydrated in `main.go` from
`acct.Fields["exclusive_to_role"]`):

- **Spread pool** (`selectAccount`, `account_spread.go`): an exclusive
  account is excluded from every **non-owner** role's pool
  (`acct.ExclusiveToRole != "" && !roleMatchesExclusive(spec.Role, ex)` → skip),
  and a role that **owns** an exclusive account short-circuits out of the general
  spread entirely (`roleOwnsExclusiveAccount` → return `""`). Rule (b) + owner
  side of (a).
- **Exhaustion resolver** (`resolveNonExhaustedAccount`, `podspec.go`): an
  exhausted **exclusive** account is **never redirected** ("exclusive ⇒ no
  spill" — the owner stays on it and the failure surfaces), and exclusive
  accounts are **excluded as fallback candidates** for non-owner roles
  (`accountIsExclusive(candidate)` → continue). Both halves of the partition.

**This is a strictly better encoding than per-role whitelists** for rule (b):
"no one else may use this account" is a property of the *account*, and putting it
on the account is one edit that is fail-*closed* for every other role — versus a
per-role exclusion that would need N edits and fail-*open* if one were forgotten
(see ka-0000005223 for the full argument). It composes with the narrower,
positive-scoping `role account_whitelist` (ka-0000003061).

**Latent bug it also fixed:** the old `isBedrockOnlyRole` matched the literal
`"fsm-triage"` only, so a *second* Bedrock-pinned role (`hema-analyst`) was
wrongly redirected onto a Claude Max account on exhaustion. Keying the no-spill
rule on the **account** unifies every role pinned to it.

### 1.3 Verification (why this counts as "done", not "claimed")

- **Source:** the old constants return **zero** hits in production Go
  (`grep bedrockOnlyRole|fallbackLastResortAccount|isBedrockOnlyRole` over
  `controller/…/*.go` excluding tests → only comments describing the migration).
- **Live data plane (prod `gardenio`, 2026-08-09):** two accounts carry
  `exclusive_to_role`, the second (`glm-bedrock → glm-lab`) added with no
  release. This is the operator's exact "add a second Bedrock-only role without a
  code change" acceptance test, satisfied in production.
- **Tests:** `account_spread_spawn_e2e_test.go`, `account_pool_exhaust_test.go`,
  `account_spread_test.go`, `auth_failover_terminal_test.go` all assert the
  data-driven pin + no-spill behavior.

**Conclusion for Goals #1 & #2 (identify + plan a de-hardcoding approach):** the
recommended approach (a field on the account bead, option (a) in the task's own
list) is the one that was implemented. No further design work is needed for the
Bedrock/role binding itself.

---

## 2. Goal #3 — account exhaustion & rotation re-audit

Files audited: `account_exhaustion.go`, `account_spread.go`,
`account_spread_model_group.go`, `auth_failover.go`, `auth_failover_helpers.go`,
`account_rotation_restart.go`, `ratelimit_probe.go`, `podspec.go`
(`applyAccountConfig` / `resolveNonExhaustedAccount` / escape hatch), and
`cmd/gb/init_agent.go` (the pod-side park/backoff).

### 2.1 Exhaustion classification is a correct 3-axis model

The mechanism cleanly separates three independent failure axes — this directly
answers the task's "whether exhaustion/cooldown semantics still match reality":

| Axis | Field(s) | Meaning | Heals by |
|---|---|---|---|
| Rate-limit window | `throttle_status`, `utilization_5h/7d`, `throttle_reset_*at` | transient per-window throttle | rolling window reset (probe-written) |
| Hard auth | `probe_auth_status = rejected` | 401/403 — revoked token / org disabled Claude Code | never on its own (needs credential fix) |
| Billing | `overage_status` / `overage_disabled_reason` (`out_of_credits`, `org_spend_cap_reached`) | pay-as-you-go credits / spend cap | a billing action (metered) |

Key correctness properties confirmed:

- **Auth-dead ≠ throttled.** `probe_auth_status=rejected` makes an account
  ineligible as a failover/switch target regardless of how healthy its
  rate-limit window looks (`fallbackTier` returns −1). This closes the
  2026-06-22 incident where a session was rotated onto a subscription-dead
  account (ka-0000002085).
- **Billing hold is scoped to metered accounts only.** `overageOutOfCredits`
  returns true only for `billing_type == metered` — a **subscription** account
  (the whole fleet default, `billingType()` defaults to `subscription`)
  carrying `out_of_credits`/`org_spend_cap_reached` is a benign steady-state and
  is **not** frozen. This is the fix for the fleet-wide starvation where the one
  healthy account was held exhausted (ka-0000003350 / ka-0000004929). The metered
  hold correctly **outranks** the cooldown-elapsed timer so a genuinely
  out-of-credits metered account cannot be cleared on a timer.
- **The monthly-spend-cap case (no reset time) — the task's specific concern —
  is handled** on both sides:
  - *Controller side:* `earliestPoolResetAt` returns `""` when no account carries
    a `throttle_reset_at`, and the pool-exhaustion escape hatch degrades the
    operator message to `"blocking new spawns until unknown"` rather than
    crashing (ka-0000002560).
  - *Pod side (`gb init`):* the blunt fixed-30m park from ka-0000008022 is
    replaced by `rateLimitBackoff` — when the throttle message carries **no reset
    hint** (the account-spend-limit signature) it uses a **jittered exponential
    backoff** (base 1m → 30m cap) keyed on a consecutive-no-hint streak, killing
    the thundering-herd re-hit pattern. A hard, unclearable spend cap is caught
    by the **rate-limit-livelock give-up** (`failAgentRateLimitLivelock`, bound =
    8 consecutive parks with no successful turn, ka-0000001209): it requeues the
    task to a fresh agent (which gets a fresh key) and closes the pod with a
    distinct `rate_limit_livelock_involuntary` reason so the death is counted and
    named honestly, not read as an idle-out.

  This maps 1:1 to the three follow-ups the original root-cause bead
  (ka-0000008022) recommended: **B** (smarter backoff) ✅, **C** (surface the
  billing cap distinctly) ✅, and **A** (a cross-role Bedrock fallback) correctly
  **NOT** done — it was withdrawn (MR !4425 closed) because it violates the
  role partition (policy ka-0000002660).

### 2.2 Recovery is single-path and cannot leak capacity

- **Switchback is abolished** (ka-0000008240): in the primary-free regime there
  is no privileged "home" to steer a project back to. The sole recovery path is
  `SweepStaleExhausted`, which clears `status=exhausted` on either of two
  independent conditions:
  1. a **fresh recovering probe** (`throttle_status` allows AND probe within
     `probeStaleThreshold` = 20m), OR
  2. **`cooldown_until` has elapsed** — the timer backstop that recovers a
     Rust-coopmux-marked or otherwise-unprobed account, and any account behind a
     stale/down probe.
  This dual clause is what prevents the "stuck exhausted forever after a
  controller restart" failure that pinned the whole fleet to Bedrock on
  2026-04-25 (ka-0000002663). Accounts stuck past cooldown by a real blocker are
  logged (`stuck_past_cooldown`) rather than silently leaking.
- **Restart state survives a controller restart** via `RebuildState` (reads
  cooldown metadata off the account beads) — the runtime map is a cache, not the
  source of truth.

### 2.3 The active→exhausted pod-restart churn guard is sound (ka-0000007997)

`accountRotationRestarter` restarts pods whose baked LiteLLM key belongs to an
account that just went exhausted. It is defended against every churn/collateral
failure mode observed:
- **Edge-triggered** on the active→exhausted transition (per-account
  `prevStatus`), not on every reconcile while exhausted.
- **Key-exact matching** — a pod already re-baked onto a healthy fallback carries
  a different key and is never re-matched (self-idempotent).
- **Drain gate** — a delete waits for the agent to finish its turn (bounded by
  `drainTimeout`), so a controller restart never kills a pod mid-response
  (ka-0000001482).
- **Live-session verification** — a pod whose coopmux session already switched
  accounts in-process is skipped, so a healthy session's context is not destroyed
  (ka-0000005463); the blind-sweep counter fails open after a bounded coopmux
  outage so coopmux is not a SPOF.
- **Level-triggered counterpart** `ReconcileStrandedRecovered` handles the case
  where the whole pool was exhausted *before* pods spawned (no edge ever fired) —
  it re-bakes stranded pods once a healthy sibling reappears (ka-0000005398), with
  a live-count bump so a batch spreads instead of dogpiling one sibling.

### 2.4 Observations worth monitoring (not bugs)

- **Live snapshot (2026-08-09):** 6 of 7 general-pool accounts read
  `status=exhausted`; only `fimbaz` carries a `cooldown_until`. The others rely on
  the probe-recovery clause of `SweepStaleExhausted`. This is expected given the
  primary-free model, but a persistently-mostly-exhausted pool is worth a
  dashboard alert (the sweep already emits per-tick counters for this — see the
  `exhausted` / `cleared` / `stuck_past_cooldown` heartbeat).
- **Exclusive accounts have no redundancy by design.** Because "exclusive ⇒ no
  spill", an `fsm-triage` or `glm-lab` agent hard-fails if its single account
  exhausts — there is deliberately no fallback (that is the whole point of the
  partition). This is correct per policy, but operators of those roles should
  know the single-point exposure is intentional, not a gap to "fix" with a
  cross-role fallback.

---

## 3. Other hard-codes found in the re-audit (categorized)

A full literal sweep of `cmd/controller`, `internal/`, `helm/`, and
`deploy-values/` classified every region/model/account/role literal. Summary:

- **Account/role name literals in production `cmd/controller` Go: none.** Every
  `litellm-bedrock` / `fsm-triage` / `glm-lab` occurrence there is a comment or a
  test asserting the data-driven behavior. The partition is fully data-driven.
- **Region + Bedrock model-id literals live in Helm values / deploy-values**
  (`values.yaml` `litellm.bedrock.region: us-east-1`, the `bedrock.models` list,
  `us-east-2` per-model override for `zai.glm-5`). This is **config in the right
  place** (chart values, not Go constants) — low priority.
- **Model-id defaults in `internal/config`** (`AGENT_DEFAULT_FABLE_MODEL`,
  `AGENT_DEFAULT_HAIKU_MODEL`) are env-overridable defaults — acceptable.
- **The `model_group_alias` map is the real remaining duplication** — see §4.

---

## 4. Recommended follow-up: de-duplicate the model_group_alias map

The single alias contract (`claude-opus-5 → claude-opus-5-oauth`,
`glm-5 → bedrock-glm-5`, …) is currently written out in **four** places, kept in
sync only by drift tests:

1. **Go** — `claudeCodeModelAliases` in
   `controller/cmd/controller/litellm_aliases.go` (synced into the LiteLLM router
   at controller startup).
2. **Helm ConfigMap** — `router_settings.model_group_alias` +
   `model_list` defaults in `helm/gardenio/templates/litellm/configmap.yaml`.
3. **deploy-values overlays** — `modelGroupAlias` / `model_list` blocks in
   `deploy-values/gardenio-rwx*.yaml` (with per-env drift, e.g. `glm-5` present in
   prod but not the plain `gardenio-rwx.yaml`).
4. **Slack bridge** — `shortModelAliases` + `fullModelAllowlist` in
   `controller/internal/bridge/bot_mentions_model.go` (`--model=glm → glm-5`).

Adding or renaming a model today requires editing up to all four and cutting a
controller release for (1)/(4). The drift guards
(`litellm_router_alias_test.go`, `TestBridgeAllowlistMatchesAliasMap`) catch
divergence but don't eliminate the multi-edit burden.

**Suggested direction** (to be designed in the follow-up, not decided here):
make the Helm-rendered `model_group_alias` the single source of truth and have
the controller *read* it (it already fetches the live router config in
`fetchModelGroupAliases`) rather than owning its own copy, or generate the Go map
and the bridge allowlist from a shared chart-values file at build time. This is a
**model** de-hardcode, orthogonal to the Bedrock/role **partition** de-hardcode
(which is already done), so it belongs in its own task — filed as follow-up
**ka-0000003830** referencing this doc.

> **RESOLVED (ka-0000003830, 2026-08-12).** The chart's rendered
> `router_settings.model_group_alias` is now the **single source of truth** for
> the claude-*/glm-5 base alias map. The root cause of the quadruplication was
> that setting `litellm.modelGroupAlias` in an overlay *replaced* the chart
> default (a Helm `if/else`), so every overlay had to re-copy the whole base map
> just to add its env-specific `gpt-*` entries. `litellm.modelGroupAlias` is now
> **additive**: the chart always renders the base map and *merges* the overlay's
> keys on top. This removed the duplicated base block from all four overlay sites
> (`deploy-values/gardenio-rwx{,-poc,-prod}.yaml`, `helm/gardenio/values-inbox.yaml`,
> the canonical-prod `grafana-assistant-values.yaml` day0 overlay, since retired) and
> fixed real drift the manual copies had accumulated (the `glm-5` selector was
> missing from the rwx/poc overlays; `claude-opus-5` was missing from the
> grafana-assistant overlay). The controller Go map (`litellm_aliases.go`) and the
> Slack bridge allowlist (`bot_mentions_model.go`) remain drift-guarded **subsets**
> — `TestBridgeAllowlistMatchesAliasMap` still enforces the subset relation, and
> `TestLitellmAlias_GrafanaOpenAIToBedrock` now asserts the additive-merge
> property (base survives + overlay adds). Reading the live router at build time
> was NOT adopted: it would couple `helm template` / CI to a running proxy and
> lose the fail-closed static guard, whereas the additive-merge keeps the base
> declared in one text block that the existing drift tests already parse.

---

## 5. Source map

| Concern | Symbol | File |
|---|---|---|
| Exclusive-account config field | `AccountCacheEntry.ExclusiveToRole` | `internal/config/config.go` |
| Owner short-circuit / non-owner exclude | `roleOwnsExclusiveAccount`, `roleMatchesExclusive`, `accountIsExclusive` | `podspec.go`, `account_spread.go` |
| Spread selection | `selectAccount` / `selectAccountAt` | `account_spread.go` |
| Exhaustion resolver (no-spill) | `resolveNonExhaustedAccount`, `accountFallbackChain` | `podspec.go` |
| Pool-exhaustion escape hatch | `applyAccountConfig` block, `earliestPoolResetAt`, `accountCapacityExhausted` | `podspec.go` |
| Reactive failover | `authFailover.Handle`, `findHealthyFallback`, `fallbackTier` | `auth_failover.go` |
| Recovery sweep (sole path) | `SweepStaleExhausted`, `cooldownUntilElapsed` | `auth_failover.go`, `auth_failover_helpers.go` |
| Classification helpers | `billingType`, `overageOutOfCredits`, `reconcileLegacyStatusFields` | `ratelimit_probe.go` |
| Pod-restart churn guard | `accountRotationRestarter` (`Reconcile`, `ReconcileStrandedRecovered`) | `account_rotation_restart.go` |
| Pod-side park/backoff | `rateLimitBackoff`, `failAgentRateLimitLivelock` | `cmd/gb/init_agent.go` |
| Model alias map (Go) | `claudeCodeModelAliases`, `syncLiteLLMModelAliases` | `litellm_aliases.go` |

**Related history:** ka-0000005223 (the de-hardcode, MR !4929), ka-0000002660
(Bedrock partition policy), ka-0000007193 (account-routing.md), ka-0000008022
(nudge root-cause), ka-0000008240 (abolish-primary / switchback removal),
ka-0000007997 (rotation-restart churn guard), ka-0000002560 (pool-exhaustion
escape hatch), ka-0000002085 (probe_auth_status gate), ka-0000003350 /
ka-0000004929 (metered-only billing hold), ka-0000002663 (2026-04-25 stuck-Bedrock
incident), ka-0000001209 (rate-limit-livelock give-up).
