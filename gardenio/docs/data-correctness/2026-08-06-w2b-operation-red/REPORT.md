# W2b — Port parity panels as beads-embedded chart-catalog entries

> Task [[ka-0000001551]] · epic [[ka-0000000756]] · inventory [[ka-0000003866]] ·
> rubric `gardenio/docs/data-correctness-rubric.md`.

## What W2b ports, and what already existed

The parity target is the ACME **distributed-traces** dashboard — the W2a inventory's
#1-ranked source ([[ka-0000003866]]) — mirrored in-chart as
`gardenio/helm/gardenio/dashboards/monorepo-distributed-traces.json`. Its best features
split into two tiers:

- **per-service RED** (which *service* is hot) — request rate / error rate / p95
  latency by `service_name`. **Already landed** as catalog charts
  `perf-span-{request-rate,error-ratio,latency-quantiles}` ([[ka-0000008091]]).
- **per-operation RED** (which *endpoint within it*) — the distributed-traces
  headline panels (ACME panels 9/10/11): *Top Operations by Volume*, *Operations
  with Errors*, *Slowest Operations (p99)*, keyed by the OTel `span_name` label.
  **This was the gap.** W2b ports them.

## Delivered

### 1. Three operation-level catalog charts (`kbeads/internal/server/charts_catalog.go`)

| catalog id | question | shape |
|---|---|---|
| `perf-span-top-operations` | noisiest endpoints | `sum by(span_name)(rate(span_calls_total{server}[step]))`, top_n by window volume |
| `perf-span-operation-errors` | loudest failing endpoints | `sum by(span_name)(rate(span_calls_total{server,ERROR}[step]))`, top_n by error volume |
| `perf-span-slowest-operations` | slowest tail | `histogram_quantile(0.99, sum by(span_name, le)(rate(span_duration_milliseconds_bucket{server}[step])))`, top_n |

`{server}` above is the pinned matcher `source="perf",span_kind="SPAN_KIND_SERVER"`.

These read the **same** federated spanmetrics families as the per-service charts
(`span_calls_total` / `span_duration_milliseconds_bucket` under `source="perf"`,
federated by the `perf-federation` vmagent job — see the span-derived-RED report),
so they **reconcile by construction**: summing any per-operation chart over
`span_name` equals its per-service sibling.

### 2. `duration_ms` → Grafana `ms` unit (`charts_grafana_panel.go`)

The slowest-operations panel carries MILLISECONDS (`span_duration_milliseconds_*`).
`grafanaUnit` now maps `duration_ms` → Grafana's auto-scaling `ms` unit, so a
6300 ms p99 reads "6.3 s" and a 4 ms one reads "4 ms" (C3 unit-scale honesty)
instead of falling back to `short`.

### 3. A generated **perf-traces** Grafana dashboard (`charts_grafana_dashboard.go`)

`dashboardCatalog()` gains `gardenio-perf-traces-red`, composing all six span-derived RED
charts (per-service RED strip first, per-operation topk drill-downs after). This is
the **"beads-embedded graphs"** half of the ask: once the dashboard bead exists and
`kd admin gen-dashboards` has run, any panel embeds into a bead description, comment,
or bundle via the live directive:

> **UID note (ka-0000002712):** the catalog board's uid is `gardenio-perf-traces-red`,
> **not** `gardenio-perf-traces`. The bare `gardenio-perf-traces` is already owned by the
> chart-shipped VictoriaTraces trace-search board
> (`helm/gardenio/dashboards/perf-traces.json`); reusing it made two provisioning paths
> fight over one Grafana dashboard. The `-red` suffix keeps both boards.

```
:::grafana gardenio-perf-traces-red panel=4   # top operations by volume
:::grafana gardenio-perf-traces-red panel=6   # slowest operations p99
```

## On `:::chart` vs `:::grafana`

The task title names both `:::grafana` and `:::chart`. Only **`:::grafana`** is a
live embed primitive. The `:::chart` inline-ECharts directive is documented in the
rubric but was never implemented, and the ECharts catalog driver it would depend on
(`echarts_bead.js` / `echarts_catalog.js`) is loaded on **zero** viewer pages — the
per-bead ECharts panels were retired in favour of fleet-wide Grafana embeds
([[ka-0000007859]]), and catalog charts now reach beads exclusively through the
catalog → generated-Grafana-dashboard → `:::grafana` path. Reviving `:::chart` would
mean reviving dead client infrastructure, so W2b deliberately ports onto the live
`:::grafana` primitive instead. The charts remain independently consumable as JSON
via `GET /api/v1/charts/{id}` (iOS / any client) regardless.

## Correctness (rubric)

- **C2 honest-aggregation** — `source="perf"` + `span_kind="SPAN_KIND_SERVER"` pinned
  on every query; client/internal spans can never inflate the operation ranking.
  Enforced by `TestSpanOperation_PresentAndScoped`.
- **C3 unit-scale** — errors panel is a RATE (errored-spans/sec) so it ranks by error
  VOLUME (loudest failing endpoint first), NOT a per-operation ratio that would float
  a single-call 100%-error operation above a busy 5%-error one; slowest panel is
  `duration_ms`. Enforced by `TestSpanOperation_ErrorsRankByVolume` /
  `TestSpanOperation_SlowestIsMillisecondsP99`.
- **R2 honest-empty** — a quiet window yields no series (not a flat-zero line).
  Enforced by `TestSpanOperation_HonestEmptyWhenNoTraffic`.
- **top_n ranking** — membership resolved client-side by window total (`topNSeries`),
  the tail dropped (no synthetic "other" on a ranking panel).
  Enforced by `TestSpanOperation_TopNTrimsRanking`.
- The static **data-correctness gate** (`TestCatalog_DataCorrectnessGate`) iterates
  every catalog entry, so all three new charts pass C2/C3/C6/R1/R4/R5 as a merge gate.

## Follow-ups (out of scope here)

- **Live embed provisioning.** `gardenio-perf-traces-red` assembles offline
  (`TestDashboardCatalog_PerfTracesParityBoard`); a live embed needs a
  `type=dashboard` bead (`dashboard_uid=gardenio-perf-traces-red`) plus a
  `kd admin gen-dashboards` run — the standard step for every catalog-generated board.
- **W2c trace-native panels** (error-correlation by HTTP status, service map) remain
  that task's scope; the topology gap (relay drops cross-service span refs) persists.
