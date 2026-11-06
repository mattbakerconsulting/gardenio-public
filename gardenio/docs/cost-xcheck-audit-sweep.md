# Cost-audit cross-check sweep (C2)

The **cross-check sweep** turns the operator's "double-check principle" into a
standing signal: *if you can add up the same quantity from two independent data
sources and they stay close, you're probably measuring something real; a
single-source number is only ever spot-checkable by hand.*

- Plan: `ka-0000003747` · bundle `ka-0000004761` · step **C2** (`ka-0000000950`).
- Code: `gardenio/controller/cmd/controller/xcheck_sweep.go` (sweep + pairs),
  `xcheck_vmquery.go` (vmsingle instant-query helper),
  `internal/metrics/xcheck.go` (metrics + agreement scoring).

## What it publishes

A periodic controller loop recomputes a set of conservation **pairs** — the same
quantity derived two independent ways — and publishes each as three series:

| Metric | Labels | Meaning |
|---|---|---|
| `gardenio_xcheck_value` | `{pair,side,env}` | one side of a pair (`side ∈ {a,b}`) — graph a vs b, or their ratio |
| `gardenio_xcheck_agreement` | `{pair,env}` | corroboration score `1 − \|A−B\| / max(A,B,ε)` ∈ [0,1] |

`agreement = 1.0` means the two derivations agree exactly; it falls toward 0 as
they diverge. The **corroboration score** is the headline: the C5 `CostXCheckDrift`
alert fires on sustained low agreement, and the C6 beads-viewer `/costs` trust
strip badges each number with the agreement of the pair(s) that qualify it.

Scoring lives in one exported pure function, `metrics.XCheckAgreement(a,b)`, so
the sweep, its tests, and the trust strip score a pair identically.

## The pairs (P1, P2, P4–P6, P9, P10 — P3 & P7 retired)

| Pair (`{pair}` label) | Side A | Side B | Catches |
|---|---|---|---|
| P1 `bill-conservation` | `sum(max by(account,env)(gardenio_account_cost_usd))` | `sum(gardenio_agent_attributed_cost_usd) + sum(gardenio_unattributed_cost_usd)` | proxy-merge bugs, partial reads |
| P2 `tag-conservation` | `sum(gardenio_tag_cost_usd{dim="role"})` | `sum(gardenio_tag_cost_usd{dim="project"})` | untagged requests, per-dimension tag drops |
| ~~P3 `settled-vs-bill`~~ | **RETIRED (ka-0000000543).** Side A was `increase(gardenio_agent_spend_usd_total[24h])` — a counter that went dead in prod ($36 lifetime, ~0.09% of a ~$40k bill), so the pair could not agree with the live bill at any floor. P1 `bill-conservation` (the bill) + P10 `agent-spend-parity` (per-agent settled attribution) cover its ground. | | |
| P4 `sessions-vs-agent` | Σ session `settled_cost_usd` | Σ agent (`litellm_spend_close − litellm_spend_first_baseline`) | lost/double session settlement |
| P5 `rollup-vs-tree` | Σ gardener `gardener_spend_usd` | live BFS Σ agent `total_cost_usd` | rollup staleness, missed descendants |
| P6 `token-parity` | `increase(gardenio_session_tokens_total[1h])` (gb-pushed) | `increase(gardenio_session_{input,output,cache_read,cache_write}_tokens[1h])` (coop-scraped) | JSONL parse/dedup divergence |
| P9 `interrupt-parity` | `increase(gardenio_agent_involuntary_deaths_total[24h])` | kbeads scan of `close_reason` the emitter's classifier counts (`_involuntary` families + `context_cancelled_unclaimed`, C9 ka-0000001146), same window | deaths counter vs bead close-reason record disagree |
| P10 `agent-spend-parity` | Σ agent `total_cost_usd` (OLD stateful settle) | Σ agent `spend_logs_cost_usd` (NEW stateless `/spend/logs` recompute) | old-vs-new attribution divergence — the C4 cutover gate |

P7 `estimate-vs-settled` was **removed** (`ka-0000007296`). It summed session
`total_cost_usd` (the in-pod JSONL `response_cost` estimate) against session
`settled_cost_usd`, but coop went strictly cost-free on 2026-06-06
(`b44b0b3c9`, `ka-0000002983`) — nothing writes `total_cost_usd` to a session
bead — so the pair was added (2026-07-19) with a producer that was already gone
and never published a score (NO-DATA in every `crosscheck.json` since). The
settled figure it was meant to qualify is still independently witnessed by P4
`sessions-vs-agent` (and, for per-agent settled attribution, P10
`agent-spend-parity`).

P8 (provider-side reconciliation vs Anthropic/AWS) was **dropped** at plan time —
LiteLLM is the trusted external anchor (operator steer Q1, `ka-0000002724`).

## Two data-source classes

The sweep deliberately audits the plumbing **without doubling its load**:

- **PromQL against vmsingle** (P1, P2, P6, P9-side-A). Most quantities are
  *already* published as fleet metrics. The honest "second source" is to read
  those series back two ways rather than re-hitting LiteLLM — which would double
  the API load *and* skip the emit→scrape path the pair is meant to verify.
  Reading `gardenio_account_cost_usd` (fed by `/key/info`) against
  `gardenio_agent_attributed_cost_usd + gardenio_unattributed_cost_usd` (fed by
  `/spend/tags` + `/global/spend`) IS the `/key/info`-vs-`/spend/tags` cross-check,
  sourced from the gauges instead of a fresh poll.
- **Bead scans against the daemon** (P4, P5, P9-side-B, P10). The stateful-era
  per-agent/per-session/per-gardener cost fields (`settled_cost_usd`,
  `litellm_spend_close`/`litellm_spend_first_baseline`, `total_cost_usd`,
  `gardener_spend_usd`) are not in Prometheus, so these are summed directly off the
  beads. Per the plan these compare stateful-era fields that C4 later deletes —
  built anyway because they **gate that cutover** (P4/P5/P10 must agree before the
  stateful machinery is removed).

The PromQL upstream is `VMSINGLE_QUERY_URL` (default the in-cluster
`gardenio-vmsingle` Service), the same upstream the beads-viewer `/costs` headline
already reads. With no upstream wired the PromQL pairs are skipped wholesale; the
bead-scan pairs still run.

## Honest degrade (no fabricated agreement)

A pair is published **only when both sides are derived this cycle**. Any of these
leaves it **absent** rather than emitting a misleading score:

- a PromQL side's metric is missing from vmsingle (metric not yet live here);
- a bead scan hits the `xcheckScanMaxBeads` safety cap (a half-scanned sum is
  worse than a visible gap — it is logged and the pair is skipped);
- the relevant population is empty (no sessions, no gardeners) — a `0/0` pair would
  read as a hollow, misleadingly-healthy `1.0`.

A pair that was published last cycle but can no longer derive both sides is
**actively cleared** (`ClearPair`), not left frozen — a stale "healthy" score
while a source is broken is the one failure mode worse than a gap.

**P9 exception (the divergence P9 exists to catch):** the involuntary-deaths
counter is created lazily on the first death, so an absent side-A metric is
ambiguous. If the bead scan found deaths but the metric is absent, that IS the
drift P9 watches for, so side A is treated as a present `0` and the disagreement
surfaces (agreement → 0). If the window is genuinely clean (no beads, no metric),
the pair stays absent.

## P10: the old-vs-new attribution gate (C3)

C3 (`ka-0000000774`, decision `ka-0000006903` "replace-now") added **P10**
(`agent-spend-parity`): per agent, the OLD stateful figure (`total_cost_usd`,
written by the baseline/watermark settle pipeline) against the NEW stateless one
(`spend_logs_cost_usd`, recomputed each cycle from raw `/spend/logs` rows by
`spendlogs_sweep.go`). Only beads carrying **both** fields contribute, so the two
sums always cover one population, and the population is bounded to beads updated
in the last 48h (the sweep's tracking window) so frozen historical pairs cannot
dilute a fresh divergence. Two *expected* sub-1.0 contributors, both old-path
defects (measured in `ka-0000001899`): close-time flush lag (settled reads
$0.01–0.40 low per agent) and gb-estimate residue on never-settled sessions.
Read the score as a trend; C4 cuts over when it holds (operator suggested
≥0.95 for a few days).

## Adding a pair

The framework is a flat registry — `xcheckSweep.pairs()`. A new pair is one struct
literal `{name, compute}` plus one compute closure returning both sides as an
`xcheckResult`. No sweep-loop changes — P10 above landed as exactly that.

## Configuration

| Env | Default | Purpose |
|---|---|---|
| `GARDENIO_XCHECK_SWEEP_INTERVAL` | `15m` | sweep cadence; `0` disables the loop |
| `VMSINGLE_QUERY_URL` | `http://gardenio-vmsingle:8428` | vmsingle instant-query upstream for PromQL pairs |
