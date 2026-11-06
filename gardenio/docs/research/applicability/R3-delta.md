# R3 applicability delta — per-agent LLM spend attribution

> **Task:** ka-0000007126 · **Parent:** ka-0000001798 (E0) · **Umbrella:** ka-0000007704
> **Re-validates:** `gardenio/docs/research/timeseries-spend-attribution.md` (ka-0000001029, 2026-04-25)
> **Verified against:** tip-of-main `755d74f69` (release 2026.149.12, 2026-05-29)
> **Method:** every cited file re-read at tip-of-main; line numbers below are *current*, not the 2026-04-25 numbers.

## TL;DR

The R3 research **still holds in its essentials**: LiteLLM is the authoritative cost
meter, the per-agent attribution chain (session bead → agent bead → Prometheus) is
intact, the session-close vmagent push works, and the **central gap is unchanged** —
coop forwards neither `model` nor `response_cost` on the NATS `usage` event or its
`/metrics` endpoint. The proposed `spendforwarder` component remains unbuilt.

**One material drift changes the shortest path (for the better):** the research said
"no NATS consumer of `messages.*.usage` exists today." That is **no longer true**. The
slack-bridge `hookstream` now subscribes to `coop.events.messages.>` and routes `usage`
events into `AgentStreamManager.HandleMessageEvent` (live agent-card token display). The
coop→controller usage pipe is therefore **already live and already consumed in
production** — it just throws the data away after formatting a display string. The v1
deliverable shrinks accordingly: we no longer need to *stand up* a forwarder/subscriber;
we extend the payload at the coop end and teach the existing consumer to increment a
counter.

## 1. Claims still holding

| # | Claim (2026-04-25) | Verdict | Current evidence |
|---|---|---|---|
| 1 | Every JSONL `result`/`assistant` entry carries `response_cost`/`_litellm_response_cost`; `session_usage.go` sums it. | **HOLDS** | `gardenio/controller/cmd/gb/session_usage.go` — `extractResponseCost()` checks both keys; accumulated in `parseSessionUsage`. No in-house pricing. |
| 2 | `GET /key/info?key=<vk>` → `info.spend`; `/spend/logs` for daily/weekly; `spend_sync.go` runs a 5-min loop. | **HOLDS** | `gardenio/controller/cmd/controller/spend_sync.go` — `querySpend()` hits `/key/info`, `queryPeriodSpend()` hits `/spend/logs`; `RunLoop` ticker = `5 * time.Minute`. |
| 3 | No in-house pricing table; controller never multiplies tokens × $/token. | **HOLDS** | Grep across `controller/**.go` finds zero `tokens × price` arithmetic. LiteLLM owns pricing. |
| 4 | Session bead carries agent/agent_bead_id/project/role/total_cost_usd/tokens/counts. | **HOLDS** | `internal/beadsapi/client_session.go` — `SessionFields` + `SessionUsage` structs; `CloseSession` writes them via `UpdateBeadFieldsTyped`. (`ThinkingTokens` was added — see drift.) |
| 5 | Atomic `IncrementBeadFields` updates agent/account bead totals (no RMW race). | **HOLDS** | `client.go` `IncrementBeadFields` → `POST /v1/beads/{id}/increment`; driven by `UpdateAgentUsage`/`UpdateAccountUsage` via `usageDeltas`. |
| 6 | Controller scrapes agent beads → `gardenio_agent_cost_usd{agent,project,role}`, `agentLabels=["agent","project","role"]`. | **HOLDS** | `internal/metrics/metrics.go` — `agentLabels` unchanged; `collectAgents()` emits the full `gardenio_agent_*` gauge family. |
| 7 | `account` is NOT a session-bead field; account resolved by project via `FindAccountBeadIDForProject`. | **HOLDS** | `client_session.go` — no `account` field on `SessionFields`; `FindAccountBeadIDForProject` resolves project→account at close time. |
| 8 | Session-close push: `session_metrics_push.go` POSTs `/api/v1/import/prometheus` with `gardenio_session_cost_usd{agent_name,project,account,session_id}` + token/duration/count series. | **HOLDS** | `cmd/gb/session_metrics_push.go` — `pushSessionMetrics` builds the exact metric/label set; wired into `closeSessionBead` and `runSessionClose`. |
| 9 | **Central gap:** neither coop `/metrics` nor the NATS `usage` event carries `model` or `response_cost`. | **HOLDS** (intensified — see drift) | `coop/crates/cli/src/transport/nats_forward.rs` `extract_usage()` and `http/metrics.rs` carry tokens + duration only; grep for `response_cost`/`model` across `coop/crates/` → none. |
| 10 | `litellm_aliases.go` is the canonical model-name normalizer (`model_group_alias`). | **HOLDS** | `cmd/controller/litellm_aliases.go` `claudeCodeModelAliases`; synced at startup via `syncLiteLLMModelAliases()`. Still the right normalization point. |
| 11 | vmagent scrapes the controller `/metrics` and coop pods via `prometheus.io/scrape`; controller has a Prom registry a new counter can attach to. | **HOLDS** | `templates/vmagent/configmap.yaml` `kubernetes-pods` job keeps `prometheus.io/scrape=true`; controller deployment annotates port 9092 `/metrics`; `main.go` registers `metricsCollector`. |
| 12 | Dashboard ships the buggy `rate(gardenio_session_cost_usd[1h])*3600` panel. | **HOLDS** (still buggy) | `helm/gardenio/dashboards/cost-overview.json` — "Cost Rate" panel still `rate()`s a one-shot session-close sample. No `gardenio_agent_spend_usd_total` panel pre-added. |
| 13 | `spendforwarder` component is unbuilt. | **HOLDS** | No `internal/spendforwarder/` (or any `*spend*` package) exists. |

**Net: 13/13 of the research's load-bearing claims survive re-verification.** None are
BROKEN. The recommendation architecture (Option C hybrid) is still sound.

## 2. Drift since 2026-04-25

Ordered by impact on the v1 path.

### D1 — A live NATS `usage` consumer now exists (HIGH impact, *helps us*)

The research's risk #1 said *"No consumer exists today — verified by grepping
`messages..*\.usage`."* **That is now false.**

- `internal/bridge/hookstream.go` subscribes to `coop.events.messages.>` and dispatches
  to `handleMessageForward` → `AgentStreamManager.HandleMessageEvent`.
- The `messageEvent` struct (`hookstream.go`) already parses `input_tokens`,
  `output_tokens`, `cache_read`, `cache_write`, `duration_ms` and the `k8s` metadata
  block — i.e. the whole `usage` payload **except** `model`/`response_cost`.
- `agentstream.go` `HandleMessageEvent` `case "usage"` calls `formatUsageSummary(...)`
  and emits a `"12K in / 8K out · 3.4s"` display string to the Slack agent card. **It
  persists nothing and discards model/cost.**

Implication: the coop→controller-bridge usage pipe is **production-live and load-bearing
for the agent card**. We do not need to build a subscriber. We need to (a) widen the
payload at coop, (b) widen the `messageEvent` struct, and (c) add a counter increment
next to the existing `formatUsageSummary` call. This also relocates the recommended
forwarder home: the research floated "controller vs kbeads vs new service"; the de-facto
answer is **the slack-bridge `hookstream`/`agentstream` path that already owns this
subscription** (it lives in the controller module, so the Prom-registry argument still
holds — but the subscription is the bridge's, not a fresh controller subscriber).

> Caveat: confirm the production NATS subject prefix. coop builds `{prefix}.messages.…`;
> `hookstream` listens on `coop.events.messages.>`, so the deployed `prefix` is
> `coop.events`. R2/implementer should pin this against the running Helm values, not
> assume.

### D2 — `cost_usd` was explicitly *removed* from coop, not merely "never added" (MEDIUM)

The research framed the gap as "coop has the data but doesn't forward it." Sharper truth:
commit `5b73b0e0e` (2026-04-11, *before* the research date) **removed** a `cost_usd`
field that previously existed in `MessageKind::Usage`, in `metrics.rs`
(`gardenio_session_cost_usd` gauge), and in `extract_usage()`. So re-adding cost to coop is
a *revert-and-extend*, not net-new — there is prior art for the exact wiring, and a prior
reason it was pulled (cost tracking was deliberately centralized in the controller via the
LiteLLM spend API). Re-introducing it as a counter source must not re-create whatever
double-count/ownership concern motivated the removal — coordinate with whoever owns
`5b73b0e0e`. `model` was *never* extracted by coop; that part is genuinely net-new (~1 line
in `extract_usage`).

### D3 — `BOAT_ACCOUNT` env var removed; account is now dynamic (MEDIUM)

`podspec.go` documents that `BOAT_ACCOUNT` was removed — coop now learns its account
name dynamically via `POST /api/v1/credentials/reload` (`account_name` field) from
coopmux credential distribution. The research assumed the metric's `account` label is
read from `BOAT_ACCOUNT` env at session-close push. **That env var is gone.** Two
consequences:

- The session-close push (`session_metrics_push.go`) gets `account` via
  `FindAccountBeadIDForProject` (project→account), not env — already correct, unaffected.
- For the **live counter's** `account` label, coop must source the account from its
  active credential profile (`profile.active_name()` — already used by `http/metrics.rs`
  for the `account` label on the existing token gauges), **not** from `BOAT_ACCOUNT`.
  The research's "read from `BOAT_ACCOUNT`" instruction is stale; use the profile name.

### D4 — `shadow_*` cost fields on account beads (LOW, POC-only)

Commit `f970487de` (2026-05-28) adds `shadow_total_cost_usd`, `shadow_daily_cost_usd`,
`shadow_weekly_cost_usd` to account beads in `spend_sync.go`, mirroring the canonical
fields for manual reconciliation against the Anthropic Console (POC decision D-h3). Same
source (LiteLLM `querySpend`/`queryPeriodSpend`), 5-min cadence unchanged. **Does not
alter the cost-source story** — informational only. The reconciliation backstop the
research recommended already has a sibling here.

### D5 — `ThinkingTokens` added but not pushed (LOW)

`SessionUsage` gained a `ThinkingTokens` field (tracked in `usageDeltas`), but
`buildPrometheusPayload` does *not* emit a `gardenio_session_tokens_total{type="thinking"}`
series. Minor existing gap; when adding per-model token counters, include `thinking` for
completeness. Not a blocker.

### D6 — model alias churn (LOW, expected cadence)

`b3d7511cd` dropped stale dated sonnet/opus-4.6 IDs; `c07aac1c8` wired Opus 4.8. The
`claudeCodeModelAliases` map now covers sonnet-4-6, haiku-4-5, opus-4-6/4-7/4-8 (→ their
`-oauth`/`anthropic/` upstreams). Normalization is still needed and still feasible at the
same point. Risk #3 (raw-ID vs alias splitting a model across series) is unchanged —
normalize at emission using this map.

## 3. Shortest path now to `gardenio_agent_spend_usd_total`

Smaller than the research estimated, because the subscriber half of the forwarder already
exists (D1). Concrete, ordered:

1. **coop — widen the `usage` payload** (`coop/crates/cli/src/transport/nats_forward.rs`):
   in `extract_usage()`, also pull `model` (top-level or `message.model`) and
   `response_cost` (or `_litellm_response_cost`) from the JSONL turn and add them to the
   published `usage` JSON. This is a revert-and-extend of `5b73b0e0e` for cost + ~1 new
   line for model. Skip the field silently when absent (mock/claudeless turns) — risk #2
   unchanged. **~30 LOC + tests.**
2. **coop — mirror to `/metrics`** (`coop/crates/cli/src/transport/http/metrics.rs`):
   re-add a `gardenio_session_cost_usd` and add per-`model` gauges so the scrape path
   covers the live case independent of NATS. Use `profile.active_name()` for the
   `account` label (per D3). **~40–50 LOC.** *(Optional for a first cut — NATS path alone
   yields the counter; do this for scrape-path redundancy.)*
3. **controller/bridge — widen the consumer struct** (`internal/bridge/hookstream.go`):
   add `Model string` and `ResponseCost float64` to `messageEvent`.
4. **controller/bridge — increment a counter** (`internal/bridge/agentstream.go`, the
   existing `case "usage"`): alongside `formatUsageSummary`, increment a new
   `gardenio_agent_spend_usd_total{agent,project,role,model,account}` **monotonic
   `CounterVec`** (registered in `internal/metrics/`, exposed on the controller's existing
   port-9092 `/metrics` registry — already vmagent-scraped per claim 11). Normalize
   `model` through `litellm_aliases.go` at increment time (D6). Skip zero-cost turns.
   Mirror token counters (`gardenio_agent_*_tokens_total{…,model,account}`) for consistency.
   **~40–60 LOC + tests.**
5. **Query/dashboard:** `sum by (agent)(increase(gardenio_agent_spend_usd_total[1h]))`;
   replace the buggy `rate(gardenio_session_cost_usd[1h])*3600` panel (claim 12 / risk in
   research). **R2 dashboard work.**
6. **Backfill (unchanged):** one-shot `gb session backfill-metrics --since Nd` replaying
   closed session beads into `/api/v1/import/prometheus` with backdated timestamps. Do
   **not** backfill the live counter from `/spend/logs` (double-counts).

**Reconciliation backstop:** keep `spend_sync.go` (5-min, account-level) as the drift
check; the new `shadow_*` fields (D4) already give a manual audit anchor. No automatic
correction.

**Cardinality:** unchanged from research — agents × models (× account) ≈ ≤1.6k–4.8k
series. R2 owns the formal budget and the CI guard against a future `task_id`/`session_id`
label footgun (risk #7).

**Counter-reset correctness (risk #4):** the counter is now in the **bridge** process
(slack-bridge), which restarts independently of the controller. Use `NewCounterVec`
(monotonic, never `.Set()`); `increase()` handles resets. If the bridge's restart cadence
is a concern, host the counter in the controller proper and have the bridge's
`HandleMessageEvent` call into it — but since the subscription already lives in the
bridge, co-locating the counter there is the least-surgery option. Implementer's call;
flag for R2/synthesis.

## 4. Updated decision-point recommendation

**Unchanged in direction, sharpened in detail. Proceed with Option C (hybrid).** The
research's three open decisions (ka-0000000386) get updated answers:

- **Where does the NATS→counter forwarder live?** *Resolved by reality:* the
  subscriber already exists in the **slack-bridge `hookstream`/`agentstream`** path
  (controller Go module). Reuse it; do not build a new service or a parallel controller
  subscriber. Host the `CounterVec` either in that path or in `internal/metrics/` and
  call into it — both expose on the already-scraped controller/bridge `/metrics`.
- **Keep/deprecate/rename the bead-derived `gardenio_agent_cost_usd` gauge?** Still an R2
  call. Recommend keeping it as a lifetime gauge (rename `…_lifetime_cost_usd` later) and
  letting the new counter own time-series queries — they answer different questions.
- **Backfill CLI shape (`gb session backfill-metrics` vs script)?** Either; align with
  whatever R2's instrumentation plan picks for other backfills.

**New decision surfaced by this delta (for synthesis / R2):**
- *Re-adding cost to coop reverts `5b73b0e0e`, which removed it deliberately.* Confirm
  the original removal rationale (centralize-cost-in-controller) does not conflict with
  re-emitting cost as a **counter increment** (not a gauge the controller also derives).
  If there is genuine concern about coop re-owning cost, the alternative is to forward
  only `model` from coop and have the **controller** join `response_cost` from the
  session JSONL it already parses (`session_usage.go`) — but that loses live (per-turn)
  cost and reverts to session-close granularity, defeating the live-counter goal. Net
  recommendation: **re-emit `response_cost` from coop** as a forward-only signal; the
  controller remains the authority via `spend_sync` reconciliation (D4 shadow fields are
  the audit anchor).

**Risk register delta:** research risks #2 (missing cost), #3 (model normalization),
#4 (counter resets), #7 (cardinality) all still apply. Risk #1 (no consumer / schema
break) **flips**: a consumer now exists, so the schema is no longer free to change
blindly — additive fields are safe, but verify `messageEvent` JSON-unmarshal tolerance
(it already uses `omitempty`, so additive is fine) and coordinate with the agent-card
owner before touching the `usage` event shape.

---

*Verification fan-out: 5 independent code-reading agents (one per claim cluster) +
direct spot-checks of the coop NATS gap and the `hookstream`/`agentstream` usage
consumer. All file paths and identifiers above were read at tip-of-main `755d74f69`.*
