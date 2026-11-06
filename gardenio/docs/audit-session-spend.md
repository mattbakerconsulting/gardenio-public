# Audit: session-level spend/cost metrics & charts

**Bead:** ka-0000006509 · **Operator:** Matt Baker · **Date:** 2026-06-06

> "all the spend stuff in gardenio at the session level seems to be wrong"

This audit traces every session-level cost path from source (the Claude Code
session JSONL) through to the surfaces that render it (session beads, agent
beads, `/costs`, the dashboard, Prometheus metrics, charts), and identifies
where the numbers are wrong and why.

## TL;DR

**Session-level cost (`total_cost_usd`) is structurally always `$0`.** The JSONL
parser sources cost *exclusively* from a `response_cost` field that Claude Code
never writes into the session log. LiteLLM returns per-request cost in an HTTP
**response header** (`x-litellm-response-cost`), not in the JSON body, so it
never lands in the JSONL that `parseSessionUsage` reads. Every downstream
session surface that depends on that value — session beads, the per-agent
`total_cost_usd` rollup, the agent card cost line, `gardenio_session_cost_usd`,
`gardenio_operator_cost_usd_total`, molecule cost rollups, and the
`agent_cost_usd` gauge — therefore reports `$0` or omits the cost entirely.

The **only** correct cost surface is **account-level**, which is sourced
independently from LiteLLM's `/key/info` + `/spend/logs` HTTP API by the
controller's `spendSync` daemon (`spend_sync.go`). That path does not touch the
JSONL and is accurate.

## How cost is *supposed* to flow

```
Claude Code session ──(JSONL on disk)──> parseSessionUsage (session_usage.go)
                                              │  reads usage{} + response_cost
                                              ▼
                                         SessionUsage{TotalCostUSD}
                       ┌──────────────────────┼───────────────────────────┐
                       ▼                       ▼                           ▼
               session bead             agent bead                   vmagent push
            (CloseSession)        (UpdateAgentUsage,            (pushSessionMetrics:
          total_cost_usd=…      total_cost_usd += …)        gardenio_session_cost_usd,
                                                          gardenio_operator_cost_usd_total)
                       │                       │
                       ▼                       ▼
              molecule rollup          agent_cost_usd gauge
          (RollupMoleculeUsage)        (metrics.go collector)
                                              │
                                              ▼
                                       agent card cost line
                                      (formatCostSummary)
```

Account cost flows on a **separate, healthy** path:

```
LiteLLM /key/info + /spend/logs ──> spendSync.syncAll (spend_sync.go, 5-min loop)
   ──> account bead {total_cost_usd, daily_cost_usd, weekly_cost_usd}
   ──> /costs page, dashboard fleet cost line, gardenio_account_cost_usd gauge
```

## Findings

### F1 — CRITICAL: session cost is always `$0` (cost source absent from JSONL)

`accumulateUsage` (`cmd/gb/session_usage.go:104-148`) only adds to
`usage.TotalCostUSD` when `extractResponseCost` finds `response_cost` or
`_litellm_response_cost` in a JSONL entry. The doc-comment asserts "All API
traffic routes through LiteLLM, which … injects response_cost into the session
JSONL."

**This assertion is false.** Verified against a live production session JSONL
(opus-4-8, 46 usage-bearing turns): the persisted `message.usage` blocks contain
`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
`cache_read_input_tokens` — **and no cost field of any kind**. LiteLLM surfaces
per-call cost via the `x-litellm-response-cost` HTTP response header; Claude Code
does not persist response headers into the JSONL, so the field the parser keys on
is never present.

**Blast radius (all read `total_cost_usd` derived from this path):**
- session beads (`CloseSession`, `client_session.go:97`)
- per-agent rollup (`UpdateAgentUsage` → `total_cost_usd` field)
- agent card cost line (`formatCostSummary`, `bot_agents.go:2288` — silently
  omits `$…` because `f > 0` is false)
- `gardenio_session_cost_usd` + `gardenio_operator_cost_usd_total` pushed to
  vmagent (`session_metrics_push.go:122,148`) — **resolved (ka-0000000999):** the
  gb push no longer emits either cost series. `gardenio_session_cost_usd` is dropped
  entirely (its only consumer was the broken Grafana panel, fixed under
  ka-0000005397); `gardenio_operator_cost_usd_total` is now emitted authoritatively
  by the controller's session-close reconciler (`reconcile_session_spend.go`) from
  the LiteLLM-reconciled session cost, as a real monotonic counter rather than the
  low-biased, last-write-wins estimate.
- `agent_cost_usd` Prometheus gauge (`metrics.go:358`)
- molecule cost rollup `SessionUsage.TotalCostUSD` half (`molecule_rollup.go`)
- `gb session list` / `gb session show` / `gb session analyze` cost columns

The token counts on these surfaces are correct; only **cost** is broken.

### F2 — coop's NATS spend forwarder is broken by the same root cause

`extract_usage` (`coop/.../nats_forward.rs:270-289`) reads `response_cost` from
the same JSONL stream. Because the field is absent, the published usage event's
`response_cost` is always `None`, so `spendForwarder.handleMessage`
(`spend_forwarder.go:159`) **drops every event** ("A turn with no response_cost
… drop it"). Result: `gardenio_agent_spend_usd_total` (the E3 counter) never
increments, so the three spend charts (`spend-by-agent`, `hourly-spend-rate`,
`agent-cost-per-bead`) render permanently empty — exactly the "honest empty
state" the chart code anticipated, but for the wrong reason (no producer, not no
data).

### F3 — session-close LiteLLM sync writes ACCOUNT cost, not session cost

`syncAccountSpendFromLiteLLM` (`session.go:566`) correctly pulls authoritative
spend from `/key/info` at session close — but writes it to the **account** bead's
`total_cost_usd`, never back to the just-closed **session** bead or the agent
rollup. So even though an authoritative number is fetched at exactly the right
moment, the session/agent surfaces don't benefit. The hook to fix session cost
is already present; it just isn't wired to the session.

### F4 — `thinking_tokens` is dropped on the session bead write path

`CloseSession` (`client_session.go:92-102`) writes `input/output/cache_*` but
**omits `thinking_tokens`**, even though `parseSessionUsage`/`countThinkingTokens`
computes it and `usageDeltas` (agent rollup) *does* include it. So per-session
`thinking_tokens` is always absent on the session bead, while the agent rollup
has it — an inconsistency between the two surfaces. (Lower severity; tokens, not
cost.)

> **Resolved (ka-0000003128).** `CloseSession` now writes `thinking_tokens` onto
> the session bead, and a `thinking_tokens` integer field was added to the
> per-session type schema in both `kbeads/internal/server/config.go` and the
> bridge bootstrap (`bridge/init.go`) so the typed-config write validator
> accepts it. Covered by `TestCloseSessionWithUsage`.

### F5 — thinking-token estimate is a chars/4 heuristic

`countThinkingTokens` (`session_usage.go:231-253`) estimates thinking tokens at
`len(text)/4`. This is a rough BPE approximation, not a measured value, and is
double-counted conceptually (the Anthropic API already includes thinking inside
`output_tokens`). Worth documenting/labeling as an estimate so it isn't summed
into "real" token totals as if authoritative.

> **Resolved (ka-0000003128).** The estimate is now labeled at every surface:
> the `gb molecule` CLI prints `thinking(est)`, the beads-viewer molecule-cost
> page renders `thinking (est)` (with a tooltip), the Slack agent card prints
> `(… thinking est.)`, and the `SessionUsage.ThinkingTokens` field +
> `countThinkingTokens` doc comments warn that it is an estimate already counted
> inside `output_tokens` and must not be summed into authoritative totals.

### F6 — `CostPerSession` / cost-per-session mixes daily cost with lifetime sessions

`costAccountView.CostPerSession` (`costs.go:70`) divides `DailyCostUSD` by
`SessionCount`, but `session_count` on the account bead is a **lifetime**
counter (incremented once per session close, never reset), while `daily_cost_usd`
is a 1-day window. Dividing a daily numerator by a lifetime denominator yields a
meaningless unit-economics number that shrinks over time regardless of actual
per-session cost. Same class of mismatch in `costsSummaryView.CostPerSessionUSD`
(`costs.go:330`).

### F7 — MTD/projection math is a rolling-7d proxy presented as month-to-date

`buildCostsSummary` (`costs.go:332-345`) computes `MTDEstimateUSD` as
`(WeeklyTotalUSD/7) * dayOfMonth`. This is labeled "Month-to-date" in the view
model but is actually a rolling-7d-average extrapolation — it will diverge
sharply from true MTD whenever spend is non-uniform (e.g. a spike 10 days ago is
invisible). The code comments acknowledge this; the audit flags it as a
correctness/labeling issue for the charts surface, not just an internal note.

## Severity ranking

| # | Finding | Severity | Surface |
|---|---|---|---|
| F1 | Session cost always $0 (response_cost absent) | **Critical** | all session/agent cost |
| F2 | NATS spend counter never increments | **High** | spend charts |
| F3 | LiteLLM session-close sync writes account, not session | **High** | session/agent cost |
| F4 | thinking_tokens dropped on session bead | Medium | session token detail |
| F6 | cost-per-session mixes daily ÷ lifetime | Medium | /costs unit economics |
| F7 | MTD is a 7d proxy mislabeled | Low | /costs projections |
| F5 | thinking estimate is chars/4 heuristic | Low | token detail |

## Recommended resolution shape (for the epics)

1. **Fix the cost source (F1/F2/F3).** Options, in preference order:
   - **(a) Capture cost at the proxy.** Have coop/LiteLLM persist the
     `x-litellm-response-cost` header value into the JSONL (or a sidecar) so
     `parseSessionUsage` has a real per-turn cost to sum. This is the cleanest —
     it fixes session beads, agent rollup, the NATS counter, and the charts in
     one move.
   - **(b) Authoritative reconciliation at session close.** Extend
     `syncAccountSpendFromLiteLLM` to query `/spend/logs` scoped to the session's
     time window (or LiteLLM request tags) and write the real cost back to the
     **session** bead + agent rollup (fixes F3 directly, F1 indirectly).
   - Pick one as source-of-truth; don't keep two estimators that disagree.
2. **Wire thinking_tokens into CloseSession (F4).** One-line parity fix.
3. **Correct the derived unit-economics (F6) and projection labeling (F7)** on
   `/costs` — either compute against matching windows or relabel honestly.
4. **Label the thinking-token estimate (F5)** as an estimate everywhere it's
   surfaced.

Each numbered item becomes a child task under the resolution epic; the audit
itself (this doc) is the first epic's deliverable.
