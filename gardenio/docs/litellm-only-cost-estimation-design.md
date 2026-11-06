# Design Doc: LiteLLM-only Cost Estimation — Expurgate All Other Cost Methods

**Epic:** ka-0000000758 · **Research:** ka-0000007494 (R1) · **Status:** canonical design-of-record
**Gates:** I1 ka-0000000717 · I2 ka-0000003594 · I3 ka-0000002983 · T1 ka-0000008138 · REL ka-0000001853

---

## 1. Executive Summary

**State of the world (one sentence):** Every USD cost figure in the platform already originates from LiteLLM — there are no token×rate estimators, no hardcoded prices, and no alternative cost methods to expurgate — but the codebase still carries two *structurally dead* LiteLLM-derived paths (the per-turn NATS `spend_forwarder` and the gb-side `syncAccountSpendFromLiteLLM` hook) plus one *misleading comment* that conflates an unused JSONL field with the authoritative cost sources.

**Target end-state:** A single, documented, authoritative LiteLLM cost pipeline (account-level via `spend_sync`, session/agent-level via `reconcile_session_spend`), with all dead code removed, all downstream surfaces (gb, beads-viewer, iOS, kbeads charts) reading *only* LiteLLM-sourced bead fields / Prometheus series, a deliberate decision recorded for coop's `response_cost`, and a guard test that fails the build if any token×rate cost math is ever reintroduced.

The audit found **zero** non-LiteLLM cost computations. "Expurgate all other cost methods" therefore reduces to: **delete the dead LiteLLM plumbing, fix the misleading comment, decide coop's fate, and lock the invariant with a guard.**

---

## 2. The ONE Canonical LiteLLM Cost Pipeline

LiteLLM is the single source of truth for cost. Cost enters the system through exactly **two live controller writers**, lands on **bead fields** and **one Prometheus counter**, and every other surface is **read-only** against those.

### 2.1 The two authoritative writers (controller only — has the master key)

**A. Account-level cumulative spend — `spend_sync.go` (5-minute loop)**
- Queries `GET /key/info?key=<virtualKey>` → `result.Info.Spend` (`spend_sync.go:147-176`).
- Queries `GET /spend/logs?api_key=<hash>&start_date=…&end_date=…`, sums `entry.Spend` into daily/weekly (`spend_sync.go:192-233`).
- Writes `total_cost_usd`, `daily_cost_usd`, `weekly_cost_usd` onto the **account bead** (`spend_sync.go:122-138`).

**B. Session/agent-level reconciled cost — `reconcile_session_spend.go` (per session)**
- At spawn: `captureBaseline` snapshots the agent tag's lifetime spend via `GET /spend/tags` (matched client-side on `agent:<name>`), stashes it on the agent bead field `litellm_spend_baseline` (`reconcile_session_spend.go:104-134, 136-155`; called from `main.go:1034`).
- At close: `reconcileAtClose` re-queries `GET /spend/tags`, computes `sessionCost = closeTotal - baseline` (clamped ≥0), writes `total_cost_usd` + `litellm_spend_close` onto the **agent bead** (`reconcile_session_spend.go:157-222`; called from `main.go:1282`).
- On the **first** reconcile only, increments the Prometheus counter `gardenio_agent_spend_usd_total` by `sessionCost`, labeled `{agent,project,role,model,account}` (`reconcile_session_spend.go:207-221` → `metrics/spend.go:50-64`). `RecordSpend` drops non-positive cost, so a $0 session never advances the counter.

A third, narrower live reader exists inside the agent pod: `gb session` parses the session JSONL and accumulates LiteLLM's injected `response_cost` field into `SessionUsage.TotalCostUSD` (`session_usage.go:84-148`). This feeds the `gb session`/`gb molecule`/`gb session analyze` displays and session-bead writes. It is **LiteLLM-sourced** (reads `response_cost`/`_litellm_response_cost`, never tokens×rate) and is **kept**. Note: the controller-side reconcile (B) is the *authoritative overwrite* of the agent bead's `total_cost_usd`; the JSONL accumulation is the in-pod estimate that the reconcile supersedes.

### 2.2 Flow diagram

```
                          LiteLLM (single source of truth for $)
                          /key/info   /spend/logs   /spend/tags
                              |            |            |
        ┌─────────────────────┘            |            └────────────────────┐
        |                                  |                                 |
  [WRITER A] spend_sync.go            (daily/weekly)                  [WRITER B] reconcile_session_spend.go
   5-min loop, controller                  |                          spawn: captureBaseline (tag snapshot)
        |                                   |                          close: closeTotal - baseline = sessionCost
        v                                   v                                 |
  ACCOUNT BEAD fields            ACCOUNT BEAD fields                          v
  total_cost_usd                 daily_cost_usd                       AGENT BEAD field
                                 weekly_cost_usd                      total_cost_usd
        |                                                                     |
        |                                                                     +--> Prometheus counter
        |                                                                     |    gardenio_agent_spend_usd_total
        |                                                                     |    (+1 per session, first reconcile)
        |                                                                     |
        |    in-pod (agent): gb session reads JSONL response_cost ───────────┤
        |    session_usage.go -> SessionUsage.TotalCostUSD -> session bead    |
        |                        (reconcile B overwrites agent bead)          |
        v                                                                     v
  ===================================  READ-ONLY DOWNSTREAM  ============================================
   beads-viewer/costs.go      iOS CostsTab / Models.swift     kbeads charts (PromQL only)
   reads daily/weekly/total   reads totalCostUsd/dailyCostUsd  gardenio_agent_spend_usd_total (counter)
   from account bead fields   /weeklyCostUsd from bead fields  gardenio_operator_cost_usd_total
   metrics.go exports         EngagementDashboard reads        gardenio_beads_total_cost_usd (SQL SUM
   account/agent gauges       kbeads /v1/charts/.../spend       of total_cost_usd bead field)
   molecule_rollup.go sums    (no direct LiteLLM)              rate()/increase()/division ONLY — no estimation
   session+workflow pools
```

### 2.3 Read contract for downstream surfaces

Every downstream surface MUST derive cost **only** from:
1. **Account bead fields** `total_cost_usd` / `daily_cost_usd` / `weekly_cost_usd` (written by Writer A), or
2. **Agent/session bead field** `total_cost_usd` (written by Writer B, or by `gb` JSONL accumulation for session beads), or
3. **Prometheus series** that are themselves populated from (1)/(2): `gardenio_agent_spend_usd_total`, `gardenio_operator_cost_usd_total`, `gardenio_account_cost_usd`, `gardenio_beads_total_cost_usd`.

Permitted transformations are **pure arithmetic / aggregation only**: sum, division (cost/session, cost/bead), wall-clock projection (hourly-rate × 24, daily-avg × days-in-month), PromQL `rate()`/`increase()`. **No surface may multiply tokens by a price.**

---

## 3. Full Inventory Table

`source` legend: `litellm` = directly from a LiteLLM endpoint/field; `bead_field` = reads a bead field that was LiteLLM-populated; `none` = no cost data flows (dead). All `keep` touchpoints are confirmed LiteLLM-rooted; all `dead` are confirmed unreachable/never-populated.

### Cluster: controller — live LiteLLM writers & wiring

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `spend_sync.go:147-176` | account spend from `GET /key/info` | litellm | keep | no action |
| `spend_sync.go:122-138` | write total/daily/weekly to account bead | litellm | keep | no action |
| `spend_sync.go:192-233` | daily/weekly from `GET /spend/logs` | litellm | keep | no action |
| `reconcile_session_spend.go:104-134` | per-agent spend from `GET /spend/tags` | litellm | keep | no action |
| `reconcile_session_spend.go:156-188` | session delta closeTotal−baseline → `total_cost_usd` | litellm | keep | no action |
| `reconcile_session_spend.go:207-221` | counter increment by reconciled session cost (F2) | litellm | keep | no action |
| `reconcile_session_spend.go:136-155` | captureBaseline snapshot to `litellm_spend_baseline` | litellm | keep | no action |
| `metrics/spend.go:50-64` | `RecordSpend` increments `gardenio_agent_spend_usd_total` | litellm | keep | no action |
| `metrics/metrics.go:358` | `agentCostUSD` gauge ← bead `total_cost_usd` | bead_field | keep | no action |
| `metrics/metrics.go:399` | `accountCostUSD` gauge ← bead `total_cost_usd` | bead_field | keep | no action |
| `main.go:546-556` | wires spendReconciler + spend_sync + metrics | litellm | keep | no action |
| `main.go:1034` | captureBaseline at spawn | litellm | keep | no action |
| `main.go:1282` | reconcileAtClose at close | litellm | keep | no action |
| `reconcile_session_spend.go:40-51` | comment documenting forwarder is dead | n/a | keep | retain as-is |

### Cluster: controller — DEAD code (I1 targets)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `spend_forwarder.go` (entire file) | per-turn NATS→counter forwarder; drops every event at `:159-161` (`evt.ResponseCost == nil`) | none | **dead** | **DELETE file** (struct, `usageEvent`, `agentLabel`, `accountResolver`, `Start`, `handleMessage`) |
| `spend_forwarder.go:33` | `ResponseCost *float64` field | none | dead | removed with file |
| `spend_forwarder.go:174` | unreachable `RecordSpend` call | none | dead | removed with file |
| `main.go:476-496` | spendForwarder startup block + comment | none | **dead** | **DELETE block** (`newSpendForwarder(...)` + goroutine); also delete `resolveSpendAccount` if now unused |
| `cmd/gb/session.go:566-606` | `syncAccountSpendFromLiteLLM` — early-returns in agent pods (no `LITELLM_MASTER_KEY`) | litellm | **dead** | **DELETE function + its 2 call sites** (`session.go:323, 478`) and the comment block at `session.go:318-326`. Superseded by Writer B. |

### Cluster: gb — session JSONL (live, keep)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `session_usage.go:84-94` | `extractResponseCost` reads `response_cost`/`_litellm_response_cost` | litellm | keep | no action |
| `session_usage.go:104-148` | `accumulateUsage` sums `response_cost` (no token math) | litellm | keep | no action |
| `session_usage.go:15-59` | `parseSessionUsage` JSONL reader | litellm | keep | corrected from earlier misclassification |
| `session_usage.go:100-103` | comment "sourced exclusively from response_cost…" | litellm | keep | **CLARIFY comment** — see I1 spec |
| `session.go:159-164` | `gb session` list displays `total_cost_usd` bead field | bead_field | keep | no action |
| `session.go:307-315` | `pushSessionMetrics` passes JSONL usage to vmagent | litellm | keep | no action |
| `session_analyze.go:39-53, 175-196` | `TotalCostUSD` carried to analyze output | litellm | keep | no action |
| `session_metrics_push.go:115-151` | emits `gardenio_session_cost_usd` / `gardenio_operator_cost_usd_total` | litellm | keep | no action |
| `molecule.go:77-91, 99-119, 127-133` | molecule/step cost display + zero-filter | bead_field | keep | no action |

### Cluster: beadsapi — session + molecule rollup (live, keep)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `client_session.go:35` | `SessionUsage.TotalCostUSD` field | litellm | keep | no action |
| `client_session.go:106` | `CloseSession` writes `total_cost_usd` to session bead | litellm | keep | no action |
| `client_session.go:127-141, 151` | `usageDeltas` / `UpdateAgentUsage` pass-through (no arithmetic on cost) | litellm | keep | no action |
| `molecule_rollup.go:38, 53, 72` | `WorkflowSpend`/step/molecule `TotalCostUSD` sums | litellm | keep | no action |
| `molecule_rollup.go:198-209, 215-223` | `addSessionUsage` / `addWorkflowSpend` pure `+=` | litellm | keep | no action |
| `molecule_rollup.go:228-241, 247-259` | `parseSessionUsage` / `parseWorkflowSpend` read bead fields | litellm | keep | no action |

### Cluster: beads-viewer (read-only, keep)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `costs.go:285-291` | parse daily/weekly/total + `HasSpend` from account bead | bead_field | keep | no action |
| `costs.go:289` | `SinceResetSpend = max(total − baseline, 0)` | bead_field | keep | no action |
| `costs.go:74-79` | `CostPerSession() = total/sessions` | bead_field | keep | no action |
| `costs.go:310-313, 331-332, 343, 357-359` | summary aggregates + projections (labeled) | bead_field | keep | no action |
| `costs.go:406-408, 432, 466-467, 501-503` | provider/model rollups, share %, top spenders | bead_field | keep | no action |
| `molecule_cost.go:119-121, 130-131, 136-137` | molecule/step display + filter | bead_field | keep | no action |
| `molecule_rollup.go:150-167, 172` | step/molecule cost = session pool + workflow pool | bead_field | keep | no action |
| `session_usage.go:145-147` | session cost from JSONL `response_cost` | litellm | keep | no action |
| `charts.go:1-33` | references `gardenio_account_cost_usd`; PromQL is server-side | bead_field | keep | no action (corrected from "unknown") |

### Cluster: kbeads server — charts/metrics queries (read-only, keep)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `charts_engagement.go:224, 270-290` | `gardenio_operator_cost_usd_total` PromQL + spend handler | litellm | keep | no action |
| `http_metrics.go:83-94` | `gardenio_beads_total_cost_usd` fleet collector | litellm | keep | no action |
| `store/postgres/queries_fleet_metrics.go:78-88` | SQL `SUM(fields->>'total_cost_usd')` | litellm | keep | no action |
| `charts_catalog.go:120-142` | `spend-by-agent` = `increase(gardenio_agent_spend_usd_total)` | litellm | keep | no action |
| `charts_catalog.go:164-186` | `agent-cost-per-bead` = spend ÷ beads (PromQL division) | litellm | keep | no action |
| `charts_catalog.go:208-234` | `hourly-rate` = `rate(...) * 3600` | litellm | keep | no action |

### Cluster: coop-rust (I3 decision target)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `nats_forward.rs:40-45` | `MessageKind::Usage.response_cost: Option<f64>` | litellm | **decide (I3)** | live, but only ever `None` today |
| `nats_forward.rs:273-276` | `extract_usage` reads `response_cost` from JSONL | litellm | **decide (I3)** | reachable; field never present in Claude Code JSONL |
| `nats_forward.rs:149-161` | publishes `response_cost` only when `Some` | litellm | **decide (I3)** | correct None/zero distinction; never fires today |

### Cluster: iOS Swift (read-only, keep)

| Location | What | Source | Class | Recommendation |
|---|---|---|---|---|
| `Models.swift:516-518` | `totalCostUSD`/`dailyCostUSD`/`weeklyCostUSD` from `BeadFields` | bead_field | keep | no action |
| `Models.swift:524` | `sinceResetSpend = max(total − baseline, 0)` | bead_field | keep | no action |
| `Models.swift:99-101` | `BeadFields` cost schema (`StringOrNumber?`) | bead_field | keep | no action |
| `UsageMonitor.swift:92, 103` | grandTotal sum; hourlyRate (pure client math) | bead_field/none | keep | no action |
| `CostsTab.swift:98, 194, 268, 272, 277` | weekly total + card displays | bead_field | keep | no action |
| `EngagementDashboard.swift:102, 118` | per-operator spend + othersUsd from kbeads `/v1/charts/engagement/spend` | bead_field | keep | source corrected litellm→bead_field (via kbeads chart, not direct) |

---

## 4. Per-Child Change Spec

### I1 — ka-0000000717: expurgate dead non-LiteLLM paths in gb/controller

**Goal:** remove dead plumbing; make the codebase reflect that Writer A + Writer B are the only cost producers.

1. **Delete `gardenio/controller/cmd/controller/spend_forwarder.go` entirely.**
   - The whole file is dead: `handleMessage` drops every event at `:159-161` (`if evt.ResponseCost == nil { return }`) because coop never emits `response_cost` (confirmed §2.1/cluster coop). Nothing downstream depends on it that isn't covered by Writer B's counter increment.

2. **Delete the spend-forwarder startup in `main.go:476-496`.**
   - Remove the comment block (`:476-480`), the `newSpendForwarder(spendForwarderConfig{...})` call, and the `if spendForwarder != nil { go func(){…} }` goroutine.
   - Grep for `resolveSpendAccount` — it was only referenced by the deleted `Resolve` closure (`main.go:486`). If no other caller remains, delete `resolveSpendAccount` too. (Writer B uses `resolveLabels`, a *different* resolver wired at `main.go:546-556`; do not touch it.)
   - Grep for `newSpendForwarder`, `spendForwarder`, `spendForwarderConfig`, `usageEvent`, `accountResolver`, `agentLabel` to confirm no dangling references after file deletion.

3. **Delete `syncAccountSpendFromLiteLLM` and its call sites in `cmd/gb/session.go`.**
   - Remove the function (`:566-606`) and the doc comment (`:318-326`).
   - Remove both invocations at `session.go:323` and `session.go:478`.
   - This hook early-returns in agent pods (no `LITELLM_MASTER_KEY`, `:573-575`) and is fully superseded by controller-side Writer B (documented at `reconcile_session_spend.go:41-44`). If `queryLiteLLMKeyInfo` and the `liteLLMKeyInfoResponse` struct become unused after removal, delete them too — but first grep, because `spend_sync.go` has its *own* `/key/info` path (`querySpend`, `:147-176`); confirm the gb-side helper is distinct and orphaned before deleting.

4. **Fix the misleading comment in `cmd/gb/session_usage.go:100-103`.**
   - Replace with text that states accurately: per-turn `response_cost` *is* read from the session JSONL here and feeds `SessionUsage.TotalCostUSD`; the **authoritative** agent/account totals are written by the controller (`reconcile_session_spend.go` per-session via `/spend/tags`; `spend_sync.go` per-account via `/key/info` + `/spend/logs`), which overwrite this in-pod estimate at close. Do not delete the extraction logic itself — it is live and LiteLLM-sourced.

5. **Sweep for any token×rate math (should find none).** Grep the controller + gb tree for price/rate constants (`per_token`, `* 1e-6`, `pricePerM`, `inputRate`, `costPerToken`, etc.). The audit found zero; if I1 surfaces any, that is the real "other cost method" to expurgate — escalate to T1's guard.

**Do NOT touch:** `spend_sync.go`, `reconcile_session_spend.go`, `metrics/spend.go`, `metrics/metrics.go:358/399`, `main.go:546-556/1034/1282`, any `beadsapi` rollup. All confirmed live LiteLLM.

### I2 — ka-0000003594: viewer + iOS read only LiteLLM fields

**Goal:** assert (and where needed, tighten) that the read-only surfaces consume only the bead fields / chart endpoints from §2.3 — no fallbacks, no client-side estimation.

**beads-viewer (`gardenio/controller/cmd/beads-viewer/`):** Audit found all touchpoints already compliant. Required changes are confirmation + comment hygiene, not behavior:
- Verify `costs.go:285-291` is the *only* ingress of cost into `costAccountView`; ensure no estimated fallback exists when fields are absent (a missing field must yield `0`, which then sets `HasSpend=false`). Keep `HasSpend` gating so empty accounts render no spend rather than a fabricated one.
- Keep projections at `costs.go:331-332, 357-359` but ensure their labels/help text say "projection" (they already do at the cited comments). No token math present.
- `charts.go:1-33` references metric names only; PromQL lives in the kbeads E5 catalog. No change.

**iOS (`usr/Gardenio/Gardenio/`):** All cost reads are from `BeadFields` (`Models.swift:99-101, 516-524`) or the kbeads chart endpoint (`EngagementDashboard.swift:102/118`). Required changes:
- Confirm `BeadFields` decoding tolerates string-or-number (`StringOrNumber?`) and missing → `0` (`Models.swift:516-518`), so an account with no LiteLLM spend shows `$0`, never an estimate.
- Keep `hourlyRate` (`UsageMonitor.swift:103`) — pure wall-clock division on bead-sourced totals, no embedded price.
- Correct any stale code comment that calls `EngagementDashboard` spend "from LiteLLM directly" — its actual source is kbeads `/v1/charts/engagement/spend`, which aggregates LiteLLM-rooted metrics server-side.
- Sweep iOS for hardcoded USD constants / token-rate tables — audit found none; assert via review.

**Net:** I2 is largely a *verification + comment-accuracy* task; the surfaces are already LiteLLM-only. The deliverable is (a) confirmation no estimation fallback exists, and (b) the read-only contract from §2.3 is documented at each ingress point.

### I3 — ka-0000002983: coop cost decision (forward `response_cost` vs stay cost-free)

**The decision to make:** coop's `response_cost` plumbing (`nats_forward.rs:40-45, 149-161, 273-276`) is *correct and live* but only ever produces `None`, because the Claude Code driver never writes `response_cost` into the session JSONL. The downstream consumer (`spend_forwarder.go`) is being deleted in I1. So after I1, this coop plumbing forwards a field nobody reads.

**Two options:**

- **Option A — Stay cost-free (recommended default):** Remove coop's `response_cost` extraction/publish so coop carries no cost concept. Concretely: drop `response_cost` from `MessageKind::Usage` (`:44`), drop the extraction (`:273-276`), drop the conditional publish (`:157-161`), and update the field comments. Rationale: the authoritative per-session cost is Writer B (`/spend/tags` reconcile), which does not need a per-turn coop signal; keeping a never-populated field is the exact kind of dead plumbing this epic exists to remove. The None/zero-distinction safety machinery exists *only* to feed the forwarder we are deleting.

- **Option B — Keep forwarding (only if per-turn real-time spend is a near-term roadmap item):** Retain coop's plumbing AND rebuild a live consumer. This requires (1) the Claude Code driver to actually emit `response_cost` into JSONL (today it does not — confirmed at `nats_forward.rs:270-272` comment and the audit), and (2) re-introducing a forwarder→counter path. This is explicitly out of scope per `reconcile_session_spend.go:50-51` ("Per-turn real-time resolution is out of scope — would need a proxy callback / audit option A").

**Recommendation: Option A.** Delete coop's `response_cost` plumbing. There is no consumer after I1, Claude Code does not produce the field, and per-turn resolution is out of scope. If/when per-turn spend becomes a priority, it should be re-introduced as a deliberate feature with a working producer, not kept alive speculatively. **This must be decided by a human owner before T1/REL** — flag it explicitly (see §7).

If Option A is chosen, the coop change is independent of I1/I2 and can land in parallel; T1's guard should then assert coop emits no `response_cost`.

---

## 5. Surfaces Losing Their Only Cost Source

After I1 (delete dead paths) and I3-Option-A (coop cost-free), audit the surfaces that *appeared* to depend on a removed path:

| Surface | Apparent dependency removed | Decision | Justification |
|---|---|---|---|
| `gardenio_agent_spend_usd_total` counter | per-turn `spend_forwarder` increment | **No loss — keep, no $0/unavailable change** | The counter is *already* driven solely by Writer B (`reconcile_session_spend.go:207-221`); the forwarder never incremented it (dropped 100% of events). Deleting the forwarder changes nothing about the series. |
| Account bead `total_cost_usd` at session close | gb-side `syncAccountSpendFromLiteLLM` near-real-time top-up | **Report from Writer A only (accept ≤5-min staleness)** | The gb hook was dead in agent pods (no master key). The account total is authoritatively refreshed by `spend_sync` every 5 min (`spend_sync.go`). The only thing lost is a *theoretical* sub-5-min top-up that never actually ran. No new gap is introduced. |
| Per-turn / real-time spend granularity | coop `response_cost` forwarding (I3-A) | **Report at session-close granularity (the existing reality)** | coop never populated `response_cost`, so there was never per-turn data. Session-close reconcile is and remains the finest real granularity. Surfaces showing "spend over time" already use `increase(gardenio_agent_spend_usd_total)` which is fed at close. No surface needs to switch to "$0" or "unavailable". |
| A session that genuinely incurred **zero** LiteLLM spend | n/a | **Report `$0`, never an estimate** | `RecordSpend` drops non-positive (`metrics/spend.go:59-61`); `reconcileAtClose` leaves `total_cost_usd` untouched if the agent tag is absent (`reconcile_session_spend.go:172-176`), so the field reads `0`/absent → viewer `HasSpend=false`, iOS `?? 0`. This is correct: absence of LiteLLM spend = $0, not a fabricated number. |

**Conclusion:** No surface loses a *real* cost source — every removed path was dead. No surface needs to be switched to "unavailable" or LiteLLM-backfilled. The single behavioral guarantee to preserve is: **no-LiteLLM-spend ⇒ `$0`, never an estimate.**

---

## 6. Test / Guard Requirements (T1 — ka-0000008138)

T1 is blocked by I1/I2/I3. Concrete assertions and where they live:

**G1 — No token×rate cost math anywhere (the core guard).**
- A repository-level guard test (Go test in `gardenio/controller/internal/metrics/` or a dedicated `cost_guard_test.go`) that greps the controller + gb + beadsapi source for forbidden patterns: token-price constants and multiplications (`costPerToken`, `pricePerM`, `per_1k_tokens`, `inputTokens *`, `* rate`, hardcoded `0.000…` USD literals adjacent to token counts). Fails the build if any match. Mirror with a `cargo test`/CI grep guard in `coop/` and a lint/CI grep in the iOS + viewer trees.
- Rationale: this is the literal "no other cost method may ever return" enforcement. It must live in CI so a future PR cannot silently reintroduce an estimator.

**G2 — Cost derives only from LiteLLM bead fields / response_cost.**
- `session_usage_test.go`: assert `parseSessionUsage` returns `TotalCostUSD == 0` for JSONL with no `response_cost` entry (no fallback estimate) — extend existing `TestParseSessionUsage_BasicCounts`. Assert it sums only `response_cost`/`_litellm_response_cost` (existing `TestParseSessionUsage_LiteLLMResponseCost`, e.g. 0.25+0.10=0.35).
- `reconcile_session_spend_test.go`: assert `sessionCost = closeTotal − baseline`, clamped ≥0, written to `total_cost_usd`; assert counter increments exactly once (re-delivered close events do not double-increment — covered by `firstReconcile` at `:194`).
- `molecule_rollup_test.go` (beadsapi): assert molecule total = session pool + workflow pool, both read from bead fields, no recomputation (`molecule_rollup.go:172`).

**G3 — A no-LiteLLM-spend session reports $0, not an estimate (the headline behavior).**
- `reconcile_session_spend_test.go`: when `queryTagSpend` returns `found=false`, `total_cost_usd` is left untouched and the counter is NOT advanced (`reconcile_session_spend.go:172-176`).
- `metrics/spend_test.go`: `RecordSpend(..., 0)` and `RecordSpend(..., -x)` return `false` and do not advance the counter (`metrics/spend.go:59-61`).
- beads-viewer test (`costs_test.go`): an account bead with no cost fields yields `TotalCostUSD==0`, `HasSpend==false`, and renders `$0` — no projection or estimate fabricated.

**G4 — Dead-path deletion is real (regression lock).**
- A test/CI grep asserting `spend_forwarder.go` does not exist and `syncAccountSpendFromLiteLLM` / `newSpendForwarder` symbols are absent. Prevents reintroduction.
- If I3-Option-A: a `coop` test asserting a usage NATS payload never contains a `response_cost` key (or that `MessageKind::Usage` has no such field).

**G5 — kbeads charts are pure PromQL reshaping.**
- `charts_catalog_test.go` (kbeads): assert the three cost chart templates reference only `gardenio_agent_spend_usd_total` / `gardenio_operator_cost_usd_total` and apply only `sum`/`rate`/`increase`/division/scalar-multiply — no embedded constants that look like prices.

---

## 7. Open Questions / Risks

1. **I3 decision needs a human owner.** The audit can prove the coop `response_cost` path is currently dead-ending, but cannot decide product intent. If per-turn real-time spend is on the roadmap, Option B (keep + build a producer) is warranted; otherwise Option A (delete). **Recommend Option A; block REL until confirmed.** This is the single genuinely undecided item.

2. **`/spend/tags` lifetime-cumulative assumption.** Writer B relies on exactly one coop session per agent pod so the agent-tag delta over pod lifetime equals session cost (`reconcile_session_spend.go:33-39`). If a pod ever hosts multiple sessions, the delta attributes all spend to the close event. Out of scope for this epic but worth a note in the guard/runbook.

3. **`queryLiteLLMKeyInfo` orphaning.** I1 step 3 may orphan the gb-side `queryLiteLLMKeyInfo` helper. Must grep before deleting — `spend_sync.go` has an independent `/key/info` path; do not delete the controller's copy.

4. **5-minute account staleness is now the only account-level top-up.** With the gb-side close-time sync removed, account `total_cost_usd` freshness is bounded by `spend_sync`'s 5-min loop. This was already the effective behavior (the gb hook was dead), but it should be documented so operators don't expect sub-5-min account accuracy.

5. **iOS/viewer "no estimation fallback" is asserted by review, not yet by a compiled test in those trees.** G1/G3 cover Go; the Swift and JS surfaces rely on a CI grep guard. If those trees lack a CI grep step, T1 must add one, or the guarantee is review-only for those surfaces.

6. **Misleading-comment fix (I1 step 4) is behavior-neutral but load-bearing for future audits.** The current `session_usage.go:100-103` comment conflates the in-pod JSONL estimate with the authoritative controller sources; leaving it would re-seed the confusion this epic is resolving. Treat the comment fix as a required deliverable, not optional cleanup.
