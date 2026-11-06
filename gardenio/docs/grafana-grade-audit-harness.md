# Grafana GRADE audit — research pass + harness

Source of truth for the **accuracy-first** Grafana dashboard audit loop:
epic [`ka-0000008066`](https://gitlab.com/example-org/gardenio/-/issues),
formula `ka-0000003015` (definition checked in at
[`grafana-grade-audit-formula.json`](grafana-grade-audit-formula.json)),
continual gardener `ka-0000001426` (ships **disabled** until operator go-ahead).

Driven by Slack thread `ka-0000004732` (Matt Baker): *"we need a gardenio grafana
dashboard improvement gardener and mega epic … plan to use playwright and a rubric
to make beautiful graphs … generate plenty of ideas for good graphs … focus
seriously on accuracy, cross-checking displayed stuff with actual data from the
system and fixing."*

## Lineage — two proven molecules, fused

| Molecule | What it proved | What we take |
|---|---|---|
| **maps-beauty CLUB** (`ka-0000001933`, formula `ka-0000004339`) | A continual `type=trigger role:gardener` bead that Playwright-captures a live surface, scores a 0-10 rubric, files one bug per low dimension, and lands a roll-up `REPORT.md` per iteration under `docs/<kind>/<iter>/`. | The whole continual-gardener shape + per-iteration diffable reports + per-dimension bug beads. |
| **monorepo Grafana DVFC** (`ka-0000004859`) | A Grafana-specific fix/critic loop (Data/Vis/Feat/Comp) that took 7 ACME dashboards to ≥8/10, including hard datasource/unit bugs (the decmbytes→mbytes 1e15 bug). | The Grafana render + screenshot-critic mechanics, and the "iterate until ≥8" bar. |

The gap neither molecule fully closes is **accuracy** — proving the displayed
number equals reality. That is the heaviest dimension here.

## The GRADE rubric (0-10 each; bar for "good" = 8)

- **G — Grounded (accuracy)** — every panel's displayed value replayed against
  VictoriaMetrics; displayed must equal truth and the query must be honest.
- **R — Richness** — panels populated, or *honestly* empty.
- **A — Aesthetics** — Tufte signal-per-pixel + dark-theme craft, both viewports.
- **D — Depth** — does it ask the right questions? Drives graph-idea generation.
- **E — Ergonomics** — template vars, drill-downs, default ranges, links, responsive.

## Access (verified from an agent pod, 2026-06-17)

Unlike the beads-viewer audits (which need a Keycloak `client_credentials`
bearer + port-forward because the public ingress is behind AVA forwardAuth that
agent pods cannot complete), the Grafana audit runs **entirely in-cluster by
ClusterIP** — no tunnel, no Keycloak. **The full populated-render recipe lives in
[`grafana-headless-render.md`](grafana-headless-render.md)** — read it; the
essentials:

```bash
GRAFANA_URL=http://gardenio-grafana:3000              # ClusterIP, in-cluster
VM_URL=http://gardenio-vmsingle:8428                  # ground truth
H="X-Auth-Request-Email: someone@acme.example"       # stamp the auth-proxy header → authenticated

curl -fsS -H "$H" "$GRAFANA_URL/api/search?type=dash-db"          # lists dashboards
curl -fsS -H "$H" "$GRAFANA_URL/api/dashboards/uid/<uid>"         # model (panel queries)
curl -fsS "$VM_URL/api/v1/label/__name__/values"                 # ground-truth series

# POPULATED panel PNG — render per panel and tile (full-page /render/d/ is broken, see below):
curl -fsS -H "$H" -o p<N>.png \
  "$GRAFANA_URL/render/d-solo/<uid>/x?panelId=<N>&width=700&height=350&theme=dark&from=now-24h&to=now"
```

Two corrections to the earlier (2026-06-09) note, both verified 2026-06-17:

- **Render via the `X-Auth-Request-Email` auth-proxy header**, not `admin:admin`.
  Grafana is header-auth-proxy with the pod's `10.0.0.0/8` IP whitelisted, so a
  stamped header authenticates as a real Viewer — the same path humans use, no
  break-glass admin creds. (`admin:admin` still works as a fallback.)
- **The full-page `/render/d/<uid>` route is broken** under our `/grafana`
  subpath — it renders Grafana's "Page not found" to PNG. Use the per-panel
  `/render/d-solo/<uid>?panelId=N` route (which is unaffected) and tile the
  panels into a montage. See the Gotchas in `grafana-headless-render.md`.

The public `/grafana` ingress (`https://gardenio.e2e.example.com/grafana`)
stays behind AVA — do not target it from agent pods.

## The cross-check (heart of the loop)

For every panel: extract each `target.expr` from the dashboard model
(`GET /api/dashboards/uid/<uid>`), replay it against `VM_URL/api/v1/query` over
the panel's own time window, and reconcile *displayed* vs *truth*. Classify each
mismatch: **MISSING-SERIES** (naming-bug-to-fix vs honest-no-data),
**AGGREGATION-DRIFT** (query technically runs but misleads),
**UNIT/SCALE**, **STALE/FROZEN**, **BLIND-EMPTY**, or **OK**.

### BLIND-EMPTY — run the static detector before the replay

The replay reconciles what a panel *displayed against what VM held at that
instant*. It cannot see the class of bug that only shows up on the day the
exporter dies: an empty-result branch that renders a confident reading it has no
evidence for.

> **Invariant.** A stat panel's empty-result branch must be able to distinguish a
> real zero from a blind emitter.

`gardenio/controller/internal/grafanagrade` checks that invariant statically over
every chart-shipped board — no cluster, no render, no VM:

```bash
cd gardenio/controller && go test ./internal/grafanagrade/
```

It flags five classes: `ungated-zero` (`or vector(0)` with no liveness term —
paints a green 0 over a dark counter, ka-0000007164), `stale-window`
(`sum(last_over_time(...))` on an instant panel — re-dates the last sample
anywhere in the window as *now*), `unsupported-novalue` and
`novalue-claims-zero` (empty-branch text asserting a cause the empty result
cannot establish — "counter not emitting", or conversely "no defer events"), and
the advisory `blind-empty` (honest, but the operator still cannot tell a real
zero from a dead exporter).

The fix is always the same shape — an instant read whose zero branch is gated on
an *independent* control that is measurably advancing:

```promql
sum(gardenio_agents_deferred_total) or (0 * (sum(increase(gardenio_reconcile_total[15m])) > 0))
```

If the reconciler is advancing and nothing deferred, the panel reads a real `0`;
if the exporter is dark, the gate is empty too and the panel stays empty so
`noValue` can say so. `max(up{job=~"..."})` and
`or (vector(0) and (count(<sibling series>) > 0))` are the same move.

**Do not** reach for the tempting alternative — comparing the panel's window
against the emitter's cadence and widening the window when they disagree.
Widening a window does not recover a missing reading, it re-dates a stale one;
that remedy is what produced the `stale-window` class in the first place
(ka-0000002930, ka-0000004423). And a reset-`GaugeVec` such as
`gardenio_agents_deferred_total` — `Reset()` per reconcile pass, `Inc()` per
deferred agent — is *meaningfully* absent whenever the last pass deferred
nothing, so a cadence check over-fires on exactly the panels that are correct.

`testdata/empty_branch_baseline.json` is the ratchet's waiver list. It opened at
**64 recorded defects** and is now **empty** — every chart-shipped board passes
the invariant (ka-0000006178 fixed the last 50; the rest were cleared by earlier
in-flight MRs). Keep the file: with nothing waived the guard is a clean gate, so
any new defect reds CI. Regenerate after a deliberate, reviewed change with
`GRAFANAGRADE_UPDATE_BASELINE=1 go test ./internal/grafanagrade/`.

The 78 remaining `blind-empty` findings are **advisory** and do not gate CI: they
are instant panels that are honest (no fabricated zero, no unsupported claim) but
still cannot tell an operator a real zero from a dead exporter — usually because
the reading is a ratio or a latency percentile that has no meaningful `0` to
fall back to. Tightening those is follow-up work, not a regression.

### UNIT/SCALE — read all four places a unit lives before counting

The 2026-09-02 iteration reported **91 of 338 viz panels carry no unit**. That
number was a detector artifact: the scorer counted a panel as united iff
`fieldConfig.defaults.unit` was set, and four legitimate fleet shapes put the
unit somewhere else (bug `ka-0000002317`).

`CheckUnits` in `internal/grafanagrade/unit.go` knows all four, so a panel is
flagged only when nothing anywhere says what its number means:

| Shape | Where the unit actually is | Fleet count |
|---|---|---|
| Heterogeneous table | `fieldConfig.overrides` — per column, `byName`/`byRegexp`/`byFrameRefID` | 23 |
| Heatmap | `options.yAxis.unit` (the bucket axis, outside `fieldConfig` entirely) | 11 |
| State-timeline / flag stat | `value`/`range` mappings — renders `OK`/`ACTIVE`, never a number | 6 |
| Label-only table | numeric `Value` column dropped in an `organize` transform | 1 |

A `special` (null) mapping does **not** exempt: that is empty-branch text, and
the numbers still render.

The honest residue behind the 91 was **16 panels across 8 boards**, all fixed in the same
change. Pick each unit from the *metric's* dimension — read the exporter and
confirm against `gardenio-vmsingle`, never the panel title. `increase()` of a
`_total` counter is a dimensionless count (`short`); a `_seconds` histogram is
`s`; a `_bytes` gauge is `bytes`; a ratio is `percentunit` unless the series is
already 0-100. eks-api-forensics' "Request rate by source IP" is the trap: its
column is a `count()` over the range, so a title-derived `reqps` would be wrong.

`testdata/missing_unit_baseline.json` is the ratchet's waiver list, and it opened
**empty** — any unit-less viz panel reds CI. Regenerate after a deliberate,
reviewed change with `GRAFANAGRADE_UPDATE_UNIT_BASELINE=1 go test
./internal/grafanagrade/`.

### First-pass findings (filed as seed beads under the epic)

| Bead | Class | Finding |
|---|---|---|
| `ka-0000002809` | AGGREGATION-DRIFT | cost-overview "Active Agents" = `sum(gardenio_agents_active)` displays 17 but sums *every* state (prewarmed/idle/queued/spawning included) — overstates working agents. |
| `ka-0000004193` | MISSING-SERIES | kbeads-embedding's 5 coverage/latency/error panels reference metrics absent from live VM; names match the code, so it's a no-data condition the panels should state honestly, not a typo. |
| `ka-0000007849` | verify | cost headline correctly uses `gardenio_agent_spend_usd_total` (~$211) while `gardenio_beads_total_cost_usd` / `gardenio_agent_cost_usd` read 0 — confirm no panel reads the zero source. |

Positives confirmed: `account` + `agent` labels exist on the spend series (45
series), so "Cost by Account" and "Top Spenders by Agent" group correctly; all
operated-by panels resolve.

## Metric surface (idea fuel)

132 series live in VM. Beyond the gauges the current dashboards already draw,
these histograms/counters are emitted but **under-visualised** — fuel for the
Depth dimension's idea catalogue:
`gardenio_agent_llm_request_latency_seconds_*`,
`gardenio_agent_tool_call_latency_seconds_*`,
`gardenio_agent_session_duration_seconds_*`,
`gardenio_agent_deferred_wait_seconds_*`,
`gardenio_agent_pod_phase_transitions_total`,
`gardenio_mention_cold_respawn_seconds_*`, `gardenio_freeze_latency_seconds_*`,
`gardenio_authz_denials_total`, `gardenio_operator_activity_total`,
`gardenio_beads_events_total` (+ recording rule `gardenio:beads_events:rate5m`),
`gardenio_agent_lifecycle_events_total`.

## Running it

Operator go-ahead pending (decision on the epic). Once approved:

```bash
# One-shot manual pour:
kd formula apply ka-0000003015 \
  --var ITERATION=$(date -u +%Y-%m-%dT%H%M) \
  --var PARENT_EPIC=ka-0000008066 \
  --var GRAFANA_URL=http://gardenio-grafana:3000 \
  --var VM_URL=http://gardenio-vmsingle:8428

# Or flip the gardener on for the daily cadence:
#   set ka-0000001426 field enabled=true  (cron 23 7 * * * UTC)
```
