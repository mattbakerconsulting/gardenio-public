# DAU/WAU/MAU query-time panels — correctness + p99 benchmark

**Bead:** ka-0000006905 · **Epic:** ka-0000007629 · **Plan:** [`customer-1-telemetry-plan.md`](./customer-1-telemetry-plan.md) §1.2, §3.5 · **Arch:** [`timeseries-chart-backend-arch.md`](./timeseries-chart-backend-arch.md) (R5)
**Operator:** Matthew Baker

## TL;DR

- DAU/WAU/MAU are computed **query-time** from the backbone counter
  `gardenio_operator_activity_total`, with **no HyperLogLog and no recording
  rules**, via:
  ```promql
  count( count by (operator_id) ( last_over_time(gardenio_operator_activity_total[1d|7d|30d]) ) )
  ```
- **Correctness verified** against a real VictoriaMetrics fixture (the exact
  v1.108.1 the helm chart pins): the fixture is constructed to yield
  **DAU=4, WAU=5, MAU=6**, and the implementation returns exactly that —
  including a counter-reset case, a single-sample case, a multi-series
  same-operator case, and an out-of-window case.
- **Measured p99 ≈ 4 ms** (worst window, MAU/30d) at 500-operator /
  ~870-series scale — **~125× under the 500 ms threshold.**
- **Therefore v2 precomputed rollups (`gardenio_operator_dau/wau/mau` gauges,
  kbeads nightly push, staleness gauge, >25h alert) are NOT built**, per the
  acceptance criteria. The query-time path is the v1 *and* steady-state answer
  at gardenio's scale.

## What shipped

The first vertical slice of the in-house chart backend (R5 §3: a set of
`/v1/charts/*` endpoints inside `kbeads`, not a new microservice):

| File | Purpose |
|---|---|
| `kbeads/internal/server/charts_vmclient.go` | Thin VictoriaMetrics instant-query client (`VMClient.QueryScalar`). ~150 LOC over `/api/v1/query`; deliberately avoids `github.com/prometheus/prometheus`, which drags the entire k8s client tree into the lean daemon. |
| `kbeads/internal/server/charts_uniqueness.go` | `GET /v1/charts/uniqueness` handler. Centralised `uniquenessQuery()` (the `last_over_time` PromQL), all three windows evaluated at one instant (so DAU ≤ WAU ≤ MAU holds), 30 s L1 TTL cache. |
| wiring | Route registered `RoleViewer` in `http.go`; `BEADS_VMSELECT_URL` (+ optional `BEADS_VMSELECT_TOKEN`) enables it in `serve.go`. Unset → endpoint returns `503` and kbeads serves no charts. |

Response shape:

```json
{
  "evaluated_at": "2026-05-30T03:22:31Z",
  "panels": [
    { "key": "dau", "label": "DAU", "window": "1d",  "count": 134 },
    { "key": "wau", "label": "WAU", "window": "7d",  "count": 294 },
    { "key": "mau", "label": "MAU", "window": "30d", "count": 501 }
  ]
}
```

## Why `last_over_time`, not `increase`

Distinct-operator **membership** only needs the question "did this operator's
series have *any* sample in the window?". `last_over_time(metric[window])`
answers that correctly regardless of counter resets. `increase()` does **not**:

1. **Single-sample series** — an operator with exactly one scrape in the
   window has no delta, so `increase()` produces no value and the operator is
   dropped.
2. **Counter reset inside the window** — a pod restart resets the counter;
   `increase()` accounts for the reset in its *rate* math but the series'
   membership in the distinct set is what we care about, and the
   reset-handling can still drop or distort a low-sample series.

Demonstrated on the correctness fixture below (real VM):

| Window | `last_over_time` (correct) | `increase(... ) > 0` (wrong) |
|---|---|---|
| 1d  | **4** | 3 |
| 7d  | **5** | 4 |
| 30d | **6** | 5 |

`increase()` undercounts by exactly the single-sample / reset operators the
fixture plants. This is why the task mandates `last_over_time`.

## Correctness fixture

Loaded into a real VictoriaMetrics via `/api/v1/import`, evaluated at `now`.
Each operator is planted to exercise a specific membership case:

| operator_id | samples | windows it should count in | case under test |
|---|---|---|---|
| `1` | 2 samples today, monotonic | DAU, WAU, MAU | ordinary active operator |
| `2` | **single** sample 3 days ago | WAU, MAU | single-sample (kills `increase`) |
| `3` | 2 samples 20 days ago | MAU | mid-window |
| `4` | 1 sample 45 days ago | *(none)* | out-of-window exclusion |
| `5` | 4 samples today incl. a **reset** (6 → 1) | DAU, WAU, MAU | counter reset inside window |
| `6` | **two** label series today (`mutation/kbeads` + `spawn/slack`) | DAU, WAU, MAU | `count by(operator_id)` collapse → 1 |
| `0` | 1 sample today | DAU, WAU, MAU | unattributed sentinel still counted |

Expected distinct counts:

- **DAU** `{1,5,6,0}` = **4**
- **WAU** `{1,2,5,6,0}` = **5**
- **MAU** `{1,2,3,5,6,0}` = **6**

This is asserted end-to-end through the production `VMClient` /
`computeUniqueness` code path by
`kbeads/internal/server/charts_uniqueness_vm_test.go`
(`TestUniquenessVMFixture_DistinctCountsCorrect`), gated on
`BEADS_TEST_VMSELECT_URL`. It **passed** against VictoriaMetrics v1.108.1.

The stubbed-VM unit tests
(`charts_uniqueness_test.go`) additionally pin the PromQL string (fails if
anyone swaps in `increase`/`rate`), the parse path, the 503-when-unconfigured
behaviour, and the cache collapse — these run in normal CI with no VM.

## p99 benchmark

- **Engine:** VictoriaMetrics v1.108.1 (matches `gardenio/helm/gardenio/values.yaml` `vmsingle.image.tag`).
- **Scale:** 500 distinct operators, ~870 active series (1–3 surfaces/operator,
  samples spread over 30 days) — at/above the plan's stated O(50–500) operator
  ceiling.
- **Driver:** the real `VMClient.QueryScalar` Go code path (not curl), 10
  warm-up + 500 measured iterations per window.

| Window | distinct count | p50 | p90 | **p99** | max |
|---|---|---|---|---|---|
| DAU (1d)  | 134 | 0.63 ms | 0.75 ms | **1.74 ms** | 2.73 ms |
| WAU (7d)  | 294 | 0.98 ms | 1.14 ms | **2.77 ms** | 4.44 ms |
| MAU (30d) | 501 | 1.50 ms | 1.86 ms | **3.96 ms** | 4.48 ms |

**Worst-case p99 ≈ 4 ms** vs. the 500 ms gate. There is a ~125× margin, and
the endpoint additionally has a 30 s L1 cache in front of it, so a dashboard
with all three panels open across many viewers costs at most one VM round-trip
per 30 s.

> Note: VictoriaMetrics reports `executionTimeMsec` of 0–4 ms internally; the
> numbers above are full client-observed round-trips (request build + HTTP +
> decode) from the daemon, so they already include serialization overhead.

## Decision: v2 rollups NOT built

The acceptance criteria gate v2 on `p99 > 500 ms`. Measured p99 is ~4 ms, so
the v2 precomputed-rollup work (`gardenio_operator_dau/wau/mau` gauges, kbeads
nightly `/api/v1/import/prometheus` push, staleness gauge, `>25h` alert) is
**deliberately not implemented**. Should series volume grow by two-plus orders
of magnitude and push p99 over the gate, re-run this benchmark (set
`BEADS_TEST_VMSELECT_URL` and the throwaway measurement described in the VM
test header) and revisit.

## Reproducing

```sh
# 1. real VM, same version as the chart pins
victoria-metrics-prod -httpListenAddr=127.0.0.1:8428 &

# 2. correctness (end-to-end through the production code path)
BEADS_TEST_VMSELECT_URL=http://127.0.0.1:8428 \
  go test ./internal/server/ -run VMFixture -v

# 3. stubbed-VM unit tests (no VM needed; run in CI)
go test ./internal/server/ -run 'Uniqueness|VMClient|ChartsUniqueness'
```
