# Opus-4-8 → Opus-5 cost/quality validation (retrospective)

Bead: `ka-0000002221` · Epic: `ka-0000000949` (Opus-5 cutover + model-switch/refusal
observability) · Cutover boundary: **2026-08-09** · Analysis run: 2026-08-28

## Verdict: **PASS — no material regression**

Every measured cost and quality dimension is **flat-or-improved** after the account-default
flip from `claude-opus-4-8-oauth` to `claude-opus-5-oauth`. No decision was filed because no
regression was found.

| Dimension | Before (opus-4-8) | After (opus-5) | Δ | Assessment |
|---|---|---|---|---|
| Task success rate (done / terminal) | 99.45% (5240/5269) | 99.81% (7907/7922) | +0.36pp | ✅ improved |
| Failure rate (`agent_state=failed`) | 0.204% | 0.174% | −0.03pp | ✅ flat/better |
| Killed rate (`agent_state=killed`) | 0.333% | 0.012% | −0.32pp | ✅ improved |
| Cost per session (mean `litellm_spend_close`) | $4.05 | $3.03 | −25.1% | ✅ cheaper |
| Cost per session (median) | $1.09 | $0.83 | −24.0% | ✅ cheaper |
| Cost per session (p90) | $12.27 | $7.45 | −39.3% | ✅ cheaper |
| Cost per turn (mean) | $0.181 | $0.157 | −13.3% | ✅ cheaper |
| Turns per session (mean / median) | 27.9 / 14 | 24.3 / 13 | −12.9% / −1 | ✅ more efficient |
| Refusal rate (`gardenio_model_refusal_total`) | 0 | 0 | 0 | ✅ none observed¹ |
| Fallback rate (`gardenio_model_fallback_total`) | 0 | 0 | 0 | ✅ none observed¹ |

¹ Zero is a real observed zero (deployed detector + healthy scrape pipeline), with one
measurement caveat — see [Refusals & silent fallbacks](#refusals--silent-fallbacks).

## Method & data sources

The cutover was **19 days** in the past at analysis time, so this is a retrospective. Two
independent data planes were used so the derivations cross-check (ka-0000005924):

1. **Durable bead store (authoritative for the retrospective).** Fleet-wide `type=agent`
   config beads carry terminal metrics stamped at close — `agent_state`,
   `litellm_spend_close`, `live_turns` — and persist indefinitely regardless of metrics
   retention. Pulled all **13,816** agent beads created 2026-07-29 → 2026-08-28 (all
   projects, all types), bucketed by `created_at` around the 2026-08-09 boundary.
   - `BEFORE` = created 2026-07-29 … 2026-08-08 (n=5,404)
   - `AFTER`  = created 2026-08-10 … 2026-08-28 (n=8,051)
   - Cutover day 2026-08-09 (n=361) excluded from both windows as the transition boundary.

2. **VictoriaMetrics / Grafana PromQL** (`gardenio-vmsingle.gardenio-system:8428`, the store
   behind the gardenio Grafana). Retention reaches back past the boundary (earliest
   `gardenio_account_cost_usd` datapoint 2026-07-29). Used to (a) confirm the fleet is now
   100% on opus-5, and (b) read the refusal/fallback time series.

**Cost = LiteLLM spend (bill impact).** `litellm_spend_close` is the per-session spend the
LiteLLM proxy attributes to each agent at close, i.e. the direct bill line. Summed spend was
$20,364 (BEFORE, n=5,029 sessions with spend>0) vs $22,805 (AFTER, n=7,521) — the AFTER
window is longer and busier, so the meaningful comparison is **per-session** and **per-turn**
cost, both of which fell.

**Cutover confirmation (VM cross-check).** `gardenio_agent_spend_usd_total{model=...}` now
shows **0** active `claude-opus-4-8-oauth` series and **43** `claude-opus-5-oauth` series —
the fleet is entirely on opus-5. LiteLLM model aliases + the account-default flip are live.

## Refusals & silent fallbacks

The two metrics delivered by MR !7075 (`ka-0000002213`) —
`gardenio_model_refusal_total{served_group,served_by_fallback}` and
`gardenio_model_fallback_total{...}` — report **zero series over the full 30-day VM window**,
and the durable ground-truth beads they mirror 1:1 (`type=rating`, `source=refusal` /
`source=model-fallback`; the counters reconstruct the bead count per the MR design) number
**zero fleet-wide**. Both derivations agree: no refusals and no silent model-fallbacks have
been recorded since the plumbing deployed.

This zero was validated against known-good controls rather than taken at face value
(ka-0000002586):

- **Detector is deployed.** The slack-bridge runs `controller:2026.240.19`; the metric code
  (`model_switch_metrics.go`) merged 2026-08-09 (calver day 221), well before the running
  build (day 240).
- **Scrape pipeline is healthy.** Sibling slack-bridge promauto metrics on the same default
  gatherer carry live series (`gardenio_assistant_event_total`,
  `gardenio_gitlab_mention_stale_task_passthrough_total`), so a 0 on the refusal/fallback
  counters is "no events classified", not "endpoint not scraped".
- **Clean cutover explains zero fallbacks.** A silent fallback is `served_group !=
  requested_group`. The flip moved accounts wholesale to opus-5, so served == requested and
  zero divergences is the *expected* outcome.

**Caveat (honest sensor reading).** A promauto `CounterVec` exposes a series only after its
first `.Inc()`, so "0 series" and "0 beads" both mean "the classifier never fired" — they do
not independently prove the *upstream* refusal signal (coop `stop_reason=refusal` →
`coop.events.messages.<actor>.refusal` NATS event → bridge subscription) is live. Refusals on
routine engineering work are genuinely rare, so a true zero is plausible; but this validation
confirms **no observed refusal/fallback regression**, not a positively-exercised detector. A
cheap follow-up would inject one synthetic refusal event and confirm the counter increments.

## Notes & confounders

- **Fleet mix.** Non-oauth models (`bedrock-claude-opus-4-7`, `claude-haiku-4-5`,
  `claude-fable-5`, glm) run in both windows and are unaffected by the flip; they add equal
  noise to both sides, so the aggregate still answers "did the fleet regress across the
  boundary?" (no). Agent beads carry no per-session model field, so a model-filtered cut
  was not possible from the bead plane.
- **Cost mechanism.** The per-session cost drop is a mix of opus-5 pricing and slightly
  fewer turns per session; the task asks for bill impact, and net bill impact per unit of
  work is **down**.
- **Cutover-day blip.** 2026-08-09 itself shows 89.3% success / 10.5% failed — expected churn
  during the flip, isolated to the transition day and recovered immediately after.

## Reproduction

```bash
VM=http://gardenio-vmsingle.gardenio-system.svc.cluster.local:8428
# fleet now 100% opus-5:
curl -s "$VM/api/v1/query" --data-urlencode 'query=count(gardenio_agent_spend_usd_total{model="claude-opus-5-oauth"})'
curl -s "$VM/api/v1/query" --data-urlencode 'query=count(gardenio_agent_spend_usd_total{model="claude-opus-4-8-oauth"})'
# refusal/fallback series (expect empty):
curl -s "$VM/api/v1/query" --data-urlencode 'query=sum(gardenio_model_refusal_total)'
curl -s "$VM/api/v1/query" --data-urlencode 'query=sum(gardenio_model_fallback_total)'

# quality/cost from the durable plane (server-side counts):
kd count --type=agent --all-projects --all-types \
  --created-after 2026-07-29T00:00:00Z --created-before 2026-08-09T00:00:00Z -f agent_state=done
kd count --type=agent --all-projects --all-types \
  --created-after 2026-08-10T00:00:00Z --created-before 2026-08-29T00:00:00Z -f agent_state=done
# spend/turns: pull agent beads projecting fields.litellm_spend_close + fields.live_turns,
# bucket by created_at around 2026-08-09, aggregate.
```
