# Span-derived RED into VM — data-correctness report

> Bug [[ka-0000008091]] · epic [[ka-0000000756]] · rubric `gardenio/docs/data-correctness-rubric.md`.
> Probed in-cluster against live perf VM (`https://perf.acme.example/victoria-metrics`),
> gardenio VM (`gardenio-vmsingle:8428`) and gardenio VT (`gardenio-victoria-traces:10428`), 2026-06-22.

## Premise correction (the load-bearing finding)

The bug's premise was: *"no spanmetrics families exist anywhere — we must build a
spanmetrics connector (OTel spanmetrics processor or VT-side equivalent) to
aggregate spans into VM."*

**That is wrong, and in a useful way.** A spanmetrics connector **already runs
perf-side**. The perf VM exposes, live:

| family | series (perf VM) | labels |
|---|---|---|
| `span_calls_total` | 351 | `service_name`, `status_code`, `span_kind`, `span_name`, `http_route`, … |
| `span_duration_milliseconds_bucket` | 4212 | + `le` (5,10,25,50,100,250,500,1000,2500,5000,10000,+Inf) |
| `span_duration_milliseconds_count` | 351 | |
| `span_duration_milliseconds_sum` | 351 | |

Covering the 4 app services: `keycloak`, `site-api`, `sponsor-api`, `study-web`.

The reason VM had **zero** `span_*` is that the gardenio `perf-federation` vmagent
job (epic ka-0000004560, design `obs-perf-to-gardenio-vm-federation.md`)
**deliberately dropped all `span_*`** as "high-cardinality trace buckets that
belong in VictoriaTraces" — both via its `match[]` allowlist (no span family
listed) and a defensive `drop span_.+` metric_relabel.

So the fix is **not a new component**. It is selectively federating the four RED
span families that already exist.

## What changed

### 1. Metrics half — federate the RED span families (`deploy-values/gardenio.yaml`)

The `perf-federation` vmagent scrape job's `match[]` allowlist now includes the
four RED families explicitly:

```
…|span_calls_total|span_duration_milliseconds_bucket|span_duration_milliseconds_count|span_duration_milliseconds_sum
```

The old `drop span_.+` metric_relabel was **removed** — it was already dead code
(the `match[]` never selected any `span_*`, so nothing span-shaped ever reached
metric_relabel), and with the RED families now allowlisted it would have deleted
exactly the series we want. Federation is gated **entirely** by `match[]`, so any
*other* span family the perf connector might emit in future is excluded by
omission, not by a drop rule.

Cardinality added: ~5.3k series (the one fat family is
`span_duration_milliseconds_bucket` at ~4.2k) — well within the gp3/1y headroom
sized in design doc §3.

### 2. Trace-derived half — three span-derived RED catalog entries (`kbeads/internal/server/charts_catalog.go`)

| ID | Panel |
|---|---|
| `perf-span-request-rate` | server-span rate/sec by service (the service-map node tier) |
| `perf-span-error-ratio` | `STATUS_CODE_ERROR` / all server spans, `>0`-guarded |
| `perf-span-latency-quantiles` | p50/p95/p99 over `span_duration_milliseconds_bucket` (ms) |

All scoped `source="perf"` **and** `span_kind="SPAN_KIND_SERVER"`.

### 3. Two pre-existing main-branch reds fixed (in blast radius)

- `knownYFormats` (charts_audit.go) was missing `duration_seconds` — the spelling
  the W2b perf-RED entries + `grafanaUnit()` actually use. `TestCatalog_DataCorrectnessGate`
  was **red on clean main** because of it. Added.
- `vmagent_perf_federation_test.go`'s `perfScrapeJSON` fixture still carried
  `write_relabel_configs`, which the configmap template now hard-rejects
  (ka-0000007382). `TestVmagent_ExtraScrapeConfigs_Rendered` was **red on clean
  main**. Migrated the fixture to `relabel_configs` and updated its assertions to
  the selective-federation contract.

## Gate matrix (span-derived RED panels)

| Gate | Verdict | Evidence |
|---|---|---|
| **C1 Replay-match** | OK | `/federate` with the new `match[]` returns 636 `span_calls_total` + 7632 `span_duration_milliseconds_bucket` series live; queries replay against perf VM. |
| **C2 Honest-aggregation** | OK | Every query pins `source="perf"` AND `span_kind="SPAN_KIND_SERVER"` — RED measures each service's own request surface; client/internal spans (DB calls, outbound) excluded (would inflate). Test `TestSpanRED_PresentAndScoped`. |
| **C3 Unit-scale** | OK | Family is `span_duration_MILLISECONDS_*`; YFormat `duration_ms` (not `duration_seconds`). Counters (`_total`, `_bucket`) all `rate()`-wrapped. Test `TestSpanRED_LatencyIsMilliseconds`. |
| **C4 Freshness** | OK | Each render queries live `[from,to]`; federation scrape_interval 60s. |
| **C5 Existence-honesty** | OK | The 4 families + 4 services exist in VM post-federation; error panel `>0`-guards its denominator so quiet services NaN-gap. Test `TestSpanRED_HonestEmptyWhenNoTraffic`. |
| **C6 Window-integrity** | OK | `rate()[{{.Step}}]` parameterized window; no bare rate. Static gate `TestCatalog_DataCorrectnessGate`. |
| **R1–R5** | OK | Titles state the question ("service request rate", "service error ratio", "service latency p50/p95/p99"); error panel honest-empty; p50+p99 overlays give scale-sanity; per-service legend. |

## §2.1 count-reconciliation — population caveat (documented, not faked)

Rubric §2.1 says `sum(increase(span_calls_total[w]))` must reconcile ±5% with the
raw VT trace count. There is a **population mismatch** that callers must understand:

- The federated `span_calls_total` is perf's **full-fidelity** spanmetric — the
  perf collector aggregates *every* span.
- The in-chart gardenio VT holds only the **relay-sampled** subset:
  `perftracesrelay` caps at `MaxTracesPerService = 200`/service/cycle.

Live probe (site-api, 1h): perf-side spanmetric server-span increase ≈ **12,354**;
the in-chart VT carries far fewer (sampled). They are **expected to diverge** — they
measure different populations.

Consequence: §2.1 reconciliation for the **VM span-derived panels** is against the
**perf-side VT** (the same store the spanmetric connector reads), NOT the relay
copy. The W2c VT-direct panels (`charts_traces.go`) remain the trace-side view of
the *relay-sampled* population. Both are honest about their own population; they
are not interchangeable. This caveat is captured in the catalog entry comment.

## Replay commands

```bash
# (creds from k8s secret gardenio-perf-vm-federation)
PERF=https://perf.acme.example/victoria-metrics
SEL='{__name__=~"span_calls_total|span_duration_milliseconds_.+"}'
# RED families present in the federation output:
curl -fsS -u "$U:$P" "$PERF/federate" --data-urlencode "match[]=$SEL" | grep -oE '^span_[a-z_]+\{' | sort | uniq -c
# p95 server-span latency (ms), site-api 1h — NOT +Inf-pinned (rubric §2.2):
curl -fsS -u "$U:$P" "$PERF/api/v1/query" --data-urlencode \
  'query=histogram_quantile(0.95, sum by(le)(rate(span_duration_milliseconds_bucket{service_name="site-api",span_kind="SPAN_KIND_SERVER"}[1h])))'
```

## Deploy note

The federation change lands when the gardenio env redeploys with this overlay
(the `perf-federation` vmagent job picks up the new `match[]`). The catalog entries
ship with the next kbeads image. Until the env redeploys, the span-derived RED
panels read honest-empty (R2) — no `span_*` in VM yet — exactly as designed.
